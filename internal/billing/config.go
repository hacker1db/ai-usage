package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Account is one billing account to show on the dashboard, from dashboard.json's billing list.
type Account struct {
	Label        string   `json:"label"`
	Provider     string   `json:"provider"`       // azure or copilot
	Subscription string   `json:"subscription"`   // azure; "" = the Azure CLI's current subscription
	Resources    []string `json:"resources"`      // azure: full Foundry resource IDs; none = all in the subscription
	Endpoints    []string `json:"endpoints"`      // azure: Foundry endpoint subdomains; default: detected from Claude/Codex config
	Scope        string   `json:"github_scope"`   // copilot: users or organizations
	GHAccount    string   `json:"github_account"` // "" = the logged-in gh user (users scope only)
	Mode         string   `json:"github_mode"`    // copilot: ai_credit or premium_request
}

// ID is a stable key for the account (no secrets: subscription IDs and logins are identifiers).
func (a Account) ID() string {
	if a.Provider == "azure" {
		return "azure:" + a.Subscription + ":" + strings.Join(a.Resources, ",") + ":" + strings.Join(a.Endpoints, ",")
	}
	return "copilot:" + a.Scope + "/" + a.GHAccount + ":" + a.Mode
}

// Name is the label, or a readable default.
func (a Account) Name() string {
	if a.Label != "" {
		return a.Label
	}
	if a.Provider == "azure" {
		return "Azure Foundry"
	}
	if a.GHAccount == "" {
		return "Copilot" // the logged-in gh user, filled in once known
	}
	return "Copilot · " + a.GHAccount
}

// Fetch retrieves one account's report for month (YYYY-MM).
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

// Config is the billing part of dashboard.json.
type Config struct {
	Accounts []Account
	Explicit bool          // dashboard.json has a "billing" key; otherwise accounts are detected (AutoAccounts)
	Refresh  time.Duration // billing_refresh_minutes; 0 = each provider's default (Account.Refresh)
}

// Provider refresh cadence. Azure Cost Management refreshes cost data about every four hours and
// its query quota (12 per 10 s, 60 per minute, 600 per hour) is shared by the whole tenant, so
// querying more often returns the same numbers and spends quota others need. GitHub's billing
// usage allows 5,000 requests per hour, so hourly is far below its limits. A closed month barely
// changes, so it is refreshed once a day.
const (
	AzureRefresh   = 4 * time.Hour
	CopilotRefresh = time.Hour
	ClosedRefresh  = 24 * time.Hour
	MinRefresh     = 30 * time.Minute // floor for billing_refresh_minutes
)

// Refresh is how long a report for this account and month stays fresh. current is the month in
// progress; override is billing_refresh_minutes (0 = provider default).
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

// LoadConfig reads the optional "billing" list and "billing_refresh_minutes" from dashboard.json.
// A missing file or key is no accounts and no error; invalid entries are skipped with a warning.
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

// AutoAccounts are the billing accounts found on this machine without any configuration: Azure
// Foundry when the local Claude/Codex profiles use a Foundry endpoint and the Azure CLI is
// installed, and the logged-in gh user's Copilot AI-credit usage when the GitHub CLI is installed
// and Copilot has been used here. have reports whether a CLI is on PATH.
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

// OnPath reports whether bin is an executable on PATH.
func OnPath(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

// validate runs the provider's own argument checks without calling it.
func (a Account) validate() error {
	_, err := a.Fetch(context.Background(), func(context.Context, string, ...string) ([]byte, error) {
		return nil, errValid
	}, time.Now().UTC().Format("2006-01"))
	if err == errValid || (a.Provider == "azure" && FixOf(err) != "") { // endpoints are detected later
		return nil
	}
	return err
}

var errValid = fmt.Errorf("valid")

// Status is one account's latest result for one month, as shown on the dashboard.
type Status struct {
	ID       string    `json:"id"`
	Label    string    `json:"label"`
	Provider string    `json:"provider"`
	Mode     string    `json:"mode,omitempty"` // copilot billing system
	Month    string    `json:"month"`
	Report   *Report   `json:"report,omitempty"`   // last successful report
	Error    string    `json:"error,omitempty"`    // last attempt's failure (Report may still be an older success)
	Retrying bool      `json:"retrying,omitempty"` // Error is transient (rate limit); retried automatically
	Fix      string    `json:"fix,omitempty"`      // command or step that fixes Error, e.g. gh auth refresh -s user
	Checked  time.Time `json:"checked"`            // last attempt
	Next     time.Time `json:"next"`               // earliest next query: refresh cadence or rate-limit back-off
	Failures int       `json:"failures,omitempty"` // consecutive failed attempts (drives back-off)
}

// Months returns the current and previous UTC billing months, newest first.
func Months(now time.Time) []string {
	t := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	return []string{t.Format("2006-01"), t.AddDate(0, -1, 0).Format("2006-01")}
}
