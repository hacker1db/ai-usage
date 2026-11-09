package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"ai-usage/internal/paths"
	"ai-usage/internal/snapshot"
	"ai-usage/internal/web"
)

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	host := fs.String("host", "127.0.0.1", "loopback address to bind (127.0.0.1, localhost, or ::1)")
	port := fs.Int("port", envInt("AI_USAGE_PORT", paths.DefaultPort), "port")
	interval := fs.Duration("interval", 30*time.Second, "time between collections (min 5s)")
	days := fs.Int("days", 35, "history to load in days (2-400)")
	dataDir := fs.String("data-dir", paths.DataDir(), "history and work-report folder")
	config := fs.String("config", paths.ConfigFile(), "budgets file (see dashboard.example.json)")
	projects := fs.String("projects", paths.ProjectsFile(), "ownership rules (see projects.example.yaml)")
	noHistory := fs.Bool("no-history", false, "keep history in memory only")
	opts := sourceFlags(fs)
	_ = fs.Parse(args)

	if ip := net.ParseIP(*host); *host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("-host %q must be a loopback address", *host)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	histPath := paths.HistoryFile(*dataDir)
	if *noHistory {
		histPath = ""
	}
	workDir := paths.WorkReportDir(*dataDir)
	c := snapshot.New(snapshot.Options{Days: *days, Interval: *interval, Sources: opts(), ConfigPath: *config,
		HistoryPath: histPath, WorkReportDir: workDir, ProjectsPath: *projects, Logger: log})
	h, err := web.New(c, *port, workDir, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go c.Run(ctx) // first collection runs in the background so the page answers right away

	ln, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	log.Info("ai-usage dashboard", "url", "http://"+ln.Addr().String(), "interval", interval.String(),
		"history", histPath, "version", version)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func envInt(name string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return d
}
