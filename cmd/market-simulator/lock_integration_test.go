//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"voyage-feed/store"
)

func TestAdvisoryLockConflictExitsNonZero(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		return
	}
	dbURL := requireTestDB(t)
	const market = "one-piece-lock"
	ctx := context.Background()
	holder, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(context.Background())
	if err := store.Migrate(ctx, holder); err != nil {
		t.Fatal(err)
	}
	wipeMarket(t, ctx, holder, market)
	origin := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	if _, err := store.Publish(ctx, holder, store.PublishConfig{
		MarketID: market, Dataset: tinyDataset(), Seed: 7, TradeProbabilityBps: 10000,
		MaxMoveBps: 20, CapBudgetCents: 100000000000, ClockOrigin: origin,
	}); err != nil {
		t.Fatal(err)
	}
	var key int64
	if err := holder.QueryRow(ctx, `SELECT writer_lock_key FROM market_instances WHERE market_id = $1`, market).Scan(&key); err != nil {
		t.Fatal(err)
	}
	var locked bool
	if err := holder.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("测试连接没有持有写锁")
	}
	defer holder.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)

	dataset := writeTinyDataset(t)
	addr := freeAddr(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestServeHelper$", "--")
	cmd.Env = []string{
		"GO_WANT_HELPER_PROCESS=1",
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"DATABASE_URL=" + dbURL,
		"HTTP_ADDR=" + addr,
		"MARKET_ID=" + market,
		"DATASET_PATH=" + dataset,
		"MARKET_SEED=7",
		"MARKET_START_TIME=2026-11-18T00:00:00Z",
		"TICK_INTERVAL_MS=10",
		"TRADE_PROBABILITY_BPS=10000",
		"MAX_MOVE_BPS=20",
		"INITIAL_CAP_BUDGET_CENTS=100000000000",
		"EVENT_RETENTION_DAYS=7",
		"MAX_STREAM_CLIENTS=8",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	listened := make(chan struct{})
	go func() {
		client := &http.Client{Timeout: 100 * time.Millisecond}
		url := "http://" + addr + "/healthz"
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := client.Get(url)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					close(listened)
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	var stopped error
	select {
	case stopped = <-waitErr:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-waitErr
		t.Fatal("进程没有因抢锁失败自行退出")
	}
	select {
	case <-listened:
	default:
		t.Fatalf("退出前没有开始监听\n%s", hideURL(out.String(), dbURL))
	}
	state := cmd.ProcessState
	text := hideURL(out.String(), dbURL)
	if state == nil {
		t.Fatalf("没有进程状态：%v\n%s", stopped, text)
	}
	if state.ExitCode() == 0 {
		t.Fatalf("抢锁失败退出码为 0，Wait=%v\n%s", stopped, text)
	}
	if state.ExitCode() < 0 {
		t.Fatalf("进程被信号结束，退出码 %d，Wait=%v\n%s", state.ExitCode(), stopped, text)
	}
	if !strings.Contains(text, "同市场已经启动") {
		t.Fatalf("stderr 没有同市场已经启动，退出码 %d，Wait=%v\n%s", state.ExitCode(), stopped, text)
	}

	probe, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close(ctx)
	var acquired bool
	if err := probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired); err != nil {
		t.Fatal(err)
	}
	if acquired {
		_, _ = probe.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key)
		t.Fatal("退出后写锁已经不在原持有者手上")
	}
	var released bool
	if err := holder.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&released); err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("原持有者已经不再持有写锁")
	}
}

func hideURL(text, dbURL string) string {
	if dbURL == "" {
		return text
	}
	return strings.ReplaceAll(text, dbURL, "DATABASE_URL")
}
