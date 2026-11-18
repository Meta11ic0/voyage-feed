package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

func verifyCommand(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	base := fs.String("base-url", "http://127.0.0.1:8787", "base")
	market := fs.String("market", "one-piece-dev", "market")
	profile := fs.String("profile", "", "profile")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, "verify 参数无效。")
		return 2
	}
	switch *profile {
	case "ready":
		return verifyReady(strings.TrimRight(*base, "/"), stderr)
	case "smoke":
		return verifySmoke(strings.TrimRight(*base, "/"), *market, stderr)
	case "scale":
		return verifyScale(strings.TrimRight(*base, "/"), *market, stderr)
	default:
		fmt.Fprintln(stderr, "verify 需要 --profile ready、smoke 或 scale。")
		return 2
	}
}

func verifyReady(base string, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/readyz", nil)
	if err != nil {
		fmt.Fprintln(stderr, "readyz 请求失败。")
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, "readyz 没有响应。")
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(stderr, "readyz 未就绪。")
		return 1
	}
	return 0
}

func verifySmoke(base, market string, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if verifyReady(base, io.Discard) == 0 {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(stderr, "smoke 等待就绪超时。")
			return 1
		}
		time.Sleep(50 * time.Millisecond)
	}
	if verifyReady(base, io.Discard) != 0 {
		fmt.Fprintln(stderr, "smoke 等待就绪超时。")
		return 1
	}
	first, err := getJSON(ctx, base+"/v1/markets/"+market+"/status")
	if err != nil {
		fmt.Fprintln(stderr, "status 读取失败。")
		return 1
	}
	startSeq := asString(first["last_event_seq"])
	var status map[string]any
	for {
		status, err = getJSON(ctx, base+"/v1/markets/"+market+"/status")
		if err != nil {
			fmt.Fprintln(stderr, "status 读取失败。")
			return 1
		}
		if asString(status["last_event_seq"]) != startSeq || asString(status["logical_tick"]) != asString(first["logical_tick"]) {
			break
		}
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "成交水位没有上涨。")
			return 1
		}
		time.Sleep(20 * time.Millisecond)
	}
	snap, err := getJSON(ctx, base+"/v1/markets/"+market+"/snapshot")
	if err != nil {
		fmt.Fprintln(stderr, "snapshot 读取失败。")
		return 1
	}
	if _, err := getJSON(ctx, base+"/v1/markets/"+market+"/events?after_seq=0&limit=5"); err != nil {
		fmt.Fprintln(stderr, "events 读取失败。")
		return 1
	}
	if _, err := getJSON(ctx, base+"/v1/markets/"+market+"/sectors"); err != nil {
		fmt.Fprintln(stderr, "sectors 读取失败。")
		return 1
	}
	symbol := tradedSymbol(snap)
	if symbol == "" {
		fmt.Fprintln(stderr, "快照里没有成交。")
		return 1
	}
	if _, err := getJSON(ctx, base+"/v1/markets/"+market+"/bars?symbol="+symbol+"&interval=1m"); err != nil {
		fmt.Fprintln(stderr, "bars 读取失败。")
		return 1
	}
	asOf := asString(snap["as_of_seq"])
	wsURL := strings.Replace(base, "http", "ws", 1) + "/v1/stream?market_id=" + market + "&after_seq=" + asOf
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		fmt.Fprintln(stderr, "订阅失败。")
		return 1
	}
	events, err := readBusiness(ctx, conn, 5)
	conn.Close(websocket.StatusNormalClosure, "")
	conn.CloseNow()
	if err != nil || len(events) < 5 {
		fmt.Fprintln(stderr, "实时事件不足。")
		return 1
	}
	last := events[len(events)-1]
	againURL := strings.Replace(base, "http", "ws", 1) + "/v1/stream?market_id=" + market + "&after_seq=" + asString(last["event_seq"])
	conn, _, err = websocket.Dial(ctx, againURL, nil)
	if err != nil {
		fmt.Fprintln(stderr, "重连失败。")
		return 1
	}
	more, err := readBusiness(ctx, conn, 1)
	conn.CloseNow()
	if err != nil || len(more) == 0 {
		fmt.Fprintln(stderr, "重连没有补到事件。")
		return 1
	}
	want := more[0]
	for _, item := range more {
		if item["type"] == "trade" {
			want = item
			break
		}
	}
	seq := asString(want["event_seq"])
	prev, _ := strconv.ParseInt(seq, 10, 64)
	page, err := getJSON(ctx, fmt.Sprintf("%s/v1/markets/%s/events?after_seq=%d&limit=20", base, market, prev-1))
	if err != nil {
		fmt.Fprintln(stderr, "重连对照失败。")
		return 1
	}
	if !sameEvent(want, findEvent(page, seq)) {
		fmt.Fprintln(stderr, "重连事件与 REST 不一致。")
		return 1
	}
	return 0
}

func verifyScale(base, market string, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	if verifyReady(base, stderr) != 0 {
		return 1
	}
	status, err := getJSON(ctx, base+"/v1/markets/"+market+"/status")
	if err != nil {
		fmt.Fprintln(stderr, "status 读取失败。")
		return 1
	}
	after := asString(status["last_event_seq"])
	wsURL := strings.Replace(base, "http", "ws", 1) + "/v1/stream?market_id=" + market + "&after_seq=" + after
	const clients = 10
	type result struct {
		seqs []int64
		lags []float64
		err  error
	}
	results := make([]result, clients)
	var wg sync.WaitGroup
	stop := time.Now().Add(60 * time.Second)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, _, err := websocket.Dial(ctx, wsURL, nil)
			if err != nil {
				results[i].err = err
				return
			}
			defer conn.CloseNow()
			for time.Now().Before(stop) {
				readCtx, cancelRead := context.WithDeadline(ctx, stop)
				_, payload, err := conn.Read(readCtx)
				cancelRead()
				if err != nil {
					if ctx.Err() == nil && time.Now().Before(stop.Add(-time.Second)) {
						results[i].err = err
					}
					return
				}
				var body map[string]any
				if json.Unmarshal(payload, &body) != nil {
					results[i].err = fmt.Errorf("事件无法解析")
					return
				}
				kind := asString(body["type"])
				if kind != "trade" && kind != "session" {
					continue
				}
				seq, err := strconv.ParseInt(asString(body["event_seq"]), 10, 64)
				if err != nil {
					results[i].err = err
					return
				}
				results[i].seqs = append(results[i].seqs, seq)
				if stamp := asString(body["generated_at"]); stamp != "" {
					when, err := time.Parse(time.RFC3339, stamp)
					if err == nil {
						lag := time.Since(when).Seconds()
						if lag < 0 {
							lag = 0
						}
						results[i].lags = append(results[i].lags, lag)
					}
				}
			}
		}(i)
	}
	wg.Wait()
	var lags []float64
	var total int
	for i, item := range results {
		if item.err != nil {
			fmt.Fprintf(stderr, "订阅 %d 失败。\n", i+1)
			return 1
		}
		if len(item.seqs) == 0 {
			fmt.Fprintln(stderr, "压测期间没有事件。")
			return 1
		}
		for j := 1; j < len(item.seqs); j++ {
			if item.seqs[j] != item.seqs[j-1]+1 {
				fmt.Fprintf(stderr, "订阅 %d 序号不连续。\n", i+1)
				return 1
			}
		}
		lags = append(lags, item.lags...)
		total += len(item.seqs)
	}
	caught := false
	var committed, dispatched int64
	deadline := time.Now().Add(5 * time.Second)
	var writeP95 float64
	for {
		body, err := getText(ctx, base+"/metrics")
		if err == nil {
			committed, dispatched, writeP95 = scaleMetrics(body)
			if dispatched >= committed {
				caught = true
				break
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	receiveP95 := percentile(lags, 0.95)
	fmt.Fprintf(stderr, "scale 订阅=10 秒=60 事件=%d 写入p95上限=%.3fs 收到p95=%.3fs 已提交=%d 已发送=%d\n", total/clients, writeP95, receiveP95, committed, dispatched)
	if !caught {
		fmt.Fprintln(stderr, "结束后 5 秒内没有追平。")
		return 1
	}
	if writeP95 > 0.25 {
		fmt.Fprintln(stderr, "写入 p95 超过 250 毫秒。")
		return 1
	}
	if receiveP95 >= 1 {
		fmt.Fprintln(stderr, "收到事件 p95 达到 1 秒。")
		return 1
	}
	return 0
}

func getText(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

func scaleMetrics(body string) (committed, dispatched int64, writeP95 float64) {
	var total uint64
	writeP95 = math.Inf(1)
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "market_tick_commit_seconds_bucket{le=\"+Inf\"} ") {
			total, _ = strconv.ParseUint(strings.TrimPrefix(line, "market_tick_commit_seconds_bucket{le=\"+Inf\"} "), 10, 64)
		}
	}
	need := 0.95 * float64(total)
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "market_event_last_committed "):
			committed, _ = strconv.ParseInt(strings.TrimPrefix(line, "market_event_last_committed "), 10, 64)
		case strings.HasPrefix(line, "market_event_last_dispatched "):
			dispatched, _ = strconv.ParseInt(strings.TrimPrefix(line, "market_event_last_dispatched "), 10, 64)
		case strings.HasPrefix(line, "market_tick_commit_seconds_bucket{le=\"") && !strings.Contains(line, "+Inf"):
			parts := strings.SplitN(strings.TrimPrefix(line, "market_tick_commit_seconds_bucket{le=\""), "\"} ", 2)
			if len(parts) != 2 {
				continue
			}
			count, _ := strconv.ParseUint(parts[1], 10, 64)
			if writeP95 == math.Inf(1) && float64(count) >= need && need > 0 {
				writeP95, _ = strconv.ParseFloat(parts[0], 64)
			}
		}
	}
	return committed, dispatched, writeP95
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return math.Inf(1)
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	index := int(math.Ceil(p*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func getJSON(ctx context.Context, url string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	var body map[string]any
	err = dec.Decode(&body)
	return body, err
}

func readBusiness(ctx context.Context, conn *websocket.Conn, need int) ([]map[string]any, error) {
	var out []map[string]any
	for len(out) < need {
		_, payload, err := conn.Read(ctx)
		if err != nil {
			return out, err
		}
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			return out, err
		}
		kind := asString(body["type"])
		if kind == "trade" || kind == "session" {
			out = append(out, body)
		}
	}
	return out, nil
}

func tradedSymbol(snap map[string]any) string {
	items, _ := snap["items"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["last_price_cents"] != nil {
			return asString(item["symbol"])
		}
	}
	return ""
}

func findEvent(page map[string]any, seq string) map[string]any {
	items, _ := page["items"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if asString(item["event_seq"]) == seq {
			return item
		}
	}
	return nil
}

func sameEvent(left, right map[string]any) bool {
	if left == nil || right == nil {
		return false
	}
	for _, key := range []string{"event_seq", "type", "symbol", "price_cents", "quantity"} {
		if asString(left[key]) != asString(right[key]) {
			return false
		}
	}
	return true
}

func asString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}
