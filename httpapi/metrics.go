package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const tableBytesTTL = 10 * time.Second

var tableByteNames = []string{"market_events", "bars_1m", "instrument_states", "instruments"}

type tableByteCache struct {
	at   time.Time
	size map[string]int64
	wait chan struct{}
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	buckets := s.buckets
	inf := s.inf
	sum := s.sum
	count := s.count
	last := s.lastSeq
	dbErrors := s.dbErrors
	disconnects := s.disconnect
	s.mu.Unlock()
	dispatched := int64(0)
	if s.Hub != nil {
		dispatched = s.Hub.Last()
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "# TYPE market_tick_commit_seconds histogram\n")
	var cumulative uint64
	for i, bound := range histBounds {
		cumulative += buckets[i]
		fmt.Fprintf(w, "market_tick_commit_seconds_bucket{le=%q} %d\n", strconv.FormatFloat(bound, 'f', -1, 64), cumulative)
	}
	fmt.Fprintf(w, "market_tick_commit_seconds_bucket{le=\"+Inf\"} %d\n", cumulative+inf)
	fmt.Fprintf(w, "market_tick_commit_seconds_sum %s\n", strconv.FormatFloat(sum, 'f', -1, 64))
	fmt.Fprintf(w, "market_tick_commit_seconds_count %d\n", count)
	fmt.Fprintf(w, "# TYPE market_event_last_committed gauge\nmarket_event_last_committed %d\n", last)
	fmt.Fprintf(w, "# TYPE market_event_last_dispatched gauge\nmarket_event_last_dispatched %d\n", dispatched)
	fmt.Fprintf(w, "# TYPE market_stream_disconnects_total counter\nmarket_stream_disconnects_total %d\n", disconnects)
	fmt.Fprintf(w, "# TYPE market_db_errors_total counter\nmarket_db_errors_total %d\n", dbErrors)
	sizes, ok := s.tableBytes(r.Context())
	if !ok {
		return
	}
	fmt.Fprintf(w, "# TYPE market_db_table_bytes gauge\n")
	for _, name := range tableByteNames {
		value, found := sizes[name]
		if !found {
			continue
		}
		fmt.Fprintf(w, "market_db_table_bytes{table=%q} %d\n", name, value)
	}
}

func (s *Server) tableBytes(ctx context.Context) (map[string]int64, bool) {
	if s.Pool == nil {
		return nil, false
	}
	s.mu.Lock()
	if s.tables.size != nil && time.Since(s.tables.at) < tableBytesTTL {
		out := s.tables.size
		s.mu.Unlock()
		return out, true
	}
	if s.tables.wait != nil {
		ch := s.tables.wait
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, false
		}
		s.mu.Lock()
		fresh := s.tables.size != nil && time.Since(s.tables.at) < tableBytesTTL
		out := s.tables.size
		s.mu.Unlock()
		if !fresh {
			return nil, false
		}
		return out, true
	}
	ch := make(chan struct{})
	s.tables.wait = ch
	s.mu.Unlock()

	sizes, err := s.queryTableBytes(ctx)
	s.mu.Lock()
	if err == nil {
		s.tables.size = sizes
		s.tables.at = time.Now()
	}
	s.tables.wait = nil
	close(ch)
	fresh := err == nil
	s.mu.Unlock()
	if !fresh {
		return nil, false
	}
	return sizes, true
}

func (s *Server) queryTableBytes(ctx context.Context) (map[string]int64, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT c.relname, pg_total_relation_size(c.oid)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relname = ANY($1::text[])`, tableByteNames)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var size int64
		if err := rows.Scan(&name, &size); err != nil {
			return nil, err
		}
		out[name] = size
	}
	return out, rows.Err()
}
