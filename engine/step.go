package engine

import (
	"time"
)

var beijing = time.FixedZone("UTC+8", 8*3600)

func MarketDate(t time.Time) string {
	return t.In(beijing).Format("2006-01-02")
}

func TradeAccepted(gate uint64, probabilityBps int) bool {
	return gate < uint64(probabilityBps)
}

func NextPriceCents(prev int64, bps int) (int64, bool) {
	next := (prev*int64(10000+bps) + 5000) / 10000
	if next < 1 || next > 100000000 {
		return 0, false
	}
	return next, true
}

type PlanEvent struct {
	Type         string
	InstrumentID string
}

func PlanStep(prevDate, nextDate string, tradeIDs []string) []PlanEvent {
	out := make([]PlanEvent, 0, len(tradeIDs)+1)
	if prevDate != nextDate {
		out = append(out, PlanEvent{Type: "session"})
	}
	for _, id := range tradeIDs {
		out = append(out, PlanEvent{Type: "trade", InstrumentID: id})
	}
	return out
}
