package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"voyage-feed/dataset"
	"voyage-feed/engine"
)

type PublishConfig struct {
	MarketID            string
	Dataset             []byte
	Seed                uint64
	TradeProbabilityBps int
	MaxMoveBps          int
	CapBudgetCents      int64
	ClockOrigin         time.Time
}

type PublishResult struct {
	Created bool
}

func Publish(ctx context.Context, conn *pgx.Conn, cfg PublishConfig) (PublishResult, error) {
	var release dataset.Release
	if err := json.Unmarshal(cfg.Dataset, &release); err != nil {
		return PublishResult{}, fmt.Errorf("发布文件无法解析：%w", err)
	}
	if release.SchemaVersion != 1 {
		return PublishResult{}, fmt.Errorf("不支持的发布版本")
	}
	if len(release.Instruments) == 0 {
		return PublishResult{}, fmt.Errorf("发布文件没有标的")
	}
	sum := sha256.Sum256(cfg.Dataset)
	fingerprint := hex.EncodeToString(sum[:])
	inputs := make([]engine.InstrumentInput, len(release.Instruments))
	for i, item := range release.Instruments {
		inputs[i] = engine.InstrumentInput{
			InstrumentID: item.InstrumentID,
			Role:         item.Role,
			ChapterCount: item.ChapterCount,
			Rank:         item.PopularityRank,
		}
	}
	inits, err := engine.Initialize(cfg.Seed, cfg.CapBudgetCents, inputs)
	if err != nil {
		return PublishResult{}, err
	}
	initByID := map[string]engine.InstrumentInit{}
	for _, item := range inits {
		initByID[item.InstrumentID] = item
	}
	origin := cfg.ClockOrigin.UTC().Truncate(time.Second)
	marketDate := engine.MarketDate(origin)

	tx, err := conn.Begin(ctx)
	if err != nil {
		return PublishResult{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(8787, 1)"); err != nil {
		return PublishResult{}, err
	}
	var existing struct {
		found       bool
		fingerprint string
		seed        string
		model       string
		prob        int
		move        int
		budget      int64
		origin      time.Time
	}
	err = tx.QueryRow(ctx, `
		SELECT dataset_sha256, seed::text, model_version, trade_probability_bps, max_move_bps, cap_budget_cents, clock_origin
		FROM market_instances WHERE market_id = $1`, cfg.MarketID).Scan(
		&existing.fingerprint, &existing.seed, &existing.model, &existing.prob, &existing.move, &existing.budget, &existing.origin,
	)
	if err == nil {
		existing.found = true
	} else if err != pgx.ErrNoRows {
		return PublishResult{}, err
	}
	if existing.found {
		if !sameConfig(existing.fingerprint, existing.seed, existing.model, existing.prob, existing.move, existing.budget, existing.origin, fingerprint, cfg, origin) {
			return PublishResult{}, fmt.Errorf("市场配置与已保存值不一致，请创建新的市场 ID")
		}
		if err := tx.Commit(ctx); err != nil {
			return PublishResult{}, err
		}
		return PublishResult{Created: false}, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO market_instances (
			market_id, dataset_sha256, dataset_id, model_version, seed,
			trade_probability_bps, max_move_bps, cap_budget_cents, clock_origin,
			logical_tick, last_event_seq, market_date
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,0,0,$10)`,
		cfg.MarketID, fingerprint, release.DatasetID, engine.ModelVersion, strconv.FormatUint(cfg.Seed, 10),
		cfg.TradeProbabilityBps, cfg.MaxMoveBps, cfg.CapBudgetCents, origin, marketDate,
	); err != nil {
		return PublishResult{}, err
	}
	for _, item := range release.Instruments {
		init, ok := initByID[item.InstrumentID]
		if !ok {
			return PublishResult{}, fmt.Errorf("缺少初始化结果 %s", item.InstrumentID)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO instruments (
				market_id, instrument_id, symbol, name, sector, role, chapter_count, popularity_rank,
				initial_price_cents, target_cap_cents, shares_total, shares_float
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			cfg.MarketID, item.InstrumentID, item.Symbol, item.Name, item.Sector, item.Role,
			item.ChapterCount, item.PopularityRank, init.InitialPriceCents, init.TargetCapCents, init.SharesTotal, init.SharesFloat,
		); err != nil {
			return PublishResult{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO instrument_states (
				market_id, instrument_id, prev_close_cents, day_volume, day_turnover_cents
			) VALUES ($1,$2,$3,0,0)`,
			cfg.MarketID, item.InstrumentID, init.InitialPriceCents,
		); err != nil {
			return PublishResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return PublishResult{}, err
	}
	return PublishResult{Created: true}, nil
}

func sameConfig(fingerprint, seed, model string, prob, move int, budget int64, origin time.Time, wantFingerprint string, cfg PublishConfig, wantOrigin time.Time) bool {
	seedText := stringsTrimNumeric(seed)
	return fingerprint == wantFingerprint &&
		seedText == strconv.FormatUint(cfg.Seed, 10) &&
		model == engine.ModelVersion &&
		prob == cfg.TradeProbabilityBps &&
		move == cfg.MaxMoveBps &&
		budget == cfg.CapBudgetCents &&
		origin.UTC().Truncate(time.Second).Equal(wantOrigin)
}

func stringsTrimNumeric(seed string) string {
	for len(seed) > 1 && seed[0] == '0' {
		seed = seed[1:]
	}
	if i := indexByte(seed, '.'); i >= 0 {
		seed = seed[:i]
	}
	if seed == "" {
		return "0"
	}
	return seed
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
