package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestSmokeStubNormal(t *testing.T) {
	stderr := runStub(t, "normal", func(base string, stderr io.Writer) int {
		return verifySmokeWithin(base, "stub", stderr, 3*time.Second)
	})
	if !strings.Contains(stderr, "smoke 通过") || !strings.Contains(stderr, "对照=trade") {
		t.Fatalf("摘要：%s", stderr)
	}
}

func TestSmokeStubTickOnly(t *testing.T) {
	stderr := runStub(t, "tick-only", func(base string, stderr io.Writer) int {
		code := verifySmokeWithin(base, "stub", stderr, 350*time.Millisecond)
		if code == 0 {
			t.Fatal("只动时钟时应失败")
		}
		return code
	})
	if !strings.Contains(stderr, "水位没有同时上涨。") {
		t.Fatalf("stderr：%s", stderr)
	}
}

func TestSmokeStubSkipReplay(t *testing.T) {
	stderr := runStub(t, "skip-replay", func(base string, stderr io.Writer) int {
		code := verifySmokeWithin(base, "stub", stderr, 3*time.Second)
		if code == 0 {
			t.Fatal("补发跳号时应失败")
		}
		return code
	})
	if !strings.Contains(stderr, "快照后首条序号不连续。") {
		t.Fatalf("stderr：%s", stderr)
	}
}

func TestSmokeStubSkipReconnect(t *testing.T) {
	stderr := runStub(t, "skip-reconnect", func(base string, stderr io.Writer) int {
		code := verifySmokeWithin(base, "stub", stderr, 3*time.Second)
		if code == 0 {
			t.Fatal("重连跳号时应失败")
		}
		return code
	})
	if !strings.Contains(stderr, "重连首条序号不连续。") {
		t.Fatalf("stderr：%s", stderr)
	}
}

func TestScaleStubNormal(t *testing.T) {
	stderr := runStub(t, "scale", func(base string, stderr io.Writer) int {
		return verifyScaleRun(base, "stub", stderr, scaleOptions{window: 700 * time.Millisecond, clients: 2, catch: 300 * time.Millisecond})
	})
	for _, part := range []string{"视角=容器", "nproc=", "mem_total=", "cgroup_memory=", "水位差=", "表 market_events 字节=", "请求样本=", "写入p95上限="} {
		if !strings.Contains(stderr, part) {
			t.Fatalf("缺少 %s：%s", part, stderr)
		}
	}
}

func TestScaleStubSkipsFirst(t *testing.T) {
	stderr := runStub(t, "scale-skip", func(base string, stderr io.Writer) int {
		code := verifyScaleRun(base, "stub", stderr, scaleOptions{window: 2 * time.Second, clients: 2, catch: 200 * time.Millisecond})
		if code == 0 {
			t.Fatal("压测首条跳号时应失败")
		}
		return code
	})
	if !strings.Contains(stderr, "压测首条序号不连续。") {
		t.Fatalf("stderr：%s", stderr)
	}
}

func TestHistWindow(t *testing.T) {
	bounds := []float64{0.1, 0.25, 1}
	start := histSnap{bounds: bounds, counts: []uint64{0, 0, 0}}
	_, _, kind := histWindow(start, start)
	if kind != "empty" {
		t.Fatalf("空窗口 %s", kind)
	}
	restart := histSnap{bounds: bounds, counts: []uint64{0, 0, 1}}
	_, _, kind = histWindow(restart, start)
	if kind != "restart" {
		t.Fatalf("回退窗口 %s", kind)
	}
	end := histSnap{bounds: bounds, counts: []uint64{1, 10, 10}}
	p95, total, kind := histWindow(start, end)
	if kind != "ok" || total != 10 || p95 != 0.25 {
		t.Fatalf("p95=%v total=%d kind=%s", p95, total, kind)
	}
}

func runStub(t *testing.T, mode string, run func(string, io.Writer) int) string {
	t.Helper()
	stub := &feedStub{mode: mode}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	var buf strings.Builder
	code := run(srv.URL, &buf)
	if mode == "normal" || mode == "scale" {
		if code != 0 {
			t.Fatalf("exit %d stderr %s", code, buf.String())
		}
	}
	return buf.String()
}

type feedStub struct {
	mode       string
	mu         sync.Mutex
	statusHits int
	metricHits int
	wsHits     int
	events     map[int64]map[string]any
}

func (s *feedStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/readyz":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	case strings.HasSuffix(r.URL.Path, "/status"):
		tick, seq := s.nextStatus()
		writeStubJSON(w, map[string]any{"logical_tick": tick, "last_event_seq": seq, "state": "running"})
	case strings.HasSuffix(r.URL.Path, "/snapshot"):
		writeStubJSON(w, map[string]any{
			"as_of_seq": "4",
			"items":     []any{map[string]any{"symbol": "D14001F4649BOP", "last_price_cents": "100"}},
		})
	case strings.HasSuffix(r.URL.Path, "/events"):
		after, _ := strconv.ParseInt(r.URL.Query().Get("after_seq"), 10, 64)
		items := make([]any, 0, 5)
		for seq := after + 1; seq <= after+5; seq++ {
			items = append(items, s.event(seq))
		}
		writeStubJSON(w, map[string]any{"items": items})
	case strings.HasSuffix(r.URL.Path, "/sectors") || strings.HasSuffix(r.URL.Path, "/bars"):
		writeStubJSON(w, map[string]any{"items": []any{}})
	case r.URL.Path == "/metrics":
		s.writeMetrics(w)
	case r.URL.Path == "/v1/stream":
		s.stream(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *feedStub) nextStatus() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusHits++
	if s.mode == "scale" || s.mode == "scale-skip" {
		return "8", "20"
	}
	if s.statusHits == 1 {
		return "1", "1"
	}
	if s.mode == "tick-only" {
		return "2", "1"
	}
	return "3", "4"
}

func (s *feedStub) writeMetrics(w http.ResponseWriter) {
	s.mu.Lock()
	s.metricHits++
	n := s.metricHits
	s.mu.Unlock()
	filled := uint64(0)
	if n > 1 {
		filled = 10
	}
	w.Header().Set("Content-Type", "text/plain")
	var b strings.Builder
	for _, bound := range []string{"0.01", "0.025", "0.05"} {
		b.WriteString("market_tick_commit_seconds_bucket{le=\"" + bound + "\"} 0\n")
	}
	for _, bound := range []string{"0.1", "0.25", "0.5", "1", "2", "+Inf"} {
		b.WriteString("market_tick_commit_seconds_bucket{le=\"" + bound + "\"} " + strconv.FormatUint(filled, 10) + "\n")
	}
	b.WriteString("market_event_last_committed 30\nmarket_event_last_dispatched 30\n")
	for _, name := range scaleTables {
		b.WriteString("market_db_table_bytes{table=\"" + name + "\"} 4096\n")
	}
	_, _ = io.WriteString(w, b.String())
}

func (s *feedStub) stream(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	after, _ := strconv.ParseInt(r.URL.Query().Get("after_seq"), 10, 64)
	s.mu.Lock()
	s.wsHits++
	hits := s.wsHits
	mode := s.mode
	s.mu.Unlock()
	start := after + 1
	if mode == "skip-replay" && hits == 1 {
		start = after + 2
	}
	if mode == "skip-reconnect" && hits > 1 {
		start = after + 2
	}
	if mode == "scale-skip" {
		start = after + 2
	}
	errc := make(chan struct{}, 1)
	go func() {
		for {
			if _, _, err := conn.Read(context.Background()); err != nil {
				errc <- struct{}{}
				return
			}
		}
	}()
	for i := 0; i < 8; i++ {
		raw, _ := json.Marshal(s.event(start + int64(i)))
		writeCtx, cancel := context.WithTimeout(r.Context(), time.Second)
		err := conn.Write(writeCtx, websocket.MessageText, raw)
		cancel()
		if err != nil {
			return
		}
	}
	select {
	case <-errc:
	case <-r.Context().Done():
	}
}

func (s *feedStub) event(seq int64) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events == nil {
		s.events = map[int64]map[string]any{}
	}
	if existing, ok := s.events[seq]; ok {
		return existing
	}
	body := map[string]any{
		"market_id":      "stub",
		"type":           "trade",
		"event_seq":      strconv.FormatInt(seq, 10),
		"symbol":         "D14001F4649BOP",
		"logical_tick":   strconv.FormatInt(seq, 10),
		"market_date":    "2026-11-18",
		"event_time":     "2026-11-18T00:00:01Z",
		"generated_at":   time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		"price_cents":    "100",
		"quantity":       "100",
		"turnover_cents": "10000",
		"schema_version": 1,
	}
	s.events[seq] = body
	return body
}

func writeStubJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
