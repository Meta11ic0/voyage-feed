//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"voyage-feed/engine"
)

func TestStepsAndRecovery(t *testing.T) {
	ctx := context.Background()
	conn := openTestDB(t, ctx)
	defer conn.Close(ctx)
	origin := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	resetMarket(t, ctx, conn, "one-piece-step", 7, 10000, origin)
	if err := CommitExpecting(ctx, conn, "one-piece-step", 99); err == nil {
		t.Fatal("wrong tick should fail")
	}
	assertTick(t, ctx, conn, "one-piece-step", 0, 0)

	first, err := CommitNext(ctx, conn, "one-piece-step", Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CommitNext(ctx, conn, "one-piece-step", Hooks{}); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM market_events WHERE market_id = 'one-piece-step'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 4 {
		t.Fatalf("events %d", events)
	}
	recomputeBars(t, ctx, conn, "one-piece-step")

	calls := 0
	hooks := Hooks{BeforeCommit: func() error {
		calls++
		if calls == 1 {
			return errors.New("注入")
		}
		return nil
	}}
	if _, err := CommitNext(ctx, conn, "one-piece-step", hooks); err == nil {
		t.Fatal("injected failure should roll back")
	}
	assertTick(t, ctx, conn, "one-piece-step", 2, first.LastSeq+2)
	if _, err := CommitNext(ctx, conn, "one-piece-step", hooks); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitNext(ctx, conn, "one-piece-step", Hooks{AfterCommit: func() error { return errors.New("停") }}); !errors.Is(err, ErrStoppedAfterCommit) {
		t.Fatalf("after commit: %v", err)
	}
	var tick int64
	if err := conn.QueryRow(ctx, `SELECT logical_tick FROM market_instances WHERE market_id = 'one-piece-step'`).Scan(&tick); err != nil {
		t.Fatal(err)
	}
	next, err := CommitNext(ctx, conn, "one-piece-step", Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if next.Tick != tick+1 {
		t.Fatalf("resume tick %d after %d", next.Tick, tick)
	}
}

func TestZeroTradeAndLock(t *testing.T) {
	ctx := context.Background()
	conn := openTestDB(t, ctx)
	defer conn.Close(ctx)
	origin := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	resetMarket(t, ctx, conn, "one-piece-step", 7, 1, origin)
	sawZero := false
	for i := 0; i < 30 && !sawZero; i++ {
		var before int
		var priceBefore *int64
		var volumeBefore int64
		conn.QueryRow(ctx, `SELECT count(*) FROM market_events WHERE market_id = 'one-piece-step'`).Scan(&before)
		conn.QueryRow(ctx, `SELECT last_price_cents, day_volume FROM instrument_states WHERE market_id = 'one-piece-step' AND instrument_id = 'codex:chr:1'`).Scan(&priceBefore, &volumeBefore)
		var tickBefore int64
		conn.QueryRow(ctx, `SELECT logical_tick FROM market_instances WHERE market_id = 'one-piece-step'`).Scan(&tickBefore)
		if _, err := CommitNext(ctx, conn, "one-piece-step", Hooks{}); err != nil {
			t.Fatal(err)
		}
		var after int
		var priceAfter *int64
		var volumeAfter int64
		var tickAfter int64
		conn.QueryRow(ctx, `SELECT count(*) FROM market_events WHERE market_id = 'one-piece-step'`).Scan(&after)
		conn.QueryRow(ctx, `SELECT last_price_cents, day_volume FROM instrument_states WHERE market_id = 'one-piece-step' AND instrument_id = 'codex:chr:1'`).Scan(&priceAfter, &volumeAfter)
		conn.QueryRow(ctx, `SELECT logical_tick FROM market_instances WHERE market_id = 'one-piece-step'`).Scan(&tickAfter)
		if after == before && tickAfter == tickBefore+1 && volumeAfter == volumeBefore && samePrice(priceBefore, priceAfter) {
			var bars int
			conn.QueryRow(ctx, `SELECT count(*) FROM bars_1m WHERE market_id = 'one-piece-step'`).Scan(&bars)
			if bars == 0 || priceBefore != nil {
				sawZero = true
			}
		}
	}
	if !sawZero {
		t.Fatal("no zero-trade step")
	}

	holder, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(ctx)
	resetMarket(t, ctx, holder, "one-piece-step", 7, 10000, origin)
	runCtx, cancel := context.WithCancel(ctx)
	runner := &Runner{MarketID: "one-piece-step", Interval: time.Hour}
	done := make(chan error, 1)
	go func() { done <- runner.Run(runCtx, holder) }()
	waitTick(t, ctx, conn, "one-piece-step", 1)
	other, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(ctx)
	var before int
	conn.QueryRow(ctx, `SELECT count(*) FROM market_events WHERE market_id = 'one-piece-step'`).Scan(&before)
	err = (&Runner{MarketID: "one-piece-step"}).Run(ctx, other)
	if err == nil || !strings.Contains(err.Error(), "同市场已经启动") {
		t.Fatalf("lock: %v", err)
	}
	var after int
	conn.QueryRow(ctx, `SELECT count(*) FROM market_events WHERE market_id = 'one-piece-step'`).Scan(&after)
	if after != before {
		t.Fatalf("second runner wrote %d -> %d", before, after)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := CommitNext(ctx, other, "one-piece-step", Hooks{}); err != nil {
		t.Fatal(err)
	}
}

func TestCrossDay(t *testing.T) {
	ctx := context.Background()
	conn := openTestDB(t, ctx)
	defer conn.Close(ctx)
	seed := seedWithPositiveMove(t, "codex:chr:3", 2)
	origin := time.Date(2026, 11, 17, 15, 59, 58, 0, time.UTC)
	resetMarket(t, ctx, conn, "one-piece-step", seed, 10000, origin)
	if _, err := CommitNext(ctx, conn, "one-piece-step", Hooks{}); err != nil {
		t.Fatal(err)
	}
	var sessions int
	conn.QueryRow(ctx, `SELECT count(*) FROM market_events WHERE market_id = 'one-piece-step' AND type = 'session'`).Scan(&sessions)
	if sessions != 0 {
		t.Fatal("first day inserted a session")
	}
	var price1 int64
	conn.QueryRow(ctx, `SELECT last_price_cents FROM instrument_states WHERE market_id = 'one-piece-step' AND instrument_id = 'codex:chr:1'`).Scan(&price1)
	if _, err := conn.Exec(ctx, `
		INSERT INTO instruments (
			market_id, instrument_id, symbol, name, sector, role, chapter_count, initial_price_cents, target_cap_cents, shares_total, shares_float
		) VALUES ('one-piece-step', 'codex:chr:3', 'BBBBBBBBBBBBOP', '丙', '其他', 'unknown', 0, 100000000, 100, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO instrument_states (
			market_id, instrument_id, prev_close_cents, last_price_cents, day_open_cents, day_high_cents, day_low_cents, day_volume, day_turnover_cents
		) VALUES ('one-piece-step', 'codex:chr:3', 100, 100000000, 500, 500, 500, 99, 99)`); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitNext(ctx, conn, "one-piece-step", Hooks{}); err != nil {
		t.Fatal(err)
	}
	var sessionSeq, minTrade int64
	if err := conn.QueryRow(ctx, `
		SELECT event_seq FROM market_events
		WHERE market_id = 'one-piece-step' AND logical_tick = 2 AND type = 'session'`).Scan(&sessionSeq); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `
		SELECT min(event_seq) FROM market_events
		WHERE market_id = 'one-piece-step' AND logical_tick = 2 AND type = 'trade'`).Scan(&minTrade); err != nil {
		t.Fatal(err)
	}
	if sessionSeq >= minTrade {
		t.Fatalf("session %d trade %d", sessionSeq, minTrade)
	}
	var prev int64
	var volume int64
	var open *int64
	conn.QueryRow(ctx, `SELECT prev_close_cents, day_volume, day_open_cents FROM instrument_states WHERE market_id = 'one-piece-step' AND instrument_id = 'codex:chr:1'`).Scan(&prev, &volume, &open)
	if prev != price1 || volume == 0 || open == nil {
		t.Fatalf("trader prev %d volume %d open %v", prev, volume, open)
	}
	var quietPrev, quietVolume int64
	var quietOpen *int64
	conn.QueryRow(ctx, `SELECT prev_close_cents, day_volume, day_open_cents FROM instrument_states WHERE market_id = 'one-piece-step' AND instrument_id = 'codex:chr:3'`).Scan(&quietPrev, &quietVolume, &quietOpen)
	if quietPrev != 100000000 || quietVolume != 0 || quietOpen != nil {
		t.Fatalf("quiet prev %d volume %d open %v", quietPrev, quietVolume, quietOpen)
	}
	var traded int
	conn.QueryRow(ctx, `SELECT count(*) FROM market_events WHERE market_id = 'one-piece-step' AND logical_tick = 2 AND instrument_id = 'codex:chr:3'`).Scan(&traded)
	if traded != 0 {
		t.Fatal("capped instrument traded")
	}
}

func TestReplayAndRetention(t *testing.T) {
	ctx := context.Background()
	conn := openTestDB(t, ctx)
	defer conn.Close(ctx)
	origin := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	resetMarket(t, ctx, conn, "one-piece-step", 11, 10000, origin)
	resetMarket(t, ctx, conn, "one-piece-step-replay", 11, 10000, origin)
	for i := 0; i < 4; i++ {
		if _, err := CommitNext(ctx, conn, "one-piece-step", Hooks{}); err != nil {
			t.Fatal(err)
		}
		if _, err := CommitNext(ctx, conn, "one-piece-step-replay", Hooks{}); err != nil {
			t.Fatal(err)
		}
	}
	left := eventSignature(t, ctx, conn, "one-piece-step")
	right := eventSignature(t, ctx, conn, "one-piece-step-replay")
	if left != right {
		t.Fatalf("replay mismatch\n%s\n%s", left, right)
	}

	resetMarket(t, ctx, conn, "one-piece-step", 7, 10000, origin)
	if _, err := conn.Exec(ctx, `DELETE FROM market_events WHERE market_id = 'one-piece-step'`); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for seq := 1; seq <= 5001; seq++ {
		kind := "trade"
		if seq == 1 {
			kind = "session"
		}
		if kind == "session" {
			_, err = tx.Exec(ctx, `INSERT INTO market_events (market_id, event_seq, logical_tick, event_time, generated_at, market_date, type) VALUES ('one-piece-step', $1, $1, $2, $2, '2026-11-16', 'session')`, seq, old)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO market_events (market_id, event_seq, logical_tick, event_time, generated_at, market_date, type, instrument_id, price_cents, quantity, turnover_cents) VALUES ('one-piece-step', $1, $1, $2, $2, '2026-11-16', 'trade', 'codex:chr:1', 1000, 100, 100000)`, seq, old)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE market_instances SET last_event_seq = 5001 WHERE market_id = 'one-piece-step'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO bars_1m (market_id, instrument_id, bucket_start, open_cents, high_cents, low_cents, close_cents, volume, turnover_cents, trade_count, first_event_seq, last_event_seq) VALUES ('one-piece-step', 'codex:chr:1', $1, 1000, 1000, 1000, 1000, 100, 100000, 1, 2, 2)`, old); err != nil {
		t.Fatal(err)
	}
	resetMarket(t, ctx, conn, "one-piece-step-replay", 3, 10000, origin)
	if _, err := CommitNext(ctx, conn, "one-piece-step-replay", Hooks{}); err != nil {
		t.Fatal(err)
	}
	n, err := DeleteExpiredBatch(ctx, conn, "one-piece-step", 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5000 {
		t.Fatalf("batch %d", n)
	}
	var replayLeft int
	conn.QueryRow(ctx, `SELECT count(*) FROM market_events WHERE market_id = 'one-piece-step-replay'`).Scan(&replayLeft)
	if err := Maintain(ctx, conn, "one-piece-step", 1); err != nil {
		t.Fatal(err)
	}
	var leftEvents int
	var watermark int64
	conn.QueryRow(ctx, `SELECT count(*) FROM market_events WHERE market_id = 'one-piece-step'`).Scan(&leftEvents)
	conn.QueryRow(ctx, `SELECT first_retained_seq FROM market_instances WHERE market_id = 'one-piece-step'`).Scan(&watermark)
	if leftEvents != 0 || watermark != 5002 {
		t.Fatalf("left %d watermark %d", leftEvents, watermark)
	}
	var replayAfter int
	conn.QueryRow(ctx, `SELECT count(*) FROM market_events WHERE market_id = 'one-piece-step-replay'`).Scan(&replayAfter)
	if replayAfter != replayLeft || replayAfter == 0 {
		t.Fatalf("other market %d -> %d", replayLeft, replayAfter)
	}
	var barVolume int64
	conn.QueryRow(ctx, `SELECT volume FROM bars_1m WHERE market_id = 'one-piece-step'`).Scan(&barVolume)
	if barVolume != 100 {
		t.Fatalf("bar changed %d", barVolume)
	}
}

func TestRunnerStops(t *testing.T) {
	ctx := context.Background()
	conn := openTestDB(t, ctx)
	defer conn.Close(ctx)
	dialer, err := net.DialTimeout("tcp", "127.0.0.1:8787", 200*time.Millisecond)
	if err == nil {
		dialer.Close()
		t.Fatal("8787 is listening")
	}
	origin := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	resetMarket(t, ctx, conn, "one-piece-step", 7, 10000, origin)
	writer, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close(ctx)
	runCtx, cancel := context.WithCancel(ctx)
	runner := &Runner{MarketID: "one-piece-step", Interval: 0, Sleep: func(context.Context, time.Duration) {}}
	done := make(chan error, 1)
	go func() { done <- runner.Run(runCtx, writer) }()
	waitTick(t, ctx, conn, "one-piece-step", 3)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var tick int64
	conn.QueryRow(ctx, `SELECT logical_tick FROM market_instances WHERE market_id = 'one-piece-step'`).Scan(&tick)
	time.Sleep(150 * time.Millisecond)
	var again int64
	conn.QueryRow(ctx, `SELECT logical_tick FROM market_instances WHERE market_id = 'one-piece-step'`).Scan(&again)
	if again != tick {
		t.Fatalf("tick advanced after cancel %d -> %d", tick, again)
	}
}

func openTestDB(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if !strings.Contains(url, "/voyage_test") {
		t.Fatal("DATABASE_URL must target voyage_test")
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "voyage_test" {
		t.Fatalf("database %s", name)
	}
	if err := Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	return conn
}

func resetMarket(t *testing.T, ctx context.Context, conn *pgx.Conn, id string, seed uint64, probability int, origin time.Time) {
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
	raw := []byte(`{"schema_version":1,"dataset_id":"one-piece-c1-step","cutoff_chapter":1,"sources":[],"instruments":[{"instrument_id":"codex:chr:1","symbol":"D14001F4649BOP","name":"甲","sector":"草帽一伙","role":"leader","chapter_count":4,"popularity_rank":1},{"instrument_id":"codex:chr:2","symbol":"AAAAAAAAAAAAAAAAOP","name":"乙","sector":"海军","role":"member","chapter_count":2,"popularity_rank":11}]}`)
	if _, err := Publish(ctx, conn, PublishConfig{
		MarketID: id, Dataset: raw, Seed: seed, TradeProbabilityBps: probability,
		MaxMoveBps: 20, CapBudgetCents: 100000000000, ClockOrigin: origin,
	}); err != nil {
		t.Fatal(err)
	}
}

func assertTick(t *testing.T, ctx context.Context, conn *pgx.Conn, id string, tick, seq int64) {
	t.Helper()
	var gotTick, gotSeq int64
	if err := conn.QueryRow(ctx, `SELECT logical_tick, last_event_seq FROM market_instances WHERE market_id = $1`, id).Scan(&gotTick, &gotSeq); err != nil {
		t.Fatal(err)
	}
	if gotTick != tick || gotSeq != seq {
		t.Fatalf("tick %d seq %d, want %d %d", gotTick, gotSeq, tick, seq)
	}
}

func recomputeBars(t *testing.T, ctx context.Context, conn *pgx.Conn, id string) {
	t.Helper()
	rows, err := conn.Query(ctx, `
		SELECT instrument_id, event_time, price_cents, quantity, turnover_cents
		FROM market_events WHERE market_id = $1 AND type = 'trade' ORDER BY event_seq`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type bar struct {
		open, high, low, close, volume, turnover, count int64
		seen                                            bool
	}
	got := map[string]*bar{}
	for rows.Next() {
		var instrument string
		var when time.Time
		var price, qty, turn int64
		if err := rows.Scan(&instrument, &when, &price, &qty, &turn); err != nil {
			t.Fatal(err)
		}
		key := instrument + "|" + when.UTC().Truncate(time.Minute).Format(time.RFC3339)
		item := got[key]
		if item == nil {
			item = &bar{open: price, high: price, low: price}
			got[key] = item
		}
		if price > item.high {
			item.high = price
		}
		if price < item.low {
			item.low = price
		}
		item.close = price
		item.volume += qty
		item.turnover += turn
		item.count++
		item.seen = true
	}
	var stored int
	conn.QueryRow(ctx, `SELECT count(*) FROM bars_1m WHERE market_id = $1`, id).Scan(&stored)
	if stored != len(got) {
		t.Fatalf("bars %d recomputed %d", stored, len(got))
	}
	for key, item := range got {
		parts := strings.Split(key, "|")
		var open, high, low, close, volume, turnover, count int64
		err := conn.QueryRow(ctx, `
			SELECT open_cents, high_cents, low_cents, close_cents, volume, turnover_cents, trade_count
			FROM bars_1m WHERE market_id = $1 AND instrument_id = $2 AND bucket_start = $3`,
			id, parts[0], parts[1]).Scan(&open, &high, &low, &close, &volume, &turnover, &count)
		if err != nil {
			t.Fatal(err)
		}
		if open != item.open || high != item.high || low != item.low || close != item.close || volume != item.volume || turnover != item.turnover || count != item.count {
			t.Fatalf("bar %s stored %d %d %d %d %d %d %d", key, open, high, low, close, volume, turnover, count)
		}
	}
}

func eventSignature(t *testing.T, ctx context.Context, conn *pgx.Conn, id string) string {
	t.Helper()
	rows, err := conn.Query(ctx, `
		SELECT event_seq, logical_tick, type, COALESCE(instrument_id, ''), COALESCE(price_cents, 0), COALESCE(quantity, 0), COALESCE(turnover_cents, 0)
		FROM market_events WHERE market_id = $1 ORDER BY event_seq`, id)
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
	return b.String()
}

func waitTick(t *testing.T, ctx context.Context, conn *pgx.Conn, id string, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var tick int64
		if err := conn.QueryRow(ctx, `SELECT logical_tick FROM market_instances WHERE market_id = $1`, id).Scan(&tick); err != nil {
			t.Fatal(err)
		}
		if tick >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("tick did not reach %d", want)
}

func seedWithPositiveMove(t *testing.T, id string, tick int64) uint64 {
	t.Helper()
	for seed := uint64(1); seed < 200; seed++ {
		raw, err := engine.Uniform(engine.ModelVersion, seed, id, tick, "move_bps", 41)
		if err != nil {
			t.Fatal(err)
		}
		if int(raw)-20 > 0 {
			return seed
		}
	}
	t.Fatal("no seed")
	return 0
}

func samePrice(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
