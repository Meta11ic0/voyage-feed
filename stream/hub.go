package stream

import (
	"context"
	"errors"
	"sync"
	"time"
)

const QueueLimit = 1024

type Message struct {
	Seq  int64
	Body []byte
}

type Sub struct {
	ch   chan Message
	slow chan struct{}
}

func (s *Sub) Slow() <-chan struct{} { return s.slow }

func (s *Sub) Chan() <-chan Message { return s.ch }

type Hub struct {
	Fetch     func(ctx context.Context, after, until int64, limit int) ([]Message, error)
	Watermark func(ctx context.Context) (high, retained int64, err error)
	// PrepareSnapshot 在同一只读快照内读取高水位、保留边界和补发事件，limit 传入
	// 实际接收上限加一（多取一条用于判断缺口超限）。生产接线必须设置。
	// 未设置时 Validate 拒绝启动，Prepare 直接失败，不再回退到两次读取。
	PrepareSnapshot func(ctx context.Context, after int64, limit int) (high, retained int64, msgs []Message, err error)
	Notify          chan struct{}
	OnDBError       func()
	OnSlow          func()
	mu              sync.Mutex
	subs            map[*Sub]struct{}
	last            int64
	started         bool
}

// PrepareResult 是建连补发的结果：成功时 Status 为 0，Sub 与 Replay 可用；
// 410 时 Retained 为快照内保留边界，用于向客户端说明可续订起点。
type PrepareResult struct {
	Sub      *Sub
	High     int64
	Retained int64
	Replay   []Message
	Status   int
	Err      error
}

func NewHub() *Hub {
	return &Hub{subs: map[*Sub]struct{}{}, Notify: make(chan struct{}, 1)}
}

// Validate 在监听前调用。PrepareSnapshot 为空时拒绝启动。
func (h *Hub) Validate() error {
	if h == nil || h.PrepareSnapshot == nil {
		return errors.New("PrepareSnapshot 未配置")
	}
	return nil
}

func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func (h *Hub) Last() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

func (h *Hub) Prepare(ctx context.Context, after int64) PrepareResult {
	item := &Sub{ch: make(chan Message, QueueLimit), slow: make(chan struct{})}
	h.mu.Lock()
	h.subs[item] = struct{}{}
	h.mu.Unlock()
	fail := func(status int, err error) PrepareResult {
		h.remove(item)
		return PrepareResult{Status: status, Err: err}
	}
	if h.PrepareSnapshot == nil {
		return fail(503, errors.New("PrepareSnapshot 未配置"))
	}
	high, retained, replay, err := h.PrepareSnapshot(ctx, after, QueueLimit+1)
	if err != nil {
		return fail(503, err)
	}
	if after < retained-1 {
		h.remove(item)
		return PrepareResult{Retained: retained, Status: 410}
	}
	if after > high {
		return fail(400, nil)
	}
	if len(replay) > QueueLimit {
		return fail(409, nil)
	}
	return PrepareResult{Sub: item, High: high, Replay: replay}
}

func (h *Hub) remove(item *Sub) {
	h.mu.Lock()
	delete(h.subs, item)
	h.mu.Unlock()
}

func (h *Hub) Release(item *Sub) {
	if item == nil {
		return
	}
	h.remove(item)
}

func (h *Hub) Run(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	h.prime(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.Notify:
			h.flush(ctx)
		case <-ticker.C:
			h.flush(ctx)
		}
	}
}

func (h *Hub) prime(ctx context.Context) {
	if h.Watermark == nil {
		return
	}
	high, _, err := h.Watermark(ctx)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.last = high
	h.started = true
	h.mu.Unlock()
}

func (h *Hub) flush(ctx context.Context) {
	if h.Fetch == nil || h.Watermark == nil {
		return
	}
	for {
		high, retained, err := h.Watermark(ctx)
		if err != nil {
			if h.OnDBError != nil {
				h.OnDBError()
			}
			return
		}
		h.mu.Lock()
		last := h.last
		started := h.started
		h.mu.Unlock()
		if !started {
			// prime 未成功（数据库故障）后首次读到水位：对齐到当前高水位，
			// 不重推全部历史。窗口内已有订阅时，跳位会漏发其补发基线之后的
			// 已提交事件，按协议断开，由客户端用最后连续消费序号重连续读。
			h.resync(high)
			return
		}
		if last+1 < retained {
			// 缺口起点已被保留期清理，无法继续补发，断开重连。
			h.resync(high)
			return
		}
		if high <= last {
			return
		}
		batch, err := h.Fetch(ctx, last, high, 1000)
		if err != nil {
			if h.OnDBError != nil {
				h.OnDBError()
			}
			return
		}
		if len(batch) == 0 {
			return
		}
		if batch[0].Seq != last+1 {
			// 水位与事件两次读取之间发生了清理等变化，批次起点不连续。
			// 静默投递会漏发这段已提交事件，按协议断开并重新对齐。
			h.resync(high)
			return
		}
		var slow []*Sub
		h.mu.Lock()
		for _, item := range batch {
			if item.Seq <= h.last {
				continue
			}
			for one := range h.subs {
				select {
				case one.ch <- item:
				default:
					slow = append(slow, one)
				}
			}
			h.last = item.Seq
		}
		for _, one := range slow {
			delete(h.subs, one)
		}
		h.mu.Unlock()
		for _, one := range slow {
			select {
			case <-one.slow:
			default:
				close(one.slow)
			}
			if h.OnSlow != nil {
				h.OnSlow()
			}
		}
		if len(batch) < 1000 {
			return
		}
	}
}

// resync 在同一临界区内把分发游标跳到 high 并断开全部现有订阅的注册，
// 锁外再关闭 slow 通道通知断开。用于首次对齐、保留边界越过和批次不连续
// 三种无法继续补发的场景。
func (h *Hub) resync(high int64) {
	h.mu.Lock()
	h.last = high
	h.started = true
	kicked := make([]*Sub, 0, len(h.subs))
	for one := range h.subs {
		kicked = append(kicked, one)
	}
	h.subs = map[*Sub]struct{}{}
	h.mu.Unlock()
	for _, one := range kicked {
		select {
		case <-one.slow:
		default:
			close(one.slow)
		}
		if h.OnSlow != nil {
			h.OnSlow()
		}
	}
}
