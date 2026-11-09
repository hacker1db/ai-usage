package history

import (
	"os"
	"path/filepath"
	"testing"

	"ai-usage/internal/usage"
)

// A file in the layout the Python dashboard wrote must load unchanged.
const pythonStore = `{"columns":["hour","tool","profile","model","requests","input","cached","cache_write","output","reasoning","total","sessions","copilot_aiu"],
"rows":[["2026-11-07T09:00","claude-code","personal","claude-opus",5,1000,800,50,40,0,1040,1,0.0]],
"sessions":[{"tool":"claude-code","profile":"personal","id":"s1","title":null,"title_src":null,"project":"x","path":null,"repo":null,
"branch":"main","first":"2026-11-07T17:00:00+00:00","last":"2026-11-07T17:30:00+00:00","active_min":10,"requests":5,"total":1040,
"uncached":240,"output":40,"model":"claude-opus","detail":{"prompts":["hi"],"files":[["~/a.go",2]],"files_total":1,"commands":[],"tools":[["Edit",2]],"refs":[],"models":[["claude-opus",5]]}}]}`

func TestLoadsPythonStoreAndKeepsLargerRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hourly.json")
	if err := os.WriteFile(path, []byte(pythonStore), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Open(path, false)
	if s.Warning != "" {
		t.Fatal(s.Warning)
	}
	rows := s.Since("2026-11-01T00:00")
	if len(rows) != 1 || rows[0].Requests != 5 || rows[0].T.IsZero() {
		t.Fatalf("rows = %+v", rows)
	}
	x, ok := s.Session("claude-code|personal|s1")
	if !ok || x.Detail == nil || x.Detail.Files[0] != (usage.Count{Name: "~/a.go", N: 2}) {
		t.Fatalf("session = %+v", x)
	}

	// A live row with fewer requests means transcripts were deleted: keep the stored row.
	smaller := rows[0]
	smaller.Requests = 2
	held, err := s.Merge([]HourRow{smaller}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := held[smaller.Key()]; !ok || s.Since("2026-11-01T00:00")[0].Requests != 5 {
		t.Fatal("smaller live row replaced the stored one")
	}
	// A larger one replaces it and is written back in the same layout.
	larger := rows[0]
	larger.Requests = 9
	if _, err := s.Merge([]HourRow{larger}, nil); err != nil {
		t.Fatal(err)
	}
	if got := Open(path, false).Since("2026-11-01T00:00")[0].Requests; got != 9 {
		t.Fatalf("reloaded requests = %d, want 9", got)
	}
}

func TestReadOnlyNeverWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hourly.json")
	s := Open(path, true)
	r := FromUsage(usage.Row{Key: usage.Key{Bucket: "2026-11-07T09:00", Tool: "codex", Profile: "p", Model: "m"}, Metrics: usage.Metrics{Requests: 1}})
	if _, err := s.Merge([]HourRow{r}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("read-only store wrote %s", path)
	}
}
