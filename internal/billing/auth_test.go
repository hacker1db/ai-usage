package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func cliErr(name, stderr string) error {
	return &CLIError{Name: name, Stderr: stderr, Err: errors.New("exit status 1")}
}

func TestCopilotUsesLoggedInUserAndChecksScope(t *testing.T) {
	var asked string
	ok := func(scopes string) Run {
		return func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if isIdentity(args) {
				return ghUser("CyberDSO", scopes), nil
			}
			asked = args[len(args)-1]
			return []byte(`{"usageItems":[]}`), nil
		}
	}
	r, err := Copilot(context.Background(), ok("repo, user"), "users", "", "ai_credit", "2026-11")
	if err != nil || r.Account != "users/CyberDSO" || !strings.HasPrefix(asked, "/users/CyberDSO/") {
		t.Fatalf("%+v %v %s", r, err, asked)
	}
	_, err = Copilot(context.Background(), ok("repo, read:org"), "users", "", "ai_credit", "2026-11")
	if FixOf(err) != "gh auth refresh -h github.com -s user" || !strings.Contains(err.Error(), "CyberDSO") {
		t.Fatalf("missing scope: %v / %q", err, FixOf(err))
	}
	_, err = Copilot(context.Background(), ok("repo, user"), "users", "someone-else", "ai_credit", "2026-11")
	if !strings.Contains(FixOf(err), "gh auth switch -h github.com -u someone-else") {
		t.Fatalf("wrong account: %v / %q", err, FixOf(err))
	}
}

func TestGHDiagnostics(t *testing.T) {
	for stderr, fix := range map[string]string{
		`gh: This API operation needs the "admin:org" scope. To request it, run: gh auth refresh -h github.com -s admin:org`: "gh auth refresh -h github.com -s admin:org",
		"To get started with GitHub CLI, please run:  gh auth login":                                                         "gh auth login -h github.com",
	} {
		_, err := Copilot(context.Background(), func(context.Context, string, ...string) ([]byte, error) {
			return nil, cliErr("gh", stderr)
		}, "users", "", "ai_credit", "2026-11")
		if FixOf(err) != fix {
			t.Errorf("%q: fix %q, want %q", stderr, FixOf(err), fix)
		}
	}
}

func TestAzureDefaultsAndDiagnostics(t *testing.T) {
	var calls []string
	run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls = append(calls, args[0]+" "+args[1])
		switch args[0] {
		case "account":
			return []byte(sub + "\n"), nil
		case "cognitiveservices":
			return []byte(`["` + resource + `"]`), nil
		}
		return []byte(`{"properties":{"columns":[{"name":"Currency"},{"name":"ResourceId"},{"name":"PreTaxCost"},{"name":"UsageDate"}],"rows":[]}}`), nil
	}
	r, err := Azure(context.Background(), run, "", nil, "2026-11")
	if err != nil || r.Account != sub || strings.Join(calls, ",") != "account show,cognitiveservices account,rest --method" {
		t.Fatalf("%+v %v %v", r, err, calls)
	}
	expired := "ERROR: AADSTS70043: The refresh token has expired.\nRun the command below to authenticate interactively; additional arguments may be added as needed:\naz logout\naz login --tenant \"t\" --scope \"https://management.core.windows.net//.default\"\n"
	for stderr, fix := range map[string]string{
		expired: `az logout && az login --tenant "t" --scope "https://management.core.windows.net//.default"`,
		"ERROR: Please run 'az login' to setup account.": "az login",
	} {
		_, err := Azure(context.Background(), func(context.Context, string, ...string) ([]byte, error) {
			return nil, cliErr("az", stderr)
		}, "", nil, "2026-11")
		if FixOf(err) != fix {
			t.Errorf("fix %q, want %q", FixOf(err), fix)
		}
	}
	_, err = Azure(context.Background(), func(_ context.Context, _ string, args ...string) ([]byte, error) {
		return nil, cliErr("az", "(AuthorizationFailed) The client does not have authorization to perform action")
	}, sub, []string{resource}, "2026-11")
	if !strings.Contains(FixOf(err), "Cost Management Reader") {
		t.Fatalf("authz fix %q", FixOf(err))
	}
}

func TestAzureEndpointsFindsSubscriptions(t *testing.T) {
	other := "87654321-4321-4321-4321-cba987654321"
	res2 := "/subscriptions/" + other + "/resourceGroups/g/providers/Microsoft.CognitiveServices/accounts/two"
	var queried []string
	run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if args[0] == "graph" {
			if !strings.Contains(joined, "in ('one-foundry','two-foundry')") {
				t.Fatalf("graph query %s", joined)
			}
			return []byte(`[{"id":"` + resource + `","subscriptionId":"` + sub + `","endpoint":"one-foundry"},{"id":"` + res2 + `","subscriptionId":"` + other + `","endpoint":"two-foundry"}]`), nil
		}
		for _, s := range []string{sub, other} {
			if strings.Contains(joined, "/subscriptions/"+s+"/providers/Microsoft.CostManagement") {
				queried = append(queried, s)
			}
		}
		return []byte(`{"properties":{"columns":[{"name":"Currency"},{"name":"ResourceId"},{"name":"PreTaxCost"},{"name":"UsageDate"}],"rows":[["USD","x",2,20261101]]}}`), nil
	}
	r, err := AzureEndpoints(context.Background(), run, []string{"one-foundry", "two-foundry"}, "2026-11")
	if err != nil || len(r.Items) != 2 || len(queried) != 2 || r.Account != sub+","+other {
		t.Fatalf("%+v %v %v", r, err, queried)
	}
	_, err = AzureEndpoints(context.Background(), func(context.Context, string, ...string) ([]byte, error) { return []byte(`[]`), nil },
		[]string{"hidden"}, "2026-11")
	if !strings.Contains(err.Error(), "hidden") || !strings.Contains(FixOf(err), "Cost Management Reader") {
		t.Fatalf("invisible resource: %v / %q", err, FixOf(err))
	}
	if _, err = AzureEndpoints(context.Background(), run, nil, "2026-11"); !strings.Contains(FixOf(err), "ANTHROPIC_FOUNDRY_RESOURCE") {
		t.Fatalf("no endpoints: %q", FixOf(err))
	}
	if _, err = AzureEndpoints(context.Background(), run, []string{"x') or true //"}, "2026-11"); err == nil {
		t.Fatal("accepted an unsafe endpoint name")
	}
	_, err = AzureEndpoints(context.Background(), func(context.Context, string, ...string) ([]byte, error) {
		return nil, cliErr("az", "ERROR: 'graph' is misspelled or not recognized by the system.")
	}, []string{"one-foundry"}, "2026-11")
	if FixOf(err) != "az extension add --name resource-graph" {
		t.Fatalf("graph fix %q", FixOf(err))
	}
}

func TestRateLimitsAreTransientWithRetryAfter(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if got := retryAfter("x-ms-ratelimit-microsoft.costmanagement-qpu-retry-after: 47", now); got != 47*time.Second {
		t.Errorf("azure header: %v", got)
	}
	if got := retryAfter(`{"error":{"message":"Too many requests. Please retry after 30 seconds."}}`, now); got != 30*time.Second {
		t.Errorf("azure message: %v", got)
	}
	if got := retryAfter("x-ratelimit-reset: 1800000090", now); got != 90*time.Second {
		t.Errorf("github reset: %v", got)
	}
	if got := retryAfter("Too Many Requests", now); got != 0 {
		t.Errorf("no hint: %v", got)
	}
	az := explainAz(&CLIError{Name: "az", Stderr: `ERROR: Too Many Requests({"code":"429"})`, Err: errors.New("exit 1")}, "s")
	gh := explainGH(&CLIError{Name: "gh", Stderr: "gh: API rate limit exceeded for user ID 1. (HTTP 403)", Err: errors.New("exit 1")}, "users", "me")
	if !Transient(az) || !Transient(gh) || FixOf(gh) != "" {
		t.Fatalf("az=%v gh=%v", az, gh)
	}
}
