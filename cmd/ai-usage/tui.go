package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"ai-usage/internal/palette"
	"ai-usage/internal/paths"
	"ai-usage/internal/snapshot"
	"ai-usage/internal/tui"
	"ai-usage/internal/view"
)

func runTUI(args []string) error {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	once := fs.Bool("once", false, "print one frame to stdout and exit")
	width := fs.Int("width", 0, "frame width for -once (default $COLUMNS, the terminal, or 120)")
	theme := fs.String("theme", "dark", "dark or light terminal background")
	color := fs.String("color", "auto", "auto, always, or never (NO_COLOR disables auto)")
	dashURL := fs.String("url", "http://127.0.0.1:"+strconv.Itoa(paths.DefaultPort), "running dashboard to read from")
	local := fs.Bool("local", false, "collect in-process instead of reading the dashboard")
	dataDir := fs.String("data-dir", paths.DataDir(), "history and work-report folder (read-only here)")
	config := fs.String("config", paths.ConfigFile(), "budgets file")
	projects := fs.String("projects", paths.ProjectsFile(), "ownership rules (used when collecting locally)")
	days := fs.Int("days", 35, "history to load in days, for local collection")
	interval := fs.Duration("interval", 30*time.Second, "time between local collections")
	tab := fs.String("tab", view.Defaults.Tab, "initial tab: all, claude, codex, copilot, foundry")
	metric := fs.String("metric", view.Defaults.Metric, "initial metric: total, uncached, output, requests")
	rng := fs.String("range", view.Defaults.Range, "initial range: 24h, 7d, 30d")
	opts := sourceFlags(fs)
	_ = fs.Parse(args)

	mode := palette.Dark
	switch *theme {
	case "dark":
	case "light":
		mode = palette.Light
	default:
		return fmt.Errorf("-theme must be dark or light, not %q", *theme)
	}
	esc := tui.NoEscapes
	tty := tui.IsTerminal(int(os.Stdout.Fd()))
	switch *color {
	case "always":
		esc = tui.FullColor
	case "never":
		if tty {
			esc = tui.StyleOnly
		}
	case "auto":
		switch {
		case !tty && *once:
		case os.Getenv("NO_COLOR") != "":
			esc = tui.StyleOnly
		default:
			esc = tui.FullColor
		}
	default:
		return fmt.Errorf("-color must be auto, always, or never, not %q", *color)
	}

	o := tui.Options{
		Filters: view.Filters{Tab: *tab, Metric: *metric, Range: *rng, Burn: view.Defaults.Burn, Sort: view.Defaults.Sort}.Normalize(),
		URL:     *dashURL, Local: *local, Theme: tui.NewTheme(mode, esc), Out: os.Stdout,
		Collect: snapshot.Options{
			Days: *days, Interval: *interval, Sources: opts(), ConfigPath: *config,
			HistoryPath: paths.HistoryFile(*dataDir), ReadOnly: true, WorkReportDir: paths.WorkReportDir(*dataDir),
			ProjectsPath: *projects,
			Logger:       slog.New(slog.DiscardHandler),
		},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *once {
		o.Width = *width
		if o.Width <= 0 {
			if c, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && c > 0 {
				o.Width = c
			} else if c, _, ok := tui.Size(int(os.Stdout.Fd())); ok {
				o.Width = c
			}
		}
		return tui.Once(ctx, o)
	}
	return tui.Run(ctx, o)
}
