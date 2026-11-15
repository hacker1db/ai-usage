// Command ai-gateway is a loopback API gateway that gives local tools (Caido, scripts, editors) one
// endpoint and a gateway-issued key, and forwards their OpenAI- or Anthropic-format calls to the
// upstream accounts in its config. See docs/ai-gateway.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"ai-usage/internal/gateway"
)

var version = "dev"

const defaultConfig = "~/.config/ai-gateway/config.json"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "keygen":
			if err := keygen(os.Args[2:], os.Stdout); err != nil {
				fmt.Fprintln(os.Stderr, "ai-gateway keygen:", err)
				os.Exit(2)
			}
			return
		case "check":
			if err := check(os.Args[2:], os.Stdout); err != nil {
				fmt.Fprintln(os.Stderr, "ai-gateway check:", err)
				os.Exit(1)
			}
			return
		}
	}

	fs := flag.NewFlagSet("ai-gateway", flag.ExitOnError)
	cfgPath := fs.String("config", envOr("AI_GATEWAY_CONFIG", defaultConfig), "config file")
	showVersion := fs.Bool("version", false, "print version and exit")
	_ = fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println(version)
		return
	}
	if err := serve(*cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "ai-gateway:", err)
		os.Exit(1)
	}
}

// keygen prints a new client key and the config entry that accepts it. The key is shown once and
// only its hash is stored.
func keygen(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	name := fs.String("name", "", "client name, for example caido (required)")
	keyFile := fs.String("key-file", "", "also write the key to this file with mode 600")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("-name is required")
	}
	key, hash, err := gateway.NewClientKey()
	if err != nil {
		return err
	}
	if *keyFile != "" {
		path := expandHome(*keyFile)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(f, key); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Fprintf(out, "key written to %s\n", path)
	} else {
		fmt.Fprintf(out, "API key (shown once; paste it into %s):\n  %s\n", *name, key)
	}
	fmt.Fprintf(out, "config entry for \"clients\":\n  {\"name\": %q, \"key_sha256\": %q}\n", *name, hash)
	return nil
}

// check validates a config file and reports, per provider, whether its credential resolves.
func check(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	cfgPath := fs.String("config", envOr("AI_GATEWAY_CONFIG", defaultConfig), "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := gateway.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	results := gateway.CheckCredentials(ctx, cfg, os.Getenv)
	failed := false
	for _, r := range results {
		status := "ok"
		if r.Err != nil {
			status, failed = "FAIL: "+r.Err.Error(), true
		}
		fmt.Fprintf(out, "%-20s %-10s %-8s %s\n", r.Provider, r.Kind, r.Auth, status)
	}
	fmt.Fprintf(out, "%d clients, %d models, listen %s\n", len(cfg.Clients), len(cfg.Models), cfg.Listen)
	if failed {
		return errors.New("one or more credentials failed")
	}
	return nil
}

func serve(cfgPath string) error {
	cfg, err := gateway.LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("log_level: %w", err)
	}
	logOut := io.Writer(os.Stderr)
	if cfg.LogFile != "" {
		path := expandHome(cfg.LogFile)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		logOut = f
	}
	logger := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: level}))

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: time.Duration(cfg.ResponseHeaderTimeout),
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   16,
	}
	handler := gateway.New(gateway.Options{Config: cfg, Transport: transport, Logger: logger, Version: version, Getenv: os.Getenv})

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	// The bound address goes to stdout so a supervisor (or the integration test) can find a
	// ":0" port.
	fmt.Printf("ai-gateway listening on http://%s\n", ln.Addr())
	logger.Info("listening", "addr", ln.Addr().String(), "providers", len(cfg.Providers), "models", len(cfg.Models), "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	logger.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); errors.Is(err, context.DeadlineExceeded) {
		return srv.Close()
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}
