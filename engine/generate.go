package engine

import (
	"fmt"
	"math"
	"sort"
	"time"
)

type InstrumentView struct {
	InstrumentID string
	InitialPrice int64
	LastPrice    *int64
	DayVolume    int64
	DayTurnover  int64
}

func (v InstrumentView) EffectivePrice() int64 {
	if v.LastPrice != nil {
		return *v.LastPrice
	}
	return v.InitialPrice
}

type Trade struct {
	InstrumentID string
	Price        int64
	Quantity     int64
	Turnover     int64
}

type StepDraft struct {
	MarketDate string
	CrossDay   bool
	Trades     []Trade
}

type StepInput struct {
	Seed           uint64
	Model          string
	ProbabilityBps int
	MaxMoveBps     int
	Tick           int64
	PrevDate       string
	EventTime      time.Time
	Instruments    []InstrumentView
}

func GenerateStep(in StepInput) (StepDraft, error) {
	if in.MaxMoveBps < 1 {
		return StepDraft{}, fmt.Errorf("变动幅度必须为正")
	}
	items := append([]InstrumentView(nil), in.Instruments...)
	sort.Slice(items, func(i, j int) bool { return items[i].InstrumentID < items[j].InstrumentID })
	date := MarketDate(in.EventTime)
	draft := StepDraft{MarketDate: date, CrossDay: date != in.PrevDate, Trades: []Trade{}}
	for _, item := range items {
		trade, ok, err := oneTrade(in, item, draft.CrossDay)
		if err != nil {
			return StepDraft{}, err
		}
		if ok {
			draft.Trades = append(draft.Trades, trade)
		}
	}
	return draft, nil
}

func oneTrade(in StepInput, item InstrumentView, crossDay bool) (Trade, bool, error) {
	gate, err := Uniform(in.Model, in.Seed, item.InstrumentID, in.Tick, "trade_gate", 10000)
	if err != nil {
		return Trade{}, false, err
	}
	if !TradeAccepted(gate, in.ProbabilityBps) {
		return Trade{}, false, nil
	}
	span := uint64(2*in.MaxMoveBps + 1)
	raw, err := Uniform(in.Model, in.Seed, item.InstrumentID, in.Tick, "move_bps", span)
	if err != nil {
		return Trade{}, false, err
	}
	bps := int(raw) - in.MaxMoveBps
	price, ok := NextPriceCents(item.EffectivePrice(), bps)
	if !ok {
		return Trade{}, false, nil
	}
	lots, err := Uniform(in.Model, in.Seed, item.InstrumentID, in.Tick, "quantity_lots", 10)
	if err != nil {
		return Trade{}, false, err
	}
	quantity := (1 + int64(lots)) * 100
	turnover, err := mulInt64(price, quantity)
	if err != nil {
		return Trade{}, false, fmt.Errorf("%s 成交额溢出", item.InstrumentID)
	}
	baseVolume := item.DayVolume
	baseTurnover := item.DayTurnover
	if crossDay {
		baseVolume = 0
		baseTurnover = 0
	}
	if _, err := addInt64(baseVolume, quantity); err != nil {
		return Trade{}, false, fmt.Errorf("%s 日内成交量溢出", item.InstrumentID)
	}
	if _, err := addInt64(baseTurnover, turnover); err != nil {
		return Trade{}, false, fmt.Errorf("%s 日内成交额溢出", item.InstrumentID)
	}
	return Trade{InstrumentID: item.InstrumentID, Price: price, Quantity: quantity, Turnover: turnover}, true, nil
}

func mulInt64(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, fmt.Errorf("乘积参数为负")
	}
	if a != 0 && b > math.MaxInt64/a {
		return 0, fmt.Errorf("乘积溢出")
	}
	return a * b, nil
}

func addInt64(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, fmt.Errorf("加数为负")
	}
	if a > math.MaxInt64-b {
		return 0, fmt.Errorf("加法溢出")
	}
	return a + b, nil
}

func ShouldMaintain(tick int64) bool {
	return tick > 0 && tick%60 == 0
}
