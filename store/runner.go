package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

type Runner struct {
	MarketID       string
	Interval       time.Duration
	RetentionDays  int
	Hooks          Hooks
	Notify         chan struct{}
	Open           func(context.Context) (*pgx.Conn, error)
	Dial           func(context.Context) (*pgx.Conn, error)
	OnLock         func()
	OnStop         func()
	OnDBError      func(error)
	OnCommit       func(StepResult, time.Duration)
	Now            func() time.Time
	Sleep          func(context.Context, time.Duration)
	mu             sync.Mutex
	maintaining    bool
	maintainFailed error
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
	key, err := lockMarket(ctx, conn, r.MarketID)
	if err != nil {
		return err
	}
	held := conn
	if r.OnLock != nil {
		r.OnLock()
	}
	defer func() {
		if held != nil {
			held.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
			held = nil
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
				if strings.Contains(lockErr.Error(), "同市场已经启动") {
					return lockErr
				}
				r.noteDB(lockErr)
				r.Sleep(ctx, backoff)
				backoff = nextBackoff(backoff)
				continue
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
				held.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
				held.Close(context.Background())
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
	r.mu.Unlock()
	go r.maintain(ctx)
}

func (r *Runner) maintain(ctx context.Context) {
	defer func() {
		r.mu.Lock()
		r.maintaining = false
		r.mu.Unlock()
	}()
	conn, err := r.Open(ctx)
	if err != nil {
		r.maintainFailed = err
		return
	}
	defer conn.Close(ctx)
	if err := Maintain(ctx, conn, r.MarketID, r.RetentionDays); err != nil {
		r.maintainFailed = err
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
