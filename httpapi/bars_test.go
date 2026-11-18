package httpapi

import (
	"testing"
	"time"
)

func TestAggregateBars(t *testing.T) {
	base := time.Date(2026, 11, 17, 15, 58, 0, 0, time.UTC)
	rows := []minutePoint{
		{Start: base, Open: 10, High: 12, Low: 9, Close: 11, Volume: 100, Turnover: 1000, Count: 1},
		{Start: base.Add(time.Minute), Open: 11, High: 13, Low: 11, Close: 12, Volume: 200, Turnover: 2000, Count: 2},
		{Start: time.Date(2026, 11, 17, 16, 0, 0, 0, time.UTC), Open: 20, High: 20, Low: 20, Close: 20, Volume: 50, Turnover: 500, Count: 1},
	}
	five := aggregateBars("5m", rows, base.Add(10*time.Minute))
	if len(five) != 2 || five[0].Open != 10 || five[0].Close != 12 || five[0].Volume != 300 || five[0].High != 13 || five[0].Low != 9 {
		t.Fatalf("5m %+v", five)
	}
	if five[0].Start.Equal(five[1].Start) {
		t.Fatal("crossed the trading day")
	}
	daily := aggregateBars("1d", rows[:2], base)
	if len(daily) != 1 || daily[0].Complete {
		t.Fatalf("day %+v", daily)
	}
	done := aggregateBars("1m", rows[:1], base.Add(time.Minute))
	if len(done) != 1 || !done[0].Complete || done[0].End.Sub(done[0].Start) != time.Minute {
		t.Fatalf("minute %+v", done)
	}
}
