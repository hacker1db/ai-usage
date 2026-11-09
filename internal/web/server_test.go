package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-usage/internal/history"
	"ai-usage/internal/ownership"
	"ai-usage/internal/snapshot"
	"ai-usage/internal/usage"
	"ai-usage/internal/view"
)

// fixture is a small snapshot with two profiles, a session, and proxy rows.
func fixture(now time.Time) *snapshot.Snapshot {
	var rows []history.HourRow
	for h := 0; h < 72; h++ {
		at := now.Add(-time.Duration(h) * time.Hour).Local()
		for _, x := range []struct{ tool, profile, model string }{
			{"claude-code", "personal", "claude-opus"}, {"codex", "work-foundry", "gpt-6"}, {"foundry-proxy", "codex_cli_rs", "gpt-6"},
		} {
			rows = append(rows, history.FromUsage(usage.Row{
				Key:     usage.Key{Bucket: at.Format("2006-01-02T15") + ":00", Tool: x.tool, Profile: x.profile, Model: x.model},
				Metrics: usage.Metrics{Requests: 3, Input: 9000, Cached: 6000, Output: 800, Total: 9800}, Sessions: 1,
			}))
		}
	}
	return &snapshot.Snapshot{
		Meta: snapshot.Meta{Generated: now, Since: now.AddDate(0, 0, -34), TZ: "PST", Days: 35, Interval: 30,
			Ownership: snapshot.Ownership{Source: "built-in", Auto: true, Labels: []string{"personal", "org"}, Default: "org"}},
		Columns: history.Columns, Rows: rows,
		Sessions: []usage.SessionRow{{Tool: "claude-code", Profile: "personal", ID: "abc", Title: `<b>Fix & ship</b>`,
			Project: "ai-usage", First: now.Add(-2 * time.Hour).Format(time.RFC3339), Last: now.Format(time.RFC3339),
			ActiveMin: 45, Requests: 30, Total: 120000, Uncached: 40000, Output: 9000, Model: "claude-opus",
			Owner: "hacker1db", Class: "personal"},
			{Tool: "codex", Profile: "work-foundry", ID: "def", Title: "Team API", Project: "api",
				First: now.Add(-time.Hour).Format(time.RFC3339), Last: now.Format(time.RFC3339),
				ActiveMin: 10, Requests: 5, Total: 60000, Uncached: 20000, Output: 3000, Model: "gpt-6",
				Owner: "example-org", Class: "org"}},
	}
}

func TestOwnershipFilter(t *testing.T) {
	now := time.Now()
	d := view.Build(fixture(now), view.Filters{Own: "org"}, now)
	if len(d.Sessions) != 1 || d.Sessions[0].ID != "def" || len(d.Projects) != 1 || d.Projects[0].Class != "org" {
		t.Fatalf("own=org: sessions %+v projects %+v", d.Sessions, d.Projects)
	}
	o := d.Ownership
	if o.Total != 180000 || len(o.Rows) != 2 || o.Rows[0].Label != "personal" || o.Rows[0].Value != 120000 || o.Rows[1].Projects != 1 {
		t.Errorf("ownership = %+v", o)
	}
	if d := view.Build(fixture(now), view.Filters{Own: "nope"}, now); d.Filters.Own != "" || len(d.Sessions) != 2 {
		t.Errorf("unknown label kept: %q, %d sessions", d.Filters.Own, len(d.Sessions))
	}
	if got := (view.Filters{Own: "org"}).Normalize().Link("sort", "tokens"); got != "?own=org&sort=tokens" {
		t.Errorf("Link = %q", got)
	}
}

func TestTemplatesRender(t *testing.T) {
	s, err := New(nil, 8790, "", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, f := range []view.Filters{view.Defaults, {Tab: "codex", Metric: "requests", Range: "24h", Burn: "1", Sort: "tokens"},
		{Tab: "copilot", Range: "30d"}, {Own: "personal"}} {
		rec := httptest.NewRecorder()
		s.render(rec, "page", pageData{D: view.Build(fixture(now), f, now), Tabs: view.TabOptions(), Metrics: view.MetricOptions(),
			Burns: view.BurnOptions(), Ranges: view.RangeOptions(), Sorts: view.SessionSorts})
		body := rec.Body.String()
		if rec.Code != 200 {
			t.Fatalf("%+v: status %d: %s", f, rec.Code, body)
		}
		if strings.Contains(body, "<b>Fix") {
			t.Errorf("session title was not escaped")
		}
		if strings.Contains(body, ` style="`) {
			t.Errorf("inline style attribute would violate the CSP")
		}
	}
}

func TestHostAndLoopbackChecks(t *testing.T) {
	s, err := New(nil, 8790, "", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		host, remote, method, path string
		hdr                        map[string]string
		want                       int
	}{
		{"127.0.0.1:8790", "127.0.0.1:5000", "GET", "/healthz", nil, 200},
		{"evil.example:8790", "127.0.0.1:5000", "GET", "/healthz", nil, 421},
		{"127.0.0.1:8790", "10.0.0.5:5000", "GET", "/healthz", nil, 403},
		{"127.0.0.1:8790", "127.0.0.1:5000", "POST", "/api/refresh", nil, 403},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		r.Host, r.RemoteAddr = c.host, c.remote
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, r)
		if rec.Code != c.want {
			b, _ := io.ReadAll(rec.Body)
			t.Errorf("%s %s host=%s remote=%s: got %d want %d (%s)", c.method, c.path, c.host, c.remote, rec.Code, c.want, b)
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("missing CSP on %s", c.path)
		}
	}
	_ = http.StatusOK
}

func TestProfileBarRenders(t *testing.T) {
	s, err := New(nil, 8790, "", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	snap := fixture(now)
	snap.Meta.Ownership.Profiles = []ownership.ProfileSet{{Name: "mine", Label: `<i>Me & "mine"</i>`, Patterns: []string{"claude-code:personal"}}}
	render := func(f view.Filters) string {
		rec := httptest.NewRecorder()
		s.render(rec, "page", pageData{D: view.Build(snap, f, now), Tabs: view.TabOptions(), Metrics: view.MetricOptions(),
			Burns: view.BurnOptions(), Ranges: view.RangeOptions(), Sorts: view.SessionSorts})
		if rec.Code != 200 {
			t.Fatalf("%+v: status %d: %s", f, rec.Code, rec.Body)
		}
		b := rec.Body.String()
		i := strings.Index(b, `<nav class="profile-bar"`)
		if i < 0 {
			t.Fatal("no profile bar")
		}
		return b[i : i+strings.Index(b[i:], "</nav>")] // the profile bar only
	}
	body := render(view.Filters{Profile: "mine"}.Normalize())
	if strings.Contains(body, "<i>Me") {
		t.Error("profile label was not escaped")
	}
	if !strings.Contains(body, "&lt;i&gt;Me &amp; &#34;mine&#34;&lt;/i&gt;") {
		t.Error("escaped profile label missing")
	}
	chip := func(body, href string) string {
		i := strings.Index(body, `href="`+href+`"`)
		if i < 0 {
			t.Fatalf("no chip linking %s", href)
		}
		j := strings.Index(body[i:], ">")
		return body[strings.LastIndex(body[:i], "<a ") : i+j]
	}
	if c := chip(body, "?profile=mine"); !strings.Contains(c, `class="pchip set"`) || !strings.Contains(c, `aria-current="true"`) {
		t.Errorf("selected set chip = %s", c)
	}
	if c := chip(body, "?"); strings.Contains(c, "aria-current") || !strings.Contains(c, "pchip all") {
		t.Errorf("all chip = %s", c)
	}
	if strings.Count(body, `<a class="pchip`) != 4 { // all, mine, claude-code:personal, codex:work-foundry
		t.Errorf("chips = %d:\n%s", strings.Count(body, `<a class="pchip`), body)
	}

	body = render(view.Filters{Profile: "codex:work-foundry"}.Normalize())
	if c := chip(body, "?profile=codex%3Awork-foundry"); !strings.Contains(c, `class="pchip one c5"`) || !strings.Contains(c, `aria-current="true"`) {
		t.Errorf("selected single chip = %s", c)
	}
	if c := chip(body, "?profile=mine"); strings.Contains(c, "aria-current") {
		t.Errorf("unselected set chip = %s", c)
	}
}
