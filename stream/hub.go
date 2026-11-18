package stream

import (
	"context"
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
	Notify    chan struct{}
	OnDBError func()
	OnSlow    func()
	mu        sync.Mutex
	subs      map[*Sub]struct{}
	last      int64
	started   bool
}

func NewHub() *Hub {
	return &Hub{subs: map[*Sub]struct{}{}, Notify: make(chan struct{}, 1)}
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

func (h *Hub) Prepare(ctx context.Context, after int64) (*Sub, int64, []Message, int, error) {
	item := &Sub{ch: make(chan Message, QueueLimit), slow: make(chan struct{})}
	h.mu.Lock()
	h.subs[item] = struct{}{}
	h.mu.Unlock()
	fail := func(status int, err error) (*Sub, int64, []Message, int, error) {
		h.remove(item)
		return nil, 0, nil, status, err
	}
	high, retained, err := h.Watermark(ctx)
	if err != nil {
		return fail(503, err)
	}
	if after < retained-1 {
		return fail(410, nil)
	}
	if after > high {
		return fail(400, nil)
	}
	replay, err := h.Fetch(ctx, after, high, QueueLimit+1)
	if err != nil {
		return fail(503, err)
	}
	if len(replay) > QueueLimit {
		return fail(409, nil)
	}
	return item, high, replay, 0, nil
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
		if !h.started {
			h.last = high
			h.started = true
			h.mu.Unlock()
			return
		}
		behind := last+1 < retained
		h.mu.Unlock()
		if behind {
			h.kickAll()
			h.mu.Lock()
			h.last = high
			h.mu.Unlock()
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

func (h *Hub) kickAll() {
	h.mu.Lock()
	all := make([]*Sub, 0, len(h.subs))
	for one := range h.subs {
		all = append(all, one)
	}
	h.subs = map[*Sub]struct{}{}
	h.mu.Unlock()
	for _, one := range all {
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
