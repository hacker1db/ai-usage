package budget

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBounds(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 11, 7, 15, 0, 0, 0, loc) // a Saturday
	cases := []struct {
		p              Period
		day, weekday   int
		start, endDate string
	}{
		{Day, 1, 0, "2026-11-07", "2026-11-08"},
		{Week, 1, 0, "2026-11-02", "2026-11-09"}, // resets Monday
		{Week, 1, 5, "2026-11-07", "2026-11-14"}, // resets Saturday (today)
		{Month, 1, 0, "2026-11-01", "2026-12-01"},
		{Month, 15, 0, "2026-10-15", "2026-11-15"}, // reset day not reached yet
	}
	for _, c := range cases {
		s, e := Bounds(now, c.p, c.day, c.weekday)
		if s.Format("2006-01-02") != c.start || e.Format("2006-01-02") != c.endDate {
			t.Errorf("%s day=%d wd=%d: got %s..%s, want %s..%s", c.p, c.day, c.weekday,
				s.Format("2006-01-02"), e.Format("2006-01-02"), c.start, c.endDate)
		}
	}
}

func TestLoadValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dashboard.json")
	_ = os.WriteFile(path, []byte(`{"budgets":{"_note":{},"codex:work-foundry":{"tokens":100,"period":"week","reset_weekday":9},
		"bad":{"tokens":1},"claude-code:personal":{"tokens":0}}}`), 0o600)
	b, warn := Load(path)
	got, ok := b["codex:work-foundry"]
	if !ok || got.Metric != Total || got.ResetWeekday != 6 || len(b) != 1 {
		t.Fatalf("budgets = %+v", b)
	}
	if !strings.Contains(warn, "bad:") || !strings.Contains(warn, "claude-code:personal") {
		t.Errorf("warning = %q", warn)
	}
	if b, warn := Load(filepath.Join(t.TempDir(), "missing.json")); len(b) != 0 || warn != "" {
		t.Errorf("missing file: %v %q", b, warn)
	}
}
