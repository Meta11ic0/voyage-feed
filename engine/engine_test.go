package engine

import (
	"math"
	"testing"
	"time"
)

func TestWeight(t *testing.T) {
	if Weight(nil) != 1000000 {
		t.Fatalf("missing weight")
	}
	zero := 0
	if Weight(&zero) != 1000000 {
		t.Fatalf("zero weight")
	}
	one := 1
	if Weight(&one) != 1693147 {
		t.Fatalf("c=1 weight %d", Weight(&one))
	}
	if math.Log(2) <= 0 {
		t.Fatal("ln")
	}
}

func TestPriceBands(t *testing.T) {
	r10, r11, r30, r31 := 10, 11, 30, 31
	cases := []struct {
		rank   *int
		lo, hi int64
	}{
		{&r10, 2200, 3500},
		{&r11, 1600, 3000},
		{&r30, 1600, 3000},
		{&r31, 1000, 2400},
		{nil, 800, 2000},
	}
	for _, tc := range cases {
		lo, hi := PriceBand(tc.rank)
		if lo != tc.lo || hi != tc.hi {
			t.Fatalf("band %+v got %d-%d", tc.rank, lo, hi)
		}
	}
}

func TestShareExample(t *testing.T) {
	total, floatShares := SharesFromExample(25000000000, 2500, 50)
	if total != 10000000 || floatShares != 5000000 {
		t.Fatalf("shares %d %d", total, floatShares)
	}
	if _, err := RoleRatio("leader"); err != nil {
		t.Fatal(err)
	}
	leader, _ := RoleRatio("leader")
	member, _ := RoleRatio("member")
	unknown, _ := RoleRatio("unknown")
	if leader != 30 || member != 70 || unknown != 50 {
		t.Fatalf("ratios %d %d %d", leader, member, unknown)
	}
}

func TestTargetCapRemainderDropped(t *testing.T) {
	got, err := TargetCaps(10, []int64{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 3 || got[1] != 3 || got[2] != 3 {
		t.Fatalf("%v", got)
	}
}

func TestSampleDigestAndReplay(t *testing.T) {
	const want = "bda72c6303143c6e84450616836ec3a7b4442cee81c5f32040d799d64098490c"
	got := SampleDigest(ModelVersion, 7, "codex:chr:1", 0, "initial_price", 0)
	if got != want {
		t.Fatalf("digest %s", got)
	}
	a, err := Uniform(ModelVersion, 7, "codex:chr:1", 0, "initial_price", 1301)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Uniform(ModelVersion, 7, "codex:chr:1", 0, "initial_price", 1301)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("replay %d %d", a, b)
	}
	changed, err := Uniform(ModelVersion, 7, "codex:chr:1", 0, "move_bps", 41)
	if err != nil {
		t.Fatal(err)
	}
	if changed == a {
		t.Fatal("purpose should change the draw")
	}
}

func TestPopularityDoesNotAffectMove(t *testing.T) {
	left, err := Uniform(ModelVersion, 7, "codex:chr:4", 3, "move_bps", 41)
	if err != nil {
		t.Fatal(err)
	}
	right, err := Uniform(ModelVersion, 7, "codex:chr:4", 3, "move_bps", 41)
	if err != nil {
		t.Fatal(err)
	}
	if left != right || left > 40 {
		t.Fatalf("move %d", left)
	}
}

func TestTradeGateAndPrice(t *testing.T) {
	if TradeAccepted(2000, 2000) {
		t.Fatal("gate equal to probability is not a trade")
	}
	if !TradeAccepted(1999, 2000) {
		t.Fatal("gate below probability should trade")
	}
	next, ok := NextPriceCents(2500, 0)
	if !ok || next != 2500 {
		t.Fatalf("flat price %d %v", next, ok)
	}
	if _, ok := NextPriceCents(1, -9999); ok {
		t.Fatal("sub-cent candidate should be skipped")
	}
	if _, ok := NextPriceCents(100000000, 20); ok {
		t.Fatal("above cap should be skipped")
	}
}

func TestSessionBoundary(t *testing.T) {
	prev := time.Date(2026, 11, 17, 15, 59, 59, 0, time.UTC)
	next := prev.Add(time.Second)
	if MarketDate(prev) != "2026-11-17" || MarketDate(next) != "2026-11-18" {
		t.Fatalf("dates %s %s", MarketDate(prev), MarketDate(next))
	}
	same := PlanStep("2026-11-17", "2026-11-17", nil)
	if len(same) != 0 {
		t.Fatalf("zero trades should not emit a session: %+v", same)
	}
	crossed := PlanStep(MarketDate(prev), MarketDate(next), nil)
	if len(crossed) != 1 || crossed[0].Type != "session" {
		t.Fatalf("session %+v", crossed)
	}
	withTrade := PlanStep(MarketDate(prev), MarketDate(next), []string{"codex:chr:1"})
	if withTrade[0].Type != "session" || withTrade[1].Type != "trade" {
		t.Fatalf("order %+v", withTrade)
	}
}

func TestInitializeRejectsZeroShares(t *testing.T) {
	rank := 1
	_, err := Initialize(7, 1, []InstrumentInput{{
		InstrumentID: "codex:chr:1",
		Role:         "leader",
		Rank:         &rank,
	}})
	if err == nil {
		t.Fatal("tiny budget should fail")
	}
}

func TestInitializeStable(t *testing.T) {
	rank := 1
	chapters := 4
	items := []InstrumentInput{{
		InstrumentID: "codex:chr:1",
		Role:         "leader",
		ChapterCount: &chapters,
		Rank:         &rank,
	}}
	a, err := Initialize(7, 100000000000, items)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Initialize(7, 100000000000, items)
	if err != nil {
		t.Fatal(err)
	}
	if a[0] != b[0] {
		t.Fatalf("%+v %+v", a[0], b[0])
	}
	lo, hi := PriceBand(&rank)
	if a[0].InitialPriceCents < lo || a[0].InitialPriceCents > hi {
		t.Fatalf("price %d", a[0].InitialPriceCents)
	}
	if a[0].SharesFloat > a[0].SharesTotal || a[0].SharesFloat <= 0 {
		t.Fatalf("shares %+v", a[0])
	}
}
