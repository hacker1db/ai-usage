// Package billing retrieves provider-reported costs, separately from token estimates.
package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

type Run func(context.Context, string, ...string) ([]byte, error)

// Command uses existing CLI credentials without capturing tokens or logging command output. On
// failure it returns a *CLIError carrying the CLI's own diagnostic (stderr), which names a missing
// scope, role, or expired login.
func Command(ctx context.Context, name string, args ...string) ([]byte, error) {
	b, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		e := &CLIError{Name: name, Err: err}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			e.Stderr = string(ee.Stderr)
			if len(e.Stderr) > 4000 {
				e.Stderr = e.Stderr[:4000]
			}
		}
		return nil, e
	}
	return b, nil
}

type Item struct {
	Date     string      `json:"date,omitempty"`
	Resource string      `json:"resource,omitempty"`
	Model    string      `json:"model,omitempty"`
	SKU      string      `json:"sku,omitempty"`
	Currency string      `json:"currency"`
	Net      json.Number `json:"net_amount"`
}

type Report struct {
	Provider  string    `json:"provider"`
	Account   string    `json:"account"`
	Month     string    `json:"month"`
	Retrieved time.Time `json:"retrieved"`
	Basis     string    `json:"basis"`
	Items     []Item    `json:"items"`
}

var accountRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
var subscriptionRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func Month(s string) (time.Time, error) { return time.Parse("2006-01", s) }

// Copilot queries exactly one billing system; AI credits and premium requests must not be summed.
func Copilot(ctx context.Context, run Run, kind, account, mode, month string) (Report, error) {
	r := Report{Provider: "github-copilot", Account: kind + "/" + account, Month: month, Retrieved: time.Now().UTC(), Basis: "Provider-reported net metered usage, USD; excludes subscription seats and taxes; not a final invoice", Items: []Item{}}
	t, err := Month(month)
	if err != nil {
		return r, err
	}
	if (kind != "users" && kind != "organizations") || (account != "" && !accountRE.MatchString(account)) {
		return r, fmt.Errorf("GitHub scope must be users or organizations, with a valid account name")
	}
	if mode != "ai_credit" && mode != "premium_request" {
		return r, fmt.Errorf("GitHub mode must be ai_credit or premium_request")
	}
	if kind == "organizations" && account == "" {
		return r, fmt.Errorf("organizations scope needs github_account set to the organization")
	}
	// Use whoever gh is logged in as, and say exactly what to run when the token can't do this.
	login, scopes, err := ghIdentity(ctx, run)
	if err != nil {
		return r, err
	}
	if kind == "users" {
		if account == "" {
			account = login
		} else if !strings.EqualFold(account, login) {
			return r, &Error{Msg: "gh is logged in as " + login + ", but personal billing only covers the logged-in user (" + account + ")",
				Fix: "gh auth switch -h github.com -u " + account + "   (or remove github_account to use " + login + ")"}
		}
		if scopes != nil && !slices.Contains(scopes, "user") {
			return r, &Error{Msg: "the gh token for " + login + " is missing the \"user\" scope that personal billing needs",
				Fix: "gh auth refresh -h github.com -s user"}
		}
	}
	r.Account = kind + "/" + account
	endpoint := fmt.Sprintf("/%s/%s/settings/billing/%s/usage?year=%d&month=%d", kind, account, mode, t.Year(), t.Month())
	b, err := run(ctx, "gh", "api", "--hostname", "github.com", "--method", "GET", "-H", "Accept: application/vnd.github+json", "-H", "X-GitHub-Api-Version: 2026-03-10", endpoint)
	if err != nil {
		return r, explainGH(err, kind, account)
	}
	var raw struct {
		Items *[]struct {
			Product, SKU, Model string
			Net                 *json.Number `json:"netAmount"`
		} `json:"usageItems"`
	}
	if err = json.Unmarshal(b, &raw); err != nil {
		return r, err
	}
	if raw.Items == nil {
		return r, fmt.Errorf("GitHub response missing usageItems")
	}
	for _, x := range *raw.Items {
		if !strings.Contains(strings.ToLower(x.Product), "copilot") {
			continue
		}
		if x.Net == nil {
			return r, fmt.Errorf("Copilot item missing netAmount")
		}
		if _, err := x.Net.Float64(); err != nil {
			return r, err
		}
		r.Items = append(r.Items, Item{Model: x.Model, SKU: x.SKU, Currency: "USD", Net: *x.Net})
	}
	return r, nil
}

// Azure restricts costs to explicit Foundry resource IDs in one subscription.
func Azure(ctx context.Context, run Run, subscription string, resources []string, month string) (Report, error) {
	r := Report{Provider: "azure-foundry", Account: subscription, Month: month, Retrieved: time.Now().UTC(), Basis: "Azure ActualCost / PreTaxCost for selected resources; excludes taxes; subject to billing adjustments", Items: []Item{}}
	t, err := Month(month)
	if err != nil {
		return r, err
	}
	if subscription != "" && !subscriptionRE.MatchString(subscription) {
		return r, fmt.Errorf("Azure subscription must be a UUID")
	}
	if subscription == "" && len(resources) > 0 {
		return r, fmt.Errorf("Azure resources need their subscription set too")
	}
	checkResources := func() error {
		prefix := "/subscriptions/" + subscription + "/resourceGroups/"
		for _, id := range resources {
			if !strings.HasPrefix(strings.ToLower(id), strings.ToLower(prefix)) || strings.ContainsAny(id, "?#\r\n") || !strings.Contains(strings.ToLower(id), "/providers/microsoft.cognitiveservices/accounts/") {
				return fmt.Errorf("resource must be a Microsoft.CognitiveServices account in the selected subscription")
			}
		}
		return nil
	}
	if err := checkResources(); err != nil {
		return r, err
	}
	// Default to the Azure CLI's current subscription and every Foundry account in it.
	if subscription == "" {
		if subscription, err = azSubscription(ctx, run); err != nil {
			return r, err
		}
	}
	if len(resources) == 0 {
		if resources, err = azResources(ctx, run, subscription); err != nil {
			return r, err
		}
		if err := checkResources(); err != nil {
			return r, err
		}
	}
	r.Account = subscription
	body, _ := json.Marshal(map[string]any{"type": "ActualCost", "timeframe": "Custom", "timePeriod": map[string]string{"from": t.Format(time.RFC3339), "to": t.AddDate(0, 1, 0).Add(-time.Second).Format(time.RFC3339)}, "dataset": map[string]any{"granularity": "Daily", "aggregation": map[string]any{"totalCost": map[string]string{"name": "PreTaxCost", "function": "Sum"}}, "grouping": []map[string]string{{"type": "Dimension", "name": "ResourceId"}}, "filter": map[string]any{"dimensions": map[string]any{"name": "ResourceId", "operator": "In", "values": resources}}}})
	endpoint := "https://management.azure.com/subscriptions/" + subscription + "/providers/Microsoft.CostManagement/query?api-version=2026-06-01"
	seen := map[string]bool{}
	for endpoint != "" {
		u, e := url.Parse(endpoint)
		if e != nil || u.Scheme != "https" || u.Host != "management.azure.com" || u.User != nil || !strings.EqualFold(u.Path, "/subscriptions/"+subscription+"/providers/Microsoft.CostManagement/query") {
			return r, fmt.Errorf("invalid Azure continuation URL")
		}
		if seen[endpoint] || len(seen) >= 100 {
			return r, fmt.Errorf("Azure pagination loop or page limit")
		}
		seen[endpoint] = true
		b, e := run(ctx, "az", "rest", "--method", "post", "--url", endpoint, "--body", string(body), "--subscription", subscription, "--output", "json", "--only-show-errors")
		if e != nil {
			return r, explainAz(e, subscription)
		}
		if len(strings.TrimSpace(string(b))) == 0 {
			break
		} // HTTP 204
		items, next, e := azurePage(b)
		if e != nil {
			return r, e
		}
		r.Items = append(r.Items, items...)
		endpoint = next
	}
	return r, nil
}

func azurePage(b []byte) ([]Item, string, error) {
	var raw struct {
		Properties *struct {
			Columns []struct{ Name string }
			Rows    [][]json.RawMessage
			Next    string `json:"nextLink"`
		}
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, "", err
	}
	if raw.Properties == nil {
		return nil, "", fmt.Errorf("Azure response missing properties")
	}
	p := raw.Properties
	cols := map[string]int{}
	for i, c := range p.Columns {
		cols[c.Name] = i
	}
	for _, name := range []string{"PreTaxCost", "Currency", "UsageDate", "ResourceId"} {
		if _, ok := cols[name]; !ok {
			return nil, "", fmt.Errorf("Azure response missing %s column", name)
		}
	}
	items := []Item{}
	for _, row := range p.Rows {
		if len(row) != len(p.Columns) {
			return nil, "", fmt.Errorf("Azure row length mismatch")
		}
		var item Item
		var date json.Number
		for name, dst := range map[string]any{"PreTaxCost": &item.Net, "Currency": &item.Currency, "ResourceId": &item.Resource, "UsageDate": &date} {
			if string(row[cols[name]]) == "null" {
				return nil, "", fmt.Errorf("Azure null %s", name)
			}
			if err := json.Unmarshal(row[cols[name]], dst); err != nil {
				return nil, "", err
			}
		}
		if _, err := item.Net.Float64(); err != nil {
			return nil, "", err
		}
		if item.Currency == "" {
			return nil, "", fmt.Errorf("Azure empty currency")
		}
		d, err := time.Parse("20060102", date.String())
		if err != nil {
			return nil, "", err
		}
		item.Date = d.Format("2006-01-02")
		items = append(items, item)
	}
	return items, p.Next, nil
}
