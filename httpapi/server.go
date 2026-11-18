package httpapi

import (
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"voyage-feed/stream"
)

var histBounds = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2}

type Server struct {
	MarketID   string
	Interval   time.Duration
	Pool       *pgxpool.Pool
	Hub        *stream.Hub
	MaxClients int
	Log        *slog.Logger
	mu         sync.Mutex
	lockHeld   bool
	lastCommit time.Time
	lastSeq    int64
	lastTick   int64
	dbErrors   uint64
	disconnect uint64
	buckets    [8]uint64
	inf        uint64
	sum        float64
	count      uint64
}

func (s *Server) Locked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lockHeld
}

func (s *Server) SetLocked(held bool) {
	s.mu.Lock()
	s.lockHeld = held
	s.mu.Unlock()
}

func (s *Server) Observe(seq, tick int64, elapsed time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSeq = seq
	s.lastTick = tick
	s.lastCommit = time.Now()
	s.count++
	s.sum += elapsed.Seconds()
	seconds := elapsed.Seconds()
	placed := false
	for i, bound := range histBounds {
		if seconds <= bound {
			s.buckets[i]++
			placed = true
			break
		}
	}
	if !placed {
		s.inf++
	}
}

func (s *Server) NoteDBError() {
	s.mu.Lock()
	s.dbErrors++
	s.mu.Unlock()
}

func (s *Server) NoteDisconnect() {
	s.mu.Lock()
	s.disconnect++
	s.mu.Unlock()
}

func (s *Server) ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lockHeld || s.lastCommit.IsZero() {
		return false
	}
	window := 5 * time.Second
	if three := 3 * s.Interval; three > window {
		window = three
	}
	return time.Since(s.lastCommit) <= window
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method", "只接受 GET", nil)
			return
		}
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		case "/readyz":
			s.readyz(w, r)
			return
		case "/metrics":
			s.metrics(w)
			return
		case "/v1/stream":
			s.stream(w, r)
			return
		}
		const prefix = "/v1/markets/"
		if len(r.URL.Path) <= len(prefix) || r.URL.Path[:len(prefix)] != prefix {
			writeError(w, http.StatusNotFound, "not_found", "路径不存在", nil)
			return
		}
		rest := r.URL.Path[len(prefix):]
		market, action, ok := splitMarket(rest)
		if !ok || market != s.MarketID {
			writeError(w, http.StatusNotFound, "not_found", "市场不存在", nil)
			return
		}
		switch action {
		case "status":
			s.status(w, r)
		case "instruments":
			s.instruments(w, r)
		case "snapshot":
			s.snapshot(w, r)
		case "events":
			s.events(w, r)
		case "bars":
			s.bars(w, r)
		case "sectors":
			s.sectors(w, r)
		default:
			writeError(w, http.StatusNotFound, "not_found", "路径不存在", nil)
		}
	})
}

func splitMarket(rest string) (string, string, bool) {
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			if i == 0 || i == len(rest)-1 {
				return "", "", false
			}
			return rest[:i], rest[i+1:], true
		}
	}
	return "", "", false
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.Pool.Ping(r.Context()); err != nil {
		s.NoteDBError()
		s.log(err)
		http.Error(w, "unavailable\n", http.StatusServiceUnavailable)
		return
	}
	if !s.ready() {
		http.Error(w, "unavailable\n", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) log(err error) {
	if err != nil && s.Log != nil {
		s.Log.Error("数据库不可用")
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string, details map[string]string) {
	body := map[string]any{"code": code, "message": message}
	if details != nil {
		body["details"] = details
	}
	writeJSON(w, status, map[string]any{"error": body})
}

func num(v int64) string { return strconv.FormatInt(v, 10) }

func ratio(current, base *big.Int) json.Number {
	if base.Sign() == 0 {
		return json.Number("0")
	}
	rat := new(big.Rat).SetFrac(current, base)
	rat.Sub(rat, big.NewRat(1, 1))
	return json.Number(rat.FloatString(8))
}

func product(a, b int64) string {
	return new(big.Int).Mul(big.NewInt(a), big.NewInt(b)).String()
}
