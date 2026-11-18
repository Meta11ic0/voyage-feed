package main

import (
	"bytes"
	"testing"
)

func TestDispatch(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cases := []struct {
		name   string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{name: "serve", args: []string{"serve"}, code: 2, stderr: "serve 需要环境变量 DATABASE_URL。\n"},
		{name: "dataset fetch", args: []string{"dataset", "fetch"}, code: 2, stderr: "dataset fetch 需要 --out-dir。\n"},
		{name: "dataset build", args: []string{"dataset", "build"}, code: 2, stderr: "dataset build 需要 --characters、--appearances、--rankings、--source-manifest、--cutoff-chapter、--overrides 和 --out。\n"},
		{name: "verify", args: []string{"verify"}, code: 2, stderr: "verify 需要 --profile ready、smoke 或 scale。\n"},
		{name: "verify scale", args: []string{"verify", "--profile", "scale"}, code: 1, stderr: "readyz 没有响应。\n"},
		{name: "dataset missing", args: []string{"dataset"}, code: 2, stderr: "dataset 需要子命令 fetch 或 build。\n"},
		{name: "unknown", args: []string{"trade"}, code: 2, stderr: "未知命令。\n"},
		{
			name:   "help short",
			args:   []string{"-h"},
			code:   0,
			stdout: "market-simulator serve\nmarket-simulator dataset fetch\nmarket-simulator dataset build\nmarket-simulator verify\n",
		},
		{
			name:   "help long",
			args:   []string{"--help"},
			code:   0,
			stdout: "market-simulator serve\nmarket-simulator dataset fetch\nmarket-simulator dataset build\nmarket-simulator verify\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := dispatch(tc.args, &stdout, &stderr)
			if code != tc.code {
				t.Fatalf("exit %d, want %d", code, tc.code)
			}
			if stdout.String() != tc.stdout {
				t.Fatalf("stdout %q, want %q", stdout.String(), tc.stdout)
			}
			if stderr.String() != tc.stderr {
				t.Fatalf("stderr %q, want %q", stderr.String(), tc.stderr)
			}
		})
	}
}
