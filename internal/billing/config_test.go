package billing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dashboard.json")
	body := `{"billing_refresh_minutes": 1, "billing": [
		{"provider":"azure","label":"Foundry","subscription":"` + sub + `","resources":["` + resource + `"]},
		{"provider":"copilot","github_account":"me"},
		{"provider":"copilot","github_account":"me"},
		{"provider":"azure","subscription":"nope","resources":["x"]},
		{"provider":"openai"}]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, warn := LoadConfig(p)
	if len(cfg.Accounts) != 2 || cfg.Refresh != MinRefresh {
		t.Fatalf("%+v", cfg)
	}
	if c := cfg.Accounts[1]; c.Scope != "users" || c.Mode != "ai_credit" || c.Name() != "Copilot · me" {
		t.Fatalf("defaults: %+v", c)
	}
	for _, want := range []string{"billing[2]: duplicate", "billing[3]", "billing[4]"} {
		if !strings.Contains(warn, want) {
			t.Fatalf("warning %q lacks %q", warn, want)
		}
	}
	if cfg, warn := LoadConfig(filepath.Join(t.TempDir(), "missing.json")); len(cfg.Accounts) != 0 || warn != "" || cfg.Refresh != 0 {
		t.Fatalf("missing file: %+v %q", cfg, warn)
	}
}

func TestMonths(t *testing.T) {
	got := Months(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC))
	if got[0] != "2026-01" || got[1] != "2025-12" {
		t.Fatal(got)
	}
}

func TestAutoAccounts(t *testing.T) {
	all := func(string) bool { return true }
	got := AutoAccounts([]string{"myfoundry"}, true, all)
	if len(got) != 2 || got[0].Provider != "azure" || got[0].Endpoints[0] != "myfoundry" ||
		got[1].Provider != "copilot" || got[1].Scope != "users" || got[1].Mode != "ai_credit" {
		t.Fatalf("%+v", got)
	}
	// No Foundry endpoint, no az, or no Copilot use: that account is not added.
	if got := AutoAccounts(nil, false, all); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	if got := AutoAccounts([]string{"x"}, true, func(b string) bool { return b == "gh" }); len(got) != 1 || got[0].Provider != "copilot" {
		t.Fatalf("%+v", got)
	}
}

func TestExplicitBillingListDisablesDetection(t *testing.T) {
	dir := t.TempDir()
	for body, want := range map[string]bool{`{}`: false, `{"billing":[]}`: true, `{"billing":[{"provider":"copilot"}]}`: true} {
		p := filepath.Join(dir, "d.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if cfg, _ := LoadConfig(p); cfg.Explicit != want {
			t.Errorf("%s: Explicit = %v", body, cfg.Explicit)
		}
	}
	if cfg, _ := LoadConfig(filepath.Join(dir, "missing.json")); cfg.Explicit {
		t.Error("missing file should detect accounts")
	}
}
