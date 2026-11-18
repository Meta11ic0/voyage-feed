package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"voyage-feed/dataset"
)

func datasetCommand(args []string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "dataset 需要子命令 fetch 或 build。")
		return 2
	}
	switch args[0] {
	case "fetch":
		return runFetch(args[1:], stderr)
	case "build":
		return runBuild(args[1:], stderr)
	default:
		fmt.Fprintln(stderr, "未知的 dataset 子命令。")
		return 2
	}
}

func runFetch(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	outDir := fs.String("out-dir", "", "输出目录")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *outDir == "" {
		fmt.Fprintln(stderr, "dataset fetch 需要 --out-dir。")
		return 2
	}
	dest, err := dataset.Fetch(context.Background(), dataset.FetchConfig{OutDir: *outDir})
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	fmt.Println(dest)
	return 0
}

func runBuild(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	characters := fs.String("characters", "", "")
	appearances := fs.String("appearances", "", "")
	rankings := fs.String("rankings", "", "")
	manifest := fs.String("source-manifest", "", "")
	overrides := fs.String("overrides", "", "")
	out := fs.String("out", "", "")
	cutoff := fs.Int("cutoff-chapter", 0, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *characters == "" || *appearances == "" || *rankings == "" || *manifest == "" || *overrides == "" || *out == "" || *cutoff == 0 {
		fmt.Fprintln(stderr, "dataset build 需要 --characters、--appearances、--rankings、--source-manifest、--cutoff-chapter、--overrides 和 --out。")
		return 2
	}
	if filepath.Ext(*out) != ".json" {
		fmt.Fprintln(stderr, "--out 必须以 .json 结尾。")
		return 2
	}
	read := func(path string) ([]byte, error) {
		return os.ReadFile(path)
	}
	rawCharacters, err := read(*characters)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	rawAppearances, err := read(*appearances)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	rawRankings, err := read(*rankings)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	rawManifest, err := read(*manifest)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	rawOverrides, err := read(*overrides)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	body, report, err := dataset.Build(dataset.BuildInput{
		Characters:     rawCharacters,
		Appearances:    rawAppearances,
		Rankings:       rawRankings,
		SourceManifest: rawManifest,
		Overrides:      rawOverrides,
		CutoffChapter:  *cutoff,
	})
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	reportPath := (*out)[:len(*out)-len(".json")] + ".report.json"
	if err := os.WriteFile(reportPath, report, 0o644); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	fmt.Println(*out)
	fmt.Println(reportPath)
	return 0
}
