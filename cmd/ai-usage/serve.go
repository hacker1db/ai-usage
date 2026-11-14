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
	"os/exec"
	"os/signal"
	"runtime"
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
	open := fs.Bool("open", isTerminal(os.Stdout), "open the dashboard in the default browser (default true in a terminal)")
	opts := sourceFlags(fs)
	_ = fs.Parse(args)

	if ip := net.ParseIP(*host); *host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("-host %q must be a loopback address", *host)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	histPath, billingCache := paths.HistoryFile(*dataDir), paths.BillingCacheFile(*dataDir)
	if *noHistory {
		histPath, billingCache = "", ""
	}
	workDir := paths.WorkReportDir(*dataDir)
	c := snapshot.New(snapshot.Options{Days: *days, Interval: *interval, Sources: opts(), ConfigPath: *config,
		HistoryPath: histPath, WorkReportDir: workDir, ProjectsPath: *projects, Logger: log, BillingCache: billingCache})
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
	url := "http://" + ln.Addr().String()
	log.Info("ai-usage dashboard", "url", url, "interval", interval.String(),
		"history", histPath, "version", version)
	fmt.Printf("\nDashboard running at %s  (Ctrl+C to stop)\n\n", url)
	if *open {
		if err := openBrowser(url); err != nil {
			log.Warn("could not open browser", "err", err)
		}
	}
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

// openBrowser hands url to the platform's default URL handler without waiting for it.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reap the helper process
	return nil
}

// isTerminal reports whether f is an interactive terminal, so redirected or
// service runs don't try to launch a browser.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func envInt(name string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return d
}
