package m365

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fixedToken string

func (f fixedToken) Token(context.Context) (string, error) { return string(f), nil }

// fakeGraph serves the by-date report with refresh date 2026-11-20 and two users, paging the
// first date and throttling once.
func fakeGraph(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var throttled atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		if throttled.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			return
		}
		p := r.URL.Path
		i := strings.Index(p, "(date=")
		if !strings.HasPrefix(p, "/beta/copilot/reports/getMicrosoft365CopilotUsageUserDetailByDate") || i < 0 {
			w.WriteHeader(404)
			return
		}
		date := strings.TrimSuffix(p[i+len("(date="):], ")")
		if date < "2026-11-13" || date > "2026-11-20" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"code":"BadRequest","message":"date out of range"}}`))
			return
		}
		me := Day{RefreshDate: "2026-11-20", Date: date, UPN: "Me@Contoso.com", Actions: 3, Word: true, Teams: true}
		other := Day{RefreshDate: "2026-11-20", Date: date, UPN: "other@contoso.com", Actions: 9}
		page := map[string]any{}
		if r.URL.Query().Get("page") == "" {
			page["value"] = []Day{other}
			page["@odata.nextLink"] = srv.URL + p + "?page=2"
		} else {
			page["value"] = []Day{me}
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(srv.Close)
	return srv, &throttled
}

func TestSyncFiltersUserPagesAndRetries(t *testing.T) {
	srv, _ := fakeGraph(t)
	c := &Client{Base: srv.URL, Tokens: fixedToken("tok"), Sleep: func(time.Duration) {}}
	days, err := c.Sync(context.Background(), "me@contoso.com", time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 8 || days[0].Date != "2026-11-13" || days[7].Date != "2026-11-20" {
		t.Fatalf("got %d days: %+v", len(days), days)
	}
	for _, d := range days {
		if d.Actions != 3 || !strings.EqualFold(d.UPN, "me@contoso.com") {
			t.Fatalf("wrong row %+v", d)
		}
	}
	if got := strings.Join(days[0].Apps(), ","); got != "Teams,Word" {
		t.Fatalf("apps %q", got)
	}
}

func TestSyncNoRowsForUser(t *testing.T) {
	srv, _ := fakeGraph(t)
	c := &Client{Base: srv.URL, Tokens: fixedToken("tok"), Sleep: func(time.Duration) {}}
	if _, err := c.Sync(context.Background(), "nobody@contoso.com", time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)); !errors.Is(err, ErrNoRows) {
		t.Fatalf("want ErrNoRows, got %v", err)
	}
}

func TestForbiddenHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":{"code":"Forbidden","message":"nope"}}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Tokens: fixedToken("tok")}
	_, err := c.FetchDay(context.Background(), "2026-11-20")
	if err == nil || !strings.Contains(err.Error(), "Reports.Read.All") {
		t.Fatalf("got %v", err)
	}
}

func TestNextLinkOffHostRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"value":[],"@odata.nextLink":"https://evil.example/x"}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Tokens: fixedToken("tok")}
	if _, err := c.FetchDay(context.Background(), "2026-11-20"); err == nil {
		t.Fatal("followed an off-host nextLink")
	}
}

func TestStoreMergeKeepsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	s, _ := Load(path)
	s.Merge("me@x", []Day{{Date: "2026-11-01", Actions: 1}, {Date: "2026-11-02", Actions: 2}})
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Merge("ME@x", []Day{{Date: "2026-11-02", Actions: 5}})
	got := s.Sorted()
	if len(got) != 2 || got[1].Actions != 5 {
		t.Fatalf("%+v", got)
	}
	s.Merge("someone@else", []Day{{Date: "2026-11-03"}})
	if len(s.Days) != 1 {
		t.Fatalf("other user's store mixed in: %+v", s.Days)
	}
}

func TestSyncReturnsAuthErrorWithoutProbing(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(403)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Tokens: fixedToken("tok")}
	_, err := c.Sync(context.Background(), "me@x", time.Now())
	var ge *GraphError
	if !errors.As(err, &ge) || ge.Status != 403 || calls.Load() != 1 {
		t.Fatalf("err %v after %d calls", err, calls.Load())
	}
}

func TestSyncSkipsEmptyProbeDates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "2026-11-21") { // in window but no rows yet
			_, _ = w.Write([]byte(`{"value":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"value":[{"reportRefreshDate":"2026-11-20","reportSelectedDate":"2026-11-20","userPrincipalName":"me@x","allAppsCopilotActionCount":1}]}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Tokens: fixedToken("tok")}
	days, err := c.Sync(context.Background(), "me@x", time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC))
	if err != nil || len(days) == 0 {
		t.Fatalf("days %v err %v", days, err)
	}
}
