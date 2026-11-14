package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testRepo is a throwaway repository isolated from the user's git config, hooks, and signing keys.
type testRepo struct {
	t   *testing.T
	dir string
	n   int
}

func newRepo(t *testing.T) *testRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	r := &testRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{
		"-c", "user.name=t", "-c", "user.email=t@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "core.hooksPath=/dev/null",
	}, args...)...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *testRepo) commit(msg string) string {
	r.t.Helper()
	r.n++
	if err := os.WriteFile(filepath.Join(r.dir, "f"), []byte{byte(r.n)}, 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.git("add", "f")
	r.git("commit", "-q", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

func (r *testRepo) plan(rev string, force Bump) Plan {
	r.t.Helper()
	p, err := MakePlan(Git{Dir: r.dir}, rev, force)
	if err != nil {
		r.t.Fatalf("MakePlan(%s): %v", rev, err)
	}
	return p
}

func TestMakePlanCreates(t *testing.T) {
	r := newRepo(t)
	r.commit("feat: first")
	r.git("tag", "-a", "-m", "v0.2.0", "v0.2.0")
	r.git("tag", "not-a-version")
	r.git("tag", "v9.9.9-rc.1") // pre-release style tags are ignored
	r.commit("fix: a")
	head := r.commit("feat(web): b")

	p := r.plan("HEAD", None)
	if p.Action != Create || p.Tag != "v0.3.0" || p.Previous != "v0.2.0" || p.Commit != head {
		t.Fatalf("plan = %+v", p)
	}
	if p := r.plan("HEAD", Patch); p.Tag != "v0.2.1" {
		t.Fatalf("forced patch plan = %+v", p)
	}
}

func TestMakePlanIgnoresMergeSubjects(t *testing.T) {
	r := newRepo(t)
	r.commit("feat: first")
	r.git("tag", "v0.2.0")
	r.git("switch", "-q", "-c", "topic")
	r.commit("docs: only docs")
	r.git("switch", "-q", "main")
	r.git("merge", "-q", "--no-ff", "-m", "Merge pull request #9 from x/topic", "topic")
	if p := r.plan("HEAD", None); p.Action != Skip {
		t.Fatalf("a merge of docs-only work must not release: %+v", p)
	}
}

func TestMakePlanSkipsNonReleasable(t *testing.T) {
	r := newRepo(t)
	r.commit("feat: first")
	r.git("tag", "v0.2.0")
	r.commit("docs: readme")
	r.commit("ci: workflow")
	p := r.plan("HEAD", None)
	if p.Action != Skip || p.Tag != "" {
		t.Fatalf("plan = %+v", p)
	}
	if p := r.plan("HEAD", Minor); p.Action != Create || p.Tag != "v0.3.0" {
		t.Fatalf("forced minor plan = %+v", p)
	}
}

func TestMakePlanExistingTagIsIdempotent(t *testing.T) {
	r := newRepo(t)
	r.commit("feat: first")
	r.git("tag", "v0.2.0")
	r.commit("feat: second")
	r.git("tag", "v0.3.0") // a re-run after the release, or a hand-pushed signed tag
	for _, force := range []Bump{None, Major} {
		p := r.plan("HEAD", force)
		if p.Action != Existing || p.Tag != "v0.3.0" {
			t.Fatalf("force=%v plan = %+v", force, p)
		}
	}
}

func TestMakePlanSuperseded(t *testing.T) {
	r := newRepo(t)
	r.commit("feat: first")
	r.git("tag", "v0.2.0")
	old := r.commit("fix: a")
	r.commit("fix: b")
	r.git("tag", "v0.2.1")
	if p := r.plan(old, None); p.Action != Superseded || p.Previous != "v0.2.1" {
		t.Fatalf("plan = %+v", p)
	}
}

func TestMakePlanUsesNewestReachableTag(t *testing.T) {
	r := newRepo(t)
	r.commit("feat: first")
	r.git("tag", "v0.9.0")
	r.commit("fix: a")
	r.git("tag", "v0.10.0") // numeric, not lexical, order
	r.commit("fix: b")
	if p := r.plan("HEAD", None); p.Tag != "v0.10.1" || p.Previous != "v0.10.0" {
		t.Fatalf("plan = %+v", p)
	}
}

func TestMakePlanErrors(t *testing.T) {
	r := newRepo(t)
	r.commit("feat: first")
	if _, err := MakePlan(Git{Dir: r.dir}, "HEAD", None); err == nil || !strings.Contains(err.Error(), "first release") {
		t.Fatalf("untagged repo: err = %v", err)
	}
	r.git("tag", "v0.1.0")
	if _, err := MakePlan(Git{Dir: r.dir}, "no-such-rev", None); err == nil {
		t.Fatal("bad rev accepted")
	}

}
