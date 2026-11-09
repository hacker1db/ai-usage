package view_test

import (
	"net/url"
	"reflect"
	"testing"
	"time"

	"ai-usage/internal/history"
	"ai-usage/internal/ownership"
	"ai-usage/internal/snapshot"
	"ai-usage/internal/usage"
	"ai-usage/internal/view"
)

const rowTotal = 9800 // Total per hourly row in profileFixture

// profileFixture has 72 hourly rows each for claude-code:personal, codex:work-foundry,
// codex:personal-chatgpt, and a proxy series, one session per non-proxy series, and two sets.
func profileFixture(now time.Time) *snapshot.Snapshot {
	var rows []history.HourRow
	for h := 0; h < 72; h++ {
		at := now.Add(-time.Duration(h) * time.Hour).Local()
		for _, x := range []struct{ tool, profile string }{
			{"claude-code", "personal"}, {"codex", "work-foundry"}, {"codex", "personal-chatgpt"}, {usage.ToolProxy, "codex_cli_rs"},
		} {
			rows = append(rows, history.FromUsage(usage.Row{
				Key:     usage.Key{Bucket: at.Format("2006-01-02T15") + ":00", Tool: x.tool, Profile: x.profile, Model: "m"},
				Metrics: usage.Metrics{Requests: 3, Input: 9000, Cached: 6000, Output: 800, Total: rowTotal}, Sessions: 1,
			}))
		}
	}
	sess := func(tool, profile, id string) usage.SessionRow {
		return usage.SessionRow{Tool: tool, Profile: profile, ID: id, Title: id, Project: "p",
			First: now.Add(-time.Hour).Format(time.RFC3339), Last: now.Format(time.RFC3339),
			ActiveMin: 10, Requests: 5, Total: 1000, Uncached: 100, Output: 50, Model: "m", Class: "org"}
	}
	return &snapshot.Snapshot{
		Meta: snapshot.Meta{Generated: now, Since: now.AddDate(0, 0, -34), TZ: "PST", Days: 35, Interval: 30,
			Ownership: snapshot.Ownership{Source: "test", Labels: []string{"personal", "org"}, Default: "org",
				Profiles: []ownership.ProfileSet{
					{Name: "mine", Label: "Me & mine", Patterns: []string{"claude-code:personal", "personal-*"}},
					{Name: "claude-only", Label: "Claude only", Patterns: []string{"claude-code:*"}},
				}}},
		Columns: history.Columns, Rows: rows,
		Sessions: []usage.SessionRow{sess("claude-code", "personal", "c1"), sess("codex", "work-foundry", "x1"),
			sess("codex", "personal-chatgpt", "x2")},
	}
}

func ids[T any](xs []T, id func(T) string) []string {
	out := []string{}
	for _, x := range xs {
		out = append(out, id(x))
	}
	return out
}

func sessionIDs(d view.Dashboard) []string {
	return ids(d.Sessions, func(s view.SessionView) string { return s.ID })
}

func TestProfileOptions(t *testing.T) {
	now := time.Now()
	d := view.Build(profileFixture(now), view.Defaults, now)
	got := ids(d.Profiles, func(o view.ProfileOpt) string { return o.ID })
	// All first, then sets in config order, then single series with data in palette order (no proxy).
	want := []string{"", "mine", "claude-only", "claude-code:personal", "codex:personal-chatgpt", "codex:work-foundry"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("profile IDs = %v; want %v", got, want)
	}
	all := d.Profiles[0]
	if all.Label != "All profiles" || all.Set || all.Value != 3*72*rowTotal || len(all.Members) != 3 || all.Slot() != -1 {
		t.Errorf("all = %+v slot %d", all, all.Slot())
	}
	mine := d.Profiles[1]
	if !mine.Set || mine.Value != 2*72*rowTotal || mine.Slot() != -1 ||
		!reflect.DeepEqual(ids(mine.Members, func(g view.Group) string { return g.Key }), []string{"claude-code:personal", "codex:personal-chatgpt"}) {
		t.Errorf("mine = %+v", mine)
	}
	one := d.Profiles[3]
	if one.Set || one.Label != "Claude · personal" || one.Value != 72*rowTotal || one.Slot() != 1 {
		t.Errorf("single = %+v slot %d", one, one.Slot())
	}
	if d.Profile.ID != "" || d.Filters.Profile != "" {
		t.Errorf("default selection = %q", d.Profile.ID)
	}
	if d.ProfileHint != "" {
		t.Errorf("hint shown with sets configured: %q", d.ProfileHint)
	}
	s := profileFixture(now)
	s.Meta.Ownership.Profiles = nil
	if d := view.Build(s, view.Defaults, now); d.ProfileHint == "" || len(d.Profiles) != 4 {
		t.Errorf("no sets: hint %q, %d options", d.ProfileHint, len(d.Profiles))
	}
}

func TestProfileSetSelection(t *testing.T) {
	now := time.Now()
	d := view.Build(profileFixture(now), view.Filters{Profile: "mine"}.Normalize(), now)
	if d.Profile.ID != "mine" || d.Filters.Profile != "mine" {
		t.Fatalf("selection = %+v", d.Profile)
	}
	if got := sessionIDs(d); !reflect.DeepEqual(got, []string{"c1", "x2"}) && !reflect.DeepEqual(got, []string{"x2", "c1"}) {
		t.Errorf("sessions = %v", got)
	}
	if d.SessionN != 2 {
		t.Errorf("SessionN = %d", d.SessionN)
	}
	if d.Hero.Title != "All tools · Me & mine · last 7d" {
		t.Errorf("hero title = %q", d.Hero.Title)
	}
	if d.Hero.Value != 2*72*rowTotal {
		t.Errorf("hero value = %v; want %v", d.Hero.Value, 2*72*rowTotal)
	}
	if d.TabCounts["all"] != 2 || d.TabCounts["claude"] != 1 || d.TabCounts["codex"] != 1 || d.TabCounts["foundry"] != 0 {
		t.Errorf("tab counts = %v", d.TabCounts)
	}
	// Option values don't depend on the selection.
	if d.Profiles[0].Value != 3*72*rowTotal {
		t.Errorf("all value under a selection = %v", d.Profiles[0].Value)
	}
}

func TestSingleProfileSelection(t *testing.T) {
	now := time.Now()
	f := view.FromQuery(url.Values{"profile": {"Codex:Work-Foundry"}})
	if f.Profile != "codex:work-foundry" {
		t.Fatalf("normalized profile = %q", f.Profile)
	}
	d := view.Build(profileFixture(now), f, now)
	if d.Profile.ID != "codex:work-foundry" || d.Profile.Set {
		t.Fatalf("selection = %+v", d.Profile)
	}
	if got := sessionIDs(d); !reflect.DeepEqual(got, []string{"x1"}) {
		t.Errorf("sessions = %v", got)
	}
	if d.Hero.Title != "All tools · Codex · Foundry · last 7d" || d.Hero.Value != 72*rowTotal {
		t.Errorf("hero = %q %v", d.Hero.Title, d.Hero.Value)
	}
}

func TestUnknownProfileResets(t *testing.T) {
	now := time.Now()
	for _, p := range []string{"nope", "copilot:github-copilot", "foundry-proxy:codex_cli_rs", "<script>", "a b"} {
		d := view.Build(profileFixture(now), view.FromQuery(url.Values{"profile": {p}}), now)
		if d.Profile.ID != "" || d.Filters.Profile != "" || len(d.Sessions) != 3 {
			t.Errorf("profile=%q: kept %q, %d sessions", p, d.Profile.ID, len(d.Sessions))
		}
		if d.Hero.Title != "All tools · last 7d" {
			t.Errorf("profile=%q: hero title %q", p, d.Hero.Title)
		}
	}
}

func TestProfileANDTab(t *testing.T) {
	now := time.Now()
	d := view.Build(profileFixture(now), view.Filters{Tab: "codex", Profile: "claude-only"}.Normalize(), now)
	if d.Profile.ID != "claude-only" {
		t.Fatalf("selection = %q", d.Profile.ID)
	}
	if len(d.Sessions) != 0 || d.Hero.Value != 0 || d.TabCounts["codex"] != 0 {
		t.Errorf("codex tab + claude set: %d sessions, hero %v, counts %v", len(d.Sessions), d.Hero.Value, d.TabCounts)
	}
	if d.Profile.Value != 0 {
		t.Errorf("claude-only value within the codex tab = %v", d.Profile.Value)
	}
	d = view.Build(profileFixture(now), view.Filters{Tab: "codex", Profile: "mine"}.Normalize(), now)
	if got := sessionIDs(d); !reflect.DeepEqual(got, []string{"x2"}) || d.Hero.Value != 72*rowTotal || d.Profile.Value != 72*rowTotal {
		t.Errorf("codex tab + mine: sessions %v hero %v value %v", got, d.Hero.Value, d.Profile.Value)
	}
	if d.Hero.Title != "Codex · Me & mine · last 7d" {
		t.Errorf("hero title = %q", d.Hero.Title)
	}
}

func TestProfileLinkRoundTrip(t *testing.T) {
	for _, p := range []string{"mine", "claude-code:personal", "codex:work-foundry"} {
		f := view.Filters{Tab: "codex", Profile: p}.Normalize()
		link := f.Link()
		q, err := url.ParseQuery(link[1:])
		if err != nil || link[0] != '?' {
			t.Fatalf("Link = %q: %v", link, err)
		}
		if got := view.FromQuery(q); got != f {
			t.Errorf("round trip %q: %+v != %+v", link, got, f)
		}
	}
	f := view.Filters{Profile: "mine"}.Normalize()
	if got := f.Link("profile", ""); got != "?" {
		t.Errorf("clearing profile = %q", got)
	}
	if got := view.Defaults.Link("profile", "claude-code:personal"); got != "?profile=claude-code%3Apersonal" {
		t.Errorf("Link = %q", got)
	}
	if got := (view.Filters{Profile: "MINE"}).Normalize().Profile; got != "mine" {
		t.Errorf("Normalize lowercases: %q", got)
	}
}

// otherRows adds two unslotted series (both fall into the Other group) to a snapshot.
func otherRows(s *snapshot.Snapshot, now time.Time) {
	for _, p := range []string{"alpha", "beta"} {
		s.Rows = append(s.Rows, history.FromUsage(usage.Row{
			Key:     usage.Key{Bucket: now.Local().Format("2006-01-02T15") + ":00", Tool: "gemini", Profile: p, Model: "m"},
			Metrics: usage.Metrics{Requests: 1, Total: 1}, Sessions: 1,
		}))
	}
}

func TestProfileSetNamedOtherWins(t *testing.T) {
	now := time.Now()
	s := profileFixture(now)
	otherRows(s, now)
	s.Meta.Ownership.Profiles = append(s.Meta.Ownership.Profiles,
		ownership.ProfileSet{Name: "other", Label: "Other set", Patterns: []string{"claude-code:personal"}})
	d := view.Build(s, view.Filters{Profile: "other"}, now)
	if !d.Profile.Set || d.Profile.Label != "Other set" {
		t.Fatalf("profile=other picked %+v, want the set", d.Profile)
	}
	n := 0
	for _, o := range d.Profiles {
		if o.ID == "other" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d options with ID other, want 1 (the set)", n)
	}
}

func TestProfileSetMembersSeeEveryPair(t *testing.T) {
	now := time.Now()
	s := profileFixture(now)
	otherRows(s, now)
	s.Meta.Ownership.Profiles = []ownership.ProfileSet{{Name: "b", Label: "b", Patterns: []string{"gemini:beta"}}}
	d := view.Build(s, view.Filters{}, now)
	for _, o := range d.Profiles {
		if o.ID == "b" {
			if len(o.Members) != 1 || o.Members[0].Key != view.Other.Key || o.Value != 1 {
				t.Errorf("set b = members %+v value %v, want [other] 1", o.Members, o.Value)
			}
			return
		}
	}
	t.Fatal("set b missing")
}
