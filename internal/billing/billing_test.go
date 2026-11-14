package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCopilotNetAndScope(t *testing.T) {
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if isIdentity(args) {
			return ghUser("someone", "read:org"), nil // org billing doesn't need the user scope
		}
		if name != "gh" || !strings.Contains(strings.Join(args, " "), "/organizations/example/settings/billing/ai_credit/usage?year=2026&month=11") {
			t.Fatalf("wrong request: %s %v", name, args)
		}
		return []byte(`{"usageItems":[{"product":"Copilot","sku":"credits","model":"m","grossAmount":10,"discountAmount":7,"netAmount":3},{"product":"Actions","netAmount":99}]}`), nil
	}
	r, err := Copilot(context.Background(), run, "organizations", "example", "ai_credit", "2026-11")
	if err != nil || len(r.Items) != 1 || r.Items[0].Net.String() != "3" {
		t.Fatalf("%+v %v", r, err)
	}
}
func TestFailuresAreNotZero(t *testing.T) {
	for _, body := range []string{`{}`, `{"usageItems":[{"product":"Copilot"}]}`, `bad`} {
		_, err := Copilot(context.Background(), func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if isIdentity(args) {
				return ghUser("me", "user"), nil
			}
			return []byte(body), nil
		}, "users", "me", "premium_request", "2026-11")
		if err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	_, err := Copilot(context.Background(), func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("403") }, "users", "me", "ai_credit", "2026-11")
	if err == nil {
		t.Fatal("lost failure")
	}
}

const sub = "12345678-1234-1234-1234-123456789abc"
const resource = "/subscriptions/" + sub + "/resourceGroups/ai/providers/Microsoft.CognitiveServices/accounts/foundry"

func TestAzurePaginationAndColumnOrder(t *testing.T) {
	calls := 0
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls++
		joined := strings.Join(args, " ")
		if name != "az" || !strings.Contains(joined, `"type":"ActualCost"`) || !strings.Contains(joined, resource) {
			t.Fatal(joined)
		}
		next := ""
		if calls == 1 {
			next = "https://management.azure.com/subscriptions/" + sub + "/providers/Microsoft.CostManagement/query?api-version=2026-06-01&skiptoken=x"
		}
		return []byte(`{"properties":{"columns":[{"name":"Currency"},{"name":"ResourceId"},{"name":"PreTaxCost"},{"name":"UsageDate"}],"rows":[["USD","` + resource + `",1.25,20261101]],"nextLink":"` + next + `"}}`), nil
	}
	r, err := Azure(context.Background(), run, sub, []string{resource}, "2026-11")
	if err != nil || calls != 2 || len(r.Items) != 2 || r.Items[0].Net.String() != "1.25" || r.Items[0].Date != "2026-11-01" {
		t.Fatalf("%+v %v calls=%d", r, err, calls)
	}
}
func TestAzureRejectsForeignContinuation(t *testing.T) {
	calls := 0
	run := func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return []byte(`{"properties":{"columns":[{"name":"Currency"},{"name":"ResourceId"},{"name":"PreTaxCost"},{"name":"UsageDate"}],"rows":[],"nextLink":"https://evil.example/query"}}`), nil
	}
	_, err := Azure(context.Background(), run, sub, []string{resource}, "2026-11")
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
func TestAzureInvalidScopeNeverRuns(t *testing.T) {
	run := func(context.Context, string, ...string) ([]byte, error) { t.Fatal("called provider"); return nil, nil }
	for _, ids := range [][]string{{"/subscriptions/other/resourceGroups/ai/providers/Microsoft.CognitiveServices/accounts/x"}} {
		if _, err := Azure(context.Background(), run, sub, ids, "2026-11"); err == nil {
			t.Fatal("accepted scope")
		}
	}
}
func TestAzureMalformedRows(t *testing.T) {
	for _, body := range []string{`{}`, `{"properties":{}}`, `{"properties":{"columns":[{"name":"Currency"},{"name":"ResourceId"},{"name":"PreTaxCost"},{"name":"UsageDate"}],"rows":[["USD"]]}}`} {
		if _, _, err := azurePage([]byte(body)); err == nil {
			t.Fatal(body)
		}
	}
}

func TestCommandKeepsDiagnostic(t *testing.T) {
	_, err := Command(context.Background(), "sh", "-c", "echo 'gh: needs the \"user\" scope' >&2; exit 1")
	if err == nil || !strings.Contains(err.Error(), `needs the "user" scope`) {
		t.Fatalf("err = %v", err)
	}
}

func isIdentity(args []string) bool {
	return strings.Contains(strings.Join(args, " "), "--include /user")
}

func ghUser(login, scopes string) []byte {
	return []byte("HTTP/2.0 200 OK\r\nX-Oauth-Scopes: " + scopes + "\r\n\r\n{\"login\":\"" + login + "\"}")
}
