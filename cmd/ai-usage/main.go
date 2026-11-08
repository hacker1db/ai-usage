// Command ai-usage tracks local AI token usage across Claude Code, Codex CLI, GitHub Copilot CLI,
// and foundry-entra-proxy. It reads each tool's logs read-only.
//
//	ai-usage serve    live web dashboard on a loopback address
//	ai-usage tui      live terminal dashboard (same data and charts)
//	ai-usage report   dated report.md, usage.json, usage.csv
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"ai-usage/internal/sources"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usageExit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "tui":
		err = runTUI(args)
	case "report":
		err = runReport(args)
	case "version", "-version", "--version":
		fmt.Println(version)
	case "help", "-h", "-help", "--help":
		usageExit(0)
	default:
		fmt.Fprintf(os.Stderr, "ai-usage: unknown command %q\n\n", cmd)
		usageExit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ai-usage:", err)
		os.Exit(1)
	}
}

func usageExit(code int) {
	fmt.Fprint(os.Stderr, `usage: ai-usage <command> [flags]

commands:
  serve    live web dashboard on a loopback address (default http://127.0.0.1:8790)
  tui      live terminal dashboard; reads a running dashboard, else collects locally
  report   write reports/<today>/{report.md,usage.json,usage.csv}
  version  print the version

Run "ai-usage <command> -h" for a command's flags.
`)
	os.Exit(code)
}

// claudeFlag is a repeatable DIR=LABEL flag that replaces the default Claude profile dirs.
type claudeFlag []sources.ClaudeDir

func (c *claudeFlag) String() string { return "" }

func (c *claudeFlag) Set(v string) error {
	dir, label, ok := strings.Cut(v, "=")
	if !ok {
		label = strings.TrimPrefix(dir[strings.LastIndex(dir, "/")+1:], ".")
	}
	if dir == "" || label == "" {
		return fmt.Errorf("want DIR=LABEL, got %q", v)
	}
	*c = append(*c, sources.ClaudeDir{Path: dir, Label: label})
	return nil
}

// sourceFlags registers the log-location flags shared by every command.
func sourceFlags(fs *flag.FlagSet) func() sources.Options {
	d := sources.DefaultOptions()
	var claude claudeFlag
	fs.Var(&claude, "claude", "Claude config `DIR=LABEL` (repeatable; replaces the default profiles)")
	codex := fs.String("codex-home", d.CodexHome, "Codex home")
	copilot := fs.String("copilot-db", d.CopilotDB, "Copilot CLI session store")
	proxy := fs.String("proxy-log", d.ProxyLog, "foundry-entra-proxy log")
	return func() sources.Options {
		o := sources.Options{CodexHome: *codex, CopilotDB: *copilot, ProxyLog: *proxy}
		if len(claude) > 0 {
			o.Claude = claude
		}
		return o
	}
}
