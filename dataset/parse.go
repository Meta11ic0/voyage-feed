package dataset

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

type character struct {
	Name            string
	NameJP          string
	ChrID           string
	FirstAppearance string
	Affiliation     string
	FirstChapter    *int
	ChapterKnown    bool
	Aliases         []string
}

var chapterDigits = regexp.MustCompile(`[0-9]+`)

func ParseCharacters(raw []byte) ([]character, []string, error) {
	doc, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("角色目录 HTML 无法解析：%w", err)
	}
	var script strings.Builder
	var found bool
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "script" {
			for _, attr := range n.Attr {
				if attr.Key == "id" && attr.Val == "punk-records-data" {
					found = true
					for c := n.FirstChild; c != nil; c = c.NextSibling {
						if c.Type == html.TextNode {
							script.WriteString(c.Data)
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if !found {
		return nil, nil, fmt.Errorf("找不到 id=punk-records-data 的脚本")
	}
	var rows []struct {
		Name            *string `json:"name"`
		NameJP          *string `json:"name_jp"`
		ChrID           *string `json:"chr_id"`
		FirstAppearance *string `json:"first_appearance"`
		Affiliation     *string `json:"affiliation"`
	}
	if err := json.Unmarshal([]byte(script.String()), &rows); err != nil {
		return nil, nil, fmt.Errorf("角色目录 JSON 无法解析：%w", err)
	}
	out := make([]character, 0, len(rows))
	notes := make([]string, 0)
	for _, row := range rows {
		name := textOf(row.Name)
		if name == "" {
			return nil, nil, fmt.Errorf("角色缺少 name")
		}
		nameJP := textOf(row.NameJP)
		chrID := textOf(row.ChrID)
		first := textOf(row.FirstAppearance)
		item := character{
			Name:            name,
			NameJP:          nameJP,
			FirstAppearance: first,
			Affiliation:     textOf(row.Affiliation),
		}
		if first != "" {
			match := chapterDigits.FindString(first)
			if match == "" {
				notes = append(notes, fmt.Sprintf("无法解析首次登场章节：%s", name))
			} else {
				n, err := strconv.Atoi(match)
				if err != nil {
					return nil, nil, fmt.Errorf("首次登场章节超出范围：%s", name)
				}
				item.FirstChapter = &n
				item.ChapterKnown = true
			}
		}
		if chrID == "" {
			item.ChrID = "name:" + nameFingerprint(nameJP, name, item.FirstChapter)
		} else if !chrIDPattern.MatchString(chrID) {
			return nil, nil, fmt.Errorf("chr_id 格式不合法：%s", chrID)
		} else {
			item.ChrID = chrID
		}
		out = append(out, item)
	}
	return collapseDuplicateIDs(out), notes, nil
}

func collapseDuplicateIDs(rows []character) []character {
	grouped := map[string][]int{}
	for i, row := range rows {
		if strings.HasPrefix(row.ChrID, "name:") {
			continue
		}
		grouped[row.ChrID] = append(grouped[row.ChrID], i)
	}
	drop := map[int]struct{}{}
	for _, indexes := range grouped {
		if len(indexes) < 2 {
			continue
		}
		if merged, ok := mergeSameCharacter(rows, indexes); ok {
			rows[indexes[0]] = merged
			for _, index := range indexes[1:] {
				drop[index] = struct{}{}
			}
			continue
		}
		for _, index := range indexes {
			row := rows[index]
			row.ChrID = "name:" + nameFingerprint(row.NameJP, row.Name, row.FirstChapter)
			rows[index] = row
		}
	}
	out := make([]character, 0, len(rows))
	for i, row := range rows {
		if _, ok := drop[i]; ok {
			continue
		}
		out = append(out, row)
	}
	return out
}

func mergeSameCharacter(rows []character, indexes []int) (character, bool) {
	base := rows[indexes[0]]
	jp := NormalizeName(base.NameJP)
	if jp == "" {
		return character{}, false
	}
	for _, index := range indexes[1:] {
		row := rows[index]
		if NormalizeName(row.NameJP) != jp || row.FirstAppearance != base.FirstAppearance {
			return character{}, false
		}
		if base.Affiliation == "" {
			base.Affiliation = row.Affiliation
		} else if row.Affiliation != "" && row.Affiliation != base.Affiliation {
			return character{}, false
		}
		if row.Name != base.Name {
			base.Aliases = append(base.Aliases, row.Name)
		}
	}
	return base, true
}

func textOf(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func nameFingerprint(nameJP, name string, chapter *int) string {
	chapterText := ""
	if chapter != nil {
		chapterText = strconv.Itoa(*chapter)
	}
	sum := sha256.Sum256([]byte(NormalizeName(nameJP) + "\n" + NormalizeName(name) + "\n" + chapterText))
	return hex.EncodeToString(sum[:])[:16]
}

var chrIDPattern = regexp.MustCompile(`^chr:[0-9]+$`)

type appearanceRow struct {
	Chapter int
	Name    string
	Type    string
}

func ParseAppearances(raw []byte, cutoff int) ([]appearanceRow, error) {
	if bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) {
		return nil, fmt.Errorf("出场 CSV 含有 BOM")
	}
	reader := csv.NewReader(bytes.NewReader(raw))
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("出场 CSV 无法读取表头：%w", err)
	}
	if len(header) != 3 || header[0] != "chapter" || header[1] != "name" || header[2] != "type" {
		return nil, fmt.Errorf("出场 CSV 表头必须是 chapter,name,type")
	}
	var rows []appearanceRow
	maxChapter := 0
	line := 1
	for {
		line++
		rec, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("出场 CSV 第 %d 行无法读取：%w", line, err)
		}
		if len(rec) != 3 {
			return nil, fmt.Errorf("出场 CSV 第 %d 行列数不对", line)
		}
		chapter, err := strconv.Atoi(rec[0])
		if err != nil || chapter < 0 || rec[0] != strconv.Itoa(chapter) {
			return nil, fmt.Errorf("出场 CSV 第 %d 行章节不是十进制整数", line)
		}
		if chapter > maxChapter {
			maxChapter = chapter
		}
		rows = append(rows, appearanceRow{Chapter: chapter, Name: rec[1], Type: rec[2]})
	}
	if maxChapter < cutoff {
		return nil, fmt.Errorf("出场 CSV 最大章节 %d 小于截止章 %d", maxChapter, cutoff)
	}
	return rows, nil
}

type rankingRow struct {
	Rank int
	Name string
}

func ParseRankings(raw []byte) ([]rankingRow, error) {
	doc, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("人气榜 HTML 无法解析：%w", err)
	}
	var rows []rankingRow
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, inAll bool) {
		next := inAll
		if n.Type == html.ElementNode {
			if _, panel := attr(n, "data-fr-static-panel"); panel {
				if region, ok := attr(n, "data-region"); ok {
					next = region == "all"
				}
			}
			if next {
				if _, ok := attr(n, "data-portrait-name"); ok {
					name := portraitName(n)
					rankText, hasRank := nearestRank(n)
					if name != "" && hasRank {
						rank, err := strconv.Atoi(rankText)
						if err != nil || rank < 1 {
							rows = append(rows, rankingRow{Rank: -1, Name: name})
						} else {
							rows = append(rows, rankingRow{Rank: rank, Name: name})
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, next)
		}
	}
	walk(doc, false)
	for _, row := range rows {
		if row.Rank < 1 {
			return nil, fmt.Errorf("人气名次不合法：%s", row.Name)
		}
	}
	return rows, nil
}

func ValidateAppearanceStructure(raw []byte) error {
	if bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) {
		return fmt.Errorf("出场 CSV 含有 BOM")
	}
	reader := csv.NewReader(bytes.NewReader(raw))
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return fmt.Errorf("出场 CSV 无法读取表头：%w", err)
	}
	if len(header) != 3 || header[0] != "chapter" || header[1] != "name" || header[2] != "type" {
		return fmt.Errorf("出场 CSV 表头必须是 chapter,name,type")
	}
	line := 1
	for {
		line++
		rec, err := reader.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("出场 CSV 第 %d 行无法读取：%w", line, err)
		}
		if len(rec) != 3 {
			return fmt.Errorf("出场 CSV 第 %d 行列数不对", line)
		}
		if _, err := strconv.Atoi(rec[0]); err != nil {
			return fmt.Errorf("出场 CSV 第 %d 行章节不是十进制整数", line)
		}
	}
}

func attr(n *html.Node, key string) (string, bool) {
	for _, item := range n.Attr {
		if item.Key == key {
			return item.Val, true
		}
	}
	return "", false
}

func portraitName(n *html.Node) string {
	if value, ok := attr(n, "data-portrait-name"); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	var found string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if found != "" || node == nil {
			return
		}
		if node.Type == html.ElementNode {
			if _, ok := attr(node, "data-portrait-name"); ok {
				found = strings.TrimSpace(nodeText(node))
				return
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return found
}

func nearestRank(nameNode *html.Node) (string, bool) {
	for parent := nameNode; parent != nil; parent = parent.Parent {
		if countAttr(parent, "data-portrait-name") != 1 {
			continue
		}
		if rank, ok := singleRank(parent, nameNode); ok {
			return rank, true
		}
	}
	return "", false
}

func countAttr(root *html.Node, key string) int {
	count := 0
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if _, ok := attr(n, key); ok {
				count++
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return count
}

func singleRank(root, nameNode *html.Node) (string, bool) {
	var values []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if value, ok := attr(n, "data-rank"); ok && strings.TrimSpace(value) != "" {
				values = append(values, strings.TrimSpace(value))
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	if len(values) == 0 {
		return "", false
	}
	unique := map[string]struct{}{}
	for _, value := range values {
		unique[value] = struct{}{}
	}
	if len(unique) == 1 {
		return values[0], true
	}
	for parent := nameNode; parent != nil; parent = parent.Parent {
		if value, ok := attr(parent, "data-rank"); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), true
		}
		if parent == root {
			break
		}
	}
	return values[0], true
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return b.String()
}
