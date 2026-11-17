package gateway

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validConfig() string {
	return `{
		"clients": [{"name": "caido", "key_sha256": "` + HashClientKey("k") + `"}],
		"providers": {"p": {"kind": "openai", "base_url": "https://example.test/v1", "auth": {"type": "none"}}},
		"models": {"m": {"provider": "p"}}
	}`
}

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte(validConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != DefaultListen || cfg.MaxRequestBytes != DefaultMaxRequestBytes || cfg.LogLevel != "info" {
		t.Fatalf("defaults: %+v", cfg)
	}
	if time.Duration(cfg.StreamIdleTimeout) != DefaultStreamIdleTimeout || time.Duration(cfg.ResponseHeaderTimeout) != DefaultResponseHeaderTimeout {
		t.Fatal("timeouts not defaulted")
	}
	if cfg.Models["m"].Model != "m" {
		t.Fatal("upstream model should default to the alias")
	}
	p := cfg.Providers["p"]
	if p.MaxTokensField != "max_completion_tokens" || p.Auth.Header != "authorization" {
		t.Fatalf("provider defaults: %+v", p)
	}
}

func TestParseConfigErrors(t *testing.T) {
	hash := HashClientKey("k")
	cases := map[string]struct{ json, want string }{
		"unknown field":      {`{"clientz": []}`, "unknown field"},
		"public listen":      {strings.Replace(validConfig(), `"clients"`, `"listen": "0.0.0.0:8789", "clients"`, 1), "loopback"},
		"no clients":         {`{"providers": {}, "models": {}}`, "at least one client"},
		"bad hash":           {strings.Replace(validConfig(), hash, "abc", 1), "64 hex"},
		"bad kind":           {strings.Replace(validConfig(), `"kind": "openai"`, `"kind": "gemini"`, 1), "kind"},
		"http remote":        {strings.Replace(validConfig(), "https://example.test", "http://example.test", 1), "loopback hosts"},
		"url with query":     {strings.Replace(validConfig(), "/v1", "/v1?key=x", 1), "query"},
		"url with userinfo":  {strings.Replace(validConfig(), "https://", "https://u:p@", 1), "credentials"},
		"bad auth type":      {strings.Replace(validConfig(), `"type": "none"`, `"type": "oauth"`, 1), "auth.type"},
		"env without name":   {strings.Replace(validConfig(), `"type": "none"`, `"type": "env"`, 1), "auth.env"},
		"command empty":      {strings.Replace(validConfig(), `"type": "none"`, `"type": "command"`, 1), "auth.command"},
		"bad header":         {strings.Replace(validConfig(), `"type": "none"`, `"type": "none", "header": "cookie"`, 1), "auth.header"},
		"managed header":     {strings.Replace(validConfig(), `"auth"`, `"headers": {"Authorization": "x"}, "auth"`, 1), "managed"},
		"unknown provider":   {strings.Replace(validConfig(), `{"provider": "p"}`, `{"provider": "q"}`, 1), "not in providers"},
		"client bad model":   {strings.Replace(validConfig(), `"key_sha256"`, `"models": ["nope"], "key_sha256"`, 1), "not in models"},
		"bad duration":       {strings.Replace(validConfig(), `"clients"`, `"stream_idle_timeout": "-1s", "clients"`, 1), "positive"},
		"bad max tokens fld": {strings.Replace(validConfig(), `"kind": "openai"`, `"kind": "openai", "max_tokens_field": "n"`, 1), "max_tokens_field"},
		"duplicate client": {`{
			"clients": [{"name": "a", "key_sha256": "` + hash + `"}, {"name": "a", "key_sha256": "` + HashClientKey("other") + `"}],
			"providers": {"p": {"kind": "openai", "base_url": "https://x.test", "auth": {"type": "none"}}}, "models": {"m": {"provider": "p"}}}`, "duplicate name"},
		"shared key": {`{
			"clients": [{"name": "a", "key_sha256": "` + hash + `"}, {"name": "b", "key_sha256": "` + hash + `"}],
			"providers": {"p": {"kind": "openai", "base_url": "https://x.test", "auth": {"type": "none"}}}, "models": {"m": {"provider": "p"}}}`, "shared"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.json))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestParseConfigAllowsLoopbackHTTP(t *testing.T) {
	for _, host := range []string{"127.0.0.1:8787", "localhost:8787", "[::1]:8787"} {
		raw := strings.Replace(validConfig(), "https://example.test", "http://"+host, 1)
		if _, err := ParseConfig([]byte(raw)); err != nil {
			t.Fatalf("%s: %v", host, err)
		}
	}
}

func TestLoadConfigRefusesWritableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(validConfig()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("0600 config: %v", err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("world-writable config accepted: %v", err)
	}
}

func TestNewClientKey(t *testing.T) {
	k1, h1, err := NewClientKey()
	if err != nil {
		t.Fatal(err)
	}
	k2, _, _ := NewClientKey()
	if !strings.HasPrefix(k1, clientKeyPrefix) || len(k1) < 40 || k1 == k2 {
		t.Fatalf("keys %q %q", k1, k2)
	}
	if h1 != HashClientKey(k1) || len(h1) != 64 {
		t.Fatalf("hash %q", h1)
	}
}

func TestAuthenticateMatchesAnyPresentedKey(t *testing.T) {
	raw := `{
		"clients": [{"name": "a", "key_sha256": "` + HashClientKey("key-a") + `"}, {"name": "b", "key_sha256": "` + HashClientKey("key-b") + `"}],
		"providers": {"p": {"kind": "openai", "base_url": "https://x.test", "auth": {"type": "none"}}}, "models": {"m": {"provider": "p"}}}`
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	r.Header.Set("X-Api-Key", "key-b")
	if c := cfg.authenticate(r); c == nil || c.Name != "b" {
		t.Fatalf("got %+v", c)
	}
	r.Header.Set("X-Api-Key", "key-c")
	if c := cfg.authenticate(r); c != nil {
		t.Fatalf("unknown key matched %s", c.Name)
	}
}

func TestEnvAndFileCredentials(t *testing.T) {
	ctx := context.Background()
	env := envCred{name: "TOK", getenv: func(string) string { return "  abc \n" }}
	if v, err := env.Token(ctx); err != nil || v != "abc" {
		t.Fatalf("env: %q %v", v, err)
	}
	if _, err := (envCred{name: "TOK", getenv: func(string) string { return "" }}).Token(ctx); !errors.Is(err, errNoCredential) {
		t.Fatalf("empty env: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("sk-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := (fileCred{path: path}).Token(ctx); err != nil || v != "sk-file" {
		t.Fatalf("file: %q %v", v, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (fileCred{path: path}).Token(ctx); !errors.Is(err, errNoCredential) || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("readable file accepted: %v", err)
	}
	if _, err := (fileCred{path: filepath.Join(dir, "missing")}).Token(ctx); !errors.Is(err, errNoCredential) {
		t.Fatalf("missing file: %v", err)
	}
}

func TestCommandCredentialCachesAndInvalidates(t *testing.T) {
	runs := 0
	now := time.Unix(1000, 0)
	run := func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
		runs++
		if name != "gh" || strings.Join(args, " ") != "auth token" {
			t.Fatalf("argv %s %v", name, args)
		}
		return []byte("gho_token" + string(rune('0'+runs)) + "\nignored\n"), nil, nil
	}
	c := newCommandCred([]string{"gh", "auth", "token"}, time.Minute, run)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	v1, _ := c.Token(ctx)
	v2, _ := c.Token(ctx)
	if v1 != "gho_token1" || v2 != v1 || runs != 1 {
		t.Fatalf("cache: %q %q runs=%d", v1, v2, runs)
	}
	c.Invalidate("someone-else")
	if v, _ := c.Token(ctx); v != v1 || runs != 1 {
		t.Fatal("invalidating a different token must keep the cache")
	}
	c.Invalidate(v1)
	if v, _ := c.Token(ctx); v != "gho_token2" || runs != 2 {
		t.Fatalf("after invalidate: %q runs=%d", v, runs)
	}
	now = now.Add(2 * time.Minute)
	if v, _ := c.Token(ctx); v != "gho_token3" || runs != 3 {
		t.Fatalf("after ttl: %q runs=%d", v, runs)
	}
}

func TestCommandCredentialFailures(t *testing.T) {
	fail := func(context.Context, string, ...string) ([]byte, []byte, error) {
		return nil, []byte("not logged in"), errors.New("exit status 1")
	}
	_, err := newCommandCred([]string{"gh"}, time.Minute, fail).Token(context.Background())
	if !errors.Is(err, errNoCredential) || strings.Contains(err.Error(), "not logged in") || !strings.Contains(err.Error(), "withheld") {
		t.Fatalf("got %v", err)
	}
	empty := func(context.Context, string, ...string) ([]byte, []byte, error) { return []byte("\n"), nil, nil }
	if _, err := newCommandCred([]string{"gh"}, time.Minute, empty).Token(context.Background()); !errors.Is(err, errNoCredential) {
		t.Fatalf("empty output: %v", err)
	}
}

func TestAzCLICredentialUsesRunner(t *testing.T) {
	run := func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
		if name != "/opt/az" || !strings.Contains(strings.Join(args, " "), "--scope https://ai.azure.com/.default") {
			t.Fatalf("argv %s %v", name, args)
		}
		return []byte(`{"accessToken":"eyJ.entra","expires_on":` + "9999999999" + `}`), nil, nil
	}
	c := newCredential(Auth{Type: authAzCLI, AzPath: "/opt/az", Scope: "https://ai.azure.com/.default"}, nil, run)
	if v, err := c.Token(context.Background()); err != nil || v != "eyJ.entra" {
		t.Fatalf("%q %v", v, err)
	}
	login := func(context.Context, string, ...string) ([]byte, []byte, error) {
		return nil, []byte("Please run 'az login' to setup account."), errors.New("exit 1")
	}
	c = newCredential(Auth{Type: authAzCLI, AzPath: "az", Scope: "s"}, nil, login)
	if _, err := c.Token(context.Background()); !errors.Is(err, errNoCredential) {
		t.Fatalf("login error: %v", err)
	}
}

func TestSetAuthHeader(t *testing.T) {
	for header, want := range map[string][2]string{
		"authorization": {"Authorization", "Bearer t"},
		"x-api-key":     {"X-Api-Key", "t"},
		"api-key":       {"Api-Key", "t"},
	} {
		h := make(map[string][]string)
		setAuthHeader(h, header, "t")
		if got := h[want[0]]; len(got) != 1 || got[0] != want[1] {
			t.Fatalf("%s: %v", header, h)
		}
	}
	h := make(map[string][]string)
	setAuthHeader(h, "authorization", "")
	if len(h) != 0 {
		t.Fatal("empty token must not set a header")
	}
}

func TestCheckCredentials(t *testing.T) {
	raw := `{
		"clients": [{"name": "a", "key_sha256": "` + HashClientKey("k") + `"}],
		"providers": {
			"ok": {"kind": "openai", "base_url": "https://x.test", "auth": {"type": "env", "env": "SET"}},
			"bad": {"kind": "anthropic", "base_url": "https://y.test", "auth": {"type": "env", "env": "UNSET"}}
		},
		"models": {"m": {"provider": "ok"}}}`
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	res := CheckCredentials(context.Background(), cfg, func(k string) string {
		if k == "SET" {
			return "v"
		}
		return ""
	})
	if len(res) != 2 || res[0].Provider != "bad" || res[0].Err == nil || res[1].Provider != "ok" || res[1].Err != nil {
		t.Fatalf("%+v", res)
	}
}

func TestResolveAlias(t *testing.T) {
	raw := strings.Replace(validConfig(), `"models": {"m": {"provider": "p"}}`, `"models": {"m": {"provider": "p"}, "openai/gpt-x": {"provider": "p"}}`, 1)
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		"m": "m", "openai/m": "m", "anthropic/m": "m",
		"openai/gpt-x": "openai/gpt-x", // an exact alias that has a slash wins
		"google/m":     "google/m", "openai/nope": "openai/nope",
	} {
		if got := cfg.resolveAlias(in); got != want {
			t.Errorf("resolveAlias(%q) = %q, want %q", in, got, want)
		}
	}
}

// The committed example must stay a valid config once real key hashes are filled in.
func TestExampleConfigParses(t *testing.T) {
	raw, err := os.ReadFile("../../ai-gateway.example.json")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	s = strings.Replace(s, "replace-with-the-hash-from-ai-gateway-keygen-name-caido-0000000", HashClientKey("a"), 1)
	s = strings.Replace(s, "replace-with-the-hash-from-ai-gateway-keygen-name-scripts-000000", HashClientKey("b"), 1)
	cfg, err := ParseConfig([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 6 || cfg.Models["gh-gpt"].Model != "openai/gpt-6.1-sol" {
		t.Fatalf("unexpected example: %d providers", len(cfg.Providers))
	}
}
