//go:build integration

package main

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestBarsWindowIntegrity 验证 S4.1 的 /bars 修复：
//  1. SQL 上界从 to 扩展为 barWindow(interval,to).End，保证聚合周期内分钟线完整；
//  2. 响应顶层新增 as_of_seq（十进制字符串）。
//
// complete 仍按 simNow 判定（!simNow.Before(bar.End)），simNow = clock_origin +
// logical_tick 秒。全部期望值为手算常量。引擎暂停期间 GET /bars 走只读事务，
// 全程不需要 resume。
func TestBarsWindowIntegrity(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if databaseName(dbURL) != "voyage_test" {
		t.Fatal("DATABASE_URL must target voyage_test")
	}
	const market = "one-piece-s41"
	const symbolChr1 = "D14001F4649BOP"
	const symbolChr2 = "AAAAAAAAAAAAAAAAOP"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	wipeMarket(t, ctx, conn, market)
	if err := conn.Close(ctx); err != nil {
		t.Fatal(err)
	}
	origin := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	cfg := serveConfig{
		DatabaseURL: dbURL, HTTPAddr: "127.0.0.1:0", MarketID: market,
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
	waitReady(t, svc.URL)
	svc.pause()
	barsWaitStatus(t, svc.URL, market, "paused")

	// 引擎已停，直连 DB 准备稳定数据。simNow = origin + 37800s = 10:30:00Z，
	// logical_tick 与 last_event_seq 同一 UPDATE，保证 simNow 与 as_of_seq 取自同一行。
	seed, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close(context.Background())
	if _, err := seed.Exec(ctx, `
		UPDATE market_instances SET logical_tick = 37800, last_event_seq = 7777
		WHERE market_id = $1`, market); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(ctx, `DELETE FROM bars_1m WHERE market_id = $1`, market); err != nil {
		t.Fatal(err)
	}
	// 设计意图：10:00-10:05 周期的最高 130 与收 128 在 to=10:03Z 之后的 10:03/10:04
	// 分钟线里，旧上界（=to）只能读到 10:02，部分和（量 60、高 120、收 92）不等于
	// 全和（量 150、高 130、收 128），旧代码（上界=to）必挂；最低 90 在 10:02，
	// 是两个口径一致的稳定值。
	if _, err := seed.Exec(ctx, `
		INSERT INTO bars_1m (
			market_id, instrument_id, bucket_start, open_cents, high_cents, low_cents, close_cents,
			volume, turnover_cents, trade_count, first_event_seq, last_event_seq
		) VALUES
			($1,'codex:chr:1','2026-11-18T10:00:00Z',100,110, 95,105,10,1050,3,1,1),
			($1,'codex:chr:1','2026-11-18T10:01:00Z',105,120,104,118,20,2360,4,2,2),
			($1,'codex:chr:1','2026-11-18T10:02:00Z',118,119, 90, 92,30,3300,5,3,3),
			($1,'codex:chr:1','2026-11-18T10:03:00Z', 92,100, 91, 99,40,3960,6,4,4),
			($1,'codex:chr:1','2026-11-18T10:04:00Z', 99,130, 98,128,50,6400,7,5,5),
			($1,'codex:chr:1','2026-11-18T10:30:00Z',200,210,199,205, 5,1025,2,6,6)`, market); err != nil {
		t.Fatal(err)
	}
	base := svc.URL + "/v1/markets/" + market + "/bars"
	first5m := barExpect{
		start: "2026-11-18T10:00:00Z", end: "2026-11-18T10:05:00Z",
		open: 100, high: 130, low: 90, close: 128,
		volume: 150, turnover: 17070, count: 25, complete: true,
	}

	// 场景 a：interval=5m。to=10:03Z 验证上界扩展——10:00 周期必须聚合到 10:04
	// 的分钟线，end_time（10:05Z）晚于 to 是修复的可观测差异点；10:30 周期的起点
	// 不小于 to，会被 [from,to) 起点过滤排除，因此第二根 10:30-10:35 周期用
	// to=10:31Z 的同窗口查询验证（此时 10:30 周期尚未走完，complete=false）。
	urlFive := base + "?symbol=" + symbolChr1 + "&interval=5m&from=2026-11-18T09:00:00Z&to=2026-11-18T10:03:00Z"
	asOf, items := barsFetch(t, urlFive)
	if asOf != "7777" {
		t.Fatalf("as_of_seq %s want 7777", asOf)
	}
	if len(items) != 1 {
		t.Fatalf("5m to=10:03Z items %d want 1", len(items))
	}
	assertBar(t, urlFive, items[0], first5m)
	toFive, err := time.Parse(time.RFC3339, "2026-11-18T10:03:00Z")
	if err != nil {
		t.Fatal(err)
	}
	endFive, err := time.Parse(time.RFC3339, asString(items[0]["end_time"]))
	if err != nil {
		t.Fatal(err)
	}
	if !endFive.After(toFive) {
		t.Fatalf("end_time %s 未晚于 to", asString(items[0]["end_time"]))
	}
	urlFiveLate := base + "?symbol=" + symbolChr1 + "&interval=5m&from=2026-11-18T09:00:00Z&to=2026-11-18T10:31:00Z"
	asOf, items = barsFetch(t, urlFiveLate)
	if asOf != "7777" {
		t.Fatalf("as_of_seq %s want 7777", asOf)
	}
	if len(items) != 2 {
		t.Fatalf("5m to=10:31Z items %d want 2", len(items))
	}
	assertBar(t, urlFiveLate, items[0], first5m)
	assertBar(t, urlFiveLate, items[1], barExpect{
		start: "2026-11-18T10:30:00Z", end: "2026-11-18T10:35:00Z",
		open: 200, high: 210, low: 199, close: 205,
		volume: 5, turnover: 1025, count: 2, complete: false,
	})

	// 场景 b：interval=1d。北京日 2026-11-18 的周期是
	// [2026-11-17T16:00Z, 2026-11-18T16:00Z)，周期起点必须落在 [from,to) 内，
	// from 取 2026-11-17T09:00Z；旧上界（=to=10:03Z）读不到 10:03/10:04/10:30
	// 分钟线，日线聚合必挂。simNow=10:30Z 未过 16:00Z，complete=false。
	urlDay := base + "?symbol=" + symbolChr1 + "&interval=1d&from=2026-11-17T09:00:00Z&to=2026-11-18T10:03:00Z"
	asOf, items = barsFetch(t, urlDay)
	if asOf != "7777" {
		t.Fatalf("as_of_seq %s want 7777", asOf)
	}
	if len(items) != 1 {
		t.Fatalf("1d items %d want 1", len(items))
	}
	dayBar := barExpect{
		start: "2026-11-17T16:00:00Z", end: "2026-11-18T16:00:00Z",
		open: 100, high: 210, low: 90, close: 205,
		volume: 155, turnover: 18095, count: 27, complete: false,
	}
	assertBar(t, urlDay, items[0], dayBar)

	// 场景 c：simNow 推进到 2026-11-19T16:00Z（logical_tick=144000），日线已收盘，
	// 同窗口查询 complete=true。
	if _, err := seed.Exec(ctx, `UPDATE market_instances SET logical_tick = 144000 WHERE market_id = $1`, market); err != nil {
		t.Fatal(err)
	}
	_, items = barsFetch(t, urlDay)
	if len(items) != 1 {
		t.Fatalf("1d closed items %d want 1", len(items))
	}
	dayBar.complete = true
	assertBar(t, urlDay, items[0], dayBar)

	// 场景 d：simNow==End 边界。logical_tick=36300 → simNow=10:05:00Z，
	// 5m 的 10:00 周期 End 恰为 10:05:00Z，complete=true。
	if _, err := seed.Exec(ctx, `UPDATE market_instances SET logical_tick = 36300 WHERE market_id = $1`, market); err != nil {
		t.Fatal(err)
	}
	_, items = barsFetch(t, urlFive)
	if len(items) != 1 {
		t.Fatalf("5m boundary items %d want 1", len(items))
	}
	assertBar(t, urlFive, items[0], first5m)

	// 场景 e：默认 interval=1m（省略 interval）。to=10:03:30Z 非整分，上界扩到
	// 10:04:00Z，10:00-10:03 四行均为种子原值；to=10:03:00Z 整分时 10:03 周期
	// 起点被 [from,to) 过滤排除，只剩 3 行，与旧行为一致。
	minute1 := barExpect{start: "2026-11-18T10:00:00Z", end: "2026-11-18T10:01:00Z",
		open: 100, high: 110, low: 95, close: 105, volume: 10, turnover: 1050, count: 3, complete: true}
	minute2 := barExpect{start: "2026-11-18T10:01:00Z", end: "2026-11-18T10:02:00Z",
		open: 105, high: 120, low: 104, close: 118, volume: 20, turnover: 2360, count: 4, complete: true}
	minute3 := barExpect{start: "2026-11-18T10:02:00Z", end: "2026-11-18T10:03:00Z",
		open: 118, high: 119, low: 90, close: 92, volume: 30, turnover: 3300, count: 5, complete: true}
	minute4 := barExpect{start: "2026-11-18T10:03:00Z", end: "2026-11-18T10:04:00Z",
		open: 92, high: 100, low: 91, close: 99, volume: 40, turnover: 3960, count: 6, complete: true}
	_, items = barsFetch(t, base+"?symbol="+symbolChr1+"&to=2026-11-18T10:03:30Z")
	if len(items) != 4 {
		t.Fatalf("1m to=10:03:30Z items %d want 4", len(items))
	}
	assertBar(t, base, items[0], minute1)
	assertBar(t, base, items[1], minute2)
	assertBar(t, base, items[2], minute3)
	assertBar(t, base, items[3], minute4)
	_, items = barsFetch(t, base+"?symbol="+symbolChr1+"&to=2026-11-18T10:03:00Z")
	if len(items) != 3 {
		t.Fatalf("1m to=10:03:00Z items %d want 3", len(items))
	}
	assertBar(t, base, items[0], minute1)
	assertBar(t, base, items[1], minute2)
	assertBar(t, base, items[2], minute3)

	// 场景 f：回归。非法周期 400 bad_request、未知标的 404 not_found、
	// 超过 30 天的窗口 400 bad_range。
	code, errBody := streamError(t, base+"?symbol="+symbolChr1+"&interval=3m")
	if code != http.StatusBadRequest || asString(errBody["code"]) != "bad_request" {
		t.Fatalf("interval=3m got (%d, %s)", code, asString(errBody["code"]))
	}
	code, errBody = streamError(t, base+"?symbol=NOPE")
	if code != http.StatusNotFound || asString(errBody["code"]) != "not_found" {
		t.Fatalf("symbol=NOPE got (%d, %s)", code, asString(errBody["code"]))
	}
	code, errBody = streamError(t, base+"?symbol="+symbolChr1+"&interval=1m&from=2026-10-01T00:00:00Z&to=2026-11-18T10:00:00Z")
	if code != http.StatusBadRequest || asString(errBody["code"]) != "bad_range" {
		t.Fatalf("30d window got (%d, %s)", code, asString(errBody["code"]))
	}

	// 场景 g：2000 根上限。第二标的批量写入 2001 根分钟桶（值全 1，自
	// 2026-11-18T00:00Z 起每分钟一根），1m 查询覆盖全部应返回 400 bad_range。
	if _, err := seed.Exec(ctx, `
		INSERT INTO bars_1m (
			market_id, instrument_id, bucket_start, open_cents, high_cents, low_cents, close_cents,
			volume, turnover_cents, trade_count, first_event_seq, last_event_seq
		)
		SELECT $1, 'codex:chr:2', TIMESTAMPTZ '2026-11-18T00:00:00Z' + g * interval '1 minute',
		       1, 1, 1, 1, 1, 1, 1, 1, 1
		FROM generate_series(0, 2000) AS g`, market); err != nil {
		t.Fatal(err)
	}
	code, errBody = streamError(t, base+"?symbol="+symbolChr2+"&interval=1m&from=2026-11-18T00:00:00Z&to=2026-11-19T10:00:00Z")
	if code != http.StatusBadRequest || asString(errBody["code"]) != "bad_range" {
		t.Fatalf("2001 bars got (%d, %s)", code, asString(errBody["code"]))
	}
}

// barExpect 是单根 K 线的手算期望值，数字字段均为响应中的十进制字符串。
type barExpect struct {
	start, end              string
	open, high, low, close  int64
	volume, turnover, count int64
	complete                bool
}

// barsWaitStatus 等待指定市场的状态端点返回目标状态（现有 waitStatus 只认
// one-piece-http，这里按市场参数化）。
func barsWaitStatus(t *testing.T, base, market, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		url := base + "/v1/markets/" + market + "/status"
		if statusCode(t, url) == http.StatusOK && asString(getBody(t, url)["state"]) == state {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("status did not become %s", state)
}

// barsFetch 请求 /bars 并返回顶层 as_of_seq 与 items 数组。
func barsFetch(t *testing.T, url string) (string, []map[string]any) {
	t.Helper()
	body := getBody(t, url)
	raw, _ := body["items"].([]any)
	if raw == nil {
		t.Fatalf("%s 响应缺少 items 数组", url)
	}
	items := make([]map[string]any, 0, len(raw))
	for _, one := range raw {
		item, _ := one.(map[string]any)
		if item == nil {
			t.Fatalf("%s item 不是对象: %v", url, one)
		}
		items = append(items, item)
	}
	return asString(body["as_of_seq"]), items
}

// barNumber 校验数字字段为十进制字符串并解析为 int64。
func barNumber(t *testing.T, url string, item map[string]any, key string) int64 {
	t.Helper()
	value, err := strconv.ParseInt(asString(item[key]), 10, 64)
	if err != nil {
		t.Fatalf("%s %s = %q 不是十进制字符串", url, key, asString(item[key]))
	}
	return value
}

// assertBar 逐字段核对单根 K 线。
func assertBar(t *testing.T, url string, item map[string]any, want barExpect) {
	t.Helper()
	if got := asString(item["start_time"]); got != want.start {
		t.Fatalf("%s start_time %s want %s", url, got, want.start)
	}
	if got := asString(item["end_time"]); got != want.end {
		t.Fatalf("%s end_time %s want %s", url, got, want.end)
	}
	for _, field := range []struct {
		key  string
		want int64
	}{
		{"open_cents", want.open}, {"high_cents", want.high},
		{"low_cents", want.low}, {"close_cents", want.close},
		{"volume", want.volume}, {"turnover_cents", want.turnover},
		{"trade_count", want.count},
	} {
		if got := barNumber(t, url, item, field.key); got != field.want {
			t.Fatalf("%s %s = %d want %d", url, field.key, got, field.want)
		}
	}
	if item["complete"] != want.complete {
		t.Fatalf("%s complete = %v want %v", url, item["complete"], want.complete)
	}
}
