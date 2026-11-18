package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestMaintainOpenRedactsPassword(t *testing.T) {
	text := runMaintainLog(t, func(r *Runner) {
		r.Open = func(context.Context) (*pgx.Conn, error) {
			return nil, fmt.Errorf("dial postgres://voyage:s3cret-pass@db:5432/voyage_test refused")
		}
	})
	if !strings.Contains(text, "market_id=one-piece-step") || !strings.Contains(text, "phase=open") {
		t.Fatalf("日志缺少市场或阶段：%s", text)
	}
	if strings.Contains(text, "s3cret-pass") || !strings.Contains(text, "redacted@") {
		t.Fatalf("口令未脱敏：%s", text)
	}
}

func TestMaintainFailureRedactsPassword(t *testing.T) {
	text := runMaintainLog(t, func(r *Runner) {
		r.Open = func(context.Context) (*pgx.Conn, error) { return nil, nil }
		r.maintainFunc = func(context.Context, *pgx.Conn) error {
			return fmt.Errorf("cleanup postgres://voyage:s3cret-pass@db/voyage failed")
		}
	})
	if !strings.Contains(text, "phase=maintain") || strings.Contains(text, "s3cret-pass") {
		t.Fatalf("清理失败日志：%s", text)
	}
}

func TestMaintainFailureRunsAgain(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan int, 2)
	release := make(chan struct{})
	r := &Runner{
		MarketID: "one-piece-step",
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Open: func(context.Context) (*pgx.Conn, error) {
			return nil, nil
		},
		maintainFunc: func(context.Context, *pgx.Conn) error {
			n := int(calls.Add(1))
			entered <- n
			if n == 1 {
				<-release
			}
			return fmt.Errorf("cleanup failed")
		},
	}
	r.startMaintain(context.Background())
	select {
	case n := <-entered:
		if n != 1 {
			t.Fatalf("第一次调用序号 %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("第一次维护没有执行")
	}
	close(release)
	r.mu.Lock()
	done := r.maintainDone
	r.mu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("第一次维护没有结束")
	}
	r.mu.Lock()
	still := r.maintaining
	r.mu.Unlock()
	if still {
		t.Fatal("失败后 maintaining 未复位")
	}
	r.startMaintain(context.Background())
	select {
	case n := <-entered:
		if n != 2 {
			t.Fatalf("第二次调用序号 %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("第二次维护没有执行")
	}
	r.mu.Lock()
	done = r.maintainDone
	r.mu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("第二次维护没有结束")
	}
	if calls.Load() != 2 {
		t.Fatalf("维护执行次数 %d", calls.Load())
	}
}

func TestMaintainCancelDoesNotLog(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &Runner{
		MarketID: "one-piece-step",
		Log:      slog.New(slog.NewTextHandler(&buf, nil)),
		Open: func(context.Context) (*pgx.Conn, error) {
			return nil, fmt.Errorf("postgres://voyage:s3cret-pass@db/voyage")
		},
	}
	r.maintain(ctx)
	if buf.Len() != 0 {
		t.Fatalf("退出期不应告警：%s", buf.String())
	}
}

func runMaintainLog(t *testing.T, setup func(*Runner)) string {
	t.Helper()
	var buf bytes.Buffer
	r := &Runner{
		MarketID: "one-piece-step",
		Log:      slog.New(slog.NewTextHandler(&buf, nil)),
	}
	setup(r)
	r.maintain(context.Background())
	return buf.String()
}
