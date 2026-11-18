package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// dsnUserinfo 匹配 scheme://user:password@，日志里换成 scheme://redacted@。
var dsnUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@]+:[^@\s/]+@`)

type Runner struct {
	MarketID        string
	Interval        time.Duration
	RetentionDays   int
	Hooks           Hooks
	Notify          chan struct{}
	Open            func(context.Context) (*pgx.Conn, error)
	Dial            func(context.Context) (*pgx.Conn, error)
	OnLock          func()
	OnStop          func()
	OnDBError       func(error)
	OnCommit        func(StepResult, time.Duration)
	OnMaintainError func(phase string, err error)
	Log             *slog.Logger
	Now             func() time.Time
	Sleep           func(context.Context, time.Duration)
	// maintainFunc 仅测试替换 Maintain。生产为空，走真实清理。
	maintainFunc func(context.Context, *pgx.Conn) error
	mu           sync.Mutex
	maintaining  bool
	maintainDone chan struct{}
}

func (r *Runner) Run(ctx context.Context, conn *pgx.Conn) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Sleep == nil {
		r.Sleep = sleepCtx
	}
	if r.Notify == nil {
		r.Notify = make(chan struct{}, 1)
	}
	if r.RetentionDays <= 0 {
		r.RetentionDays = 7
	}
	defer r.waitMaintain()
	key, err := lockMarket(ctx, conn, r.MarketID)
	if err != nil {
		return err
	}
	held := conn
	var previous *pgx.Conn
	if r.OnLock != nil {
		r.OnLock()
	}
	defer func() {
		if held != nil {
			r.releaseLock(held, key)
			held = nil
		}
		if previous != nil {
			previous.Close(context.Background())
			previous = nil
		}
		if r.OnStop != nil {
			r.OnStop()
		}
	}()
	backoff := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return nil
		}
		if held == nil {
			if r.Dial == nil {
				return fmt.Errorf("数据库连接已关闭")
			}
			next, dialErr := r.Dial(ctx)
			if dialErr != nil {
				r.noteDB(dialErr)
				r.Sleep(ctx, backoff)
				backoff = nextBackoff(backoff)
				continue
			}
			var lockErr error
			key, lockErr = lockMarket(ctx, next, r.MarketID)
			if lockErr != nil {
				next.Close(context.Background())
				if previous != nil {
					previous.Close(context.Background())
					previous = nil
				}
				if strings.Contains(lockErr.Error(), "同市场已经启动") {
					return lockErr
				}
				r.noteDB(lockErr)
				r.Sleep(ctx, backoff)
				backoff = nextBackoff(backoff)
				continue
			}
			if previous != nil {
				previous.Close(context.Background())
				previous = nil
			}
			held = next
			if r.OnLock != nil {
				r.OnLock()
			}
		}
		started := r.Now()
		stepCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		result, err := CommitNext(stepCtx, held, r.MarketID, r.Hooks)
		cancel()
		if err != nil {
			if errors.Is(err, ErrStoppedAfterCommit) {
				return err
			}
			if r.Dial != nil {
				r.releaseLock(held, key)
				previous = held
				previous.Close(context.Background())
				if previous.IsClosed() {
					previous = nil
				}
				held = nil
				r.noteDB(err)
			}
			if ctx.Err() != nil {
				return nil
			}
			r.Sleep(ctx, backoff)
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = 250 * time.Millisecond
		select {
		case r.Notify <- struct{}{}:
		default:
		}
		if r.OnCommit != nil {
			r.OnCommit(result, r.Now().Sub(started))
		}
		if engineMaintain(result.Tick) && r.Open != nil {
			r.startMaintain(ctx)
		}
		wait := r.Interval - r.Now().Sub(started)
		if wait > 0 {
			r.Sleep(ctx, wait)
		}
	}
}

// releaseLock 释放本会话的写锁。调用失败或库返回未释放时记一条日志，不写连接串。
func (r *Runner) releaseLock(conn *pgx.Conn, key int64) {
	if conn == nil {
		return
	}
	var released bool
	err := conn.QueryRow(context.Background(), `SELECT pg_advisory_unlock($1)`, key).Scan(&released)
	if err == nil && released {
		return
	}
	logger := r.Log
	if logger == nil {
		logger = slog.Default()
	}
	detail := "未持有该写锁"
	if err != nil {
		detail = unlockDetail(err)
	}
	logger.Error("释放写锁失败", "market_id", r.MarketID, "err", detail)
}

func unlockDetail(err error) string {
	text := redactSecrets(err.Error())
	lower := strings.ToLower(text)
	if strings.Contains(text, "://") || strings.Contains(lower, "password=") || strings.Contains(lower, "postgres://") {
		return "数据库错误已省略连接信息"
	}
	return text
}

func (r *Runner) noteDB(err error) {
	if r.OnDBError != nil && err != nil {
		r.OnDBError(err)
	}
}

func lockMarket(ctx context.Context, conn *pgx.Conn, marketID string) (int64, error) {
	var key int64
	if err := conn.QueryRow(ctx, `SELECT writer_lock_key FROM market_instances WHERE market_id = $1`, marketID).Scan(&key); err != nil {
		return 0, err
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
		return 0, err
	}
	if !locked {
		return 0, fmt.Errorf("同市场已经启动")
	}
	return key, nil
}

func nextBackoff(current time.Duration) time.Duration {
	if current < 5*time.Second {
		current *= 2
	}
	if current > 5*time.Second {
		return 5 * time.Second
	}
	return current
}

func engineMaintain(tick int64) bool {
	return tick > 0 && tick%60 == 0
}

func (r *Runner) startMaintain(ctx context.Context) {
	r.mu.Lock()
	if r.maintaining {
		r.mu.Unlock()
		return
	}
	r.maintaining = true
	r.maintainDone = make(chan struct{})
	r.mu.Unlock()
	go r.maintain(ctx)
}

func (r *Runner) maintain(ctx context.Context) {
	defer func() {
		r.mu.Lock()
		r.maintaining = false
		done := r.maintainDone
		r.mu.Unlock()
		if done != nil {
			close(done)
		}
	}()
	conn, err := r.Open(ctx)
	if err != nil {
		r.noteMaintain(ctx, "open", err)
		return
	}
	if conn != nil {
		defer conn.Close(context.Background())
	}
	fn := r.maintainFunc
	if fn == nil {
		fn = func(callCtx context.Context, held *pgx.Conn) error {
			return Maintain(callCtx, held, r.MarketID, r.RetentionDays)
		}
	}
	if err := fn(ctx, conn); err != nil {
		r.noteMaintain(ctx, "maintain", err)
	}
}

// waitMaintain 让 Run 在返回前等在途清理结束，使 OnMaintainError 能在进程退出前落盘。
// 停机方另有 10 秒硬上限，这里不再套第二段超时。
func (r *Runner) waitMaintain() {
	r.mu.Lock()
	running := r.maintaining
	done := r.maintainDone
	r.mu.Unlock()
	if !running || done == nil {
		return
	}
	<-done
}

func (r *Runner) noteMaintain(ctx context.Context, phase string, err error) {
	if err == nil || ctx.Err() != nil {
		return
	}
	safe := errors.New(redactSecrets(err.Error()))
	logger := r.Log
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("保留期维护失败", "market_id", r.MarketID, "phase", phase, "err", safe.Error())
	if r.OnMaintainError != nil {
		r.OnMaintainError(phase, safe)
	}
}

func redactSecrets(text string) string {
	return dsnUserinfo.ReplaceAllString(text, "${1}redacted@")
}

func sleepCtx(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
