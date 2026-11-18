package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"voyage-feed/engine"
)

var ErrStoppedAfterCommit = errors.New("提交后停止")

type Hooks struct {
	BeforeCommit func() error
	AfterCommit  func() error
}

type StepResult struct {
	Tick     int64
	LastSeq  int64
	CrossDay bool
	Trades   int
}

func CommitNext(ctx context.Context, conn *pgx.Conn, marketID string, hooks Hooks) (StepResult, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return StepResult{}, err
	}
	defer tx.Rollback(ctx)
	var tick, lastSeq int64
	var origin time.Time
	var prevDate, seedText, model string
	var probability, maxMove int
	err = tx.QueryRow(ctx, `
		SELECT logical_tick, last_event_seq, clock_origin, to_char(market_date, 'YYYY-MM-DD'), seed::text, model_version,
		       trade_probability_bps, max_move_bps
		FROM market_instances WHERE market_id = $1 FOR UPDATE`, marketID).Scan(
		&tick, &lastSeq, &origin, &prevDate, &seedText, &model, &probability, &maxMove,
	)
	if err != nil {
		return StepResult{}, err
	}
	seed, err := parseSeed(seedText)
	if err != nil {
		return StepResult{}, err
	}
	next := tick + 1
	eventTime := origin.UTC().Add(time.Duration(next) * time.Second)
	views, err := loadViews(ctx, tx, marketID)
	if err != nil {
		return StepResult{}, err
	}
	draft, err := engine.GenerateStep(engine.StepInput{
		Seed: seed, Model: model, ProbabilityBps: probability, MaxMoveBps: maxMove,
		Tick: next, PrevDate: prevDate, EventTime: eventTime, Instruments: views,
	})
	if err != nil {
		return StepResult{}, err
	}
	seq := lastSeq
	generatedAt := time.Now().UTC()
	if draft.CrossDay {
		if err := resetDay(ctx, tx, marketID); err != nil {
			return StepResult{}, err
		}
		seq++
		if err := insertSession(ctx, tx, marketID, seq, next, eventTime, generatedAt, draft.MarketDate); err != nil {
			return StepResult{}, err
		}
	}
	for _, trade := range draft.Trades {
		seq++
		if err := insertTrade(ctx, tx, marketID, seq, next, eventTime, generatedAt, draft.MarketDate, trade); err != nil {
			return StepResult{}, err
		}
		if err := updateState(ctx, tx, marketID, trade, seq, eventTime); err != nil {
			return StepResult{}, err
		}
		if err := upsertBar(ctx, tx, marketID, trade, seq, eventTime); err != nil {
			return StepResult{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE market_instances
		SET logical_tick = $2, last_event_seq = $3, market_date = $4
		WHERE market_id = $1`, marketID, next, seq, draft.MarketDate); err != nil {
		return StepResult{}, err
	}
	if hooks.BeforeCommit != nil {
		if err := hooks.BeforeCommit(); err != nil {
			return StepResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return StepResult{}, err
	}
	result := StepResult{Tick: next, LastSeq: seq, CrossDay: draft.CrossDay, Trades: len(draft.Trades)}
	if hooks.AfterCommit != nil {
		if err := hooks.AfterCommit(); err != nil {
			return result, fmt.Errorf("%w: %v", ErrStoppedAfterCommit, err)
		}
	}
	return result, nil
}

func CommitExpecting(ctx context.Context, conn *pgx.Conn, marketID string, expectedPrev int64) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var tick int64
	if err := tx.QueryRow(ctx, `SELECT logical_tick FROM market_instances WHERE market_id = $1 FOR UPDATE`, marketID).Scan(&tick); err != nil {
		return err
	}
	if tick != expectedPrev {
		return fmt.Errorf("逻辑步不是预期的前一步")
	}
	return tx.Commit(ctx)
}

func loadViews(ctx context.Context, tx pgx.Tx, marketID string) ([]engine.InstrumentView, error) {
	rows, err := tx.Query(ctx, `
		SELECT i.instrument_id, i.initial_price_cents, s.last_price_cents, s.day_volume, s.day_turnover_cents
		FROM instruments i
		JOIN instrument_states s USING (market_id, instrument_id)
		WHERE i.market_id = $1
		ORDER BY i.instrument_id`, marketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []engine.InstrumentView
	for rows.Next() {
		var item engine.InstrumentView
		if err := rows.Scan(&item.InstrumentID, &item.InitialPrice, &item.LastPrice, &item.DayVolume, &item.DayTurnover); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func resetDay(ctx context.Context, tx pgx.Tx, marketID string) error {
	_, err := tx.Exec(ctx, `
		UPDATE instrument_states s
		SET prev_close_cents = COALESCE(s.last_price_cents, i.initial_price_cents),
		    day_open_cents = NULL,
		    day_high_cents = NULL,
		    day_low_cents = NULL,
		    day_volume = 0,
		    day_turnover_cents = 0
		FROM instruments i
		WHERE s.market_id = i.market_id AND s.instrument_id = i.instrument_id AND s.market_id = $1`, marketID)
	return err
}

func insertSession(ctx context.Context, tx pgx.Tx, marketID string, seq, tick int64, eventTime, generatedAt time.Time, marketDate string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO market_events (market_id, event_seq, logical_tick, event_time, generated_at, market_date, type)
		VALUES ($1,$2,$3,$4,$5,$6,'session')`, marketID, seq, tick, eventTime, generatedAt, marketDate)
	return err
}

func insertTrade(ctx context.Context, tx pgx.Tx, marketID string, seq, tick int64, eventTime, generatedAt time.Time, marketDate string, trade engine.Trade) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO market_events (
			market_id, event_seq, logical_tick, event_time, generated_at, market_date, type,
			instrument_id, price_cents, quantity, turnover_cents
		) VALUES ($1,$2,$3,$4,$5,$6,'trade',$7,$8,$9,$10)`,
		marketID, seq, tick, eventTime, generatedAt, marketDate, trade.InstrumentID, trade.Price, trade.Quantity, trade.Turnover)
	return err
}

func updateState(ctx context.Context, tx pgx.Tx, marketID string, trade engine.Trade, seq int64, eventTime time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE instrument_states
		SET last_price_cents = $3,
		    last_event_seq = $4,
		    last_trade_time = $5,
		    day_open_cents = COALESCE(day_open_cents, $3),
		    day_high_cents = GREATEST(day_high_cents, $3),
		    day_low_cents = LEAST(day_low_cents, $3),
		    day_volume = day_volume + $6,
		    day_turnover_cents = day_turnover_cents + $7
		WHERE market_id = $1 AND instrument_id = $2`,
		marketID, trade.InstrumentID, trade.Price, seq, eventTime, trade.Quantity, trade.Turnover)
	return err
}

func upsertBar(ctx context.Context, tx pgx.Tx, marketID string, trade engine.Trade, seq int64, eventTime time.Time) error {
	bucket := eventTime.UTC().Truncate(time.Minute)
	_, err := tx.Exec(ctx, `
		INSERT INTO bars_1m (
			market_id, instrument_id, bucket_start, open_cents, high_cents, low_cents, close_cents,
			volume, turnover_cents, trade_count, first_event_seq, last_event_seq
		) VALUES ($1,$2,$3,$4,$4,$4,$4,$5,$6,1,$7,$7)
		ON CONFLICT (market_id, instrument_id, bucket_start) DO UPDATE SET
			high_cents = GREATEST(bars_1m.high_cents, EXCLUDED.high_cents),
			low_cents = LEAST(bars_1m.low_cents, EXCLUDED.low_cents),
			close_cents = EXCLUDED.close_cents,
			volume = bars_1m.volume + EXCLUDED.volume,
			turnover_cents = bars_1m.turnover_cents + EXCLUDED.turnover_cents,
			trade_count = bars_1m.trade_count + EXCLUDED.trade_count,
			last_event_seq = EXCLUDED.last_event_seq`,
		marketID, trade.InstrumentID, bucket, trade.Price, trade.Quantity, trade.Turnover, seq)
	return err
}

func parseSeed(text string) (uint64, error) {
	if i := indexByte(text, '.'); i >= 0 {
		text = text[:i]
	}
	return strconv.ParseUint(text, 10, 64)
}
