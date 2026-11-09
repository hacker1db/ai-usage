package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"ai-usage/internal/budget"
	"ai-usage/internal/history"
	"ai-usage/internal/palette"
	"ai-usage/internal/snapshot"
	"ai-usage/internal/sources"
	"ai-usage/internal/usage"
	"ai-usage/internal/view"
)

func TestBraillePlotting(t *testing.T) {
	b := NewBraille(2, 1)
	b.Set(0, 0, "#111111")
	b.Set(1, 3, "#222222")
	b.Set(9, 9, "#333333") // outside: ignored
	if r, c := b.Cell(0, 0); r != rune(0x2800+0x01+0x80) || c != "#222222" {
		t.Fatalf("cell 0 = %U %s, want U+2881 in the last color drawn", r, c)
	}
	if r, _ := b.Cell(1, 0); r != ' ' {
		t.Fatalf("empty cell = %q, want space", r)
	}

	l := NewBraille(2, 1)
	l.Line(0, 0, 3, 0, "#000000", false)
	for cx := range 2 {
		if r, _ := l.Cell(cx, 0); r != rune(0x2800+0x01+0x08) {
			t.Fatalf("solid line cell %d = %U, want both top dots (U+2809)", cx, r)
		}
	}
	d := NewBraille(2, 1)
	d.Line(0, 0, 3, 0, "#000000", true)
	if r0, _ := d.Cell(0, 0); r0 != rune(0x2801) {
		t.Fatalf("dotted line keeps every other dot: cell 0 = %U, want U+2801", r0)
	}
}

func TestStackCellBoundary(t *testing.T) {
	segs := Stack([]float64{3, 5}, []string{"A", "B"}, 8, 1)
	want := []Segment{{"A", 0, 3}, {"B", 3, 8}}
	if !slices.Equal(segs, want) {
		t.Fatalf("Stack = %v, want %v", segs, want)
	}
	if n, fg, bg := StackCell(segs, 0); n != 3 || fg != "A" || bg != "B" {
		t.Fatalf("boundary cell = (%d,%s,%s), want 3 eighths of A over a B background", n, fg, bg)
	}

	tall := Stack([]float64{12, 4}, []string{"A", "B"}, 16, 2)
	if n, fg, bg := StackCell(tall, 0); n != 8 || fg != "A" || bg != "" {
		t.Fatalf("full cell = (%d,%s,%s), want a full A block", n, fg, bg)
	}
	if n, fg, bg := StackCell(tall, 1); n != 4 || fg != "A" || bg != "B" {
		t.Fatalf("upper cell = (%d,%s,%s), want 4 eighths of A under B", n, fg, bg)
	}
	if n, _, _ := StackCell(Stack([]float64{1}, []string{"A"}, 1000, 2), 0); n != 1 {
		t.Fatalf("a tiny non-zero value keeps one eighth, got %d", n)
	}
}

func TestTruncateWideRunes(t *testing.T) {
	for _, s := range []string{"日本語のセッションタイトル", "emoji 🚀🚀🚀 title", "plain ascii title that is long"} {
		for w := 1; w < 12; w++ {
			if got := Truncate(s, w); Width(got) > w {
				t.Fatalf("Truncate(%q, %d) = %q, width %d", s, w, got, Width(got))
			}
		}
	}
	if got := Truncate("short", 10); got != "short" {
		t.Fatalf("Truncate kept %q", got)
	}
}

func TestParseKeys(t *testing.T) {
	got := ParseKeys([]byte("q\t\x1b[Z\x1b[A\x1b[6~\x03G"))
	want := []string{"q", "tab", "shift-tab", "up", "pgdn", "ctrl-c", "G"}
	if !slices.Equal(got, want) {
		t.Fatalf("ParseKeys = %v, want %v", got, want)
	}
}

func fixture(now time.Time) *snapshot.Snapshot {
	var rows []history.HourRow
	add := func(at time.Time, tool, profile, model string, in, cached, out int64) {
		rows = append(rows, history.FromUsage(usage.Row{
			Key:      usage.Key{Bucket: at.Format("2006-01-02T15") + ":00", Tool: tool, Profile: profile, Model: model},
			Metrics:  usage.Metrics{Requests: 3, Input: in, Cached: cached, Output: out, Total: in + out},
			Sessions: 1,
		}))
	}
	for h := range 24 * 9 {
		at := now.Add(-time.Duration(h) * time.Hour)
		add(at, usage.ToolClaude, "personal", "claude-opus-5-5", int64(1000*(h%7+1)), int64(800*(h%7+1)), 120)
		if h%3 == 0 {
			add(at, usage.ToolCopilot, "github-copilot", "gpt-6-luna", 5000, 4000, 300)
			add(at, usage.ToolCodex, "work-foundry", "gpt-6.1-sol", 2000, 100, 400)
			add(at, usage.ToolProxy, "codex_cli_rs", "gpt-6.1-sol", 2100, 100, 420)
		}
	}
	last := now.Add(-time.Hour).Format(time.RFC3339)
	sess := []usage.SessionRow{
		{Tool: usage.ToolClaude, Profile: "personal", ID: "a1", Title: "日本語のとても長いセッションタイトル with 🚀 emoji and more words to overflow",
			Project: "a-really-long-project-name-that-keeps-going", Path: "~/x", First: last, Last: last, ActiveMin: 95, Requests: 40, Total: 9_000_000, Output: 1000},
		{Tool: usage.ToolCopilot, Profile: "github-copilot", ID: "b2", TitleSrc: "prompt", Title: "first prompt text",
			First: last, Last: last, ActiveMin: 5, Requests: 2, Total: 1200},
	}
	return &snapshot.Snapshot{
		Meta: snapshot.Meta{Generated: now, Since: now.AddDate(0, 0, -34), TZ: "PST", Days: 35, Interval: 30,
			History: "~/data/hourly.json", HistoryRows: len(rows), HistorySince: rows[len(rows)-1].Hour},
		Sources:  []sources.Status{{Source: "claude-code:personal", Path: "~/.claude-personal", Status: "ok", Requests: 10}, {Source: "copilot", Path: "~/x.db", Status: "not found"}},
		Budgets:  map[string]budget.Budget{"claude-code:personal": {Tokens: 500_000, Metric: budget.Total, Period: budget.Month, ResetDay: 1}},
		Columns:  history.Columns,
		Rows:     rows,
		Sessions: sess,
	}
}

func TestEveryLineFitsTheWidth(t *testing.T) {
	now := time.Date(2026, 11, 7, 17, 30, 0, 0, time.Local)
	snap := fixture(now)
	for _, tab := range view.TabOptions() {
		for _, rng := range view.RangeOptions() {
			f := view.Filters{Tab: tab.ID, Range: rng.ID}.Normalize()
			d := view.Build(snap, f, now)
			for _, w := range []int{40, 60, 80, 120, 160} {
				for _, mode := range []palette.Mode{palette.Dark, palette.Light} {
					fr := Draw(&d, NewTheme(mode, FullColor), w, Status{Source: "local collect"})
					for i, l := range append(fr.Header, fr.Body...) {
						if got := Width(Plain(l)); got > w {
							t.Fatalf("tab %s range %s width %d: line %d is %d wide: %q", tab.ID, rng.ID, w, i, got, Plain(l))
						}
					}
					if out := Render(fr.Body, NoEscapes, "\n"); strings.Contains(out, "\x1b") {
						t.Fatalf("NoEscapes output contains an escape sequence")
					}
				}
			}
		}
	}
}

func TestSectionsPresent(t *testing.T) {
	now := time.Date(2026, 11, 7, 17, 30, 0, 0, time.Local)
	frame := func(f view.Filters) string {
		d := view.Build(fixture(now), f, now)
		fr := Draw(&d, NewTheme(palette.Dark, NoEscapes), 120, Status{Source: "local collect"})
		return Render(append(fr.Header, fr.Body...), NoEscapes, "\n")
	}
	all := frame(view.Defaults)
	for _, want := range []string{"Token usage", "Cumulative tokens", "Usage by profile", "Profiles", "When you work",
		"Token mix", "Where the tokens went", "Projects", "Sessions", "Models", "Sources", "✕ Over budget", "! not found"} {
		if !strings.Contains(all, want) {
			t.Errorf("frame is missing %q", want)
		}
	}
	// The cumulative budget line needs every group in the tab to have a budget: only Claude does.
	if strings.Contains(all, "┄ budget") {
		t.Errorf("All tab drew a budget line although Copilot and Codex have no budget")
	}
	if claude := frame(view.Filters{Tab: "claude"}); !strings.Contains(claude, "┄ budget") || !strings.Contains(claude, "Budget used") {
		t.Errorf("Claude tab is missing the budget line or hero budget stat")
	}
}
