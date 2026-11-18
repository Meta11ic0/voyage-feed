package httpapi

import (
	"time"

	"voyage-feed/engine"
)

type minutePoint struct {
	Start    time.Time
	Open     int64
	High     int64
	Low      int64
	Close    int64
	Volume   int64
	Turnover int64
	Count    int64
}

type barPoint struct {
	Start    time.Time
	End      time.Time
	Open     int64
	High     int64
	Low      int64
	Close    int64
	Volume   int64
	Turnover int64
	Count    int64
	Complete bool
}

func aggregateBars(interval string, rows []minutePoint, simNow time.Time) []barPoint {
	type group struct {
		barPoint
		seen bool
	}
	order := []string{}
	groups := map[string]*group{}
	for _, row := range rows {
		start, end, key := barWindow(interval, row.Start)
		item := groups[key]
		if item == nil {
			item = &group{barPoint: barPoint{Start: start, End: end, Open: row.Open, High: row.High, Low: row.Low, Close: row.Close}}
			groups[key] = item
			order = append(order, key)
		}
		if row.High > item.High {
			item.High = row.High
		}
		if row.Low < item.Low {
			item.Low = row.Low
		}
		item.Close = row.Close
		item.Volume += row.Volume
		item.Turnover += row.Turnover
		item.Count += row.Count
		item.seen = true
	}
	out := make([]barPoint, 0, len(order))
	for _, key := range order {
		item := groups[key]
		item.Complete = !simNow.Before(item.End)
		out = append(out, item.barPoint)
	}
	return out
}

func barWindow(interval string, start time.Time) (time.Time, time.Time, string) {
	start = start.UTC()
	switch interval {
	case "5m":
		at := start.Truncate(5 * time.Minute)
		return at, at.Add(5 * time.Minute), at.Format(time.RFC3339)
	case "1d":
		day := engine.MarketDate(start)
		at := beijingMidnight(day)
		return at, at.Add(24 * time.Hour), day
	default:
		at := start.Truncate(time.Minute)
		return at, at.Add(time.Minute), at.Format(time.RFC3339)
	}
}

func beijingMidnight(day string) time.Time {
	parsed, err := time.ParseInLocation("2006-01-02", day, time.FixedZone("UTC+8", 8*3600))
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}
