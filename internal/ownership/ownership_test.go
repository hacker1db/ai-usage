package ownership

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ai-usage/internal/usage"
)

func TestParseYAMLSubset(t *testing.T) {
	src := `# comment
default: work   # trailing comment
labels:
  personal:
    owners: [me, "hacker1db"]
    paths:
      - ~/.dotfiles
      - '~/Developer/it''s # not a comment'
  shared: [Example-Corp-Shared]
  work:
    repos:
    - example-org/*
  empty:
list:
  - name: a
    v: 1
  - b
`
	doc, err := ParseYAML(src)
	if err != nil {
		t.Fatal(err)
	}
	root := doc.(*Map)
	if !reflect.DeepEqual(root.Keys, []string{"default", "labels", "list"}) || root.Get("default") != "work" {
		t.Fatalf("root = %+v", root)
	}
	labels := root.Get("labels").(*Map)
	if !reflect.DeepEqual(labels.Keys, []string{"personal", "shared", "work", "empty"}) || labels.Get("empty") != nil {
		t.Fatalf("labels = %+v", labels)
	}
	p := labels.Get("personal").(*Map)
	if got := p.Get("owners"); !reflect.DeepEqual(got, []any{"me", "hacker1db"}) {
		t.Errorf("owners = %#v", got)
	}
	if got := p.Get("paths"); !reflect.DeepEqual(got, []any{"~/.dotfiles", "~/Developer/it's # not a comment"}) {
		t.Errorf("paths = %#v", got)
	}
	if got := labels.Get("work").(*Map).Get("repos"); !reflect.DeepEqual(got, []any{"example-org/*"}) {
		t.Errorf("repos at the key's indent = %#v", got)
	}
	list := root.Get("list").([]any)
	if m, ok := list[0].(*Map); !ok || m.Get("name") != "a" || m.Get("v") != "1" || list[1] != "b" {
		t.Errorf("list = %#v", list)
	}
}

func TestParseYAMLRejects(t *testing.T) {
	for name, src := range map[string]string{
		"tab":       "a:\n\tb: c",
		"duplicate": "a: 1\na: 2",
		"anchor":    "a: &x 1",
		"block str": "a: |\n  text",
		"flow map":  "a: {b: 1}",
		"bad list":  "a: [b, c",
		"dedent":    "a:\n    b: 1\n  c: 2",
		"two docs":  "a: 1\n---\nb: 2",
	} {
		if _, err := ParseYAML(src); err == nil {
			t.Errorf("%s: parsed without error", name)
		} else if !strings.Contains(err.Error(), "line ") {
			t.Errorf("%s: error %q has no line number", name, err)
		}
	}
}

func TestGHHostsShape(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", dir)
	hosts := "github.com:\n    git_protocol: https\n    users:\n        CyberDSO:\n        hacker1db:\n            oauth_token: x\n    user: CyberDSO\n"
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := GHAccounts(); !reflect.DeepEqual(got, []string{"CyberDSO", "hacker1db"}) {
		t.Fatalf("GHAccounts = %v", got)
	}
}

func TestParseRemote(t *testing.T) {
	cases := map[string][3]string{
		"git@github.com:hacker1db/Dotfiles.git":                             {"github.com", "hacker1db", "Dotfiles"},
		"git@github-hacker1db:hacker1db/SecondBrain.git":                    {"github-hacker1db", "hacker1db", "SecondBrain"},
		"https://github.com/example-org/repo":                               {"github.com", "example-org", "repo"},
		"ssh://git@github.com:22/o/r.git":                                   {"github.com", "o", "r"},
		"example-org/ExampleAPI":                                            {"", "example-org", "ExampleAPI"},
		"my-work-org@vs-ssh.visualstudio.com:v3/my-work-org/Proj/Team.wiki": {"vs-ssh.visualstudio.com", "my-work-org", "Team.wiki"},
		"my-work-org/Example_Project/Team.wiki":                             {"dev.azure.com", "my-work-org", "Team.wiki"},
		"https://dev.azure.com/org/proj/_git/repo":                          {"dev.azure.com", "org", "repo"},
		"":            {"", "", ""},
		"just-a-name": {"", "", ""},
	}
	for in, want := range cases {
		if h, o, n := ParseRemote(in); h != want[0] || o != want[1] || n != want[2] {
			t.Errorf("ParseRemote(%q) = %q, %q, %q; want %v", in, h, o, n, want)
		}
	}
	for host, want := range map[string]bool{"": true, "github.com": true, "github-hacker1db": true,
		"vs-ssh.visualstudio.com": false, "gitlab.com": false, "github.example.com": false} {
		if IsGitHub(host) != want {
			t.Errorf("IsGitHub(%q) = %v", host, !want)
		}
	}
}

// stubTypes answers lookups from a map and counts calls.
func stubTypes(path string, answers map[string]string, calls *int) *Types {
	t := NewTypes(path)
	t.lookup = func(_ context.Context, owner string) (string, error) {
		*calls++
		if typ, ok := answers[owner]; ok {
			return typ, nil
		}
		return "", errors.New("not found")
	}
	return t
}

func TestTypesCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owners.json")
	calls := 0
	ty := stubTypes(path, map[string]string{"hacker1db": TypeUser, "example-org": TypeOrg}, &calls)
	if ty.Of("hacker1db") != TypeUser || ty.Of("HACKER1DB") != TypeUser || ty.Of("example-org") != TypeOrg {
		t.Fatal("wrong types")
	}
	if ty.Of("ghost") != "" || ty.Of("ghost") != "" {
		t.Fatal("unknown owner got a type")
	}
	if calls != 3 { // the repeat and the failed retry are both cached
		t.Errorf("lookups = %d, want 3", calls)
	}
	must(t, ty.Save())
	calls = 0
	again := stubTypes(path, nil, &calls)
	if again.Of("hacker1db") != TypeUser || calls != 0 {
		t.Errorf("cache file not reused: calls = %d", calls)
	}
}

// repo makes a git checkout at dir with the given origin, or a bare-clone layout (repo/.bare plus
// a repo/main worktree) when bare is true; it returns the working directory.
func repo(t *testing.T, dir, origin string, bare bool) string {
	t.Helper()
	cfg := "[core]\n\tbare = false\n[remote \"upstream\"]\n\turl = git@github.com:other/x.git\n[remote \"origin\"]\n\turl = " + origin + "\n"
	if !bare {
		must(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
		must(t, os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o644))
		return dir
	}
	wt := filepath.Join(dir, ".bare", "worktrees", "main")
	must(t, os.MkdirAll(wt, 0o755))
	must(t, os.MkdirAll(filepath.Join(dir, "main", "sub"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, ".bare", "config"), []byte(cfg), 0o644))
	must(t, os.WriteFile(filepath.Join(wt, "commondir"), []byte("../..\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: ./.bare\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "main", ".git"), []byte("gitdir: "+wt+"\n"), 0o644))
	return filepath.Join(dir, "main", "sub")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestClassify(t *testing.T) {
	home := t.TempDir()
	old := usage.Home
	usage.Home = home
	t.Cleanup(func() { usage.Home = old })

	dots := repo(t, filepath.Join(home, ".dotfiles"), "git@github-hacker1db:hacker1db/Dotfiles.git", false)
	mine := repo(t, filepath.Join(home, "Developer", "skills"), "git@github.com:hacker1db/skills.git", true)
	tool := repo(t, filepath.Join(home, "Developer", "clitools"), "git@github.com:hacker1db/clitools.git", false)
	team := repo(t, filepath.Join(home, "Developer", "team", "api"), "https://github.com/example-org/api", false)
	shared := repo(t, filepath.Join(home, "Developer", "dotnet"), "git@github.com:Example-Corp-Shared/dotnet.git", false)
	scratch := filepath.Join(home, "Downloads", "test")
	byDir := filepath.Join(home, "src", "hacker1db", "notes")
	must(t, os.MkdirAll(scratch, 0o755))
	must(t, os.MkdirAll(byDir, 0o755))

	calls := 0
	types := stubTypes("", map[string]string{"hacker1db": TypeUser, "example-org": TypeOrg,
		"Example-Corp-Shared": TypeOrg}, &calls)
	adoOrg := repo(t, filepath.Join(home, "Developer", "wiki"), "my-work-org@vs-ssh.visualstudio.com:v3/my-work-org/P/w", false)

	t.Run("built-in", func(t *testing.T) {
		c := Load(filepath.Join(home, "missing.yaml"), types)
		if !c.Auto || !c.ByType || !reflect.DeepEqual(c.Labels, []string{Personal, Org}) {
			t.Fatalf("Auto = %+v", c)
		}
		for dir, want := range map[string]Result{
			mine:                                 {"hacker1db", Personal, ByType},
			"~" + strings.TrimPrefix(dots, home): {"hacker1db", Personal, ByType},
			team:                                 {"example-org", Org, ByType},
			adoOrg:                               {"my-work-org", Org, ByDefault}, // Azure DevOps: never looked up on GitHub
			scratch:                              {"", Org, ByDefault},
		} {
			if got := c.Classify(dir, ""); got != want {
				t.Errorf("Classify(%s) = %+v, want %+v", dir, got, want)
			}
		}
		calls = 0
		c.Classify(adoOrg, "")
		c.Classify(adoOrg, "my-work-org/P/w")
		if calls != 0 {
			t.Errorf("non-GitHub owner was looked up %d times", calls)
		}
	})

	t.Run("exceptions on top of the built-in rule", func(t *testing.T) {
		c, err := Parse("labels:\n  shared: [Example-Corp-Shared]\n  personal:\n    paths: [~/Downloads]\n", nil, types)
		if err != nil || !c.ByType {
			t.Fatalf("Parse: %v %+v", err, c)
		}
		if !reflect.DeepEqual(c.Labels, []string{"shared", Personal, Org}) {
			t.Errorf("labels = %v", c.Labels)
		}
		for dir, want := range map[string]string{shared: "shared", mine: Personal, team: Org, scratch: Personal} {
			if got := c.Classify(dir, ""); got.Label != want {
				t.Errorf("Classify(%s) = %+v, want %s", dir, got, want)
			}
		}
	})

	t.Run("config", func(t *testing.T) {
		cfg := `default: Work
labels:
  personal:
    owners: [me]
    paths: [~/Downloads]
  shared: [Example-Corp-Shared]
  work:
    repos: [hacker1db/clitools]
    paths: [~/Downloads/test/client]
`
		c, err := Parse(cfg, []string{"hacker1db"}, types)
		if err != nil || c.Warning != "" {
			t.Fatalf("Parse: %v %q", err, c.Warning)
		}
		if !reflect.DeepEqual(c.Labels, []string{"personal", "shared", "work"}) {
			t.Errorf("labels = %v", c.Labels)
		}
		client := filepath.Join(scratch, "client", "x")
		for dir, want := range map[string]Result{
			mine:    {"hacker1db", "personal", ByOwner},
			tool:    {"hacker1db", "work", ByRepo}, // repos beats owners
			shared:  {"Example-Corp-Shared", "shared", ByOwner},
			team:    {"example-org", "work", ByDefault},
			scratch: {"", "personal", ByPath},
			client:  {"", "work", ByPath}, // the longer path wins
		} {
			if got := c.Classify(dir, ""); got != want {
				t.Errorf("Classify(%s) = %+v, want %+v", dir, got, want)
			}
		}
		// A tool-reported repository wins over the directory's remote.
		if got := c.Classify(team, "hacker1db/skills"); got.Label != "personal" {
			t.Errorf("reported repo ignored: %+v", got)
		}
		s := usage.SessionRow{Path: "~" + strings.TrimPrefix(shared, home)}
		c.Apply(&s)
		if s.Owner != "Example-Corp-Shared" || s.Class != "shared" {
			t.Errorf("Apply = %q %q", s.Owner, s.Class)
		}
	})
}

func TestParseConfigErrors(t *testing.T) {
	for name, src := range map[string]string{
		"top list":    "- a",
		"bad label":   "labels:\n  \"not/ok\": [x]",
		"unknown key": "labels:\n  work:\n    owner: [x]",
		"relative":    "labels:\n  work:\n    paths: [src/x]",
		"too many":    "labels: {}",
		"nested list": "labels:\n  work: [[x]]",
	} {
		if name == "too many" {
			var b strings.Builder
			b.WriteString("labels:\n")
			for i := range MaxLabels + 1 {
				b.WriteString("  l" + string(rune('a'+i)) + ": [x" + string(rune('a'+i)) + "]\n")
			}
			src = b.String()
		}
		if _, err := Parse(src, nil, nil); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	c, err := Parse("labels:\n  personal: [x]\n  work: [X]\nextra: 1", nil, nil)
	if err != nil || !strings.Contains(c.Warning, "under both") || !strings.Contains(c.Warning, "unknown key extra") {
		t.Errorf("warnings = %v %q", err, c.Warning)
	}
	bad := filepath.Join(t.TempDir(), "p.yaml")
	must(t, os.WriteFile(bad, []byte("labels:\n\tx: y"), 0o644))
	if c := Load(bad, nil); !c.Auto || !strings.Contains(c.Warning, "line 2") {
		t.Errorf("broken file: auto=%v warning=%q", c.Auto, c.Warning)
	}
}
