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
		asst("m1", "r1", "claude-x", u), // streamed duplicate of the same message
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
	// Every assistant line in this fixture carries the Edit block (real streamed lines carry one block each).
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
		tc(110, 100, 10), // duplicate event: no new usage
		tc(160, 140, 20),
	)
	agg := usage.NewAggregator(time.Now().Add(-time.Hour), usage.Daily)
	st := CollectCodex(agg, root, nil)
	rows := agg.Rows()
	if st.Requests != 2 || len(rows) != 1 || rows[0].Profile != "work-foundry" || rows[0].Input != 140 || rows[0].Output != 20 {
		t.Fatalf("status %+v rows %+v", st, rows)
	}
}
