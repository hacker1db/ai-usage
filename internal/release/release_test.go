package release

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		msg  string
		want Bump
	}{
		{"feat: add x", Minor},
		{"feat(web): add x", Minor},
		{"FEAT: shouty", Minor},
		{"fix: y", Patch},
		{"perf(tui): faster", Patch},
		{"refactor: move", Patch},
		{"build(deps): bump go", Patch},
		{"revert: feat: x", Patch},
		{"docs: readme", None},
		{"chore: tidy", None},
		{"ci: workflow", None},
		{"test: more", None},
		{"style: gofmt", None},
		{"feat!: drop python", Major},
		{"chore!: remove installer", Major},
		{"fix(api)!: rename field", Major},
		{"fix: y\n\nBREAKING CHANGE: flag removed", Major},
		{"docs: y\n\nBREAKING-CHANGE: config moved", Major},
		{"fix: y\n\nmentions BREAKING CHANGE: inline only", Patch},
		{"Initial commit", Patch},
		{"Update README.md", Patch},
		{"feat:no space", Patch},
		{"feat(scope: unclosed", Patch},
		{"", None},
		{"  \n\n", None},
		{"feat: windows\r\n\r\nBREAKING CHANGE: crlf", Major},
	}
	for _, c := range cases {
		if got := Classify(c.msg); got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

func TestParseTag(t *testing.T) {
	for _, bad := range []string{"", "0.2.0", "v0.2", "v0.2.0-rc.1", "v01.2.3", "v1.2.3+meta", "v1.2.3 ", "vX.Y.Z"} {
		if _, err := ParseTag(bad); err == nil {
			t.Errorf("ParseTag(%q) accepted a malformed tag", bad)
		}
	}
	v, err := ParseTag("v10.0.7")
	if err != nil || v != (Version{10, 0, 7}) {
		t.Fatalf("ParseTag(v10.0.7) = %v, %v", v, err)
	}
}

func TestNext(t *testing.T) {
	cases := []struct {
		last  string
		msgs  []string
		force Bump
		want  string
		ok    bool
	}{
		{"v0.2.0", []string{"fix: a"}, None, "v0.2.1", true},
		{"v0.2.0", []string{"fix: a", "feat: b"}, None, "v0.3.0", true},
		{"v0.2.3", []string{"feat!: b"}, None, "v0.3.0", true}, // pre-1.0: breaking bumps minor
		{"v1.2.3", []string{"feat!: b", "fix: c"}, None, "v2.0.0", true},
		{"v1.2.3", []string{"feat: b"}, None, "v1.3.0", true},
		{"v0.2.0", []string{"docs: a", "ci: b"}, None, "v0.2.0", false},
		{"v0.2.0", nil, None, "v0.2.0", false},
		{"v0.2.0", []string{"docs: a"}, Patch, "v0.2.1", true},
		{"v0.2.0", []string{"feat: a"}, Patch, "v0.2.1", true}, // force wins over computed
		{"v0.9.1", nil, Major, "v0.10.0", true},
	}
	for _, c := range cases {
		got, ok, err := Next(c.last, c.msgs, c.force)
		if err != nil || ok != c.ok || got.String() != c.want {
			t.Errorf("Next(%q, %q, %v) = %v, %v, %v; want %s, %v", c.last, c.msgs, c.force, got, ok, err, c.want, c.ok)
		}
	}
	if _, _, err := Next("latest", []string{"feat: a"}, None); err == nil {
		t.Error("Next accepted a malformed last tag")
	}
}

func TestLess(t *testing.T) {
	order := []string{"v0.0.1", "v0.1.0", "v0.2.0", "v0.10.0", "v1.0.0", "v1.0.10", "v2.0.0"}
	for i := 1; i < len(order); i++ {
		a, _ := ParseTag(order[i-1])
		b, _ := ParseTag(order[i])
		if !a.Less(b) || b.Less(a) {
			t.Errorf("want %s < %s", a, b)
		}
	}
}

func TestParseBump(t *testing.T) {
	for _, s := range []string{"patch", "minor", "major"} {
		if b, err := ParseBump(s); err != nil || b.String() != s {
			t.Errorf("ParseBump(%q) = %v, %v", s, b, err)
		}
	}
	if _, err := ParseBump("auto"); err == nil {
		t.Error("ParseBump accepted auto")
	}
}
