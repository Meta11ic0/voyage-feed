package dataset

import (
	"encoding/json"
	"fmt"
)

func ScaleJSON() []byte {
	sectors := []string{"草帽一伙", "海军", "其他"}
	items := make([]Instrument, 1500)
	for i := 1; i <= 1500; i++ {
		id := fmt.Sprintf("scale:%04d", i)
		chapters := i % 50
		rank := ((i - 1) % 100) + 1
		items[i-1] = Instrument{
			InstrumentID:   id,
			Symbol:         symbolFor(id),
			Name:           fmt.Sprintf("合成%04d", i),
			Sector:         sectors[(i-1)%3],
			Role:           "unknown",
			ChapterCount:   &chapters,
			PopularityRank: &rank,
		}
	}
	raw, err := json.Marshal(Release{
		SchemaVersion: 1,
		DatasetID:     "one-piece-scale",
		CutoffChapter: 1,
		Sources:       []Source{},
		Instruments:   items,
	})
	if err != nil {
		panic(err)
	}
	return append(raw, '\n')
}
