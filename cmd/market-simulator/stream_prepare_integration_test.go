//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
)

const streamMarket = "one-piece-s41b"

func TestStreamPrepareStatuses(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if databaseName(dbURL) != "voyage_test" {
		t.Fatal("DATABASE_URL must target voyage_test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	wipeMarket(t, ctx, conn, streamMarket)
	conn.Close(ctx)
	origin := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	cfg := serveConfig{
		DatabaseURL: dbURL, HTTPAddr: "127.0.0.1:0", MarketID: streamMarket,
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

	// 等水位越过 2500：409 用例要回退 2000，缺口必须稳定超过 1024。
	high := int64(0)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if high = streamStatusNumber(t, svc.URL, "last_event_seq"); high > 2500 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if high <= 2500 {
		t.Fatalf("last_event_seq %d did not pass 2500", high)
	}

	// 升级前 400 bad_cursor：after_seq 晚于已提交序号，+100000 的余量
	// 覆盖读取与请求之间引擎继续推进的水位。
	code, item := streamError(t, svc.URL+"/v1/stream?market_id="+streamMarket+"&after_seq="+
		strconv.FormatInt(streamStatusNumber(t, svc.URL, "last_event_seq")+100000, 10))
	if code != http.StatusBadRequest || asString(item["code"]) != "bad_cursor" {
		t.Fatalf("bad_cursor got %d %v", code, item)
	}

	// 升级前 409 replay_too_large：回退 2000 形成的缺口远超 1024 的补发上限，
	// 水位上涨只会让缺口更大。
	code, item = streamError(t, svc.URL+"/v1/stream?market_id="+streamMarket+"&after_seq="+
		strconv.FormatInt(streamStatusNumber(t, svc.URL, "last_event_seq")-2000, 10))
	if code != http.StatusConflict || asString(item["code"]) != "replay_too_large" {
		t.Fatalf("replay_too_large got %d %v", code, item)
	}

	// 升级前 410 cursor_expired：暂停后冻结水位并把保留边界推到该值，
	// after_seq=0 必须拿到 410。S4.1 起 Prepare 的 410 详情使用
	// PrepareSnapshot 同一只读快照内的保留边界，因此 details 里的
	// first_retained_seq 应等于暂停时冻结的 last_event_seq，而不是
	// Prepare 之外另读一次水位的值。
	svc.pause()
	streamWaitStatus(t, svc.URL, "paused")
	admin, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var frozen int64
	if err := admin.QueryRow(ctx, `SELECT last_event_seq FROM market_instances WHERE market_id = $1`, streamMarket).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `UPDATE market_instances SET first_retained_seq = last_event_seq WHERE market_id = $1`, streamMarket); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), `UPDATE market_instances SET first_retained_seq = 1 WHERE market_id = $1`, streamMarket)
	code, item = streamError(t, svc.URL+"/v1/stream?market_id="+streamMarket+"&after_seq=0")
	if code != http.StatusGone || asString(item["code"]) != "cursor_expired" {
		t.Fatalf("cursor_expired got %d %v", code, item)
	}
	details, _ := item["details"].(map[string]any)
	if asString(details["first_retained_seq"]) != strconv.FormatInt(frozen, 10) {
		t.Fatalf("first_retained_seq %v want frozen %d", details, frozen)
	}
	if _, err := admin.Exec(ctx, `UPDATE market_instances SET first_retained_seq = 1 WHERE market_id = $1`, streamMarket); err != nil {
		t.Fatal(err)
	}
	if err := svc.resume(); err != nil {
		t.Fatal(err)
	}
	waitReady(t, svc.URL)

	// 并发建连补发连续性：3 个连接同时在引擎推进下建连，各自从 dial 前快照
	// 水位回退不同偏移，Prepare 与引擎并发提交时单快照内的补发与实时投递
	// 必须首尾相接，任何缺号即失败。
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, offset := range []int64{50, 10, 2} {
		wg.Add(1)
		go func(offset int64) {
			defer wg.Done()
			errs <- streamFollow(svc.URL, offset)
		}(offset)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// streamFollow 读快照水位后按偏移回退建连，先取 subscribed 的
// replay_until_seq，再累计 30 条业务帧，校验 event_seq 从 after+1 起
// 严格连续递增（覆盖补发与实时投递的衔接处）。
func streamFollow(base string, offset int64) error {
	snap, err := getJSON(context.Background(), base+"/v1/markets/"+streamMarket+"/snapshot")
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	asOf, err := strconv.ParseInt(asString(snap["as_of_seq"]), 10, 64)
	if err != nil {
		return fmt.Errorf("as_of_seq: %w", err)
	}
	after := asOf - offset
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(base, "http", "ws", 1)+
		"/v1/stream?market_id="+streamMarket+"&after_seq="+strconv.FormatInt(after, 10), nil)
	if err != nil {
		return fmt.Errorf("dial after=%d: %w", after, err)
	}
	defer conn.CloseNow()
	high := int64(-1)
	for high < 0 {
		_, payload, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("subscribed after=%d: %w", after, err)
		}
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			return fmt.Errorf("frame json: %w", err)
		}
		if asString(body["type"]) == "subscribed" {
			high, _ = strconv.ParseInt(asString(body["replay_until_seq"]), 10, 64)
		}
	}
	if high < after {
		return fmt.Errorf("after=%d replay_until_seq=%d", after, high)
	}
	frames, err := readBusiness(ctx, conn, 30)
	if err != nil {
		return fmt.Errorf("after=%d high=%d frames=%d: %w", after, high, len(frames), err)
	}
	seqs := make([]int64, 0, len(frames))
	for _, body := range frames {
		seq, err := strconv.ParseInt(asString(body["event_seq"]), 10, 64)
		if err != nil {
			return fmt.Errorf("event_seq %v: %w", body["event_seq"], err)
		}
		seqs = append(seqs, seq)
	}
	for i, seq := range seqs {
		if seq != after+int64(i)+1 {
			return fmt.Errorf("after=%d high=%d: seq #%d is %d want %d, seqs %v", after, high, i, seq, after+int64(i)+1, seqs)
		}
	}
	return nil
}

// streamError 用普通 GET 请求 /v1/stream，返回状态码与解析后的 error 对象，
// 用于断言升级握手失败路径的 JSON 错误。
func streamError(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		t.Fatal(err)
	}
	item, _ := body["error"].(map[string]any)
	if item == nil {
		t.Fatalf("%s body without error object: %v", url, body)
	}
	return resp.StatusCode, item
}

func streamStatusNumber(t *testing.T, base, key string) int64 {
	t.Helper()
	body := getBody(t, base+"/v1/markets/"+streamMarket+"/status")
	value, err := strconv.ParseInt(asString(body[key]), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func streamWaitStatus(t *testing.T, base, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if statusCode(t, base+"/v1/markets/"+streamMarket+"/status") == http.StatusOK {
			body := getBody(t, base+"/v1/markets/"+streamMarket+"/status")
			if asString(body["state"]) == state {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("status did not become %s", state)
}
