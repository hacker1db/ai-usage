package sources

import (
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ai-usage/internal/usage"
)

func pbKey(field, wire int) []byte { return pbAppendVarint(nil, uint64(field<<3|wire)) } //nolint:gosec // test fixture

func pbAppendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func pbInt(field int, v uint64) []byte { return pbAppendVarint(pbKey(field, 0), v) }

func pbMsg(field int, parts ...[]byte) []byte {
	var body []byte
	for _, p := range parts {
		body = append(body, p...)
	}
	return append(pbAppendVarint(pbKey(field, 2), uint64(len(body))), body...)
}

func pbStr(field int, s string) []byte { return pbMsg(field, []byte(s)) }

func cat(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

func sqliteExec(t *testing.T, db, sql string) {
	t.Helper()
	out, err := exec.Command(SQLitePath, db, sql).CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 %s: %v: %s", db, err, out)
	}
}

func TestCollectAntigravity(t *testing.T) {
	if _, err := os.Stat(SQLitePath); err != nil {
		t.Skip("sqlite3 not available")
	}
	home := t.TempDir()
	conv := filepath.Join(home, "conversations")
	if err := os.MkdirAll(conv, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Minute)
	stamp := pbMsg(agStepCreated, pbInt(1, uint64(now.Unix())), pbInt(2, 5)) //nolint:gosec // test fixture
	steps := [][]byte{
		stamp,
		cat(stamp, pbMsg(agStepUsage, pbInt(agUsageModel, 1532), pbInt(agUsageInput, 100), pbInt(agUsageOutput, 30),
			pbInt(agUsageCacheRead, 1000), pbInt(agUsageThinking, 20), pbStr(7, "resp"))),
		cat(stamp, pbMsg(agStepUsage, pbInt(agUsageModel, 1532), pbInt(6, 24))),
		cat(stamp, pbMsg(agStepUsage, pbInt(agUsageModel, 77), pbInt(agUsageInput, 5), pbInt(agUsageOutput, 1))),
		{0xff},
	}
	gen := cat(pbStr(4, "x"), pbMsg(agGenInfo, pbInt(agGenInfoModel, 1532), pbStr(agGenInfoName, "gemini-test")))

	db := filepath.Join(conv, "conv-1.db")
	var sql strings.Builder

	sql.WriteString("PRAGMA journal_mode=WAL; CREATE TABLE steps (idx integer PRIMARY KEY, metadata blob);" +
		"CREATE TABLE gen_metadata (idx integer PRIMARY KEY, data blob);")
	for i, s := range steps {
		sql.WriteString("INSERT INTO steps VALUES (" + string(rune('0'+i)) + ", X'" + hex.EncodeToString(s) + "');")
	}
	sql.WriteString("INSERT INTO gen_metadata VALUES (0, X'" + hex.EncodeToString(gen) + "');")
	sqliteExec(t, db, sql.String())

	sqliteExec(t, filepath.Join(home, "conversation_summaries.db"),
		`CREATE TABLE conversation_summaries (conversation_id text, title text, workspace_uris text);`+
			`INSERT INTO conversation_summaries VALUES ('conv-1', 'Fix the parser', '["file:///tmp/my%20repo"]');`+
			`INSERT INTO conversation_summaries VALUES ('other', 'Unused', '[]');`)
	write(t, filepath.Join(home, "history.jsonl"),
		`{"display":"/model Gemini","type":"slash_command"}`,
		`{"display":"please fix the token=abc123 parser","conversationId":"conv-1","workspace":"/tmp/ws"}`,
		`not json`)

	agg := usage.NewAggregator(time.Now().Add(-time.Hour), usage.Daily)
	st := CollectAntigravity(agg, home)
	if st.Status != "ok" || st.Requests != 2 || len(st.Errors) != 0 {
		t.Fatalf("status = %+v, want ok with 2 requests", st)
	}
	byModel := map[string]usage.Row{}
	for _, r := range agg.Rows() {
		if r.Tool != usage.ToolAntigravity || r.Profile != AntigravityProfile {
			t.Fatalf("row %+v has wrong tool/profile", r)
		}
		byModel[r.Model] = r
	}
	g := byModel["gemini-test"]
	if g.Input != 1100 || g.Cached != 1000 || g.Output != 30 || g.Reasoning != 20 || g.Total != 1130 || g.Requests != 1 {
		t.Errorf("gemini-test row = %+v", g)
	}
	if u := byModel["antigravity-model-77"]; u.Input != 5 || u.Output != 1 {
		t.Errorf("unnamed model row = %+v (models %v)", u, byModel)
	}
	s := agg.SessionRows(true)
	if len(s) != 1 || s[0].ID != "conv-1" || s[0].Title != "Fix the parser" || s[0].Path != "/tmp/my repo" {
		t.Fatalf("sessions = %+v", s)
	}
	if p := s[0].Detail.Prompts; len(p) != 1 || strings.Contains(p[0], "abc123") {
		t.Errorf("prompts = %q, want one redacted prompt", p)
	}

	if st := CollectAntigravity(agg, filepath.Join(home, "nope")); st.Status != "not found" {
		t.Errorf("missing home reported as %q", st.Status)
	}
}

func TestPBEachRejectsTruncated(t *testing.T) {
	msg := pbMsg(9, pbInt(2, 300))
	for i := 1; i < len(msg); i++ {
		if err := pbEach(msg[:i], func(int, uint64, []byte) {}); err == nil {
			t.Errorf("truncated at %d accepted", i)
		}
	}
	if _, _, ok := antigravityStepUsage(msg); ok {
		t.Error("usage without a timestamp accepted")
	}
}
