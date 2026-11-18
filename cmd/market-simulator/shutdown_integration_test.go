//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"voyage-feed/store"
)

func TestServeHelper(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	os.Exit(serveCommand(nil, os.Stderr))
}

func TestShutdownStopsCleanly(t *testing.T) {
	dbURL := requireTestDB(t)
	cancel, svc := startManaged(t, dbURL, "one-piece-stop-clean")
	defer cancel()
	waitDBTick(t, dbURL, "one-piece-stop-clean", 2)
	before, _ := marketWatermark(t, dbURL, "one-piece-stop-clean")
	started := time.Now()
	cancel()
	waitDone(t, svc.done, 3*time.Second, "正常停机")
	if time.Since(started) > 3*time.Second {
		t.Fatalf("正常停机耗时 %s", time.Since(started))
	}
	time.Sleep(200 * time.Millisecond)
	after, seq := marketWatermark(t, dbURL, "one-piece-stop-clean")
	again, againSeq := marketWatermark(t, dbURL, "one-piece-stop-clean")
	if after != again || seq != againSeq || after < before {
		t.Fatalf("停机后水位 %d/%d -> %d/%d，停前 %d", after, seq, again, againSeq, before)
	}
	if !advisoryFree(t, dbURL, "one-piece-stop-clean") {
		t.Fatal("写锁没有释放")
	}
}

func TestShutdownWaitsForCommit(t *testing.T) {
	dbURL := requireTestDB(t)
	cancel, svc := startManaged(t, dbURL, "one-piece-stop-early")
	defer cancel()
	hold := holdRow(t, dbURL, "one-piece-stop-early")
	defer hold.conn.Close(context.Background())
	time.Sleep(150 * time.Millisecond)
	before, _ := marketWatermark(t, dbURL, "one-piece-stop-early")
	time.Sleep(100 * time.Millisecond)
	frozen, _ := marketWatermark(t, dbURL, "one-piece-stop-early")
	if frozen != before {
		t.Fatalf("持锁期间时钟仍前进 %d -> %d", before, frozen)
	}
	cancel()
	select {
	case <-svc.done:
		t.Fatal("停机没有等待提交")
	case <-time.After(200 * time.Millisecond):
	}
	if err := hold.tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDone(t, svc.done, 5*time.Second, "释放锁后停机")
	after, _ := marketWatermark(t, dbURL, "one-piece-stop-early")
	if after < before+1 {
		t.Fatalf("被挡住的一步没有提交 %d -> %d", before, after)
	}
}

func TestShutdownTransactionTimeout(t *testing.T) {
	dbURL := requireTestDB(t)
	cancel, svc := startManaged(t, dbURL, "one-piece-stop-hold")
	defer cancel()
	hold := holdRow(t, dbURL, "one-piece-stop-hold")
	defer hold.conn.Close(context.Background())
	defer hold.tx.Rollback(context.Background())
	time.Sleep(150 * time.Millisecond)
	before, _ := marketWatermark(t, dbURL, "one-piece-stop-hold")
	time.Sleep(80 * time.Millisecond)
	frozen, _ := marketWatermark(t, dbURL, "one-piece-stop-hold")
	if frozen != before {
		t.Fatalf("持锁期间时钟仍前进 %d -> %d", before, frozen)
	}
	started := time.Now()
	cancel()
	probed := make(chan struct{})
	go func() {
		deadline := time.Now().Add(9 * time.Second)
		for time.Now().Before(deadline) {
			if probeShutting(svc.URL, "one-piece-stop-hold") {
				close(probed)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	waitDone(t, svc.done, 13*time.Second, "事务超时停机")
	elapsed := time.Since(started)
	if elapsed > 13*time.Second {
		t.Fatalf("退出 %s 超过 13 秒", elapsed)
	}
	if elapsed < 8*time.Second {
		t.Fatalf("退出 %s，没有等到 10 秒事务上限", elapsed)
	}
	select {
	case <-probed:
	default:
		t.Fatal("窗口内没有同时看到新订阅 503 shutting_down 和 status 200")
	}
	after, _ := marketWatermark(t, dbURL, "one-piece-stop-hold")
	if after != before {
		t.Fatalf("未提交的一步改变了持久水位 %d -> %d", before, after)
	}
}

func TestRestartNewProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		return
	}
	dbURL := requireTestDB(t)
	const market = "one-piece-restart"
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, conn); err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	wipeMarket(t, ctx, conn, market)
	conn.Close(ctx)
	dataset := writeTinyDataset(t)
	addr := freeAddr(t)
	first, firstCancel := startHelper(t, dbURL, market, dataset, addr)
	base := "http://" + addr
	waitReady(t, base)
	waitDBTick(t, dbURL, market, 3)
	firstCancel()
	waitHelper(t, first, dbURL)
	if !advisoryFree(t, dbURL, market) {
		t.Fatal("第一代退出后写锁仍被占用")
	}
	tick1, seq1 := marketWatermark(t, dbURL, market)
	time.Sleep(200 * time.Millisecond)
	tick2, seq2 := marketWatermark(t, dbURL, market)
	if tick1 != tick2 || seq1 != seq2 {
		t.Fatalf("停稳后水位仍变化 %d/%d -> %d/%d", tick1, seq1, tick2, seq2)
	}
	stopped := eventSignature(t, dbURL, market, seq1)
	second, secondCancel := startHelper(t, dbURL, market, dataset, addr)
	waitReady(t, base)
	waitDBTick(t, dbURL, market, tick1+2)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, seq := marketWatermark(t, dbURL, market)
		if seq > seq1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("新进程没有写出新事件")
		}
		time.Sleep(20 * time.Millisecond)
	}
	secondCancel()
	waitHelper(t, second, dbURL)
	minSeq, maxSeq, count, minTick := newEventStats(t, dbURL, market, seq1)
	if minSeq != seq1+1 {
		t.Fatalf("首条新增序号 %d，停稳序号 %d", minSeq, seq1)
	}
	if minTick != tick1+1 {
		t.Fatalf("新事件最小逻辑步 %d，停稳时钟 %d", minTick, tick1)
	}
	if count != maxSeq-seq1 {
		t.Fatalf("新事件条数 %d，序号跨度 %d", count, maxSeq-seq1)
	}
	if eventSignature(t, dbURL, market, seq1) != stopped {
		t.Fatal("停稳序号及以前的事件签名变化")
	}
	assertBarsMatchTrades(t, dbURL, market)
	assertSharedMinute(t, dbURL, market, seq1, tick1)
}

type helperProc struct {
	cmd *exec.Cmd
	out *bytes.Buffer
}

type rowHold struct {
	conn *pgx.Conn
	tx   pgx.Tx
}

func requireTestDB(t *testing.T) string {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if databaseName(dbURL) != "voyage_test" {
		t.Fatal("DATABASE_URL must target voyage_test")
	}
	return dbURL
}

func tinyDataset() []byte {
	return []byte(`{"schema_version":1,"dataset_id":"one-piece-c1-restart","cutoff_chapter":1,"sources":[],"instruments":[{"instrument_id":"codex:chr:1","symbol":"D14001F4649BOP","name":"甲","sector":"草帽一伙","role":"leader","chapter_count":4,"popularity_rank":1},{"instrument_id":"codex:chr:2","symbol":"AAAAAAAAAAAAAAAAOP","name":"乙","sector":"海军","role":"member","chapter_count":2,"popularity_rank":11}]}`)
}

func startManaged(t *testing.T, dbURL, market string) (context.CancelFunc, *service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	wipeMarket(t, ctx, conn, market)
	conn.Close(ctx)
	origin := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	cfg := serveConfig{
		DatabaseURL: dbURL, HTTPAddr: "127.0.0.1:0", MarketID: market,
		Dataset: tinyDataset(), Seed: 7, Start: &origin, Interval: 10 * time.Millisecond,
		Probability: 10000, MaxMove: 20, Budget: 100000000000, Retention: 7, MaxClients: 8,
	}
	svc, err := runServe(ctx, cfg)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	waitReady(t, svc.URL)
	return cancel, svc
}

func holdRow(t *testing.T, dbURL, market string) *rowHold {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(context.Background())
	if err != nil {
		conn.Close(context.Background())
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var id string
		done <- tx.QueryRow(context.Background(), `SELECT market_id FROM market_instances WHERE market_id = $1 FOR UPDATE`, market).Scan(&id)
	}()
	select {
	case err := <-done:
		if err != nil {
			tx.Rollback(context.Background())
			conn.Close(context.Background())
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("锁住市场行超时")
	}
	return &rowHold{conn: conn, tx: tx}
}

func marketWatermark(t *testing.T, dbURL, market string) (int64, int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var tick, seq int64
	if err := conn.QueryRow(ctx, `SELECT logical_tick, last_event_seq FROM market_instances WHERE market_id = $1`, market).Scan(&tick, &seq); err != nil {
		t.Fatal(err)
	}
	return tick, seq
}

func waitDBTick(t *testing.T, dbURL, market string, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tick, _ := marketWatermark(t, dbURL, market)
		if tick >= want {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("tick 没有到达 %d", want)
}

func advisoryFree(t *testing.T, dbURL, market string) bool {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var key int64
	if err := conn.QueryRow(ctx, `SELECT writer_lock_key FROM market_instances WHERE market_id = $1`, market).Scan(&key); err != nil {
		t.Fatal(err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	if locked {
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key)
	}
	return locked
}

func waitDone(t *testing.T, done <-chan struct{}, limit time.Duration, label string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("%s超过 %s", label, limit)
	}
}

func probeShutting(base, market string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	wsURL := strings.Replace(base, "http", "ws", 1) + "/v1/stream?market_id=" + market + "&after_seq=0"
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		conn.CloseNow()
		return false
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		if resp != nil {
			resp.Body.Close()
		}
		return false
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "shutting_down") {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/markets/"+market+"/status", nil)
	if err != nil {
		return false
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	return res.StatusCode == http.StatusOK
}

func writeTinyDataset(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tiny.json")
	if err := os.WriteFile(path, tinyDataset(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func startHelper(t *testing.T, dbURL, market, dataset, addr string) (*helperProc, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServeHelper$", "--")
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
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 20 * time.Second
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	return &helperProc{cmd: cmd, out: &out}, cancel
}

func waitHelper(t *testing.T, proc *helperProc, dbURL string) {
	t.Helper()
	err := proc.cmd.Wait()
	state := proc.cmd.ProcessState
	// Go 在 Cancel 成功后，即使子进程退出码为 0，Wait 也会返回上下文错误。
	// 停稳硬门看进程退出码，不把这个包装错误当成失败。
	if state != nil && state.Success() {
		return
	}
	text := strings.ReplaceAll(proc.out.String(), dbURL, "DATABASE_URL")
	if state != nil {
		t.Fatalf("进程退出码 %d：%v\n%s", state.ExitCode(), err, text)
	}
	t.Fatalf("进程退出：%v\n%s", err, text)
}

func eventSignature(t *testing.T, dbURL, market string, until int64) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `
		SELECT event_seq, logical_tick, type, COALESCE(instrument_id, ''), COALESCE(price_cents, 0), COALESCE(quantity, 0), COALESCE(turnover_cents, 0)
		FROM market_events WHERE market_id = $1 AND event_seq <= $2 ORDER BY event_seq`, market, until)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var seq, tick, price, qty, turn int64
		var kind, instrument string
		if err := rows.Scan(&seq, &tick, &kind, &instrument, &price, &qty, &turn); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%d %d %s %s %d %d %d\n", seq, tick, kind, instrument, price, qty, turn)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func newEventStats(t *testing.T, dbURL, market string, afterSeq int64) (int64, int64, int64, int64) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var minSeq, maxSeq, count, minTick int64
	err = conn.QueryRow(ctx, `
		SELECT min(event_seq), max(event_seq), count(*), min(logical_tick)
		FROM market_events WHERE market_id = $1 AND event_seq > $2`, market, afterSeq).Scan(&minSeq, &maxSeq, &count, &minTick)
	if err != nil {
		t.Fatal(err)
	}
	return minSeq, maxSeq, count, minTick
}

func assertBarsMatchTrades(t *testing.T, dbURL, market string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `
		SELECT instrument_id, date_trunc('minute', event_time), sum(quantity), sum(turnover_cents), count(*)
		FROM market_events WHERE market_id = $1 AND type = 'trade'
		GROUP BY 1, 2`, market)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type key struct {
		id     string
		bucket time.Time
	}
	want := map[key][3]int64{}
	for rows.Next() {
		var id string
		var bucket time.Time
		var volume, turnover, count int64
		if err := rows.Scan(&id, &bucket, &volume, &turnover, &count); err != nil {
			t.Fatal(err)
		}
		want[key{id, bucket.UTC()}] = [3]int64{volume, turnover, count}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	var stored int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM bars_1m WHERE market_id = $1`, market).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != len(want) {
		t.Fatalf("K 线 %d 根，逐笔回算 %d 根", stored, len(want))
	}
	for item, value := range want {
		var volume, turnover, count int64
		err := conn.QueryRow(ctx, `
			SELECT volume, turnover_cents, trade_count FROM bars_1m
			WHERE market_id = $1 AND instrument_id = $2 AND bucket_start = $3`,
			market, item.id, item.bucket).Scan(&volume, &turnover, &count)
		if err != nil {
			t.Fatal(err)
		}
		if volume != value[0] || turnover != value[1] || count != value[2] {
			t.Fatalf("K 线量额 %s %s 库存 %d %d %d 回算 %d %d %d", item.id, item.bucket, volume, turnover, count, value[0], value[1], value[2])
		}
	}
}

func assertSharedMinute(t *testing.T, dbURL, market string, baselineSeq, baselineTick int64) {
	t.Helper()
	if baselineTick%60 == 49 {
		t.Logf("停稳 tick mod 60 == 49，跨代同分钟桶计数断言豁免")
		return
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `
		SELECT old.instrument_id, old.bucket
		FROM (
			SELECT instrument_id, date_trunc('minute', event_time) AS bucket
			FROM market_events WHERE market_id = $1 AND event_seq <= $2 AND type = 'trade'
		) old
		JOIN (
			SELECT instrument_id, date_trunc('minute', event_time) AS bucket
			FROM market_events WHERE market_id = $1 AND event_seq > $2 AND type = 'trade'
		) new USING (instrument_id, bucket)`, market, baselineSeq)
	if err != nil {
		t.Fatal(err)
	}
	type sharedBar struct {
		id     string
		bucket time.Time
	}
	var shared []sharedBar
	for rows.Next() {
		var item sharedBar
		if err := rows.Scan(&item.id, &item.bucket); err != nil {
			t.Fatal(err)
		}
		shared = append(shared, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, item := range shared {
		var count int64
		err := conn.QueryRow(ctx, `
			SELECT trade_count FROM bars_1m
			WHERE market_id = $1 AND instrument_id = $2 AND bucket_start = $3`, market, item.id, item.bucket).Scan(&count)
		if err != nil {
			t.Fatal(err)
		}
		if count < 1 {
			t.Fatalf("跨代同分钟桶计数 %d", count)
		}
	}
}
