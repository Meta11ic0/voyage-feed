package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	return dispatch(args, os.Stdout, os.Stderr)
}

func dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: market-simulator <serve|dataset|verify>。入口尚未实现。")
		return 2
	}
	switch args[0] {
	case "-h", "--help":
		fmt.Fprintln(stdout, "market-simulator serve")
		fmt.Fprintln(stdout, "market-simulator dataset fetch")
		fmt.Fprintln(stdout, "market-simulator dataset build")
		fmt.Fprintln(stdout, "market-simulator verify")
		return 0
	case "serve":
		return serveCommand(args[1:], stderr)
	case "verify":
		return verifyCommand(args[1:], stderr)
	case "dataset":
		return datasetCommand(args[1:], stderr)
	default:
		fmt.Fprintln(stderr, "未知命令。")
		return 2
	}
}
