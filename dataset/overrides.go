package dataset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type Override struct {
	ChrID         string
	Name          string
	Aliases       []string
	Sector        string
	Role          string
	InstrumentID  string
	Symbol        string
	Source        string
	HasName       bool
	HasSector     bool
	HasRole       bool
	HasInstrument bool
	HasSymbol     bool
}

func ParseOverrides(raw []byte) ([]Override, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("覆盖文件为空")
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return nil, fmt.Errorf("覆盖文件不是 JSON：%w", err)
	}
	if len(doc) == 0 {
		return nil, nil
	}
	entriesRaw, ok := doc["entries"]
	if !ok {
		return nil, fmt.Errorf("覆盖文件只接受空对象或 entries")
	}
	if string(entriesRaw) == "null" {
		return nil, fmt.Errorf("覆盖条目不能为 null")
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(entriesRaw, &entries); err != nil {
		return nil, fmt.Errorf("覆盖条目无法解析：%w", err)
	}
	out := make([]Override, 0, len(entries))
	for i, entry := range entries {
		item, err := parseOverride(entry)
		if err != nil {
			return nil, fmt.Errorf("覆盖条目 %d：%w", i, err)
		}
		out = append(out, item)
	}
	return out, nil
}

func parseOverride(entry map[string]json.RawMessage) (Override, error) {
	var out Override
	matchRaw, ok := entry["match"]
	if !ok || string(matchRaw) == "null" {
		return out, fmt.Errorf("缺少 match.chr_id")
	}
	var match map[string]json.RawMessage
	if err := json.Unmarshal(matchRaw, &match); err != nil {
		return out, fmt.Errorf("match 无法解析")
	}
	chr, err := requiredString(match, "chr_id")
	if err != nil {
		return out, err
	}
	out.ChrID = chr
	overrode := false
	if raw, ok := entry["name"]; ok {
		value, err := strictString(raw, "name")
		if err != nil {
			return out, err
		}
		out.Name = value
		out.HasName = true
		overrode = true
	}
	if raw, ok := entry["sector"]; ok {
		value, err := strictString(raw, "sector")
		if err != nil {
			return out, err
		}
		out.Sector = value
		out.HasSector = true
		overrode = true
	}
	if raw, ok := entry["role"]; ok {
		value, err := strictString(raw, "role")
		if err != nil {
			return out, err
		}
		switch value {
		case "leader", "core", "member", "unknown":
		default:
			return out, fmt.Errorf("role 不合法")
		}
		out.Role = value
		out.HasRole = true
		overrode = true
	}
	if raw, ok := entry["instrument_id"]; ok {
		value, err := strictString(raw, "instrument_id")
		if err != nil {
			return out, err
		}
		out.InstrumentID = value
		out.HasInstrument = true
		overrode = true
	}
	if raw, ok := entry["symbol"]; ok {
		value, err := strictString(raw, "symbol")
		if err != nil {
			return out, err
		}
		out.Symbol = value
		out.HasSymbol = true
		overrode = true
	}
	if raw, ok := entry["aliases"]; ok {
		if string(raw) == "null" {
			return out, fmt.Errorf("aliases 不能为 null")
		}
		var aliases []string
		if err := json.Unmarshal(raw, &aliases); err != nil {
			return out, fmt.Errorf("aliases 无法解析")
		}
		if len(aliases) == 0 {
			return out, fmt.Errorf("aliases 不能为空")
		}
		for _, alias := range aliases {
			if strings.TrimSpace(alias) == "" {
				return out, fmt.Errorf("别名不能为空")
			}
		}
		out.Aliases = aliases
		overrode = true
	}
	if !overrode {
		return out, fmt.Errorf("没有可覆盖的字段")
	}
	source, err := requiredString(entry, "source")
	if err != nil {
		return out, fmt.Errorf("填写覆盖字段时必须提供来源")
	}
	out.Source = source
	return out, nil
}

func requiredString(entry map[string]json.RawMessage, key string) (string, error) {
	raw, ok := entry[key]
	if !ok {
		return "", fmt.Errorf("缺少 %s", key)
	}
	return strictString(raw, key)
}

func strictString(raw json.RawMessage, key string) (string, error) {
	if string(raw) == "null" {
		return "", fmt.Errorf("%s 不能为 null", key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s 必须是字符串", key)
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s 不能为空", key)
	}
	return value, nil
}
