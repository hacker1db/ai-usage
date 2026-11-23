package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"ai-usage/internal/fep/token"
)

var errNoCredential = errors.New("upstream credential unavailable")

type credential = token.Source

type noCred struct{}

func (noCred) Token(context.Context) (string, error) { return "", nil }
func (noCred) Invalidate(string)                     {}

type envCred struct {
	name   string
	getenv func(string) string
}

func (c envCred) Token(context.Context) (string, error) {
	v := strings.TrimSpace(c.getenv(c.name))
	if v == "" {
		return "", fmt.Errorf("%w: environment variable %s is empty", errNoCredential, c.name)
	}
	return v, nil
}
func (envCred) Invalidate(string) {}

type fileCred struct{ path string }

func (c fileCred) Token(context.Context) (string, error) {
	path := expandHome(c.path)
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNoCredential, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%w: %s must not be readable by group or others (chmod 600)", errNoCredential, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNoCredential, err)
	}
	v := strings.TrimSpace(string(raw))
	if v == "" {
		return "", fmt.Errorf("%w: %s is empty", errNoCredential, path)
	}
	return v, nil
}
func (fileCred) Invalidate(string) {}

type commandCred struct {
	argv    []string
	ttl     time.Duration
	timeout time.Duration
	run     token.Runner
	now     func() time.Time

	mu      sync.Mutex
	value   string
	expires time.Time
}

func newCommandCred(argv []string, ttl time.Duration, run token.Runner) *commandCred {
	if run == nil {
		run = token.ExecRunner
	}
	return &commandCred{argv: argv, ttl: ttl, timeout: 30 * time.Second, run: run, now: time.Now}
}

func (c *commandCred) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.value != "" && c.now().Before(c.expires) {
		return c.value, nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	stdout, stderr, err := c.run(ctx, c.argv[0], c.argv[1:]...)
	if err != nil {

		return "", fmt.Errorf("%w: %s failed: %w (%d bytes of stderr withheld; run the command by hand to see it)", errNoCredential, c.argv[0], err, len(stderr))
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(stdout)), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("%w: %s printed nothing", errNoCredential, c.argv[0])
	}
	c.value, c.expires = line, c.now().Add(c.ttl)
	return line, nil
}

func (c *commandCred) Invalidate(rejected string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.value == rejected {
		c.value, c.expires = "", time.Time{}
	}
}

type azCred struct{ az *token.AzCLI }

func (c azCred) Token(ctx context.Context) (string, error) {
	tok, err := c.az.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNoCredential, err)
	}
	return tok, nil
}
func (c azCred) Invalidate(rejected string) { c.az.Invalidate(rejected) }

func newCredential(a Auth, getenv func(string) string, run token.Runner) credential {
	switch a.Type {
	case authEnv:
		return envCred{name: a.Env, getenv: getenv}
	case authFile:
		return fileCred{path: a.File}
	case authCommand:
		return newCommandCred(a.Command, time.Duration(a.TTL), run)
	case authAzCLI:
		az := token.NewAzCLI(a.AzPath, a.Scope, a.Tenant)
		if run != nil {
			az.Run = run
		}
		return azCred{az: az}
	}
	return noCred{}
}

func setAuthHeader(h http.Header, header, tok string) {
	if tok == "" {
		return
	}
	switch header {
	case "x-api-key":
		h.Set("X-Api-Key", tok)
	case "api-key":
		h.Set("Api-Key", tok)
	default:
		h.Set("Authorization", "Bearer "+tok)
	}
}

const clientKeyPrefix = "aigw_"

func NewClientKey() (key, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	key = clientKeyPrefix + base64.RawURLEncoding.EncodeToString(b)
	return key, HashClientKey(key), nil
}

func HashClientKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func presentedKeys(r *http.Request) []string {
	var keys []string
	for _, v := range r.Header.Values("Authorization") {
		scheme, cred, ok := strings.Cut(strings.TrimSpace(v), " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			if cred = strings.TrimSpace(cred); cred != "" {
				keys = append(keys, cred)
			}
		}
	}
	for _, h := range []string{"X-Api-Key", "Api-Key"} {
		for _, v := range r.Header.Values(h) {
			if v = strings.TrimSpace(v); v != "" {
				keys = append(keys, v)
			}
		}
	}
	return keys
}

func (c *Config) authenticate(r *http.Request) *Client {
	var found *Client
	for _, key := range presentedKeys(r) {
		sum := sha256.Sum256([]byte(key))
		for i := range c.Clients {
			if subtle.ConstantTimeCompare(sum[:], c.Clients[i].hash) == 1 && found == nil {
				found = &c.Clients[i]
			}
		}
	}
	return found
}

type CredentialResult struct {
	Provider, Kind, Auth string
	Err                  error
}

func CheckCredentials(ctx context.Context, cfg *Config, getenv func(string) string) []CredentialResult {
	var out []CredentialResult
	for _, name := range sortedKeys(cfg.Providers) {
		p := cfg.Providers[name]
		_, err := newCredential(p.Auth, getenv, nil).Token(ctx)
		out = append(out, CredentialResult{Provider: name, Kind: p.Kind, Auth: p.Auth.Type, Err: err})
	}
	return out
}
