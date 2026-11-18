package dataset

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeAndSector(t *testing.T) {
	if NormalizeName("  ＡＢ  c ") != "ab c" {
		t.Fatalf("normalize %q", NormalizeName("  ＡＢ  c "))
	}
	if got := SectorFromAffiliation("草帽一伙（船长）;海军"); got != "海军" {
		t.Fatalf("sector %s", got)
	}
	if got := SectorFromAffiliation("  "); got != "其他" {
		t.Fatalf("empty sector %s", got)
	}
}

func TestSymbolAndEmptyOverrides(t *testing.T) {
	if symbolFor("codex:chr:1") != "D14001F4649BOP" {
		t.Fatalf("symbol %s", symbolFor("codex:chr:1"))
	}
	rows, err := ParseOverrides([]byte("{}\n"))
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty overrides %+v %v", rows, err)
	}
	if _, err := ParseOverrides([]byte(`{"entries":[{"match":{"chr_id":"chr:1"},"role":null,"source":"x"}]}`)); err == nil {
		t.Fatal("null role should fail")
	}
}

func TestBuildCases(t *testing.T) {
	base := BuildInput{
		Characters:     characterHTML(defaultCharacters),
		Appearances:    []byte("chapter,name,type\n1,蒙其·D·路飞,full\n1,蒙其·D·路飞,full\n6,路飞,flashback\n2,罗罗诺亚·索隆,cover\n10,蒙其·D·路飞,mention\n10,无人,full\n"),
		Rankings:       rankingHTML("1", "蒙其·D·路飞", "11", "罗罗诺亚·索隆", "11", "娜美"),
		SourceManifest: []byte(manifest),
		Overrides:      []byte(`{"entries":[{"match":{"chr_id":"chr:1"},"aliases":["路飞"],"role":"leader","source":"test"}]}`),
		CutoffChapter:  10,
	}
	// 娜美 is not in the character list of this fixture. Replace rankings with two real ties.
	base.Rankings = rankingHTML("11", "蒙其·D·路飞", "11", "罗罗诺亚·索隆")
	base.Characters = characterHTML(`[
		{"name":"蒙其·D·路飞","name_jp":"蒙其·D·路飞","chr_id":"chr:1","first_appearance":"1","affiliation":"草帽一伙（船长）;草帽一伙"},
		{"name":"罗罗诺亚·索隆","name_jp":"罗罗诺亚·索隆","chr_id":"chr:2","first_appearance":"1","affiliation":"草帽一伙"},
		{"name":"迟到","name_jp":"迟到","chr_id":"chr:3","first_appearance":"第99章","affiliation":"海军"}
	]`)
	left, reportRaw, err := Build(base)
	if err != nil {
		t.Fatal(err)
	}
	right, _, err := Build(base)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(left, right) {
		t.Fatal("build is not byte-stable")
	}
	release := mustRelease(t, left)
	if len(release.Instruments) != 2 {
		t.Fatalf("instruments %d", len(release.Instruments))
	}
	byName := map[string]Instrument{}
	for _, item := range release.Instruments {
		byName[item.Name] = item
	}
	luffy := byName["蒙其·D·路飞"]
	if luffy.ChapterCount == nil || *luffy.ChapterCount != 2 || luffy.Role != "leader" || luffy.Sector != "草帽一伙" {
		t.Fatalf("luffy %+v", luffy)
	}
	if luffy.Symbol != "D14001F4649BOP" {
		t.Fatalf("symbol %s", luffy.Symbol)
	}
	zoro := byName["罗罗诺亚·索隆"]
	if zoro.ChapterCount == nil || *zoro.ChapterCount != 0 || zoro.Role != "unknown" {
		t.Fatalf("zoro %+v", zoro)
	}
	report := mustReport(t, reportRaw)
	if len(report.TiedRanks) != 1 || report.TiedRanks[0].Rank != 11 {
		t.Fatalf("ties %+v", report.TiedRanks)
	}
	if report.Excluded != 1 {
		t.Fatalf("excluded %d", report.Excluded)
	}
	joined := strings.Join(report.Unmatched, "\n")
	if !strings.Contains(joined, "未知出场类型") || !strings.Contains(joined, "出场未匹配：无人") {
		t.Fatalf("unmatched %s", joined)
	}

	dup := base
	dup.Characters = characterHTML(`[
		{"name":"甲","name_jp":"甲","chr_id":"chr:1","first_appearance":"1","affiliation":"草帽一伙"},
		{"name":"乙","name_jp":"乙","chr_id":"chr:1","first_appearance":"1","affiliation":"海军"}
	]`)
	if _, _, err := Build(dup); err == nil {
		t.Fatal("duplicate chr_id should fail")
	}

	clash := base
	clash.Overrides = []byte(`{"entries":[
		{"match":{"chr_id":"chr:1"},"symbol":"AAAAAAAAAAAAOP","source":"test"},
		{"match":{"chr_id":"chr:2"},"symbol":"AAAAAAAAAAAAOP","source":"test"}
	]}`)
	if _, _, err := Build(clash); err == nil {
		t.Fatal("duplicate symbol should fail")
	}
}

func TestMissingChapterAndAliasDoNotInventRank(t *testing.T) {
	in := BuildInput{
		Characters: characterHTML(`[
			{"name":"青雉","name_jp":"青雉","chr_id":"chr:11","first_appearance":"1","affiliation":"海军"},
			{"name":"布鲁克","name_jp":"布鲁克","chr_id":"chr:9","first_appearance":"没有数字","affiliation":"草帽一伙"}
		]`),
		Appearances:    []byte("chapter,name,type\n1,布鲁克,full\n10,布鲁克,full\n"),
		Rankings:       rankingHTML("8", "没有这个人"),
		SourceManifest: []byte(manifest),
		Overrides:      []byte(`{}`),
		CutoffChapter:  10,
	}
	body, reportRaw, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	release := mustRelease(t, body)
	report := mustReport(t, reportRaw)
	if len(release.Instruments) != 2 {
		t.Fatalf("%d", len(release.Instruments))
	}
	for _, item := range release.Instruments {
		if item.Name == "青雉" && item.ChapterCount != nil {
			t.Fatal("missing appearances became a count")
		}
		if item.Name == "布鲁克" && (item.PopularityRank != nil || item.ChapterCount == nil || *item.ChapterCount != 2) {
			t.Fatalf("brook %+v", item)
		}
	}
	if strings.Join(report.MissingChapterCount, ",") != "青雉" {
		t.Fatalf("missing chapter %+v", report.MissingChapterCount)
	}
	if !strings.Contains(strings.Join(report.Unmatched, "\n"), "没有这个人") {
		t.Fatalf("rank unmatched %+v", report.Unmatched)
	}
	if !strings.Contains(strings.Join(report.Unmatched, "\n"), "无法解析首次登场") {
		t.Fatalf("first appearance note %+v", report.Unmatched)
	}
}

func TestDevSample(t *testing.T) {
	in := devInput(t)
	body, reportRaw, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, again) {
		t.Fatal("dev build drifted")
	}
	release := mustRelease(t, body)
	report := mustReport(t, reportRaw)
	if release.InstrumentCount() != 12 || report.InstrumentCount != 12 {
		t.Fatalf("count %d %d", len(release.Instruments), report.InstrumentCount)
	}
	sectors := map[string]struct{}{}
	for _, sector := range report.Sectors {
		sectors[sector] = struct{}{}
	}
	for _, name := range []string{"草帽一伙", "海军", "其他"} {
		if _, ok := sectors[name]; !ok {
			t.Fatalf("sectors %v", report.Sectors)
		}
	}
	if len(report.TiedRanks) != 1 || report.TiedRanks[0].Rank != 11 {
		t.Fatalf("tie %+v", report.TiedRanks)
	}
	names := strings.Join(report.TiedRanks[0].Names, ",")
	if !strings.Contains(names, "乌索普") || !strings.Contains(names, "山治") {
		t.Fatalf("tie names %s", names)
	}
	if strings.Join(report.MissingRank, ",") != "布鲁克" {
		t.Fatalf("missing rank %v", report.MissingRank)
	}
	if strings.Join(report.MissingChapterCount, ",") != "青雉" {
		t.Fatalf("missing chapter %v", report.MissingChapterCount)
	}
	if strings.Join(report.ZeroChapterCount, ",") != "开发夹具路人" {
		t.Fatalf("zero %v", report.ZeroChapterCount)
	}
	found := map[string]Instrument{}
	for _, item := range release.Instruments {
		found[item.Name] = item
	}
	for _, name := range []string{"蒙其·D·路飞", "罗罗诺亚·索隆", "娜美"} {
		item := found[name]
		if item.ChapterCount == nil || *item.ChapterCount <= 0 || item.PopularityRank == nil {
			t.Fatalf("%s %+v", name, item)
		}
	}
	if found["蒙其·D·路飞"].Role != "leader" || *found["蒙其·D·路飞"].PopularityRank != 1 {
		t.Fatalf("luffy %+v", found["蒙其·D·路飞"])
	}
	if *found["罗罗诺亚·索隆"].PopularityRank != 2 {
		t.Fatalf("zoro rank")
	}
	if found["托尼托尼·乔巴"].Role != "member" {
		t.Fatalf("chopper role %s", found["托尼托尼·乔巴"].Role)
	}
	if found["青雉"].PopularityRank == nil || found["青雉"].ChapterCount != nil {
		t.Fatalf("aokiji %+v", found["青雉"])
	}
	if found["开发夹具路人"].Sector != "其他" || found["开发夹具路人"].Role != "unknown" {
		t.Fatalf("extra %+v", found["开发夹具路人"])
	}
	if *found["乌索普"].PopularityRank != *found["山治"].PopularityRank {
		t.Fatal("tie rank diverged")
	}
	committed, err := os.ReadFile(filepath.Join("..", "data", "dev.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, body) {
		t.Fatal("data/dev.json is not the build output")
	}
	committedReport, err := os.ReadFile(filepath.Join("..", "data", "dev.report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committedReport, reportRaw) {
		t.Fatal("data/dev.report.json is not the build output")
	}
}

func TestFetchRetryAndKeepSnapshot(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/characters.html" {
			hits++
			if hits < 3 {
				http.Error(w, "nope", http.StatusBadGateway)
				return
			}
		}
		switch r.URL.Path {
		case "/characters.html":
			_, _ = w.Write(characterHTML(defaultCharacters))
		case "/appearances.csv":
			_, _ = w.Write([]byte("chapter,name,type\n1,蒙其·D·路飞,full\n"))
		case "/rankings.html":
			_, _ = w.Write(rankingHTML("1", "蒙其·D·路飞"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "characters.html"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 18, 1, 2, 3, 0, time.UTC)
	dest, err := Fetch(context.Background(), FetchConfig{
		OutDir: dir,
		Client: srv.Client(),
		Now:    now,
		Sources: []FetchSource{
			{Name: "characters", URL: srv.URL + "/characters.html", File: "characters.html"},
			{Name: "appearances", URL: srv.URL + "/appearances.csv", File: "appearances.csv"},
			{Name: "rankings", URL: srv.URL + "/rankings.html", File: "rankings.html"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if hits != 3 {
		t.Fatalf("retries %d", hits)
	}
	old, err := os.ReadFile(filepath.Join(dir, "characters.html"))
	if err != nil || string(old) != "old" {
		t.Fatalf("snapshot overwritten %q %v", old, err)
	}
	if dest != filepath.Join(dir, "20261118T010203Z") {
		t.Fatalf("dest %s", dest)
	}
	if _, err := os.Stat(filepath.Join(dest, "sources.json")); err != nil {
		t.Fatal(err)
	}
}

func (r Release) InstrumentCount() int { return len(r.Instruments) }

func devInput(t *testing.T) BuildInput {
	t.Helper()
	read := func(name string) []byte {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("..", "data", "dev-src", name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	return BuildInput{
		Characters:     read("characters.html"),
		Appearances:    read("appearances.csv"),
		Rankings:       read("rankings.html"),
		SourceManifest: read("sources.json"),
		Overrides:      read("overrides.json"),
		CutoffChapter:  10,
	}
}

func mustRelease(t *testing.T, raw []byte) Release {
	t.Helper()
	var out Release
	if err := jsonUnmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func mustReport(t *testing.T, raw []byte) Report {
	t.Helper()
	var out Report
	if err := jsonUnmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

const manifest = `{
  "items": [
    {"name":"characters","url":"https://example.test/characters.html","fetched_at":"2026-11-18T00:00:00Z"},
    {"name":"appearances","url":"https://example.test/appearances.csv","fetched_at":"2026-11-18T00:00:00Z"},
    {"name":"rankings","url":"https://example.test/rankings.html","fetched_at":"2026-11-18T00:00:00Z"}
  ]
}`

const defaultCharacters = `[
  {"name":"蒙其·D·路飞","name_jp":"蒙其·D·路飞","chr_id":"chr:1","first_appearance":"1","affiliation":"草帽一伙"}
]`

func characterHTML(body string) []byte {
	return []byte(`<!DOCTYPE html><html><body><script id="punk-records-data" type="application/json">` + body + `</script></body></html>`)
}

func rankingHTML(pairs ...string) []byte {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><body><section data-fr-static-panel data-region="all">`)
	for i := 0; i < len(pairs); i += 2 {
		b.WriteString(`<article data-rank="`)
		b.WriteString(pairs[i])
		b.WriteString(`" data-portrait-name="`)
		b.WriteString(pairs[i+1])
		b.WriteString(`"></article>`)
	}
	b.WriteString(`</section><section data-fr-static-panel data-region="jp"><article data-rank="1" data-portrait-name="区域外角色"></article></section></body></html>`)
	return []byte(b.String())
}

func TestRankingTextName(t *testing.T) {
	raw := []byte(`<!DOCTYPE html><html><body><div data-fr-static-panel data-region="all"><div data-rank="1"><div data-portrait-name>蒙其·D·路飞</div></div></div><div data-fr-static-panel data-region="jp"><div data-rank="1"><div data-portrait-name>区域外角色</div></div></div></body></html>`)
	rows, err := ParseRankings(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Rank != 1 || rows[0].Name != "蒙其·D·路飞" {
		t.Fatalf("%+v", rows)
	}
}

func TestOmittedCharacterFields(t *testing.T) {
	raw := characterHTML(`[
		{"name":"完整","name_jp":"完整","chr_id":"chr:1","first_appearance":"1","affiliation":"草帽一伙"},
		{"name":"缺字段"},
		{"name":"同人","name_jp":"同名"},
		{"name":"同人","name_jp":"同名"},
		{"name":"   "}
	]`)
	if _, _, err := ParseCharacters(raw); err == nil || !strings.Contains(err.Error(), "缺少 name") {
		t.Fatalf("empty name: %v", err)
	}
	body := characterHTML(`[
		{"name":"完整","name_jp":"完整","chr_id":"chr:1","first_appearance":"1","affiliation":"草帽一伙"},
		{"name":"缺字段"},
		{"name":"同人","name_jp":"同名"},
		{"name":"同人","name_jp":"同名"},
		{"name":"英文甲","name_jp":"同一人","chr_id":"chr:9","first_appearance":"1","affiliation":"海军"},
		{"name":"英文乙","name_jp":"同一人","chr_id":"chr:9","first_appearance":"1","affiliation":"海军"}
	]`)
	rows, _, err := ParseCharacters(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 || rows[1].NameJP != "" || rows[1].ChrID == "" || !strings.HasPrefix(rows[1].ChrID, "name:") || rows[1].ChapterKnown {
		t.Fatalf("omitted %+v", rows[1])
	}
	if rows[2].ChrID != rows[3].ChrID {
		t.Fatalf("fingerprint %s %s", rows[2].ChrID, rows[3].ChrID)
	}
	releaseRaw, reportRaw, err := Build(BuildInput{
		Characters:     body,
		Appearances:    []byte("chapter,name,type\n1,完整,full\n"),
		Rankings:       rankingHTML("1", "完整"),
		SourceManifest: []byte(manifest),
		Overrides:      []byte("{}\n"),
		CutoffChapter:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	release := mustRelease(t, releaseRaw)
	report := mustReport(t, reportRaw)
	if len(release.Instruments) != 3 {
		t.Fatalf("instruments %d", len(release.Instruments))
	}
	var omitted Instrument
	for _, item := range release.Instruments {
		if item.Name == "缺字段" {
			omitted = item
		}
	}
	if omitted.Sector != "其他" || !strings.HasPrefix(omitted.InstrumentID, "codex:name:") || omitted.ChapterCount != nil {
		t.Fatalf("omitted instrument %+v", omitted)
	}
	if len(report.Conflicts) != 2 {
		t.Fatalf("conflicts %#v", report.Conflicts)
	}
}
