package dataset

type Instrument struct {
	InstrumentID   string `json:"instrument_id"`
	Symbol         string `json:"symbol"`
	Name           string `json:"name"`
	Sector         string `json:"sector"`
	Role           string `json:"role"`
	ChapterCount   *int   `json:"chapter_count"`
	PopularityRank *int   `json:"popularity_rank"`
}

type Source struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	FetchedAt string `json:"fetched_at"`
	SHA256    string `json:"sha256"`
	Note      string `json:"note"`
}

type Release struct {
	SchemaVersion int          `json:"schema_version"`
	DatasetID     string       `json:"dataset_id"`
	CutoffChapter int          `json:"cutoff_chapter"`
	Sources       []Source     `json:"sources"`
	Instruments   []Instrument `json:"instruments"`
}

type Tie struct {
	Rank  int      `json:"rank"`
	Names []string `json:"names"`
}

type Report struct {
	InstrumentCount     int      `json:"instrument_count"`
	Sectors             []string `json:"sectors"`
	AppearanceCovered   int      `json:"appearance_covered"`
	RankCovered         int      `json:"rank_covered"`
	CutoffChapter       int      `json:"cutoff_chapter"`
	Excluded            int      `json:"excluded"`
	Conflicts           []string `json:"conflicts"`
	Unmatched           []string `json:"unmatched"`
	MissingRank         []string `json:"missing_rank"`
	MissingChapterCount []string `json:"missing_chapter_count"`
	ZeroChapterCount    []string `json:"zero_chapter_count"`
	TiedRanks           []Tie    `json:"tied_ranks"`
}

type BuildInput struct {
	Characters     []byte
	Appearances    []byte
	Rankings       []byte
	SourceManifest []byte
	Overrides      []byte
	CutoffChapter  int
}
