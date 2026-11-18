//go:build integration

package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestMigrateIdempotent(t *testing.T) {
	appURL := os.Getenv("DATABASE_URL")
	adminURL := os.Getenv("ADMIN_DATABASE_URL")
	if !strings.Contains(appURL, "/voyage_test") {
		t.Fatal("DATABASE_URL must target voyage_test")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = 'voyage_test' AND pid <> pg_backend_pid()`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS voyage_test`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE voyage_test`); err != nil {
		t.Fatal(err)
	}
	if err := admin.Close(ctx); err != nil {
		t.Fatal(err)
	}

	conn, err := pgx.Connect(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
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
	if err := Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	var versions int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("versions %d", versions)
	}

	raw := []byte(`{"schema_version":1,"dataset_id":"one-piece-c1-test","cutoff_chapter":1,"sources":[],"instruments":[{"instrument_id":"codex:chr:1","symbol":"D14001F4649BOP","name":"路飞","sector":"草帽一伙","role":"leader","chapter_count":4,"popularity_rank":1}]}`)
	origin := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	cfg := PublishConfig{
		MarketID:            "one-piece-dev",
		Dataset:             raw,
		Seed:                7,
		TradeProbabilityBps: 2000,
		MaxMoveBps:          20,
		CapBudgetCents:      100000000000,
		ClockOrigin:         origin,
	}
	first, err := Publish(ctx, conn, cfg)
	if err != nil || !first.Created {
		t.Fatalf("create %+v %v", first, err)
	}
	second, err := Publish(ctx, conn, cfg)
	if err != nil || second.Created {
		t.Fatalf("idempotent %+v %v", second, err)
	}
	var tick int64
	var price int64
	if err := conn.QueryRow(ctx, `SELECT logical_tick FROM market_instances WHERE market_id = $1`, cfg.MarketID).Scan(&tick); err != nil {
		t.Fatal(err)
	}
	if tick != 0 {
		t.Fatalf("tick %d", tick)
	}
	if err := conn.QueryRow(ctx, `SELECT initial_price_cents FROM instruments WHERE market_id = $1`, cfg.MarketID).Scan(&price); err != nil {
		t.Fatal(err)
	}
	cfg.Seed = 8
	if _, err := Publish(ctx, conn, cfg); err == nil {
		t.Fatal("conflicting seed should fail")
	}
	var priceAfter int64
	if err := conn.QueryRow(ctx, `SELECT initial_price_cents FROM instruments WHERE market_id = $1`, cfg.MarketID).Scan(&priceAfter); err != nil {
		t.Fatal(err)
	}
	if priceAfter != price {
		t.Fatalf("price changed %d %d", price, priceAfter)
	}
}

func TestPublishDevMarket(t *testing.T) {
	if os.Getenv("ACCEPT_VOYAGE") != "1" {
		t.Skip()
	}
	appURL := os.Getenv("DATABASE_URL")
	path := strings.Split(appURL, "?")[0]
	if !strings.HasSuffix(path, "/voyage") {
		t.Fatal("acceptance target must be the voyage database")
	}
	raw, err := os.ReadFile(os.Getenv("DEV_DATASET"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var name string
	if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "voyage" {
		t.Fatalf("database %s", name)
	}
	if err := Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	origin := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	cfg := PublishConfig{
		MarketID:            "one-piece-dev",
		Dataset:             raw,
		Seed:                7,
		TradeProbabilityBps: 2000,
		MaxMoveBps:          20,
		CapBudgetCents:      100000000000,
		ClockOrigin:         origin,
	}
	if _, err := Publish(ctx, conn, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(ctx, conn, cfg); err != nil {
		t.Fatal(err)
	}
	var count int
	var tick int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM instruments WHERE market_id = $1`, cfg.MarketID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 12 {
		t.Fatalf("instruments %d", count)
	}
	if err := conn.QueryRow(ctx, `SELECT logical_tick FROM market_instances WHERE market_id = $1`, cfg.MarketID).Scan(&tick); err != nil {
		t.Fatal(err)
	}
	if tick != 0 {
		t.Fatalf("tick %d", tick)
	}
	assertNullChapter := func(name string) {
		t.Helper()
		var chapters *int
		if err := conn.QueryRow(ctx, `SELECT chapter_count FROM instruments WHERE market_id = $1 AND name = $2`, cfg.MarketID, name).Scan(&chapters); err != nil {
			t.Fatal(err)
		}
		if chapters != nil {
			t.Fatalf("%s chapter %d", name, *chapters)
		}
	}
	assertNullChapter("青雉")
	var brookRank *int
	if err := conn.QueryRow(ctx, `SELECT popularity_rank FROM instruments WHERE market_id = $1 AND name = '布鲁克'`, cfg.MarketID).Scan(&brookRank); err != nil {
		t.Fatal(err)
	}
	if brookRank != nil {
		t.Fatal("brook has a rank")
	}
	var zero int
	if err := conn.QueryRow(ctx, `SELECT chapter_count FROM instruments WHERE market_id = $1 AND name = '开发夹具路人'`, cfg.MarketID).Scan(&zero); err != nil {
		t.Fatal(err)
	}
	if zero != 0 {
		t.Fatalf("zero chapter %d", zero)
	}
	var usopp, sanji int
	var usoppPrice, sanjiPrice int64
	if err := conn.QueryRow(ctx, `SELECT popularity_rank, initial_price_cents FROM instruments WHERE market_id = $1 AND name = '乌索普'`, cfg.MarketID).Scan(&usopp, &usoppPrice); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT popularity_rank, initial_price_cents FROM instruments WHERE market_id = $1 AND name = '山治'`, cfg.MarketID).Scan(&sanji, &sanjiPrice); err != nil {
		t.Fatal(err)
	}
	if usopp != 11 || sanji != 11 || usoppPrice < 1600 || usoppPrice > 3000 || sanjiPrice < 1600 || sanjiPrice > 3000 {
		t.Fatalf("tie prices %d %d ranks %d %d", usoppPrice, sanjiPrice, usopp, sanji)
	}
}
