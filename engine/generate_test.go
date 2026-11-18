package engine

import (
	"math"
	"testing"
	"time"
)

func TestGenerateReplayAndFlat(t *testing.T) {
	in := StepInput{
		Seed: 7, Model: ModelVersion, ProbabilityBps: 10000, MaxMoveBps: 20, Tick: 1,
		PrevDate: "2026-11-17", EventTime: time.Date(2026, 11, 17, 16, 0, 0, 0, time.UTC),
		Instruments: []InstrumentView{{InstrumentID: "codex:chr:2", InitialPrice: 2500}, {InstrumentID: "codex:chr:1", InitialPrice: 2500}},
	}
	a, err := GenerateStep(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateStep(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Trades) != 2 || a.Trades[0].InstrumentID != "codex:chr:1" || a.Trades[1].InstrumentID != "codex:chr:2" {
		t.Fatalf("%+v", a.Trades)
	}
	if a.Trades[0] != b.Trades[0] || !a.CrossDay {
		t.Fatalf("replay %+v", a)
	}
	foundFlat := false
	for tick := int64(1); tick < 400 && !foundFlat; tick++ {
		in.Tick = tick
		in.PrevDate = MarketDate(in.EventTime)
		draft, err := GenerateStep(in)
		if err != nil {
			t.Fatal(err)
		}
		for _, trade := range draft.Trades {
			if trade.Price == 2500 {
				foundFlat = true
			}
		}
	}
	if !foundFlat {
		t.Fatal("expected a flat trade")
	}
}

func TestGenerateSkipAndZero(t *testing.T) {
	in := StepInput{
		Seed: 7, Model: ModelVersion, ProbabilityBps: 10000, MaxMoveBps: 20,
		PrevDate: "2026-11-18", EventTime: time.Date(2026, 11, 18, 1, 0, 1, 0, time.UTC),
		Instruments: []InstrumentView{
			{InstrumentID: "codex:chr:1", InitialPrice: 2500},
			{InstrumentID: "codex:chr:9", InitialPrice: 100000000},
		},
	}
	skipped := false
	for tick := int64(1); tick < 300; tick++ {
		in.Tick = tick
		draft, err := GenerateStep(in)
		if err != nil {
			t.Fatal(err)
		}
		ids := map[string]bool{}
		for _, trade := range draft.Trades {
			ids[trade.InstrumentID] = true
		}
		if ids["codex:chr:1"] && !ids["codex:chr:9"] {
			skipped = true
			break
		}
	}
	if !skipped {
		t.Fatal("expected the capped instrument to be skipped")
	}
	in.ProbabilityBps = 1
	in.Instruments = []InstrumentView{{InstrumentID: "codex:chr:1", InitialPrice: 2500}}
	zero := false
	for tick := int64(1); tick < 80; tick++ {
		in.Tick = tick
		draft, err := GenerateStep(in)
		if err != nil {
			t.Fatal(err)
		}
		if len(draft.Trades) == 0 && !draft.CrossDay {
			zero = true
			break
		}
	}
	if !zero {
		t.Fatal("expected a zero-trade step")
	}
}

func TestGenerateOverflow(t *testing.T) {
	in := StepInput{
		Seed: 7, Model: ModelVersion, ProbabilityBps: 10000, MaxMoveBps: 20, Tick: 1,
		PrevDate: "2026-11-18", EventTime: time.Date(2026, 11, 18, 1, 0, 1, 0, time.UTC),
		Instruments: []InstrumentView{{
			InstrumentID: "codex:chr:1", InitialPrice: 2500, DayTurnover: math.MaxInt64 - 1,
		}},
	}
	if _, err := GenerateStep(in); err == nil {
		t.Fatal("expected overflow")
	}
}

func TestEqualProbabilityDoesNotTrade(t *testing.T) {
	if TradeAccepted(2000, 2000) {
		t.Fatal("equal gate trades")
	}
}

func TestShouldMaintain(t *testing.T) {
	if ShouldMaintain(59) || !ShouldMaintain(60) || !ShouldMaintain(120) || ShouldMaintain(0) {
		t.Fatal("maintain ticks")
	}
}
