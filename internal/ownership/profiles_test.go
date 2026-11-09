package ownership

import (
	"reflect"
	"strings"
	"testing"
)

func TestMatchProfile(t *testing.T) {
	cases := []struct {
		pattern, tool, profile string
		want                   bool
	}{
		{"claude-code:personal", "claude-code", "personal", true},
		{"claude-code:personal", "codex", "personal", false},
		{"personal", "codex", "personal", true}, // bare profile: any tool
		{"personal", "codex", "personal-chatgpt", false},
		{"personal*", "codex", "personal-chatgpt", true},
		{"*:work-*", "codex", "work-foundry", true},
		{"*:work-*", "claude-code", "personal", false},
		{"claude-code:*", "claude-code", "legacy", true},
		{"claude-code:*", "codex", "legacy", false},
		{"Claude-Code:PERSONAL", "claude-code", "personal", true}, // case-insensitive both ways
		{"work-foundry", "CODEX", "Work-Foundry", true},
		{"*", "copilot", "github-copilot", true},
		{"[", "x", "[", false}, // bad pattern never matches
	}
	for _, c := range cases {
		if got := MatchProfile(c.pattern, c.tool, c.profile); got != c.want {
			t.Errorf("MatchProfile(%q, %q, %q) = %v; want %v", c.pattern, c.tool, c.profile, got, c.want)
		}
	}
	s := ProfileSet{Patterns: []string{"claude-code:personal", "personal-chatgpt"}}
	if !s.Match("claude-code", "personal") || !s.Match("codex", "personal-chatgpt") || s.Match("codex", "work-foundry") {
		t.Errorf("ProfileSet.Match is not an OR over its patterns")
	}
}

func TestParseProfiles(t *testing.T) {
	src := `labels:
  personal: [hacker1db]
profiles:
  Personal Stuff: [claude-code:personal, "codex:personal-chatgpt"]
  foundry:
    label: Azure Foundry
    match:
      - work-foundry
  claude:
    match: [claude-code:*]
`
	c, err := Parse(src, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Warning != "" {
		t.Errorf("warning = %q", c.Warning)
	}
	want := []ProfileSet{
		{Name: "personal-stuff", Label: "Personal Stuff", Patterns: []string{"claude-code:personal", "codex:personal-chatgpt"}},
		{Name: "foundry", Label: "Azure Foundry", Patterns: []string{"work-foundry"}},
		{Name: "claude", Label: "claude", Patterns: []string{"claude-code:*"}},
	}
	if !reflect.DeepEqual(c.Profiles, want) {
		t.Errorf("profiles =\n %+v\nwant\n %+v", c.Profiles, want)
	}
	if got := c.Classify("", "hacker1db/dotfiles"); got.Label != "personal" {
		t.Errorf("owner rule = %+v", got)
	}

	long := strings.Repeat("x", 50)
	sets, err := parseProfiles(mustYAML(t, "p:\n  label: "+long+"\n  match: [a]\n"))
	if err != nil || len(sets) != 1 || len([]rune(sets[0].Label)) != 40 {
		t.Errorf("long label: %v %+v", err, sets)
	}
	if sets, err := parseProfiles(nil); err != nil || sets != nil {
		t.Errorf("nil = %v %v", sets, err)
	}
}

func mustYAML(t *testing.T, src string) any {
	t.Helper()
	v, err := ParseYAML(src)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParseProfilesRejects(t *testing.T) {
	var many strings.Builder
	for i := range MaxProfileSets + 1 {
		many.WriteString("  p" + string(rune('a'+i)) + ": [x]\n")
	}
	for name, body := range map[string]string{
		"not a map":   "  - a\n",
		"bad name":    "  \"!!\": [a]\n",
		"duplicate":   "  Work: [a]\n  work: [b]\n",
		"empty list":  "  a: []\n",
		"no match":    "  a:\n    label: A\n",
		"unknown key": "  a:\n    label: A\n    patterns: [x]\n",
		"bad glob":    "  a: [\"claude-code:[\"]\n",
		"two colons":  "  a: [a:b:c]\n",
		"not strings": "  a:\n    match:\n      x: y\n",
		"too many":    many.String(),
	} {
		_, err := parseProfiles(mustYAML(t, "profiles:\n"+body).(*Map).Get("profiles"))
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestParseProfilesErrorIsWarning(t *testing.T) {
	src := "labels:\n  personal: [hacker1db]\nprofiles:\n  a: []\n  b: [x]\n"
	c, err := Parse(src, nil, nil)
	if err != nil {
		t.Fatalf("a broken profiles: section failed the parse: %v", err)
	}
	if !strings.Contains(c.Warning, "profile filters ignored") || !strings.Contains(c.Warning, "profiles.a") {
		t.Errorf("warning = %q", c.Warning)
	}
	if strings.Contains(c.Warning, "unknown key") {
		t.Errorf("profiles reported as an unknown key: %q", c.Warning)
	}
	if c.Profiles != nil {
		t.Errorf("profiles kept after an error: %+v", c.Profiles)
	}
	if got := c.Classify("", "hacker1db/dotfiles"); got.Label != "personal" || got.By != ByOwner {
		t.Errorf("ownership rules dropped with the profiles: %+v", got)
	}
}
