// Command foundry-entra-proxy is a loopback proxy that lets OpenAI-compatible clients reach
// Entra-only Azure AI Foundry deployments using the developer's Azure CLI session.
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
	"syscall"
	"time"

	"foundry-entra-proxy/internal/config"
	"foundry-entra-proxy/internal/proxy"
	"foundry-entra-proxy/internal/token"
)

var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "foundry-entra-proxy:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("FEP_LOG_LEVEL: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: cfg.ResponseHeaderWait,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   16,
	}

	handler := proxy.New(proxy.Options{
		Upstream:           cfg.UpstreamBase,
		ClientKey:          cfg.ClientKey,
		AllowedDeployments: cfg.AllowedDeployments,
		Tokens:             token.NewAzCLI(cfg.AzPath, cfg.TokenScope, cfg.Tenant),
		Transport:          transport,
		StreamIdleTimeout:  cfg.StreamIdleTimeout,
		MaxRequestBytes:    cfg.MaxRequestBytes,
		Logger:             logger,
	})

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}

	logger.Info("listening",
		"addr", ln.Addr().String(),
		"upstream", cfg.UpstreamBase.Host,
		"token_scope", cfg.TokenScope,
		"allowed_deployments", cfg.AllowedDeployments,
		"version", version,
	)

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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
