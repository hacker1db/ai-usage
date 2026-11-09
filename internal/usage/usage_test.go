package usage

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCleanTitle(t *testing.T) {
	cases := map[string]string{
		"  fix the\n bug  ":                      "fix the bug",
		"<system-reminder>x</system-reminder>":   "",
		"# AGENTS.md\nrules":                     "",
		"use api_key=sk-abcdefghijklmnopqrstuv":  "use [REDACTED]",
		"Authorization: Bearer abcdefghijklmnop": "Authorization: [REDACTED]",
		"see <b>this</b> now":                    "see this now",
	}
	for in, want := range cases {
		if got := CleanTitle(in, 100); got != want {
			t.Errorf("CleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
	if got := CleanTitle("abcdefghij", 5); got != "abcd…" {
		t.Errorf("truncation = %q", got)
	}
}

func TestLenientDecoding(t *testing.T) {
	var v struct {
		A Str   `json:"a"`
		B Int   `json:"b"`
		C Stamp `json:"c"`
		D Int   `json:"d"`
	}
	if err := json.Unmarshal([]byte(`{"a":5,"b":12.9,"c":"2026-11-07T10:00:00Z","d":"x"}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != "" || v.B != 12 || v.D != 0 || !v.C.Equal(time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("decoded %+v", v)
	}
}

func TestCounterMostCommonKeepsFirstSeenTies(t *testing.T) {
	var c Counter
	c.Add("b", 1)
	c.Add("a", 2)
	c.Add("c", 1)
	got := c.MostCommon(-1)
	want := []Count{{"a", 2}, {"b", 1}, {"c", 1}}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("MostCommon = %v, want %v", got, want)
		}
	}
}

func TestAggregatorWindowAndSessions(t *testing.T) {
	now := time.Now()
	a := NewAggregator(now.Add(-time.Hour), Hourly)
	if a.Add("codex", "p", Record{Time: now.Add(-2 * time.Hour), Session: "s", Input: 5}) {
		t.Fatal("record before the cutoff was counted")
	}
	a.Add("codex", "p", Record{Model: "m", Time: now, Session: "s", Input: 100, Cached: 60, Output: 10})
	a.Add("codex", "p", Record{Model: "m", Time: now, Session: "s", Input: 50, Output: 5})
	rows := a.Rows()
	if len(rows) != 1 || rows[0].Requests != 2 || rows[0].Total != 165 || rows[0].Sessions != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	s := a.SessionRows(false)
	if len(s) != 1 || s[0].Uncached != 105 || s[0].ActiveMin != SlotMinutes {
		t.Fatalf("sessions = %+v", s)
	}
}
