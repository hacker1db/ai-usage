package gateway

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

// Defaults for optional settings.
const (
	DefaultListen                = "127.0.0.1:8789"
	DefaultMaxRequestBytes       = 32 << 20
	DefaultStreamIdleTimeout     = 2 * time.Minute
	DefaultResponseHeaderTimeout = 10 * time.Minute
	DefaultCommandTTL            = 5 * time.Minute
	DefaultAnthropicVersion      = "2023-06-01"
)

// Provider kinds: the wire format the upstream speaks.
const (
	kindOpenAI    = "openai"
	kindAnthropic = "anthropic"
)

// Auth types for upstream credentials.
const (
	authNone    = "none"
	authEnv     = "env"
	authFile    = "file"
	authCommand = "command"
	authAzCLI   = "azcli"
)

// Duration is a time.Duration written as a Go duration string ("90s") in JSON.
type Duration time.Duration

// UnmarshalJSON parses a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("duration must be a string such as \"90s\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil || v <= 0 {
		return fmt.Errorf("duration %q must be positive, such as \"90s\"", s)
	}
	*d = Duration(v)
	return nil
}

// Config is the gateway configuration file (JSON). It holds no secrets: client keys are stored as
// SHA-256 hashes and upstream credentials are references (an env var, a 0600 file, a command such
// as `gh auth token`, or the Azure CLI).
type Config struct {
	Listen                string               `json:"listen"`
	LogFile               string               `json:"log_file"`
	LogLevel              string               `json:"log_level"`
	MaxRequestBytes       int64                `json:"max_request_bytes"`
	StreamIdleTimeout     Duration             `json:"stream_idle_timeout"`
	ResponseHeaderTimeout Duration             `json:"response_header_timeout"`
	Clients               []Client             `json:"clients"`
	Providers             map[string]*Provider `json:"providers"`
	Models                map[string]Model     `json:"models"`
}

// Client is one local caller (Caido, a script, ...) with its own key and model allowlist.
type Client struct {
	Name      string   `json:"name"`
	KeySHA256 string   `json:"key_sha256"`
	Models    []string `json:"models,omitempty"` // empty means every configured model

	hash []byte
}

// Provider is one upstream account.
type Provider struct {
	Kind             string            `json:"kind"`
	BaseURL          string            `json:"base_url"`
	Auth             Auth              `json:"auth"`
	Headers          map[string]string `json:"headers,omitempty"`
	MaxTokensField   string            `json:"max_tokens_field,omitempty"`  // openai kind: max_completion_tokens (default) or max_tokens
	AnthropicVersion string            `json:"anthropic_version,omitempty"` // anthropic kind

	base *url.URL
}

// Auth says where the upstream credential comes from and which header carries it.
type Auth struct {
	Type    string   `json:"type"`
	Env     string   `json:"env,omitempty"`
	File    string   `json:"file,omitempty"`
	Command []string `json:"command,omitempty"`
	TTL     Duration `json:"ttl,omitempty"`
	Scope   string   `json:"scope,omitempty"`
	Tenant  string   `json:"tenant,omitempty"`
	AzPath  string   `json:"az_path,omitempty"`
	Header  string   `json:"header,omitempty"` // authorization (Bearer, default), x-api-key, api-key
}

// Model maps a client-facing model name to a provider and the upstream model or deployment.
type Model struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"` // defaults to the alias
}

// LoadConfig reads and validates a config file. The file must not be writable by group or others:
// a "command" credential runs what it names.
func LoadConfig(path string) (*Config, error) {
	path = expandHome(path)
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s must not be writable by group or others (chmod 600)", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(raw)
}

// ParseConfig decodes and validates config JSON. Unknown fields are rejected so typos surface.
func ParseConfig(raw []byte) (*Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if err := checkLoopbackListen(c.Listen); err != nil {
		errs = append(errs, err)
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.MaxRequestBytes <= 0 {
		c.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if c.StreamIdleTimeout == 0 {
		c.StreamIdleTimeout = Duration(DefaultStreamIdleTimeout)
	}
	if c.ResponseHeaderTimeout == 0 {
		c.ResponseHeaderTimeout = Duration(DefaultResponseHeaderTimeout)
	}

	if len(c.Clients) == 0 {
		add("clients: at least one client is required (generate a key with `ai-gateway keygen -name <client>`)")
	}
	seenName, seenHash := map[string]bool{}, map[string]bool{}
	for i := range c.Clients {
		cl := &c.Clients[i]
		if cl.Name == "" {
			add("clients[%d]: name is required", i)
		} else if seenName[cl.Name] {
			add("clients[%d]: duplicate name %q", i, cl.Name)
		}
		seenName[cl.Name] = true
		h, err := hex.DecodeString(strings.ToLower(cl.KeySHA256))
		if err != nil || len(h) != 32 {
			add("clients[%d] (%s): key_sha256 must be 64 hex characters", i, cl.Name)
		} else if seenHash[string(h)] {
			add("clients[%d] (%s): key_sha256 is shared with another client", i, cl.Name)
		}
		seenHash[string(h)] = true
		cl.hash = h
		for _, m := range cl.Models {
			if _, ok := c.Models[m]; !ok {
				add("clients[%d] (%s): model %q is not in models", i, cl.Name, m)
			}
		}
	}

	if len(c.Providers) == 0 {
		add("providers: at least one provider is required")
	}
	for _, name := range sortedKeys(c.Providers) {
		p := c.Providers[name]
		if p == nil {
			add("providers.%s: must be an object", name)
			continue
		}
		if err := p.validate(); err != nil {
			add("providers.%s: %w", name, err)
		}
	}

	if len(c.Models) == 0 {
		add("models: at least one model is required")
	}
	for _, alias := range sortedKeys(c.Models) {
		m := c.Models[alias]
		if strings.TrimSpace(alias) == "" {
			add("models: empty model name")
		}
		if _, ok := c.Providers[m.Provider]; !ok {
			add("models.%s: provider %q is not in providers", alias, m.Provider)
		}
		if m.Model == "" {
			m.Model = alias
			c.Models[alias] = m
		}
	}
	return errors.Join(errs...)
}

func (p *Provider) validate() error {
	switch p.Kind {
	case kindOpenAI:
		switch p.MaxTokensField {
		case "":
			p.MaxTokensField = "max_completion_tokens"
		case "max_completion_tokens", "max_tokens":
		default:
			return fmt.Errorf("max_tokens_field %q must be max_completion_tokens or max_tokens", p.MaxTokensField)
		}
	case kindAnthropic:
		if p.AnthropicVersion == "" {
			p.AnthropicVersion = DefaultAnthropicVersion
		}
	default:
		return fmt.Errorf("kind %q must be %q or %q", p.Kind, kindOpenAI, kindAnthropic)
	}

	u, err := url.Parse(strings.TrimRight(p.BaseURL, "/"))
	if err != nil || u.Host == "" {
		return fmt.Errorf("base_url %q must be an absolute URL", p.BaseURL)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("base_url %q must not include credentials, a query, or a fragment", p.BaseURL)
	}
	switch u.Scheme {
	case "https":
	case "http":
		// Plain HTTP only to another loopback service (foundry-entra-proxy, a local model server).
		if !isLoopbackHost(u.Hostname()) {
			return fmt.Errorf("base_url %q: http is only allowed for loopback hosts", p.BaseURL)
		}
	default:
		return fmt.Errorf("base_url %q must be https", p.BaseURL)
	}
	p.base = u

	for k := range p.Headers {
		switch strings.ToLower(k) {
		case "authorization", "x-api-key", "api-key", "host", "content-length", "cookie":
			return fmt.Errorf("headers: %q is managed by the gateway", k)
		}
	}
	return p.Auth.validate()
}

func (a *Auth) validate() error {
	switch a.Header {
	case "":
		a.Header = "authorization"
	case "authorization", "x-api-key", "api-key":
	default:
		return fmt.Errorf("auth.header %q must be authorization, x-api-key, or api-key", a.Header)
	}
	switch a.Type {
	case authNone:
	case authEnv:
		if a.Env == "" {
			return errors.New("auth.env is required for type env")
		}
	case authFile:
		if a.File == "" {
			return errors.New("auth.file is required for type file")
		}
	case authCommand:
		if len(a.Command) == 0 || a.Command[0] == "" {
			return errors.New("auth.command is required for type command (argv list, no shell)")
		}
		if a.TTL == 0 {
			a.TTL = Duration(DefaultCommandTTL)
		}
	case authAzCLI:
		if a.Scope == "" {
			a.Scope = "https://ai.azure.com/.default"
		}
		if a.AzPath == "" {
			a.AzPath = "az"
		}
	default:
		return fmt.Errorf("auth.type %q must be none, env, file, command, or azcli", a.Type)
	}
	return nil
}

func checkLoopbackListen(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen %q: %w", listen, err)
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("listen %q must bind a loopback address", listen)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// resolveAlias maps a requested model to a configured alias. An exact alias always wins; otherwise
// one LiteLLM-style provider prefix ("openai/gpt-6-luna", as Caido's Shift custom models send) is
// dropped. Unknown names are returned unchanged so the caller reports them.
func (c *Config) resolveAlias(model string) string {
	if _, ok := c.Models[model]; ok {
		return model
	}
	for _, prefix := range []string{"openai/", "anthropic/"} {
		if rest, ok := strings.CutPrefix(model, prefix); ok {
			if _, ok := c.Models[rest]; ok {
				return rest
			}
		}
	}
	return model
}

// modelsFor returns the aliases a client may use, sorted.
func (c *Config) modelsFor(cl *Client) []string {
	if len(cl.Models) > 0 {
		out := slices.Clone(cl.Models)
		sort.Strings(out)
		return out
	}
	return sortedKeys(c.Models)
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return home + "/" + rest
		}
	}
	return p
}
