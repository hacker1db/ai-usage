package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Account struct {
	Label        string   `json:"label"`
	Provider     string   `json:"provider"`
	Subscription string   `json:"subscription"`
	Resources    []string `json:"resources"`
	Endpoints    []string `json:"endpoints"`
	Scope        string   `json:"github_scope"`
	GHAccount    string   `json:"github_account"`
	Mode         string   `json:"github_mode"`
}

func (a Account) ID() string {
	if a.Provider == "azure" {
		return "azure:" + a.Subscription + ":" + strings.Join(a.Resources, ",") + ":" + strings.Join(a.Endpoints, ",")
	}
	return "copilot:" + a.Scope + "/" + a.GHAccount + ":" + a.Mode
}

func (a Account) Name() string {
	if a.Label != "" {
		return a.Label
	}
	if a.Provider == "azure" {
		return "Azure Foundry"
	}
	if a.GHAccount == "" {
		return "Copilot"
	}
	return "Copilot · " + a.GHAccount
}

func (a Account) Fetch(ctx context.Context, run Run, month string) (Report, error) {
	switch a.Provider {
	case "azure":
		if a.Subscription == "" && len(a.Resources) == 0 {
			return AzureEndpoints(ctx, run, a.Endpoints, month)
		}
		return Azure(ctx, run, a.Subscription, a.Resources, month)
	case "copilot":
		return Copilot(ctx, run, a.Scope, a.GHAccount, a.Mode, month)
	}
	return Report{}, fmt.Errorf("provider must be azure or copilot")
}

type Config struct {
	Accounts []Account
	Explicit bool
	Refresh  time.Duration
}

const (
	AzureRefresh   = 4 * time.Hour
	CopilotRefresh = time.Hour
	ClosedRefresh  = 24 * time.Hour
	MinRefresh     = 30 * time.Minute
)

func (a Account) Refresh(month, current string, override time.Duration) time.Duration {
	if month != current {
		return ClosedRefresh
	}
	if override > 0 {
		return override
	}
	if a.Provider == "azure" {
		return AzureRefresh
	}
	return CopilotRefresh
}

func LoadConfig(path string) (Config, string) {
	cfg := Config{}
	if path == "" {
		return cfg, ""
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, ""
	}
	name := filepath.Base(path)
	if err != nil {
		return cfg, fmt.Sprintf("%s: %v", name, err)
	}
	var f struct {
		Billing []json.RawMessage `json:"billing"`
		Minutes *float64          `json:"billing_refresh_minutes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return cfg, fmt.Sprintf("%s: %v", name, err)
	}
	cfg.Explicit = f.Billing != nil
	if f.Minutes != nil {
		cfg.Refresh = max(time.Duration(*f.Minutes*float64(time.Minute)), MinRefresh)
	}
	var problems []string
	seen := map[string]bool{}
	for i, rawAcct := range f.Billing {
		var a Account
		if err := json.Unmarshal(rawAcct, &a); err != nil {
			problems = append(problems, fmt.Sprintf("billing[%d]: %v", i, err))
			continue
		}
		if a.Provider == "copilot" {
			if a.Scope == "" {
				a.Scope = "users"
			}
			if a.Mode == "" {
				a.Mode = "ai_credit"
			}
		}
		if err := a.validate(); err != nil {
			problems = append(problems, fmt.Sprintf("billing[%d]: %v", i, err))
			continue
		}
		if seen[a.ID()] {
			problems = append(problems, fmt.Sprintf("billing[%d]: duplicate account", i))
			continue
		}
		seen[a.ID()] = true
		cfg.Accounts = append(cfg.Accounts, a)
	}
	return cfg, strings.Join(problems, "; ")
}

func AutoAccounts(endpoints []string, copilotUsed bool, have func(bin string) bool) []Account {
	var out []Account
	if len(endpoints) > 0 && have("az") {
		out = append(out, Account{Provider: "azure", Endpoints: endpoints})
	}
	if copilotUsed && have("gh") {
		out = append(out, Account{Provider: "copilot", Scope: "users", Mode: "ai_credit"})
	}
	return out
}

func OnPath(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

func (a Account) validate() error {
	_, err := a.Fetch(context.Background(), func(context.Context, string, ...string) ([]byte, error) {
		return nil, errValid
	}, time.Now().UTC().Format("2006-01"))
	if errors.Is(err, errValid) || (a.Provider == "azure" && FixOf(err) != "") {
		return nil
	}
	return err
}

var errValid = fmt.Errorf("valid")

type Status struct {
	ID       string    `json:"id"`
	Label    string    `json:"label"`
	Provider string    `json:"provider"`
	Mode     string    `json:"mode,omitempty"`
	Month    string    `json:"month"`
	Report   *Report   `json:"report,omitempty"`
	Error    string    `json:"error,omitempty"`
	Retrying bool      `json:"retrying,omitempty"`
	Fix      string    `json:"fix,omitempty"`
	Checked  time.Time `json:"checked"`
	Next     time.Time `json:"next"`
	Failures int       `json:"failures,omitempty"`
}

func Months(now time.Time) []string {
	t := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	return []string{t.Format("2006-01"), t.AddDate(0, -1, 0).Format("2006-01")}
}
