//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
)

func TestQueryAndStream(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if databaseName(dbURL) != "voyage_test" {
		t.Fatal("DATABASE_URL must target voyage_test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admin, err := pgx.Connect(ctx, withDatabase(dbURL, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	wipeMarket(t, ctx, conn, "one-piece-http")
	conn.Close(ctx)
	origin := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	cfg := serveConfig{
		DatabaseURL: dbURL, HTTPAddr: "127.0.0.1:0", MarketID: "one-piece-http",
		Dataset: testDataset(), Seed: 7, Start: &origin, Interval: 2 * time.Millisecond,
		Probability: 10000, MaxMove: 20, Budget: 100000000000, Retention: 7, MaxClients: 8,
		SendBuffer: 64 * 1024,
	}
	if cfg.MarketID == "one-piece-dev" {
		t.Fatal("refusing the dev market")
	}
	svc, err := runServe(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	waitReady(t, svc.URL)
	if code := verifyCommand([]string{"--base-url", svc.URL, "--market", "one-piece-http", "--profile", "smoke"}, io.Discard); code != 0 {
		t.Fatalf("smoke %d", code)
	}
	assertGap(t, svc.URL)
	assertSlow(t, svc, dbURL)
	assertExpired(t, svc.URL, dbURL)
	svc.pause()
	waitStatus(t, svc.URL, "paused", http.StatusOK)
	if statusCode(t, svc.URL+"/readyz") != http.StatusServiceUnavailable {
		t.Fatal("paused readyz")
	}
	if statusCode(t, svc.URL+"/v1/markets/one-piece-http/snapshot") != http.StatusOK {
		t.Fatal("paused snapshot")
	}
	if err := svc.resume(); err != nil {
		t.Fatal(err)
	}
	waitReady(t, svc.URL)
	before := statusNumber(t, svc.URL, "logical_tick")
	if _, err := admin.Exec(ctx, `ALTER DATABASE voyage_test ALLOW_CONNECTIONS false`); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), `ALTER DATABASE voyage_test ALLOW_CONNECTIONS true`)
	if _, err := admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = 'voyage_test' AND pid <> pg_backend_pid()`); err != nil {
		t.Fatal(err)
	}
	waitCode(t, svc.URL+"/readyz", http.StatusServiceUnavailable)
	waitCode(t, svc.URL+"/v1/markets/one-piece-http/status", http.StatusServiceUnavailable)
	if _, err := admin.Exec(ctx, `ALTER DATABASE voyage_test ALLOW_CONNECTIONS true`); err != nil {
		t.Fatal(err)
	}
	waitReady(t, svc.URL)
	after := statusNumber(t, svc.URL, "logical_tick")
	if after <= before {
		t.Fatalf("tick %d did not pass %d", after, before)
	}
}

func assertGap(t *testing.T, base string) {
	t.Helper()
	snap := getBody(t, base+"/v1/markets/one-piece-http/snapshot")
	asOf, _ := strconv.ParseInt(asString(snap["as_of_seq"]), 10, 64)
	time.Sleep(40 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(base, "http", "ws", 1)+"/v1/stream?market_id=one-piece-http&after_seq="+strconv.FormatInt(asOf, 10), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var replay int64
	subscribed := false
	var seqs []int64
	for {
		_, payload, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Fatal(err)
		}
		switch asString(body["type"]) {
		case "subscribed":
			subscribed = true
			replay, _ = strconv.ParseInt(asString(body["replay_until_seq"]), 10, 64)
		case "trade", "session":
			seq, _ := strconv.ParseInt(asString(body["event_seq"]), 10, 64)
			seqs = append(seqs, seq)
		}
		if subscribed && len(seqs) > 0 && seqs[len(seqs)-1] > replay {
			break
		}
	}
	for i, seq := range seqs {
		if seq != asOf+int64(i)+1 {
			t.Fatalf("gap seqs %v from %d", seqs, asOf)
		}
	}
}

func assertSlow(t *testing.T, svc *service, dbURL string) {
	t.Helper()
	base := svc.URL
	svc.pause()
	waitStatus(t, base, "paused", http.StatusOK)
	defer svc.resume()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cursor := strconv.FormatInt(statusNumber(t, base, "last_event_seq"), 10)
	conn, _, err := websocket.Dial(ctx, strings.Replace(base, "http", "ws", 1)+"/v1/stream?market_id=one-piece-http&after_seq="+cursor, &websocket.DialOptions{HTTPClient: tightClient()})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	insertBurst(t, dbURL)
	time.Sleep(time.Second)
	sawClose := false
	reads := 0
	for !sawClose {
		_, _, err := conn.Read(ctx)
		if err == nil {
			reads++
			continue
		}
		var closed websocket.CloseError
		if !errors.As(err, &closed) || closed.Code != websocket.StatusTryAgainLater {
			t.Fatalf("slow close after %d frames: %v", reads, err)
		}
		sawClose = true
	}
	if err := svc.resume(); err != nil {
		t.Fatal(err)
	}
	waitReady(t, base)
	cursor = strconv.FormatInt(statusNumber(t, base, "last_event_seq"), 10)
	conn, _, err = websocket.Dial(ctx, strings.Replace(base, "http", "ws", 1)+"/v1/stream?market_id=one-piece-http&after_seq="+cursor, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if _, err := readBusiness(ctx, conn, 1); err != nil {
		t.Fatal(err)
	}
}

func insertBurst(t *testing.T, dbURL string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO market_events (
			market_id, event_seq, logical_tick, event_time, generated_at, market_date, type,
			instrument_id, price_cents, quantity, turnover_cents
		)
		SELECT m.market_id, m.last_event_seq + g, m.logical_tick, m.clock_origin, m.clock_origin, m.market_date,
		       'trade', 'codex:chr:1', 1000, 100, 100000
		FROM market_instances m
		CROSS JOIN generate_series(1, 4096) AS g
		WHERE m.market_id = 'one-piece-http'`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE market_instances SET last_event_seq = last_event_seq + 4096 WHERE market_id = 'one-piece-http'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func assertExpired(t *testing.T, base, dbURL string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `UPDATE market_instances SET first_retained_seq = last_event_seq + 1 WHERE market_id = 'one-piece-http'`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, `UPDATE market_instances SET first_retained_seq = 1 WHERE market_id = 'one-piece-http'`)
	if statusCode(t, base+"/v1/markets/one-piece-http/events?after_seq=0") != http.StatusGone {
		t.Fatal("expected 410")
	}
}

func wipeMarket(t *testing.T, ctx context.Context, conn *pgx.Conn, id string) {
	t.Helper()
	for _, query := range []string{
		`DELETE FROM bars_1m WHERE market_id = $1`,
		`DELETE FROM market_events WHERE market_id = $1`,
		`DELETE FROM instrument_states WHERE market_id = $1`,
		`DELETE FROM instruments WHERE market_id = $1`,
		`DELETE FROM market_instances WHERE market_id = $1`,
	} {
		if _, err := conn.Exec(ctx, query, id); err != nil {
			t.Fatal(err)
		}
	}
}

func waitReady(t *testing.T, base string) {
	t.Helper()
	waitCode(t, base+"/readyz", http.StatusOK)
}

func waitStatus(t *testing.T, base, state string, code int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if statusCode(t, base+"/v1/markets/one-piece-http/status") == code {
			body := getBody(t, base+"/v1/markets/one-piece-http/status")
			if asString(body["state"]) == state {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("status did not become %s", state)
}

func waitCode(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if statusCode(t, url) == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("code for %s did not become %d", url, want)
}

func statusCode(t *testing.T, url string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func statusNumber(t *testing.T, base, key string) int64 {
	t.Helper()
	body := getBody(t, base+"/v1/markets/one-piece-http/status")
	value, err := strconv.ParseInt(asString(body[key]), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func getBody(t *testing.T, url string) map[string]any {
	t.Helper()
	body, err := getJSON(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func tightClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetReadBuffer(64 * 1024)
		}
		return conn, nil
	}}}
}

func databaseName(url string) string {
	path := strings.Split(url, "?")[0]
	return path[strings.LastIndex(path, "/")+1:]
}

func withDatabase(url, name string) string {
	parts := strings.SplitN(url, "?", 2)
	base := parts[0]
	base = base[:strings.LastIndex(base, "/")+1] + name
	if len(parts) == 2 {
		return base + "?" + parts[1]
	}
	return base
}

func testDataset() []byte {
	return []byte(`{"schema_version":1,"dataset_id":"one-piece-c1-http","cutoff_chapter":1,"sources":[],"instruments":[{"instrument_id":"codex:chr:1","symbol":"D14001F4649BOP","name":"甲","sector":"草帽一伙","role":"leader","chapter_count":4,"popularity_rank":1},{"instrument_id":"codex:chr:2","symbol":"AAAAAAAAAAAAAAAAOP","name":"乙","sector":"海军","role":"member","chapter_count":2,"popularity_rank":11}]}`)
}
