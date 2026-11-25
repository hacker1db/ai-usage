package sources

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"ai-usage/internal/usage"
)

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeDedupeAndSkips(t *testing.T) {
	root := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339)
	asst := func(id, req, model, usageJSON string) string {
		return `{"type":"assistant","sessionId":"s1","timestamp":"` + ts + `","requestId":"` + req + `","cwd":"/tmp/x",` +
			`"message":{"id":"` + id + `","model":"` + model + `","usage":` + usageJSON + `,"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/tmp/x/a.go"}}]}}`
	}
	u := `{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":20}`
	write(t, filepath.Join(root, "projects", "p", "s1.jsonl"),
		`{"type":"user","sessionId":"s1","timestamp":"`+ts+`","message":{"content":"fix the token=abc123 bug"}}`,
		asst("m1", "r1", "claude-x", u),
		asst("m1", "r1", "claude-x", u),
		asst("m2", "r2", "<synthetic>", u),
		asst("m3", "r3", "claude-x", `{}`),
		`not json`,
		`{"type":"ai-title","sessionId":"s1","aiTitle":"Fix the bug"}`,
	)
	agg := usage.NewAggregator(time.Now().Add(-time.Hour), usage.Daily)
	st := CollectClaude(agg, []ClaudeDir{{root, "personal"}}, nil)
	if st[0].Status != "ok" || st[0].Requests != 1 {
		t.Fatalf("status = %+v, want ok with 1 request", st[0])
	}
	rows := agg.Rows()
	if len(rows) != 1 || rows[0].Input != 130 || rows[0].Cached != 100 || rows[0].Output != 5 || rows[0].Total != 135 {
		t.Fatalf("rows = %+v", rows)
	}
	s := agg.SessionRows(true)
	if len(s) != 1 || s[0].Title != "Fix the bug" || s[0].TitleSrc != "ai" {
		t.Fatalf("session = %+v", s)
	}
	if got := s[0].Detail.Prompts; len(got) != 1 || strings.Contains(got[0], "abc123") {
		t.Errorf("prompt not captured or not redacted: %q", got)
	}

	if f := s[0].Detail.Files; len(f) != 1 || f[0].Name != "/tmp/x/a.go" || f[0].N != 4 {
		t.Errorf("files = %+v", f)
	}
}

func TestCodexCumulativeTokenCountsAreDiffed(t *testing.T) {
	root := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339)
	tc := func(total, in, out int) string {
		return `{"type":"event_msg","timestamp":"` + ts + `","payload":{"type":"token_count","info":{"total_token_usage":` +
			`{"input_tokens":` + strconv.Itoa(in) + `,"output_tokens":` + strconv.Itoa(out) + `,"total_tokens":` + strconv.Itoa(total) + `}}}}`
	}
	write(t, filepath.Join(root, "sessions", "2026", "r.jsonl"),
		`{"type":"session_meta","timestamp":"`+ts+`","payload":{"id":"sess","model_provider":"foundry","model":"gpt-x"}}`,
		tc(110, 100, 10),
		tc(110, 100, 10),
		tc(160, 140, 20),
	)
	agg := usage.NewAggregator(time.Now().Add(-time.Hour), usage.Daily)
	st := CollectCodex(agg, root, nil)
	rows := agg.Rows()
	if st.Requests != 2 || len(rows) != 1 || rows[0].Profile != "work-foundry" || rows[0].Input != 140 || rows[0].Output != 20 {
		t.Fatalf("status %+v rows %+v", st, rows)
	}
}

func TestCodexHomesIncludesExtrasOnce(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	o := Options{CodexHome: a, CodexExtra: []string{b, a, ""}}
	if got := o.CodexHomes(); len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("%v", got)
	}
}

func TestUnreadableFilesAreReportedNotSkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	root := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339)
	line := func(id string) string {
		return `{"type":"assistant","sessionId":"s1","timestamp":"` + ts + `","requestId":"` + id + `",` +
			`"message":{"id":"` + id + `","model":"m","usage":{"input_tokens":1000000,"output_tokens":0}}}`
	}
	write(t, filepath.Join(root, "projects", "p", "a.jsonl"), line("m1"))
	locked := filepath.Join(root, "projects", "p", "b.jsonl")
	write(t, locked, line("m2"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	lockedDir := filepath.Join(root, "projects", "q")
	write(t, filepath.Join(lockedDir, "c.jsonl"), line("m3"))
	if err := os.Chmod(lockedDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644); _ = os.Chmod(lockedDir, 0o755) })

	agg := usage.NewAggregator(time.Time{}, usage.Daily)
	st := CollectClaude(agg, []ClaudeDir{{root, "personal"}}, nil)
	if st[0].Status != "ok" || st[0].Requests != 1 {
		t.Fatalf("status = %+v, want ok with the 1 readable request", st[0])
	}
	got := strings.Join(st[0].Errors, "\n")
	if !strings.Contains(got, "b.jsonl") || !strings.Contains(got, filepath.Base(lockedDir)) {
		t.Fatalf("unreadable file and directory not reported: %q", st[0].Errors)
	}

	codex := t.TempDir()
	cf := filepath.Join(codex, "sessions", "2026", "r.jsonl")
	write(t, cf, `{"type":"session_meta"}`)
	if err := os.Chmod(cf, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cf, 0o644) })
	if cs := CollectCodex(usage.NewAggregator(time.Time{}, usage.Daily), codex, nil); len(cs.Errors) != 1 {
		t.Fatalf("codex status = %+v, want one error", cs)
	}
}

func TestCapErrors(t *testing.T) {
	in := []string{"a", "b", "c", "d", "e", "f", "g"}
	got := capErrors(in)
	if len(got) != maxStatusErrors || got[len(got)-1] != "and 3 more" {
		t.Fatalf("got %q", got)
	}
	if len(capErrors(in[:5])) != 5 {
		t.Fatal("capped a list at the limit")
	}
}

func TestUntraversableRootIsAnErrorNotMissing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "projects", "p", "a.jsonl"), `{}`)
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	st := CollectClaude(usage.NewAggregator(time.Time{}, usage.Daily), []ClaudeDir{{root, "x"}}, nil)
	if !strings.HasPrefix(st[0].Status, "error: ") {
		t.Fatalf("untraversable Claude root reported as %q", st[0].Status)
	}
	if cs := CollectCopilot(usage.NewAggregator(time.Time{}, usage.Daily), filepath.Join(root, "db.sqlite")); !strings.HasPrefix(cs.Status, "error: ") {
		t.Fatalf("untraversable Copilot path reported as %q", cs.Status)
	}
	if ps := CollectProxy(usage.NewAggregator(time.Time{}, usage.Daily), filepath.Join(root, "proxy.log")); !strings.HasPrefix(ps.Status, "error: ") {
		t.Fatalf("untraversable proxy path reported as %q", ps.Status)
	}
	missing := filepath.Join(t.TempDir(), "nope")
	if st := CollectClaude(usage.NewAggregator(time.Time{}, usage.Daily), []ClaudeDir{{missing, "x"}}, nil); st[0].Status != "not found" {
		t.Fatalf("missing root reported as %q", st[0].Status)
	}
}
