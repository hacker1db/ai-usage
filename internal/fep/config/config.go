// Package config loads proxy settings from the environment.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	DefaultListen             = "127.0.0.1:8787"
	DefaultTokenScope         = "https://ai.azure.com/.default"
	DefaultStreamIdleTimeout  = 2 * time.Minute
	DefaultResponseHeaderWait = 10 * time.Minute
	DefaultMaxRequestBytes    = 64 << 20
)

type Config struct {
	Listen             string
	UpstreamBase       *url.URL
	TokenScope         string
	Tenant             string
	AzPath             string
	ClientKey          string
	AllowedDeployments []string
	StreamIdleTimeout  time.Duration
	ResponseHeaderWait time.Duration
	MaxRequestBytes    int64
	LogLevel           string
}

// Load reads configuration through getenv so tests can supply a fake environment.
func Load(getenv func(string) string) (*Config, error) {
	var errs []error

	cfg := &Config{
		Listen:             valueOr(getenv("FEP_LISTEN"), DefaultListen),
		TokenScope:         valueOr(getenv("FEP_TOKEN_SCOPE"), DefaultTokenScope),
		Tenant:             strings.TrimSpace(getenv("FEP_TENANT")),
		AzPath:             valueOr(getenv("FEP_AZ_PATH"), "az"),
		StreamIdleTimeout:  DefaultStreamIdleTimeout,
		ResponseHeaderWait: DefaultResponseHeaderWait,
		MaxRequestBytes:    DefaultMaxRequestBytes,
		LogLevel:           valueOr(getenv("FEP_LOG_LEVEL"), "info"),
	}

	if err := checkLoopback(cfg.Listen); err != nil {
		errs = append(errs, err)
	}

	base := strings.TrimSpace(getenv("FEP_UPSTREAM_BASE"))
	if base == "" {
		errs = append(errs, errors.New("FEP_UPSTREAM_BASE is required (e.g. https://<resource>.services.ai.azure.com)"))
	} else if u, err := parseUpstream(base); err != nil {
		errs = append(errs, err)
	} else {
		cfg.UpstreamBase = u
	}

	key, err := loadClientKey(getenv)
	if err != nil {
		errs = append(errs, err)
	}
	cfg.ClientKey = key

	cfg.AllowedDeployments = splitList(getenv("FEP_ALLOWED_DEPLOYMENTS"))
	if len(cfg.AllowedDeployments) == 0 {
		errs = append(errs, errors.New("FEP_ALLOWED_DEPLOYMENTS is required (comma-separated Foundry deployment names)"))
	}

	if v := getenv("FEP_STREAM_IDLE_TIMEOUT"); v != "" {
		d, err := parsePositiveDuration("FEP_STREAM_IDLE_TIMEOUT", v)
		if err != nil {
			errs = append(errs, err)
		}
		cfg.StreamIdleTimeout = d
	}
	if v := getenv("FEP_RESPONSE_HEADER_TIMEOUT"); v != "" {
		d, err := parsePositiveDuration("FEP_RESPONSE_HEADER_TIMEOUT", v)
		if err != nil {
			errs = append(errs, err)
		}
		cfg.ResponseHeaderWait = d
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadClientKey prefers FEP_CLIENT_KEY_FILE so the secret stays out of the process environment.
func loadClientKey(getenv func(string) string) (string, error) {
	if path := strings.TrimSpace(getenv("FEP_CLIENT_KEY_FILE")); path != "" {
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("FEP_CLIENT_KEY_FILE: %w", err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("FEP_CLIENT_KEY_FILE %s must not be readable by group or others (chmod 600)", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("FEP_CLIENT_KEY_FILE: %w", err)
		}
		return validateKey(strings.TrimSpace(string(raw)))
	}
	return validateKey(strings.TrimSpace(getenv("FEP_CLIENT_KEY")))
}

func validateKey(key string) (string, error) {
	if key == "" {
		return "", errors.New("a client key is required: set FEP_CLIENT_KEY_FILE (preferred) or FEP_CLIENT_KEY")
	}
	if len(key) < 32 {
		return "", errors.New("client key must be at least 32 characters")
	}
	return key, nil
}

func checkLoopback(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("FEP_LISTEN %q: %w", listen, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("FEP_LISTEN %q must bind a loopback address", listen)
	}
	return nil
}

func parseUpstream(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return nil, fmt.Errorf("FEP_UPSTREAM_BASE: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("FEP_UPSTREAM_BASE %q must be an https URL with a host", raw)
	}
	if u.Path != "" || u.RawQuery != "" {
		return nil, fmt.Errorf("FEP_UPSTREAM_BASE %q must not include a path or query", raw)
	}
	return u, nil
}

func parsePositiveDuration(name, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s %q must be a positive duration such as 90s", name, v)
	}
	return d, nil
}

func splitList(v string) []string {
	var out []string
	for item := range strings.SplitSeq(v, ",") {
		item = strings.TrimSpace(item)
		if item != "" && !slices.Contains(out, item) {
			out = append(out, item)
		}
	}
	return out
}

func valueOr(v, fallback string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return fallback
}
