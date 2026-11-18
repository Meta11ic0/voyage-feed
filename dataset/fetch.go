package dataset

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type FetchSource struct {
	Name string
	URL  string
	File string
}

func DefaultSources() []FetchSource {
	return []FetchSource{
		{Name: "characters", URL: "https://shimotsukicodex.com/characters.html", File: "characters.html"},
		{Name: "appearances", URL: "https://shimotsukicodex.com/appearances.csv", File: "appearances.csv"},
		{Name: "rankings", URL: "https://onepiecewt100-2026.com/ja/final-rankings/", File: "rankings.html"},
	}
}

type FetchConfig struct {
	OutDir   string
	Client   *http.Client
	Sources  []FetchSource
	Now      time.Time
	Attempts int
}

func Fetch(ctx context.Context, cfg FetchConfig) (string, error) {
	if cfg.OutDir == "" {
		return "", fmt.Errorf("缺少输出目录")
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if len(cfg.Sources) == 0 {
		cfg.Sources = DefaultSources()
	}
	if cfg.Attempts <= 0 {
		cfg.Attempts = 3
	}
	if cfg.Now.IsZero() {
		cfg.Now = time.Now().UTC()
	}
	dest := cfg.OutDir
	if _, err := os.Stat(filepath.Join(cfg.OutDir, "characters.html")); err == nil {
		dest = filepath.Join(cfg.OutDir, cfg.Now.UTC().Format("20060102T150405Z"))
	} else if !os.IsNotExist(err) {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".fetch-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	saved := map[string][]byte{}
	for _, source := range cfg.Sources {
		body, err := download(ctx, cfg.Client, source.URL, cfg.Attempts)
		if err != nil {
			return "", fmt.Errorf("抓取 %s 失败：%w", source.Name, err)
		}
		if err := validateFetched(source.Name, body); err != nil {
			return "", fmt.Errorf("%s 结构不符合预期：%w", source.Name, err)
		}
		if err := os.WriteFile(filepath.Join(tmp, source.File), body, 0o644); err != nil {
			return "", err
		}
		saved[source.Name] = body
	}
	manifest := map[string]any{
		"items": manifestItems(cfg.Sources, cfg.Now, saved),
	}
	raw, err := encodeJSON(manifest)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, "sources.json"), raw, 0o644); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func download(ctx context.Context, client *http.Client, url string, attempts int) ([]byte, error) {
	var last error
	for i := 0; i < attempts; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			last = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		return body, nil
	}
	return nil, last
}

func validateFetched(name string, body []byte) error {
	switch name {
	case "characters":
		rows, _, err := ParseCharacters(body)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("没有角色")
		}
		return nil
	case "appearances":
		return ValidateAppearanceStructure(body)
	case "rankings":
		rows, err := ParseRankings(body)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("没有全球人气条目")
		}
		return nil
	default:
		return fmt.Errorf("未知来源")
	}
}

func manifestItems(sources []FetchSource, now time.Time, saved map[string][]byte) []map[string]string {
	out := make([]map[string]string, 0, len(sources))
	stamp := now.UTC().Format(time.RFC3339)
	for _, source := range sources {
		out = append(out, map[string]string{
			"name":       source.Name,
			"url":        source.URL,
			"fetched_at": stamp,
			"sha256":     FileSHA256(saved[source.Name]),
		})
	}
	return out
}
