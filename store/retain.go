package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

func DeleteExpiredBatch(ctx context.Context, conn *pgx.Conn, marketID string, retentionDays int) (int64, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var origin time.Time
	var tick, lastSeq int64
	if err := tx.QueryRow(ctx, `
		SELECT clock_origin, logical_tick, last_event_seq
		FROM market_instances WHERE market_id = $1 FOR UPDATE`, marketID).Scan(&origin, &tick, &lastSeq); err != nil {
		return 0, err
	}
	cutoff := origin.UTC().Add(time.Duration(tick)*time.Second - time.Duration(retentionDays)*24*time.Hour)
	tag, err := tx.Exec(ctx, `
		DELETE FROM market_events
		WHERE market_id = $1 AND event_seq IN (
			SELECT event_seq FROM market_events
			WHERE market_id = $1
			  AND event_time < $2
			  AND event_seq < COALESCE((
			      SELECT min(event_seq) FROM market_events
			      WHERE market_id = $1 AND event_time >= $2
			  ), 9223372036854775807)
			ORDER BY event_seq
			LIMIT 5000
		)`, marketID, cutoff)
	if err != nil {
		return 0, err
	}
	var remain *int64
	if err := tx.QueryRow(ctx, `SELECT min(event_seq) FROM market_events WHERE market_id = $1`, marketID).Scan(&remain); err != nil {
		return 0, err
	}
	watermark := lastSeq + 1
	if remain != nil {
		watermark = *remain
	}
	if _, err := tx.Exec(ctx, `UPDATE market_instances SET first_retained_seq = $2 WHERE market_id = $1`, marketID, watermark); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func Maintain(ctx context.Context, conn *pgx.Conn, marketID string, retentionDays int) error {
	for {
		n, err := DeleteExpiredBatch(ctx, conn, marketID, retentionDays)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}
