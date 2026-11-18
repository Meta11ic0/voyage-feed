package dataset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type manifestItem struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	FetchedAt string `json:"fetched_at"`
}

func Build(in BuildInput) ([]byte, []byte, error) {
	if in.CutoffChapter < 1 {
		return nil, nil, fmt.Errorf("截止章节必须为正")
	}
	manifest, err := parseManifest(in.SourceManifest)
	if err != nil {
		return nil, nil, err
	}
	overrides, err := ParseOverrides(in.Overrides)
	if err != nil {
		return nil, nil, err
	}
	byChr := map[string]Override{}
	for _, item := range overrides {
		if _, ok := byChr[item.ChrID]; ok {
			return nil, nil, fmt.Errorf("覆盖文件重复匹配 %s", item.ChrID)
		}
		byChr[item.ChrID] = item
	}
	characters, notes, err := ParseCharacters(in.Characters)
	if err != nil {
		return nil, nil, err
	}
	appearances, err := ParseAppearances(in.Appearances, in.CutoffChapter)
	if err != nil {
		return nil, nil, err
	}
	rankings, err := ParseRankings(in.Rankings)
	if err != nil {
		return nil, nil, err
	}

	report := Report{
		CutoffChapter: in.CutoffChapter,
		Conflicts:     []string{},
		Unmatched:     append([]string{}, notes...),
	}
	kept := make([]character, 0, len(characters))
	for _, item := range characters {
		if item.ChapterKnown && *item.FirstChapter > in.CutoffChapter {
			report.Excluded++
			continue
		}
		kept = append(kept, item)
	}

	type built struct {
		character
		override      Override
		hasOverride   bool
		countable     map[int]struct{}
		evidence      bool
		matchedAppear bool
		rank          *int
		rankName      string
	}
	fingerprintCount := map[string]int{}
	for _, item := range kept {
		if strings.HasPrefix(item.ChrID, "name:") {
			fingerprintCount[item.ChrID]++
		}
	}
	rows := make([]built, 0, len(kept))
	nameIndex := map[string][]int{}
	for _, item := range kept {
		if fingerprintCount[item.ChrID] > 1 {
			report.Conflicts = append(report.Conflicts, fmt.Sprintf("身份无法区分：codex:%s %s", item.ChrID, item.Name))
			continue
		}
		row := built{character: item, countable: map[int]struct{}{}}
		if ov, ok := byChr[item.ChrID]; ok {
			row.override = ov
			row.hasOverride = true
			delete(byChr, item.ChrID)
		}
		rows = append(rows, row)
		i := len(rows) - 1
		addName(nameIndex, item.Name, i)
		addName(nameIndex, item.NameJP, i)
		for _, alias := range item.Aliases {
			addName(nameIndex, alias, i)
		}
		if row.hasOverride {
			for _, alias := range row.override.Aliases {
				addName(nameIndex, alias, i)
			}
		}
	}
	if len(byChr) > 0 {
		return nil, nil, fmt.Errorf("覆盖文件指向目录中不存在的角色")
	}

	for _, app := range appearances {
		if app.Chapter > in.CutoffChapter {
			continue
		}
		targets := uniqueIndexes(nameIndex[NormalizeName(app.Name)])
		if len(targets) == 0 {
			report.Unmatched = append(report.Unmatched, fmt.Sprintf("出场未匹配：%s 第 %d 章", app.Name, app.Chapter))
			continue
		}
		if len(targets) > 1 {
			report.Conflicts = append(report.Conflicts, fmt.Sprintf("出场对应多个角色：%s 第 %d 章", app.Name, app.Chapter))
			continue
		}
		row := &rows[targets[0]]
		switch app.Type {
		case "full", "flashback":
			row.countable[app.Chapter] = struct{}{}
			row.matchedAppear = true
		case "silhouette", "cover":
			row.evidence = true
			row.matchedAppear = true
		default:
			report.Unmatched = append(report.Unmatched, fmt.Sprintf("未知出场类型 %s：%s 第 %d 章", app.Type, app.Name, app.Chapter))
		}
	}

	rankSeen := map[int]int{}
	for _, rank := range rankings {
		targets := uniqueIndexes(nameIndex[NormalizeName(rank.Name)])
		if len(targets) == 0 {
			report.Unmatched = append(report.Unmatched, fmt.Sprintf("人气未匹配：%s", rank.Name))
			continue
		}
		if len(targets) > 1 {
			report.Conflicts = append(report.Conflicts, fmt.Sprintf("人气对应多个角色：%s", rank.Name))
			continue
		}
		row := &rows[targets[0]]
		if prev, ok := rankSeen[targets[0]]; ok && prev != rank.Rank {
			return nil, nil, fmt.Errorf("%s 出现不同名次", row.ChrID)
		}
		rankSeen[targets[0]] = rank.Rank
		value := rank.Rank
		row.rank = &value
	}

	instruments := make([]Instrument, 0, len(rows))
	symbols := map[string]string{}
	ids := map[string]struct{}{}
	for _, row := range rows {
		id := "codex:" + row.ChrID
		if row.hasOverride && row.override.HasInstrument {
			id = row.override.InstrumentID
		}
		if _, ok := ids[id]; ok {
			return nil, nil, fmt.Errorf("标的 ID 重复：%s", id)
		}
		ids[id] = struct{}{}
		symbol := symbolFor(id)
		if row.hasOverride && row.override.HasSymbol {
			symbol = row.override.Symbol
		}
		if owner, ok := symbols[symbol]; ok {
			return nil, nil, fmt.Errorf("交易代码重复：%s 同时属于 %s 与 %s", symbol, owner, id)
		}
		symbols[symbol] = id
		name := row.Name
		if strings.TrimSpace(row.Name) == "" {
			name = row.NameJP
		}
		if row.hasOverride && row.override.HasName {
			name = row.override.Name
		}
		sector := SectorFromAffiliation(row.Affiliation)
		if row.hasOverride && row.override.HasSector {
			sector = row.override.Sector
		}
		role := "unknown"
		if row.hasOverride && row.override.HasRole {
			role = row.override.Role
		}
		var chapters *int
		switch {
		case len(row.countable) > 0:
			n := len(row.countable)
			chapters = &n
			report.AppearanceCovered++
		case row.evidence:
			n := 0
			chapters = &n
			report.ZeroChapterCount = append(report.ZeroChapterCount, name)
		default:
			report.MissingChapterCount = append(report.MissingChapterCount, name)
		}
		if row.rank != nil {
			report.RankCovered++
		} else {
			report.MissingRank = append(report.MissingRank, name)
		}
		instruments = append(instruments, Instrument{
			InstrumentID:   id,
			Symbol:         symbol,
			Name:           name,
			Sector:         sector,
			Role:           role,
			ChapterCount:   chapters,
			PopularityRank: row.rank,
		})
	}
	sort.Slice(instruments, func(i, j int) bool {
		return instruments[i].InstrumentID < instruments[j].InstrumentID
	})
	sort.Strings(report.MissingRank)
	sort.Strings(report.MissingChapterCount)
	sort.Strings(report.ZeroChapterCount)
	sort.Strings(report.Unmatched)
	sort.Strings(report.Conflicts)
	report.TiedRanks = tiedRanks(instruments)
	report.Sectors = sectorsOf(instruments)
	report.InstrumentCount = len(instruments)

	release := Release{
		SchemaVersion: 1,
		DatasetID:     datasetID(in.CutoffChapter, in.Characters, in.Appearances, in.Rankings, in.Overrides),
		CutoffChapter: in.CutoffChapter,
		Sources:       sourcesFrom(manifest, in, report),
		Instruments:   instruments,
	}
	body, err := encodeRelease(release)
	if err != nil {
		return nil, nil, err
	}
	reportBody, err := encodeJSON(report)
	if err != nil {
		return nil, nil, err
	}
	return body, reportBody, nil
}

func addName(index map[string][]int, name string, i int) {
	key := NormalizeName(name)
	if key == "" {
		return
	}
	index[key] = append(index[key], i)
}

func uniqueIndexes(in []int) []int {
	if len(in) == 0 {
		return nil
	}
	seen := map[int]struct{}{}
	out := make([]int, 0, len(in))
	for _, i := range in {
		if _, ok := seen[i]; ok {
			continue
		}
		seen[i] = struct{}{}
		out = append(out, i)
	}
	return out
}

func symbolFor(id string) string {
	sum := sha256.Sum256([]byte(id))
	return strings.ToUpper(hex.EncodeToString(sum[:6])) + "OP"
}

func datasetID(cutoff int, characters, appearances, rankings, overrides []byte) string {
	parts := []string{
		shaHex(characters),
		shaHex(appearances),
		shaHex(rankings),
		shaHex(overrides),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return fmt.Sprintf("one-piece-c%d-%s", cutoff, hex.EncodeToString(sum[:])[:12])
}

func shaHex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func parseManifest(raw []byte) (map[string]manifestItem, error) {
	var doc struct {
		Items []manifestItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("来源清单无法解析：%w", err)
	}
	out := map[string]manifestItem{}
	for _, item := range doc.Items {
		if item.Name == "" || item.URL == "" || item.FetchedAt == "" {
			return nil, fmt.Errorf("来源清单缺少 name、url 或 fetched_at")
		}
		out[item.Name] = item
	}
	for _, name := range []string{"characters", "appearances", "rankings"} {
		if _, ok := out[name]; !ok {
			return nil, fmt.Errorf("来源清单缺少 %s", name)
		}
	}
	return out, nil
}

func sourcesFrom(manifest map[string]manifestItem, in BuildInput, report Report) []Source {
	files := []struct {
		name string
		raw  []byte
		note string
	}{
		{"characters", in.Characters, fmt.Sprintf("candidates:%d", report.InstrumentCount+report.Excluded)},
		{"appearances", in.Appearances, fmt.Sprintf("appearance_covered:%d", report.AppearanceCovered)},
		{"rankings", in.Rankings, fmt.Sprintf("rank_covered:%d", report.RankCovered)},
	}
	out := make([]Source, 0, len(files))
	for _, file := range files {
		item := manifest[file.name]
		out = append(out, Source{
			Name:      file.name,
			URL:       item.URL,
			FetchedAt: item.FetchedAt,
			SHA256:    shaHex(file.raw),
			Note:      file.note,
		})
	}
	return out
}

func tiedRanks(items []Instrument) []Tie {
	groups := map[int][]string{}
	var ranks []int
	for _, item := range items {
		if item.PopularityRank == nil {
			continue
		}
		rank := *item.PopularityRank
		if _, ok := groups[rank]; !ok {
			ranks = append(ranks, rank)
		}
		groups[rank] = append(groups[rank], item.Name)
	}
	sort.Ints(ranks)
	out := []Tie{}
	for _, rank := range ranks {
		names := groups[rank]
		sort.Strings(names)
		if len(names) > 1 {
			out = append(out, Tie{Rank: rank, Names: names})
		}
	}
	return out
}

func sectorsOf(items []Instrument) []string {
	seen := map[string]struct{}{}
	for _, item := range items {
		seen[item.Sector] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for sector := range seen {
		out = append(out, sector)
	}
	sort.Strings(out)
	return out
}

func encodeRelease(release Release) ([]byte, error) {
	return encodeJSON(release)
}

func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func FileSHA256(raw []byte) string {
	return shaHex(raw)
}
