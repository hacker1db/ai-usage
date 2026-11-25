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
	case "cost":
		err = runCost(args)
	case "report":
		err = runReport(args)
	case "plan-cost":
		err = runPlanCost(args)
	case "m365-sync":
		err = runM365Sync(args)
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
  serve    live web dashboard on a loopback address (default http://127.0.0.1:8790); opens the browser, -open=false to skip
  tui      live terminal dashboard; reads a running dashboard, else collects locally
  report   write reports/<today>/{report.md,usage.json,usage.csv}
  cost     retrieve actual Azure or Copilot metered costs as JSON
  plan-cost  total tokens and estimated cost of linked sessions (-session tool|profile|id ...)
  m365-sync  fetch your Microsoft 365 Copilot daily usage from Microsoft Graph (needs Reports.Read.All)
  version  print the version

Run "ai-usage <command> -h" for a command's flags.
`)
	os.Exit(code)
}

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

func sourceFlags(fs *flag.FlagSet) func() sources.Options {
	d := sources.DefaultOptions()
	var claude claudeFlag
	fs.Var(&claude, "claude", "Claude config `DIR=LABEL` (repeatable; replaces the default profiles)")
	codex := fs.String("codex-home", d.CodexHome, "Codex home")
	copilot := fs.String("copilot-db", d.CopilotDB, "Copilot CLI session store")
	antigravity := fs.String("antigravity-home", d.AntigravityHome, "Antigravity CLI data dir")
	proxy := fs.String("proxy-log", d.ProxyLog, "foundry-entra-proxy log")
	gw := fs.String("gateway-log", d.GatewayLog, "ai-gateway log")
	m365 := fs.String("m365-store", d.M365Store, "Microsoft 365 Copilot store written by m365-sync")
	return func() sources.Options {
		o := sources.Options{CodexHome: *codex, CodexExtra: sources.DefaultCodexExtras(*codex), CopilotDB: *copilot,
			AntigravityHome: *antigravity, ProxyLog: *proxy, GatewayLog: *gw, M365Store: *m365}
		if len(claude) > 0 {
			o.Claude = claude
		}
		return o
	}
}
