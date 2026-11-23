package release

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type Action string

const (
	Create Action = "create"

	Existing Action = "existing"

	Skip Action = "skip"

	Superseded Action = "superseded"
)

type Plan struct {
	Action   Action
	Tag      string
	Previous string
	Commit   string
	Reason   string
}

type Git struct{ Dir string }

func (g Git) run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (g Git) versionTags(extra ...string) (map[string]Version, error) {
	out, err := g.run(append([]string{"tag", "--list", "v*"}, extra...)...)
	if err != nil {
		return nil, err
	}
	tags := map[string]Version{}
	for _, t := range strings.Fields(out) {
		if v, err := ParseTag(t); err == nil {
			tags[t] = v
		}
	}
	return tags, nil
}

func newest(tags map[string]Version) (string, bool) {
	best, found := "", false
	for t, v := range tags {
		if !found || tags[best].Less(v) {
			best, found = t, true
		}
	}
	return best, found
}

func MakePlan(g Git, rev string, force Bump) (Plan, error) {
	out, err := g.run("rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return Plan{}, err
	}
	p := Plan{Commit: strings.TrimSpace(out)}

	all, err := g.versionTags()
	if err != nil {
		return Plan{}, err
	}
	latest, ok := newest(all)
	if !ok {
		return Plan{}, errors.New("no vMAJOR.MINOR.PATCH tag found; publish the first release by hand")
	}

	at, err := g.versionTags("--points-at", p.Commit)
	if err != nil {
		return Plan{}, err
	}
	if t, ok := newest(at); ok {
		p.Action, p.Tag, p.Previous = Existing, t, t
		p.Reason = fmt.Sprintf("%s already points at this commit", t)
		return p, nil
	}

	if _, err := g.run("merge-base", "--is-ancestor", latest, p.Commit); err != nil {
		p.Action, p.Previous = Superseded, latest
		p.Reason = fmt.Sprintf("%s is newer and not contained in this commit", latest)
		return p, nil
	}
	p.Previous = latest

	log, err := g.run("log", "--no-merges", "--format=%B%x1e", latest+".."+p.Commit)
	if err != nil {
		return Plan{}, err
	}
	var msgs []string
	for _, m := range strings.Split(log, "\x1e") {
		if m = strings.TrimSpace(m); m != "" {
			msgs = append(msgs, m)
		}
	}

	next, due, err := Next(latest, msgs, force)
	if err != nil {
		return Plan{}, err
	}
	if !due {
		p.Action = Skip
		p.Reason = fmt.Sprintf("%d commit(s) since %s, none releasable", len(msgs), latest)
		return p, nil
	}
	p.Action, p.Tag = Create, next.String()
	bump := force
	if bump == None {
		bump = Max(msgs)
	}
	p.Reason = fmt.Sprintf("%s bump from %d commit(s) since %s", bump, len(msgs), latest)
	return p, nil
}
