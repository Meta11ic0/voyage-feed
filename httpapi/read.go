package httpapi

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"voyage-feed/stream"
)

type marketHead struct {
	Dataset   string
	Model     string
	Date      string
	Origin    time.Time
	Tick      int64
	LastSeq   int64
	Retained  int64
	EventTime time.Time
}

func (s *Server) begin(ctx context.Context) (pgx.Tx, error) {
	return s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
}

func (s *Server) head(ctx context.Context, tx pgx.Tx) (marketHead, error) {
	var head marketHead
	err := tx.QueryRow(ctx, `
		SELECT dataset_id, model_version, to_char(market_date, 'YYYY-MM-DD'), clock_origin,
		       logical_tick, last_event_seq, first_retained_seq
		FROM market_instances WHERE market_id = $1`, s.MarketID).Scan(
		&head.Dataset, &head.Model, &head.Date, &head.Origin, &head.Tick, &head.LastSeq, &head.Retained,
	)
	if err != nil {
		return head, err
	}
	head.EventTime = head.Origin.UTC().Add(time.Duration(head.Tick) * time.Second)
	return head, nil
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if err := s.Pool.Ping(r.Context()); err != nil {
		s.NoteDBError()
		s.log(err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "数据库不可用", nil)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.NoteDBError()
		s.log(err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "数据库不可用", nil)
		return
	}
	defer tx.Rollback(r.Context())
	head, err := s.head(r.Context(), tx)
	if err != nil {
		s.NoteDBError()
		s.log(err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "数据库不可用", nil)
		return
	}
	state := "paused"
	if s.ready() {
		state = "running"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"market_id": s.MarketID, "dataset_id": head.Dataset, "model_version": head.Model,
		"market_date": head.Date, "event_time": head.EventTime.Format(time.RFC3339),
		"server_time":  time.Now().UTC().Format(time.RFC3339),
		"logical_tick": num(head.Tick), "last_event_seq": num(head.LastSeq),
		"first_retained_seq": num(head.Retained), "state": state,
	})
}

func (s *Server) instruments(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r.URL.Query().Get("limit"), 100, 1000)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_limit", "limit 超出范围", nil)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.failDB(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	rows, err := tx.Query(r.Context(), `
		SELECT instrument_id, symbol, name, sector, role, chapter_count, popularity_rank,
		       initial_price_cents, target_cap_cents, shares_total, shares_float
		FROM instruments
		WHERE market_id = $1 AND symbol > $2
		ORDER BY symbol
		LIMIT $3`, s.MarketID, r.URL.Query().Get("after_symbol"), limit+1)
	if err != nil {
		s.failDB(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, symbol, name, sector, role string
		var chapters, rank *int
		var price, cap, total, floatShares int64
		if err := rows.Scan(&id, &symbol, &name, &sector, &role, &chapters, &rank, &price, &cap, &total, &floatShares); err != nil {
			s.failDB(w, err)
			return
		}
		items = append(items, map[string]any{
			"instrument_id": id, "symbol": symbol, "name": name, "sector": sector, "role": role,
			"chapter_count": chapters, "popularity_rank": rank,
			"initial_price_cents": num(price), "target_cap_cents": num(cap),
			"shares_total": num(total), "shares_float": num(floatShares),
			"reference_price_cents": num(price), "initial_cap_cents": product(price, total),
		})
	}
	if err := rows.Err(); err != nil {
		s.failDB(w, err)
		return
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]["symbol"].(string)
		next = &last
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_after_symbol": next})
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	tx, err := s.begin(r.Context())
	if err != nil {
		s.failDB(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	head, err := s.head(r.Context(), tx)
	if err != nil {
		s.failDB(w, err)
		return
	}
	symbol := r.URL.Query().Get("symbol")
	sector := r.URL.Query().Get("sector")
	rows, err := tx.Query(r.Context(), `
		SELECT i.symbol, i.name, i.sector, i.initial_price_cents, s.prev_close_cents, s.last_price_cents,
		       s.last_trade_time, s.day_open_cents, s.day_high_cents, s.day_low_cents, s.day_volume, s.day_turnover_cents
		FROM instruments i
		JOIN instrument_states s USING (market_id, instrument_id)
		WHERE i.market_id = $1 AND ($2 = '' OR i.symbol = $2) AND ($3 = '' OR i.sector = $3)
		ORDER BY i.symbol`, s.MarketID, symbol, sector)
	if err != nil {
		s.failDB(w, err)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		item, err := scanQuote(rows)
		if err != nil {
			s.failDB(w, err)
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		s.failDB(w, err)
		return
	}
	if symbol != "" && len(items) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "标的不存在", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"market_id": s.MarketID, "market_date": head.Date,
		"event_time": head.EventTime.Format(time.RFC3339), "as_of_seq": num(head.LastSeq), "items": items,
	})
}

func scanQuote(rows pgx.Rows) (map[string]any, error) {
	var symbol, name, sector string
	var initial, prev, volume, turnover int64
	var last, open, high, low *int64
	var traded *time.Time
	if err := rows.Scan(&symbol, &name, &sector, &initial, &prev, &last, &traded, &open, &high, &low, &volume, &turnover); err != nil {
		return nil, err
	}
	effective := initial
	if last != nil {
		effective = *last
	}
	item := map[string]any{
		"symbol": symbol, "name": name, "sector": sector,
		"initial_price_cents": num(initial), "prev_close_cents": num(prev),
		"last_price_cents": nil, "last_trade_time": nil,
		"open_cents": nil, "high_cents": nil, "low_cents": nil,
		"volume": num(volume), "turnover_cents": num(turnover),
		"change_pct": ratio(big.NewInt(effective), big.NewInt(prev)),
	}
	if last != nil {
		item["last_price_cents"] = num(*last)
	}
	if traded != nil {
		item["last_trade_time"] = traded.UTC().Format(time.RFC3339)
	}
	if open != nil {
		item["open_cents"] = num(*open)
	}
	if high != nil {
		item["high_cents"] = num(*high)
	}
	if low != nil {
		item["low_cents"] = num(*low)
	}
	return item, nil
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	after, err := parseSeq(query.Get("after_seq"), 0, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_cursor", "after_seq 无效", nil)
		return
	}
	limit, err := parseLimit(query.Get("limit"), 1000, 5000)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_limit", "limit 超出范围", nil)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.failDB(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	head, err := s.head(r.Context(), tx)
	if err != nil {
		s.failDB(w, err)
		return
	}
	if after < head.Retained-1 {
		writeError(w, http.StatusGone, "cursor_expired", "游标早于可查询范围", map[string]string{"first_retained_seq": num(head.Retained)})
		return
	}
	until := head.LastSeq
	if raw := query.Get("until_seq"); raw != "" {
		asked, err := parseSeq(raw, 0, true)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_cursor", "until_seq 无效", nil)
			return
		}
		if asked > head.LastSeq {
			writeError(w, http.StatusBadRequest, "bad_cursor", "until_seq 晚于已提交序号", nil)
			return
		}
		until = asked
	}
	items, err := s.readEvents(r.Context(), tx, after, until, limit+1)
	if err != nil {
		s.failDB(w, err)
		return
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	next := after
	if len(items) > 0 {
		next = items[len(items)-1].Seq
	}
	bodies := make([]json.RawMessage, len(items))
	for i, item := range items {
		bodies[i] = item.Body
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": bodies, "until_seq": num(until), "next_after_seq": num(next), "has_more": hasMore,
	})
}

func (s *Server) bars(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	symbol := query.Get("symbol")
	if symbol == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "symbol 必填", nil)
		return
	}
	interval := query.Get("interval")
	if interval == "" {
		interval = "1m"
	}
	if interval != "1m" && interval != "5m" && interval != "1d" {
		writeError(w, http.StatusBadRequest, "bad_request", "周期只接受 1m、5m 或 1d", nil)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.failDB(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	head, err := s.head(r.Context(), tx)
	if err != nil {
		s.failDB(w, err)
		return
	}
	to := head.EventTime.Add(time.Second)
	from := to.Add(-24 * time.Hour)
	if raw := query.Get("to"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "to 不是 RFC3339", nil)
			return
		}
		to = parsed
	}
	if raw := query.Get("from"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "from 不是 RFC3339", nil)
			return
		}
		from = parsed
	}
	if !to.After(from) || to.Sub(from) > 30*24*time.Hour {
		writeError(w, http.StatusBadRequest, "bad_range", "请缩窄范围", nil)
		return
	}
	var instrument string
	err = tx.QueryRow(r.Context(), `SELECT instrument_id FROM instruments WHERE market_id = $1 AND symbol = $2`, s.MarketID, symbol).Scan(&instrument)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "标的不存在", nil)
		return
	}
	// 查询上界扩展到 to 所在周期的结束点，保证聚合周期内分钟线完整；
	// 由此聚合出的周期若起点落在 [from,to) 之外，由下方过滤排除。
	_, upper, _ := barWindow(interval, to)
	rows, err := tx.Query(r.Context(), `
		SELECT bucket_start, open_cents, high_cents, low_cents, close_cents, volume, turnover_cents, trade_count
		FROM bars_1m
		WHERE market_id = $1 AND instrument_id = $2 AND bucket_start >= $3 AND bucket_start < $4
		ORDER BY bucket_start`, s.MarketID, instrument, from.Add(-24*time.Hour), upper)
	if err != nil {
		s.failDB(w, err)
		return
	}
	defer rows.Close()
	var points []minutePoint
	for rows.Next() {
		var item minutePoint
		if err := rows.Scan(&item.Start, &item.Open, &item.High, &item.Low, &item.Close, &item.Volume, &item.Turnover, &item.Count); err != nil {
			s.failDB(w, err)
			return
		}
		points = append(points, item)
	}
	if err := rows.Err(); err != nil {
		s.failDB(w, err)
		return
	}
	bars := aggregateBars(interval, points, head.EventTime)
	items := []map[string]any{}
	for _, bar := range bars {
		if bar.Start.Before(from) || !bar.Start.Before(to) {
			continue
		}
		items = append(items, map[string]any{
			"start_time": bar.Start.UTC().Format(time.RFC3339), "end_time": bar.End.UTC().Format(time.RFC3339),
			"open_cents": num(bar.Open), "high_cents": num(bar.High), "low_cents": num(bar.Low), "close_cents": num(bar.Close),
			"volume": num(bar.Volume), "turnover_cents": num(bar.Turnover), "trade_count": num(bar.Count), "complete": bar.Complete,
		})
	}
	if len(items) > 2000 {
		writeError(w, http.StatusBadRequest, "bad_range", "请缩窄范围", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"as_of_seq": num(head.LastSeq), "items": items})
}

func (s *Server) sectors(w http.ResponseWriter, r *http.Request) {
	tx, err := s.begin(r.Context())
	if err != nil {
		s.failDB(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	head, err := s.head(r.Context(), tx)
	if err != nil {
		s.failDB(w, err)
		return
	}
	rows, err := tx.Query(r.Context(), `
		SELECT i.sector, i.initial_price_cents, s.prev_close_cents, s.last_price_cents, s.day_volume, i.shares_float
		FROM instruments i
		JOIN instrument_states s USING (market_id, instrument_id)
		WHERE i.market_id = $1`, s.MarketID)
	if err != nil {
		s.failDB(w, err)
		return
	}
	defer rows.Close()
	groups := map[string]*capAccum{}
	market := &capAccum{cur: new(big.Int), prev: new(big.Int)}
	for rows.Next() {
		var sector string
		var initial, prev, volume, shares int64
		var last *int64
		if err := rows.Scan(&sector, &initial, &prev, &last, &volume, &shares); err != nil {
			s.failDB(w, err)
			return
		}
		effective := initial
		if last != nil {
			effective = *last
		}
		item := groups[sector]
		if item == nil {
			item = &capAccum{cur: new(big.Int), prev: new(big.Int)}
			groups[sector] = item
		}
		addCap(item, market, effective, prev, shares, volume)
	}
	if err := rows.Err(); err != nil {
		s.failDB(w, err)
		return
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	items := make([]map[string]any, 0, len(names))
	for _, name := range names {
		items = append(items, sectorJSON(name, groups[name]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"as_of_seq": num(head.LastSeq), "items": items, "market": sectorJSON("", market),
	})
}

type capAccum struct {
	cur, prev      *big.Int
	up, down, flat int
	quiet          int
}

func addCap(item, market *capAccum, effective, prev, shares, volume int64) {
	cur := new(big.Int).Mul(big.NewInt(effective), big.NewInt(shares))
	base := new(big.Int).Mul(big.NewInt(prev), big.NewInt(shares))
	item.cur.Add(item.cur, cur)
	item.prev.Add(item.prev, base)
	market.cur.Add(market.cur, cur)
	market.prev.Add(market.prev, base)
	if volume == 0 {
		item.quiet++
		market.quiet++
		return
	}
	switch {
	case effective > prev:
		item.up++
		market.up++
	case effective < prev:
		item.down++
		market.down++
	default:
		item.flat++
		market.flat++
	}
}

func sectorJSON(name string, item *capAccum) map[string]any {
	body := map[string]any{
		"float_cap_cents": item.cur.String(), "change_pct": ratio(item.cur, item.prev),
		"up": item.up, "down": item.down, "flat": item.flat, "no_trades": item.quiet,
	}
	if name != "" {
		body["sector"] = name
	}
	return body
}

func (s *Server) readEvents(ctx context.Context, tx pgx.Tx, after, until int64, limit int) ([]stream.Message, error) {
	rows, err := tx.Query(ctx, `
		SELECT e.event_seq, e.logical_tick, e.event_time, e.generated_at, to_char(e.market_date, 'YYYY-MM-DD'),
		       e.type, i.symbol, e.price_cents, e.quantity, e.turnover_cents
		FROM market_events e
		LEFT JOIN instruments i ON i.market_id = e.market_id AND i.instrument_id = e.instrument_id
		WHERE e.market_id = $1 AND e.event_seq > $2 AND e.event_seq <= $3
		ORDER BY e.event_seq
		LIMIT $4`, s.MarketID, after, until, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stream.Message
	for rows.Next() {
		var seq, tick int64
		var when, generated time.Time
		var day, kind string
		var symbol *string
		var priceN, qtyN, turnN *int64
		if err := rows.Scan(&seq, &tick, &when, &generated, &day, &kind, &symbol, &priceN, &qtyN, &turnN); err != nil {
			return nil, err
		}
		body := map[string]any{
			"schema_version": 1, "market_id": s.MarketID, "event_seq": num(seq), "type": kind,
			"logical_tick": num(tick), "market_date": day,
			"event_time": when.UTC().Format(time.RFC3339), "generated_at": generated.UTC().Format(time.RFC3339),
		}
		if kind == "trade" && symbol != nil && priceN != nil && qtyN != nil && turnN != nil {
			body["symbol"] = *symbol
			body["price_cents"] = num(*priceN)
			body["quantity"] = num(*qtyN)
			body["turnover_cents"] = num(*turnN)
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		out = append(out, stream.Message{Seq: seq, Body: raw})
	}
	return out, rows.Err()
}

func (s *Server) Fetch(ctx context.Context, after, until int64, limit int) ([]stream.Message, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	return s.readEvents(ctx, tx, after, until, limit)
}

// PrepareSnapshot 在同一只读快照内读取高水位、保留边界和补发事件，
// 避免与保留期清理并发时水位与事件不一致。limit 由调用方传入接收上限加一。
func (s *Server) PrepareSnapshot(ctx context.Context, after int64, limit int) (int64, int64, []stream.Message, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, 0, nil, err
	}
	defer tx.Rollback(ctx)
	head, err := s.head(ctx, tx)
	if err != nil {
		return 0, 0, nil, err
	}
	msgs, err := s.readEvents(ctx, tx, after, head.LastSeq, limit)
	if err != nil {
		return 0, 0, nil, err
	}
	return head.LastSeq, head.Retained, msgs, nil
}

func (s *Server) Watermark(ctx context.Context) (int64, int64, error) {
	var high, retained int64
	err := s.Pool.QueryRow(ctx, `SELECT last_event_seq, first_retained_seq FROM market_instances WHERE market_id = $1`, s.MarketID).Scan(&high, &retained)
	return high, retained, err
}

func (s *Server) failDB(w http.ResponseWriter, err error) {
	s.NoteDBError()
	s.log(err)
	writeError(w, http.StatusServiceUnavailable, "unavailable", "数据库不可用", nil)
}

func parseLimit(raw string, fallback, max int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > max {
		return 0, strconv.ErrSyntax
	}
	return value, nil
}

func parseSeq(raw string, fallback int64, required bool) (int64, error) {
	if raw == "" && !required {
		return fallback, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}
