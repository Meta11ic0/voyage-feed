package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"voyage-feed/stream"
)

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Get("market_id") != s.MarketID {
		writeError(w, http.StatusNotFound, "not_found", "市场不存在", nil)
		return
	}
	after, err := strconv.ParseInt(query.Get("after_seq"), 10, 64)
	if query.Get("after_seq") == "" || err != nil {
		writeError(w, http.StatusBadRequest, "bad_cursor", "after_seq 无效", nil)
		return
	}
	if s.MaxClients > 0 && s.Hub.Clients() >= s.MaxClients {
		writeError(w, http.StatusServiceUnavailable, "over_capacity", "订阅已满", nil)
		return
	}
	sub, high, replay, status, prepErr := s.Hub.Prepare(r.Context(), after)
	if status != 0 {
		s.writeStreamStatus(w, r, status, prepErr)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.Hub.Release(sub)
		return
	}
	defer conn.CloseNow()
	defer s.Hub.Release(sub)
	ctx := r.Context()
	go func() {
		select {
		case <-ctx.Done():
		case <-sub.Slow():
			_ = conn.Close(websocket.StatusTryAgainLater, "队列已满")
		}
	}()
	write := func(body []byte) error {
		timer := time.AfterFunc(3*time.Second, func() {
			_ = conn.Close(websocket.StatusTryAgainLater, "发送超时")
		})
		defer timer.Stop()
		return conn.Write(ctx, websocket.MessageText, body)
	}
	subscribed, _ := json.Marshal(map[string]any{
		"type": "subscribed", "market_id": s.MarketID, "replay_until_seq": strconv.FormatInt(high, 10),
	})
	if !s.writeFrame(write, conn, sub, subscribed) {
		return
	}
	for _, item := range replay {
		if !s.writeFrame(write, conn, sub, item.Body) {
			return
		}
	}
	go s.readOnly(ctx, conn)
	go s.ping(ctx, conn)
	heart := time.NewTicker(5 * time.Second)
	defer heart.Stop()
	for {
		select {
		case <-sub.Slow():
			s.NoteDisconnect()
			_ = conn.Close(websocket.StatusTryAgainLater, "队列已满")
			return
		default:
		}
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		case <-sub.Slow():
			_ = conn.Close(websocket.StatusTryAgainLater, "队列已满")
			return
		case <-heart.C:
			if !s.writeFrame(write, conn, sub, s.heartbeat()) {
				return
			}
		case msg := <-sub.Chan():
			if msg.Seq <= high {
				continue
			}
			if !s.writeFrame(write, conn, sub, msg.Body) {
				return
			}
		}
	}
}

func (s *Server) writeFrame(write func([]byte) error, conn *websocket.Conn, sub *stream.Sub, body []byte) bool {
	if err := write(body); err != nil {
		select {
		case <-sub.Slow():
			_ = conn.Close(websocket.StatusTryAgainLater, "队列已满")
		default:
		}
		return false
	}
	return true
}

func (s *Server) writeStreamStatus(w http.ResponseWriter, r *http.Request, status int, err error) {
	switch status {
	case http.StatusGone:
		_, retained, werr := s.Watermark(r.Context())
		if werr != nil {
			s.failDB(w, werr)
			return
		}
		writeError(w, http.StatusGone, "cursor_expired", "游标早于可查询范围", map[string]string{"first_retained_seq": num(retained)})
	case http.StatusConflict:
		writeError(w, http.StatusConflict, "replay_too_large", "续订缺口超过 1024，请先查询历史", nil)
	case http.StatusBadRequest:
		writeError(w, http.StatusBadRequest, "bad_cursor", "after_seq 晚于已提交序号", nil)
	default:
		s.failDB(w, err)
	}
}

func (s *Server) heartbeat() []byte {
	s.mu.Lock()
	seq := s.lastSeq
	tick := s.lastTick
	s.mu.Unlock()
	body, _ := json.Marshal(map[string]any{
		"type": "heartbeat", "last_event_seq": num(seq), "logical_tick": num(tick),
		"server_time": time.Now().UTC().Format(time.RFC3339),
	})
	return body
}

func (s *Server) readOnly(ctx context.Context, conn *websocket.Conn) {
	for {
		_, _, err := conn.Read(ctx)
		if err != nil {
			return
		}
		_ = conn.Close(websocket.StatusPolicyViolation, "只读连接")
		return
	}
}

func (s *Server) ping(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}
