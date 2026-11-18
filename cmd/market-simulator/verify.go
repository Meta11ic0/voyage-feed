package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

var errScaleFirst = errors.New("压测首条序号不连续")
var errScaleMillis = errors.New("generated_at 缺少毫秒")

var scaleTables = []string{"market_events", "bars_1m", "instrument_states", "instruments"}

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
	return verifySmokeWithin(base, market, stderr, 20*time.Second)
}

func verifySmokeWithin(base, market string, stderr io.Writer, budget time.Duration) int {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	deadline := time.Now().Add(budget)
	for {
		if verifyReady(base, io.Discard) == 0 {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			fmt.Fprintln(stderr, "smoke 等待就绪超时。")
			return 1
		}
		time.Sleep(20 * time.Millisecond)
	}
	first, err := getJSON(ctx, base+"/v1/markets/"+market+"/status")
	if err != nil {
		fmt.Fprintln(stderr, "status 读取失败。")
		return 1
	}
	startSeq := asString(first["last_event_seq"])
	startTick := asString(first["logical_tick"])
	var status map[string]any
	for {
		status, err = getJSON(ctx, base+"/v1/markets/"+market+"/status")
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(stderr, "水位没有同时上涨。")
				return 1
			}
			fmt.Fprintln(stderr, "status 读取失败。")
			return 1
		}
		if asString(status["last_event_seq"]) != startSeq && asString(status["logical_tick"]) != startTick {
			break
		}
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "水位没有同时上涨。")
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
	asOfN, asErr := strconv.ParseInt(asOf, 10, 64)
	firstN, firstErr := strconv.ParseInt(asString(events[0]["event_seq"]), 10, 64)
	if asErr != nil || firstErr != nil || firstN != asOfN+1 {
		fmt.Fprintln(stderr, "快照后首条序号不连续。")
		return 1
	}
	for i := 1; i < len(events); i++ {
		prev, errPrev := strconv.ParseInt(asString(events[i-1]["event_seq"]), 10, 64)
		cur, errCur := strconv.ParseInt(asString(events[i]["event_seq"]), 10, 64)
		if errPrev != nil || errCur != nil || cur != prev+1 {
			fmt.Fprintln(stderr, "实时事件序号不连续。")
			return 1
		}
	}
	last := events[len(events)-1]
	lastN, _ := strconv.ParseInt(asString(last["event_seq"]), 10, 64)
	againURL := strings.Replace(base, "http", "ws", 1) + "/v1/stream?market_id=" + market + "&after_seq=" + asString(last["event_seq"])
	conn, _, err = websocket.Dial(ctx, againURL, nil)
	if err != nil {
		fmt.Fprintln(stderr, "重连失败。")
		return 1
	}
	defer conn.CloseNow()
	more, err := readBusiness(ctx, conn, 1)
	if err != nil || len(more) == 0 {
		fmt.Fprintln(stderr, "重连没有补到事件。")
		return 1
	}
	gotN, gotErr := strconv.ParseInt(asString(more[0]["event_seq"]), 10, 64)
	if gotErr != nil || gotN != lastN+1 {
		fmt.Fprintln(stderr, "重连首条序号不连续。")
		return 1
	}
	want := more[0]
	for want["type"] != "trade" {
		extra, err := readBusiness(ctx, conn, 1)
		if err != nil || len(extra) == 0 || len(more) >= 20 {
			fmt.Fprintln(stderr, "重连没有读到成交。")
			return 1
		}
		more = append(more, extra[0])
		want = extra[0]
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
	if !millisStamp(asString(want["generated_at"])) {
		fmt.Fprintln(stderr, "generated_at 小数不足三位。")
		return 1
	}
	fmt.Fprintf(stderr, "smoke 通过 tick=%s seq=%s 首条=%s 重连=%d 对照=trade\n", asString(status["logical_tick"]), asString(status["last_event_seq"]), asString(events[0]["event_seq"]), gotN)
	return 0
}

type scaleOptions struct {
	window  time.Duration
	clients int
	catch   time.Duration
}

func verifyScale(base, market string, stderr io.Writer) int {
	return verifyScaleRun(base, market, stderr, scaleOptions{window: 60 * time.Second, clients: 10, catch: 5 * time.Second})
}

func verifyScaleRun(base, market string, stderr io.Writer, opt scaleOptions) int {
	if opt.window <= 0 {
		opt.window = 60 * time.Second
	}
	if opt.clients <= 0 {
		opt.clients = 10
	}
	if opt.catch <= 0 {
		opt.catch = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), opt.window+opt.catch+15*time.Second)
	defer cancel()
	if verifyReady(base, stderr) != 0 {
		return 1
	}
	fmt.Fprintf(stderr, "scale 硬件 %s\n", containerHardware())
	status, err := getJSON(ctx, base+"/v1/markets/"+market+"/status")
	if err != nil {
		fmt.Fprintln(stderr, "status 读取失败。")
		return 1
	}
	after := asString(status["last_event_seq"])
	cursor, err := strconv.ParseInt(after, 10, 64)
	if err != nil {
		fmt.Fprintln(stderr, "status 序号无效。")
		return 1
	}
	startBody, err := getText(ctx, base+"/metrics")
	if err != nil {
		fmt.Fprintln(stderr, "metrics 读取失败。")
		return 1
	}
	startHist, ok := parseHist(startBody)
	if !ok {
		fmt.Fprintln(stderr, "写入直方图缺失。")
		return 1
	}
	wsURL := strings.Replace(base, "http", "ws", 1) + "/v1/stream?market_id=" + market + "&after_seq=" + after
	type result struct {
		seqs []int64
		lags []float64
		last int64
		err  error
	}
	results := make([]result, opt.clients)
	var wg sync.WaitGroup
	stop := time.Now().Add(opt.window)
	var reqMu sync.Mutex
	var samples []float64
	var reqErr int
	sampleCtx, sampleCancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			started := time.Now()
			_, err := getJSON(sampleCtx, base+"/v1/markets/"+market+"/status")
			reqMu.Lock()
			if err != nil {
				reqErr++
			} else {
				samples = append(samples, time.Since(started).Seconds())
			}
			reqMu.Unlock()
			select {
			case <-sampleCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	for i := 0; i < opt.clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, _, err := websocket.Dial(ctx, wsURL, nil)
			if err != nil {
				results[i].err = err
				return
			}
			defer conn.CloseNow()
			first := true
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
				if first && seq != cursor+1 {
					results[i].err = errScaleFirst
					return
				}
				first = false
				results[i].seqs = append(results[i].seqs, seq)
				results[i].last = seq
				stamp := asString(body["generated_at"])
				if !millisStamp(stamp) {
					results[i].err = errScaleMillis
					return
				}
				when, err := time.Parse(time.RFC3339Nano, stamp)
				if err != nil {
					results[i].err = err
					return
				}
				lag := time.Since(when).Seconds()
				if lag < 0 {
					lag = 0
				}
				results[i].lags = append(results[i].lags, lag)
			}
		}(i)
	}
	wg.Wait()
	sampleCancel()
	var lags []float64
	var total int
	for i, item := range results {
		if errors.Is(item.err, errScaleFirst) {
			fmt.Fprintln(stderr, "压测首条序号不连续。")
			return 1
		}
		if errors.Is(item.err, errScaleMillis) {
			fmt.Fprintln(stderr, "generated_at 缺少毫秒。")
			return 1
		}
		if item.err != nil {
			fmt.Fprintf(stderr, "订阅 %d 失败。\n", i+1)
			return 1
		}
		if len(item.seqs) == 0 {
			fmt.Fprintln(stderr, "压测期间没有事件。")
			return 1
		}
		if item.seqs[0] != cursor+1 {
			fmt.Fprintln(stderr, "压测首条序号不连续。")
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
	endBody, err := getText(ctx, base+"/metrics")
	if err != nil {
		fmt.Fprintln(stderr, "metrics 读取失败。")
		return 1
	}
	committed, dispatched := gaugePair(endBody)
	for i, item := range results {
		fmt.Fprintf(stderr, "scale 客户端 %d 水位差=%d\n", i+1, committed-item.last)
	}
	sizes := parseTableBytes(endBody)
	for _, name := range scaleTables {
		value, found := sizes[name]
		if !found {
			fmt.Fprintf(stderr, "表体积指标缺失。\n")
			return 1
		}
		fmt.Fprintf(stderr, "scale 表 %s 字节=%d\n", name, value)
	}
	endHist, ok := parseHist(endBody)
	if !ok {
		fmt.Fprintln(stderr, "写入直方图缺失。")
		return 1
	}
	writeP95, _, kind := histWindow(startHist, endHist)
	switch kind {
	case "restart":
		fmt.Fprintln(stderr, "窗口内重启。")
		return 1
	case "empty":
		fmt.Fprintln(stderr, "窗口内无提交。")
		return 1
	case "missing":
		fmt.Fprintln(stderr, "写入直方图缺失。")
		return 1
	}
	caught := dispatched >= committed
	deadline := time.Now().Add(opt.catch)
	for !caught {
		body, err := getText(ctx, base+"/metrics")
		if err == nil {
			committed, dispatched = gaugePair(body)
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
	reqMu.Lock()
	reqP95 := percentile(samples, 0.95)
	reqN := len(samples)
	reqFailed := reqErr
	reqMu.Unlock()
	receiveP95 := percentile(lags, 0.95)
	fmt.Fprintf(stderr, "scale 订阅=%d 秒=%.0f 事件=%d 请求样本=%d 请求错误=%d 请求p95=%.3fs 写入p95上限=%.3fs 收到p95=%.3fs 已提交=%d 已发送=%d\n", opt.clients, opt.window.Seconds(), total/opt.clients, reqN, reqFailed, reqP95, writeP95, receiveP95, committed, dispatched)
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
	if reqN == 0 || math.IsNaN(reqP95) || math.IsInf(reqP95, 0) {
		fmt.Fprintln(stderr, "请求样本为空或请求 p95 不是有限数。")
		return 1
	}
	return 0
}

func containerHardware() string {
	mem := "unknown"
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				mem = strings.TrimSpace(strings.TrimPrefix(line, "MemTotal:"))
				break
			}
		}
	}
	cgroup := "unknown"
	for _, path := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		cgroup = strings.TrimSpace(string(raw))
		break
	}
	return fmt.Sprintf("视角=容器 nproc=%d mem_total=%s cgroup_memory=%s", runtime.NumCPU(), mem, cgroup)
}

type histSnap struct {
	bounds []float64
	counts []uint64
}

func parseHist(body string) (histSnap, bool) {
	var snap histSnap
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "market_tick_commit_seconds_bucket{le=\"") {
			continue
		}
		rest := strings.TrimPrefix(line, "market_tick_commit_seconds_bucket{le=\"")
		parts := strings.SplitN(rest, "\"} ", 2)
		if len(parts) != 2 {
			continue
		}
		var bound float64
		if parts[0] == "+Inf" {
			bound = math.Inf(1)
		} else {
			parsed, err := strconv.ParseFloat(parts[0], 64)
			if err != nil {
				continue
			}
			bound = parsed
		}
		count, err := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			continue
		}
		snap.bounds = append(snap.bounds, bound)
		snap.counts = append(snap.counts, count)
	}
	return snap, len(snap.counts) > 0
}

func histWindow(start, end histSnap) (p95 float64, total uint64, kind string) {
	if len(start.counts) == 0 || len(start.counts) != len(end.counts) {
		return math.Inf(1), 0, "missing"
	}
	delta := make([]uint64, len(end.counts))
	for i := range end.counts {
		if end.counts[i] < start.counts[i] {
			return 0, 0, "restart"
		}
		delta[i] = end.counts[i] - start.counts[i]
		if i > 0 && delta[i] < delta[i-1] {
			return 0, 0, "restart"
		}
	}
	total = delta[len(delta)-1]
	if total == 0 {
		return 0, 0, "empty"
	}
	need := 0.95 * float64(total)
	p95 = math.Inf(1)
	for i, count := range delta {
		if float64(count) >= need {
			return end.bounds[i], total, "ok"
		}
	}
	return p95, total, "ok"
}

func gaugePair(body string) (committed, dispatched int64) {
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "market_event_last_committed "):
			committed, _ = strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "market_event_last_committed ")), 10, 64)
		case strings.HasPrefix(line, "market_event_last_dispatched "):
			dispatched, _ = strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "market_event_last_dispatched ")), 10, 64)
		}
	}
	return committed, dispatched
}

func parseTableBytes(body string) map[string]int64 {
	out := map[string]int64{}
	const prefix = "market_db_table_bytes{table=\""
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimPrefix(line, prefix)
		parts := strings.SplitN(rest, "\"} ", 2)
		if len(parts) != 2 {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			continue
		}
		out[parts[0]] = value
	}
	return out
}

func millisStamp(stamp string) bool {
	dot := strings.LastIndex(stamp, ".")
	if dot < 0 || dot+1 >= len(stamp) {
		return false
	}
	n := 0
	for _, c := range stamp[dot+1:] {
		if c < '0' || c > '9' {
			break
		}
		n++
	}
	return n >= 3
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
	for _, key := range []string{
		"market_id", "event_seq", "type", "symbol", "logical_tick", "market_date",
		"event_time", "generated_at", "price_cents", "quantity", "turnover_cents",
	} {
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
