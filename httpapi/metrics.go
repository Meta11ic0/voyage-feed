package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
)

func (s *Server) metrics(w http.ResponseWriter) {
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
}
