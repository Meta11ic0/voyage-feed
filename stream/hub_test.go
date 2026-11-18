package stream

import (
	"context"
	"errors"
	"testing"
	"time"
)

func seqMsgs(from, to int64) []Message {
	out := make([]Message, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, Message{Seq: i})
	}
	return out
}

// drain 在短暂超时内排空订阅通道，返回按序收到的消息。
func drain(t *testing.T, sub *Sub) []Message {
	t.Helper()
	var got []Message
	for {
		select {
		case msg, ok := <-sub.Chan():
			if !ok {
				return got
			}
			got = append(got, msg)
		case <-time.After(50 * time.Millisecond):
			return got
		}
	}
}

func expectSlowClosed(t *testing.T, sub *Sub) {
	t.Helper()
	select {
	case _, ok := <-sub.Slow():
		if ok {
			t.Fatal("slow 通道收到值，期望已关闭")
		}
	case <-time.After(time.Second):
		t.Fatal("slow 通道未关闭")
	}
}

func expectNoSlow(t *testing.T, sub *Sub) {
	t.Helper()
	select {
	case _, ok := <-sub.Slow():
		if !ok {
			t.Fatal("slow 通道不应关闭")
		}
		t.Fatal("slow 通道不应有值")
	default:
	}
}

// contiguous 断言消息恰为 from..to 的连续序列，无缺、无重、有序。
func contiguous(t *testing.T, msgs []Message, from, to int64) {
	t.Helper()
	if int64(len(msgs)) != to-from+1 {
		t.Fatalf("收到 %d 条，期望 %d 条（%d..%d）", len(msgs), to-from+1, from, to)
	}
	for i, msg := range msgs {
		if msg.Seq != from+int64(i) {
			t.Fatalf("第 %d 条 seq=%d，期望 %d", i, msg.Seq, from+int64(i))
		}
	}
}

// 缺陷 A 本体：prime 失败后已有订阅，首次成功 flush 跳位会漏发，
// 必须按协议断开订阅而不是静默跳号。
func TestFlushNotStartedKicksSubscribers(t *testing.T) {
	hub := NewHub()
	hub.PrepareSnapshot = func(ctx context.Context, after int64, limit int) (int64, int64, []Message, error) {
		return 100, 1, seqMsgs(99, 100), nil
	}
	hub.Watermark = func(ctx context.Context) (int64, int64, error) { return 105, 1, nil }
	hub.Fetch = func(ctx context.Context, after, until int64, limit int) ([]Message, error) {
		t.Fatal("跳位路径不应 Fetch")
		return nil, nil
	}
	prep := hub.Prepare(context.Background(), 98)
	if prep.Status != 0 || prep.High != 100 {
		t.Fatalf("Prepare 异常：%+v", prep)
	}
	contiguous(t, prep.Replay, 99, 100)
	// 模拟 prime 失败：不调用 prime，直接进入首次 flush。
	hub.flush(context.Background())
	expectSlowClosed(t, prep.Sub)
	if got := hub.Last(); got != 105 {
		t.Fatalf("last=%d，期望 105", got)
	}
	if hub.Clients() != 0 {
		t.Fatalf("订阅应全部断开，剩余 %d", hub.Clients())
	}
}

// 无订阅者时首次 flush 仅跳位，不应有额外动作。
func TestFlushNotStartedNoSubscribers(t *testing.T) {
	hub := NewHub()
	hub.Watermark = func(ctx context.Context) (int64, int64, error) { return 105, 1, nil }
	hub.Fetch = func(ctx context.Context, after, until int64, limit int) ([]Message, error) {
		t.Fatal("无订阅者跳位不应 Fetch")
		return nil, nil
	}
	hub.flush(context.Background())
	if got := hub.Last(); got != 105 {
		t.Fatalf("last=%d，期望 105", got)
	}
}

// 正常启动语义：prime 成功后从高水位往后推送，不重推历史。
func TestPrimeStartsAtHighThenPushesForward(t *testing.T) {
	hub := NewHub()
	water := int64(50)
	hub.Watermark = func(ctx context.Context) (int64, int64, error) { return water, 1, nil }
	hub.PrepareSnapshot = func(ctx context.Context, after int64, limit int) (int64, int64, []Message, error) {
		return water, 1, nil, nil
	}
	hub.Fetch = func(ctx context.Context, after, until int64, limit int) ([]Message, error) {
		if after != 50 {
			t.Fatalf("Fetch after=%d，期望 50（不重推历史）", after)
		}
		return seqMsgs(51, water), nil
	}
	hub.prime(context.Background())
	prep := hub.Prepare(context.Background(), 50)
	if prep.Status != 0 || prep.High != 50 {
		t.Fatalf("Prepare 异常：%+v", prep)
	}
	water = 53
	hub.flush(context.Background())
	expectNoSlow(t, prep.Sub)
	contiguous(t, drain(t, prep.Sub), 51, 53)
}

// 保留边界越过分发游标：断开重连，不 Fetch。
func TestFlushBehindRetentionKicks(t *testing.T) {
	hub := NewHub()
	hub.Watermark = func(ctx context.Context) (int64, int64, error) { return 30, 20, nil }
	hub.PrepareSnapshot = func(ctx context.Context, after int64, limit int) (int64, int64, []Message, error) {
		return 30, 20, seqMsgs(21, 30), nil
	}
	hub.Fetch = func(ctx context.Context, after, until int64, limit int) ([]Message, error) {
		t.Fatal("behind 路径不应 Fetch")
		return nil, nil
	}
	hub.prime(context.Background())
	// 模拟分发游标落后到保留边界之前。
	hub.mu.Lock()
	hub.last = 10
	hub.mu.Unlock()
	prep := hub.Prepare(context.Background(), 19)
	if prep.Status != 0 {
		t.Fatalf("Prepare 异常：%d", prep.Status)
	}
	hub.flush(context.Background())
	expectSlowClosed(t, prep.Sub)
	if got := hub.Last(); got != 30 {
		t.Fatalf("last=%d，期望 30", got)
	}
	if got := drain(t, prep.Sub); len(got) != 0 {
		t.Fatalf("behind 不应投递，收到 %d 条", len(got))
	}
}

// 批次起点不连续（水位与事件两次读取之间发生清理）：断开并对齐，
// 不得投递部分批次。
func TestFlushBatchGapDefence(t *testing.T) {
	hub := NewHub()
	water := int64(10)
	hub.Watermark = func(ctx context.Context) (int64, int64, error) { return water, 1, nil }
	hub.Fetch = func(ctx context.Context, after, until int64, limit int) ([]Message, error) {
		if after != 10 {
			t.Fatalf("Fetch after=%d，期望 10", after)
		}
		// 首条为 15，缺 11..14，模拟清理在两次读取之间越过分发游标。
		return seqMsgs(15, 30), nil
	}
	hub.PrepareSnapshot = func(ctx context.Context, after int64, limit int) (int64, int64, []Message, error) {
		return water, 1, nil, nil
	}
	hub.prime(context.Background())
	prep := hub.Prepare(context.Background(), 10)
	if prep.Status != 0 {
		t.Fatalf("Prepare 异常：%d", prep.Status)
	}
	water = 30
	hub.flush(context.Background())
	expectSlowClosed(t, prep.Sub)
	if got := hub.Last(); got != 30 {
		t.Fatalf("last=%d，期望 30", got)
	}
	if got := drain(t, prep.Sub); len(got) != 0 {
		t.Fatalf("不应投递部分批次，收到 %d 条", len(got))
	}
}

// 超过单批上限时逐批追平，批间衔接连续。订阅通道容量小于总量，
// 需要边投递边消费。
func TestFlushMultiBatchContiguous(t *testing.T) {
	hub := NewHub()
	water := int64(0)
	hub.Watermark = func(ctx context.Context) (int64, int64, error) { return water, 1, nil }
	hub.PrepareSnapshot = func(ctx context.Context, after int64, limit int) (int64, int64, []Message, error) {
		return water, 1, nil, nil
	}
	hub.Fetch = func(ctx context.Context, after, until int64, limit int) ([]Message, error) {
		end := after + int64(limit)
		if end > until {
			end = until
		}
		// 批间留出时间让消费协程排空通道，模拟持续在线的订阅者。
		time.Sleep(5 * time.Millisecond)
		return seqMsgs(after+1, end), nil
	}
	hub.prime(context.Background())
	prep := hub.Prepare(context.Background(), 0)
	if prep.Status != 0 {
		t.Fatalf("Prepare 异常：%d", prep.Status)
	}
	done := make(chan []Message, 1)
	go func() {
		var got []Message
		for int64(len(got)) < 2500 {
			select {
			case msg := <-prep.Sub.Chan():
				got = append(got, msg)
			case <-time.After(2 * time.Second):
				done <- got
				return
			}
		}
		done <- got
	}()
	water = 2500
	hub.flush(context.Background())
	got := <-done
	expectNoSlow(t, prep.Sub)
	contiguous(t, got, 1, 2500)
}

// 恢复链路集合等式：被踢前的补发与重连补发合并后恰为 (after, 最终高水位]
// 的连续全集，无缺、无重。覆盖缺陷 A 丢失段被重连补发补齐。
func TestKickRecoveryReplayCoversGap(t *testing.T) {
	hub := NewHub()
	water := int64(100)
	hub.Watermark = func(ctx context.Context) (int64, int64, error) { return water, 1, nil }
	hub.PrepareSnapshot = func(ctx context.Context, after int64, limit int) (int64, int64, []Message, error) {
		return water, 1, seqMsgs(after+1, water), nil
	}
	hub.Fetch = func(ctx context.Context, after, until int64, limit int) ([]Message, error) {
		t.Fatal("本场景不应 Fetch")
		return nil, nil
	}
	first := hub.Prepare(context.Background(), 98)
	if first.Status != 0 || len(first.Replay) != 2 {
		t.Fatalf("首次 Prepare 异常：%+v", first)
	}
	water = 105
	hub.flush(context.Background())
	expectSlowClosed(t, first.Sub)
	second := hub.Prepare(context.Background(), 100)
	if second.Status != 0 {
		t.Fatalf("重连 Prepare 失败：%d", second.Status)
	}
	combined := append(append([]Message{}, first.Replay...), second.Replay...)
	contiguous(t, combined, 99, 105)
}

// 状态码由注入的 PrepareSnapshot 决定，不再走 Watermark+Fetch 两次读取。
func TestPrepareSnapshotStatuses(t *testing.T) {
	ctx := context.Background()
	hub := NewHub()
	if err := hub.Validate(); err == nil {
		t.Fatal("未配置 PrepareSnapshot 时应拒绝启动")
	}
	missing := hub.Prepare(ctx, 0)
	if missing.Status != 503 || missing.Err == nil {
		t.Fatalf("nil PrepareSnapshot 应失败，得到 %d/%v", missing.Status, missing.Err)
	}
	hub.PrepareSnapshot = func(context.Context, int64, int) (int64, int64, []Message, error) {
		return 100, 1, seqMsgs(100, 100), nil
	}
	if err := hub.Validate(); err != nil {
		t.Fatal(err)
	}
	ok := hub.Prepare(ctx, 99)
	if ok.Status != 0 || len(ok.Replay) != 1 || ok.Replay[0].Seq != 100 {
		t.Fatalf("Prepare 异常：%+v", ok)
	}
	hub.Release(ok.Sub)
	hub.PrepareSnapshot = func(context.Context, int64, int) (int64, int64, []Message, error) {
		return 100, 1, nil, nil
	}
	bad := hub.Prepare(ctx, 101)
	if bad.Status != 400 {
		t.Fatalf("期望 400，得到 %d", bad.Status)
	}
	hub.PrepareSnapshot = func(context.Context, int64, int) (int64, int64, []Message, error) {
		return 100, 50, nil, nil
	}
	gone := hub.Prepare(ctx, 10)
	if gone.Status != 410 || gone.Retained != 50 {
		t.Fatalf("期望 410 且 Retained=50，得到 %d/%d", gone.Status, gone.Retained)
	}
	hub.PrepareSnapshot = func(_ context.Context, after int64, limit int) (int64, int64, []Message, error) {
		return after + int64(limit), 1, seqMsgs(after+1, after+int64(limit)), nil
	}
	big := hub.Prepare(ctx, 0)
	if big.Status != 409 {
		t.Fatalf("期望 409，得到 %d", big.Status)
	}
	hub.PrepareSnapshot = func(context.Context, int64, int) (int64, int64, []Message, error) {
		return 0, 0, nil, errors.New("db down")
	}
	unavailable := hub.Prepare(ctx, 0)
	if unavailable.Status != 503 || unavailable.Err == nil {
		t.Fatalf("期望 503 带错误，得到 %d/%v", unavailable.Status, unavailable.Err)
	}
}
