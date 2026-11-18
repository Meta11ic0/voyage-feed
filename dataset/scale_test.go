package dataset

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"voyage-feed/engine"
)

func TestScaleDataset(t *testing.T) {
	raw := ScaleJSON()
	var release Release
	if err := json.Unmarshal(raw, &release); err != nil {
		t.Fatal(err)
	}
	if release.DatasetID != "one-piece-scale" || len(release.Instruments) != 1500 {
		t.Fatalf("count %d id %s", len(release.Instruments), release.DatasetID)
	}
	file, err := os.ReadFile("../data/scale.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(file, raw) {
		t.Fatal("data/scale.json is not the generated file")
	}
	inputs := make([]engine.InstrumentInput, len(release.Instruments))
	seenID := map[string]bool{}
	seenSymbol := map[string]bool{}
	for i, item := range release.Instruments {
		if seenID[item.InstrumentID] || seenSymbol[item.Symbol] {
			t.Fatalf("duplicate %s %s", item.InstrumentID, item.Symbol)
		}
		seenID[item.InstrumentID] = true
		seenSymbol[item.Symbol] = true
		if item.Symbol != symbolFor(item.InstrumentID) {
			t.Fatalf("symbol %s", item.InstrumentID)
		}
		inputs[i] = engine.InstrumentInput{
			InstrumentID: item.InstrumentID, Role: item.Role,
			ChapterCount: item.ChapterCount, Rank: item.PopularityRank,
		}
	}
	inits, err := engine.Initialize(7, 100000000000, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(inits) != 1500 {
		t.Fatalf("inits %d", len(inits))
	}
	for _, item := range inits {
		if item.SharesTotal <= 0 || item.SharesFloat <= 0 || item.SharesFloat > item.SharesTotal {
			t.Fatalf("shares %+v", item)
		}
	}
}
