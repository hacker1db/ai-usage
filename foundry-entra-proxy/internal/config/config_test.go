package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var goodKey = strings.Repeat("k", 32)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func baseEnv() map[string]string {
	return map[string]string{
		"FEP_UPSTREAM_BASE":       "https://res.services.ai.azure.com/",
		"FEP_CLIENT_KEY":          goodKey,
		"FEP_ALLOWED_DEPLOYMENTS": "gpt-5.5, gpt-5-mini,,gpt-5.5",
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != DefaultListen || cfg.TokenScope != DefaultTokenScope || cfg.StreamIdleTimeout != DefaultStreamIdleTimeout {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.UpstreamBase.String() != "https://res.services.ai.azure.com" {
		t.Fatalf("upstream = %s", cfg.UpstreamBase)
	}
	if !slices.Equal(cfg.AllowedDeployments, []string{"gpt-5.5", "gpt-5-mini"}) {
		t.Fatalf("deployments = %v", cfg.AllowedDeployments)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name    string
		set     map[string]string
		wantErr string
	}{
		{name: "non-loopback listen", set: map[string]string{"FEP_LISTEN": "0.0.0.0:8787"}, wantErr: "loopback"},
		{name: "lan listen", set: map[string]string{"FEP_LISTEN": "192.168.1.5:8787"}, wantErr: "loopback"},
		{name: "http upstream", set: map[string]string{"FEP_UPSTREAM_BASE": "http://res.services.ai.azure.com"}, wantErr: "https"},
		{name: "upstream with path", set: map[string]string{"FEP_UPSTREAM_BASE": "https://res.services.ai.azure.com/openai/v1"}, wantErr: "path"},
		{name: "missing upstream", set: map[string]string{"FEP_UPSTREAM_BASE": ""}, wantErr: "FEP_UPSTREAM_BASE is required"},
		{name: "missing key", set: map[string]string{"FEP_CLIENT_KEY": ""}, wantErr: "client key is required"},
		{name: "short key", set: map[string]string{"FEP_CLIENT_KEY": "short"}, wantErr: "at least 32"},
		{name: "no deployments", set: map[string]string{"FEP_ALLOWED_DEPLOYMENTS": " , "}, wantErr: "FEP_ALLOWED_DEPLOYMENTS"},
		{name: "bad duration", set: map[string]string{"FEP_STREAM_IDLE_TIMEOUT": "-1s"}, wantErr: "positive duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := baseEnv()
			for k, v := range tt.set {
				e[k] = v
			}
			_, err := Load(env(e))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), goodKey) {
				t.Fatalf("error leaks client key: %v", err)
			}
		})
	}
}

func TestLoadAcceptsLoopbackVariants(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:9000", "[::1]:9000", "localhost:9000"} {
		e := baseEnv()
		e["FEP_LISTEN"] = listen
		if _, err := Load(env(e)); err != nil {
			t.Errorf("%s: %v", listen, err)
		}
	}
}

func TestClientKeyFile(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		mode    os.FileMode
		content string
		wantErr string
	}{
		{name: "private file", mode: 0o600, content: goodKey + "\n"},
		{name: "group readable", mode: 0o640, content: goodKey, wantErr: "chmod 600"},
		{name: "empty file", mode: 0o600, content: "", wantErr: "client key is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "-"))
			if err := os.WriteFile(path, []byte(tt.content), tt.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatal(err)
			}
			e := baseEnv()
			e["FEP_CLIENT_KEY"] = "ignored-when-file-is-set-ignored-xx"
			e["FEP_CLIENT_KEY_FILE"] = path
			cfg, err := Load(env(e))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ClientKey != goodKey {
				t.Fatalf("key not trimmed/loaded from file")
			}
		})
	}
}

func TestDurationOverrides(t *testing.T) {
	e := baseEnv()
	e["FEP_STREAM_IDLE_TIMEOUT"] = "45s"
	e["FEP_RESPONSE_HEADER_TIMEOUT"] = "3m"
	cfg, err := Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StreamIdleTimeout != 45*time.Second || cfg.ResponseHeaderWait != 3*time.Minute {
		t.Fatalf("durations = %v %v", cfg.StreamIdleTimeout, cfg.ResponseHeaderWait)
	}
}
