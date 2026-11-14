package billing

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CLIError is a failed az or gh invocation with the CLI's own diagnostic.
type CLIError struct {
	Name   string
	Stderr string
	Err    error
}

func (e *CLIError) Error() string {
	msg := oneLine(e.Stderr, 300)
	if msg == "" {
		msg = e.Err.Error()
	}
	return e.Name + " billing request failed: " + msg
}

func (e *CLIError) Unwrap() error { return e.Err }

// Error is a billing failure the user can fix, with the exact command (or step) to fix it.
type Error struct {
	Msg        string
	Fix        string
	Retry      bool          // transient (rate limit): nothing to fix, the next attempt should work
	RetryAfter time.Duration // the provider's requested wait, when it gave one
	Err        error
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Err }

// Transient reports whether err is a temporary provider condition such as rate limiting.
func Transient(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Retry
}

// RetryAfterOf returns the wait a rate-limited provider asked for, or 0 when it didn't say.
func RetryAfterOf(err error) time.Duration {
	var e *Error
	if errors.As(err, &e) {
		return e.RetryAfter
	}
	return 0
}

// retryAfterRE finds a provider's requested wait in CLI output: Azure's
// x-ms-ratelimit-…-retry-after / Retry-After headers (az --debug style) or "retry after N seconds".
var retryAfterRE = regexp.MustCompile(`(?i)retry[-_ ]after['":\s=]*(\d{1,6})`)

// ghResetRE finds GitHub's x-ratelimit-reset (UTC epoch seconds).
var ghResetRE = regexp.MustCompile(`(?i)x-ratelimit-reset['":\s=]*(\d{9,11})`)

func retryAfter(out string, now time.Time) time.Duration {
	if m := retryAfterRE.FindStringSubmatch(out); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	if m := ghResetRE.FindStringSubmatch(out); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			if d := time.Unix(n, 0).Sub(now); d > 0 {
				return d
			}
		}
	}
	return 0
}

// FixOf returns the suggested fix for err, if it has one.
func FixOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Fix
	}
	return ""
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n-1]) + "…"
	}
	return s
}

// ghScopeRE matches gh's own "needs the "user" scope" diagnostic.
var ghScopeRE = regexp.MustCompile(`needs the "([a-z:_]+)" scope`)

// ghIdentity returns the login gh is authenticated as and, for classic/OAuth tokens, the token's
// scopes (nil for fine-grained tokens and GitHub App tokens, which don't report scopes).
func ghIdentity(ctx context.Context, run Run) (login string, scopes []string, err error) {
	b, err := run(ctx, "gh", "api", "--hostname", "github.com", "--include", "/user")
	if err != nil {
		return "", nil, explainGH(err, "", "")
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var body strings.Builder
	inBody := false
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if inBody {
			body.WriteString(line)
			continue
		}
		if line == "" {
			inBody = true
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "X-OAuth-Scopes") {
			scopes = []string{}
			for _, x := range strings.Split(v, ",") {
				if x = strings.TrimSpace(x); x != "" {
					scopes = append(scopes, x)
				}
			}
		}
	}
	var u struct{ Login string }
	if json.Unmarshal([]byte(body.String()), &u) != nil || !accountRE.MatchString(u.Login) {
		return "", nil, &Error{Msg: "could not read the logged-in GitHub user from gh", Fix: "gh auth login -h github.com"}
	}
	return u.Login, scopes, nil
}

// explainGH turns a gh failure into an actionable error.
func explainGH(err error, kind, account string) error {
	var ce *CLIError
	if !errors.As(err, &ce) {
		return err
	}
	out := ce.Stderr
	switch {
	case strings.Contains(strings.ToLower(out), "rate limit") || strings.Contains(out, "HTTP 429"):
		return &Error{Msg: "GitHub is rate-limiting requests; retrying automatically", Retry: true,
			RetryAfter: retryAfter(out, time.Now()), Err: err}
	case ghScopeRE.MatchString(out):
		scope := ghScopeRE.FindStringSubmatch(out)[1]
		return &Error{Msg: fmt.Sprintf("the gh token is missing the %q scope", scope),
			Fix: "gh auth refresh -h github.com -s " + scope, Err: err}
	case strings.Contains(out, "gh auth login") || strings.Contains(out, "not logged") || strings.Contains(out, "HTTP 401"):
		return &Error{Msg: "gh is not logged in to github.com (or its token expired)", Fix: "gh auth login -h github.com", Err: err}
	case kind == "organizations" && (strings.Contains(out, "HTTP 403") || strings.Contains(out, "HTTP 404")):
		return &Error{Msg: "GitHub denied the billing report for organization " + account,
			Fix: "You need to be an owner or billing manager of " + account + ". If gh names a missing scope, run: gh auth refresh -h github.com -s admin:org",
			Err: err}
	}
	return err
}

// azFixRE finds az's own "Run the command below…" remedy.
var azFixRE = regexp.MustCompile(`(?s)Run the command below[^\n]*\n(.+)`)

// explainAz turns an az failure into an actionable error.
func explainAz(err error, subscription string) error {
	var ce *CLIError
	if !errors.As(err, &ce) {
		return err
	}
	out := ce.Stderr
	switch {
	case azFixRE.MatchString(out):
		var cmds []string
		for _, l := range strings.Split(azFixRE.FindStringSubmatch(out)[1], "\n") {
			if l = strings.TrimSpace(l); strings.HasPrefix(l, "az ") {
				cmds = append(cmds, l)
			}
		}
		if len(cmds) > 0 {
			return &Error{Msg: "the Azure CLI sign-in expired or needs reauthentication", Fix: strings.Join(cmds, " && "), Err: err}
		}
	case strings.Contains(out, "Too Many Requests") || strings.Contains(out, `"code":"429"`):
		return &Error{Msg: "Azure Cost Management is rate-limiting requests (its quota is shared across your tenant); retrying automatically",
			Retry: true, RetryAfter: retryAfter(out, time.Now()), Err: err}
	case strings.Contains(out, "'graph' is misspelled") || strings.Contains(out, "resource-graph"):
		return &Error{Msg: "the Azure CLI resource-graph extension is needed to find your Foundry resource", Fix: "az extension add --name resource-graph", Err: err}
	case strings.Contains(out, "az login") || strings.Contains(out, "AADSTS") || strings.Contains(out, "No subscription found"):
		return &Error{Msg: "the Azure CLI is not signed in (or its token expired)", Fix: "az login", Err: err}
	case strings.Contains(out, "AuthorizationFailed") || strings.Contains(out, "does not have authorization") || strings.Contains(out, "RBACAccessDenied"):
		return &Error{Msg: "your Azure account can't read costs for subscription " + subscription,
			Fix: "Ask a subscription owner to grant you the Cost Management Reader role: az role assignment create --assignee <your-upn> --role \"Cost Management Reader\" --scope /subscriptions/" + subscription,
			Err: err}
	}
	return err
}

// azSubscription returns the Azure CLI's current subscription.
func azSubscription(ctx context.Context, run Run) (string, error) {
	b, err := run(ctx, "az", "account", "show", "--query", "id", "--output", "tsv", "--only-show-errors")
	if err != nil {
		return "", explainAz(err, "")
	}
	id := strings.TrimSpace(string(b))
	if !subscriptionRE.MatchString(id) {
		return "", &Error{Msg: "the Azure CLI has no current subscription", Fix: "az login && az account set --subscription <id>"}
	}
	return id, nil
}

// azResources lists the Foundry (Cognitive Services) accounts in a subscription.
func azResources(ctx context.Context, run Run, subscription string) ([]string, error) {
	b, err := run(ctx, "az", "cognitiveservices", "account", "list", "--subscription", subscription,
		"--query", "[].id", "--output", "json", "--only-show-errors")
	if err != nil {
		return nil, explainAz(err, subscription)
	}
	var ids []string
	if err := json.Unmarshal(b, &ids); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, &Error{Msg: "no Foundry (Cognitive Services) accounts found in subscription " + subscription,
			Fix: "az account set --subscription <the subscription with your Foundry resource>, or list resources in dashboard.json"}
	}
	return ids, nil
}

var endpointRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// AzureEndpoints finds the Foundry accounts behind endpoint subdomains (from the local tools'
// configs) with Azure Resource Graph, across every subscription the signed-in user can read, then
// reports their costs. Accounts in several subscriptions are queried per subscription.
func AzureEndpoints(ctx context.Context, run Run, endpoints []string, month string) (Report, error) {
	r := Report{Provider: "azure-foundry", Month: month, Basis: "Azure ActualCost / PreTaxCost for selected resources; excludes taxes; subject to billing adjustments", Items: []Item{}}
	if _, err := Month(month); err != nil {
		return r, err
	}
	if len(endpoints) == 0 {
		return r, &Error{Msg: "no Azure Foundry endpoint found in your Claude or Codex config",
			Fix: `Set ANTHROPIC_FOUNDRY_RESOURCE in your Foundry Claude profile, or add "subscription" and "resources" to this billing entry in dashboard.json`}
	}
	quoted := make([]string, len(endpoints))
	for i, e := range endpoints {
		if !endpointRE.MatchString(e) {
			return r, fmt.Errorf("invalid Foundry endpoint name %q", e)
		}
		quoted[i] = "'" + e + "'"
	}
	q := "resources | where type =~ 'microsoft.cognitiveservices/accounts' and tolower(tostring(properties.customSubDomainName)) in (" +
		strings.Join(quoted, ",") + ") | project id, subscriptionId, endpoint=tolower(tostring(properties.customSubDomainName))"
	b, err := run(ctx, "az", "graph", "query", "-q", q, "--first", "1000", "--query", "data", "--output", "json", "--only-show-errors")
	if err != nil {
		return r, explainAz(err, "")
	}
	var rows []struct {
		ID, SubscriptionID, Endpoint string
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		return r, err
	}
	bySub := map[string][]string{}
	found := map[string]bool{}
	for _, x := range rows {
		bySub[x.SubscriptionID] = append(bySub[x.SubscriptionID], x.ID)
		found[x.Endpoint] = true
	}
	var missing []string
	for _, e := range endpoints {
		if !found[e] {
			missing = append(missing, e)
		}
	}
	if len(bySub) == 0 {
		return r, &Error{Msg: "your Azure account can't see the Foundry resource for " + strings.Join(missing, ", ") + " in any subscription",
			Fix: "Ask the resource's subscription owner for Reader and Cost Management Reader, or sign in to its tenant: az login --tenant <tenant-id>"}
	}
	subs := make([]string, 0, len(bySub))
	for s := range bySub {
		subs = append(subs, s)
	}
	slices.Sort(subs)
	for _, s := range subs {
		part, err := Azure(ctx, run, s, bySub[s], month)
		if err != nil {
			return r, err
		}
		r.Items = append(r.Items, part.Items...)
		r.Retrieved = part.Retrieved
	}
	r.Account = strings.Join(subs, ",")
	return r, nil
}
