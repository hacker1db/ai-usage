package release

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Bump int

const (
	None Bump = iota
	Patch
	Minor
	Major
)

func (b Bump) String() string {
	return [...]string{"none", "patch", "minor", "major"}[b]
}

func ParseBump(s string) (Bump, error) {
	switch s {
	case "patch":
		return Patch, nil
	case "minor":
		return Minor, nil
	case "major":
		return Major, nil
	}
	return None, fmt.Errorf("unknown bump %q (want patch, minor, or major)", s)
}

type Version struct{ Major, Minor, Patch int }

var tagRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func ParseTag(tag string) (Version, error) {
	m := tagRE.FindStringSubmatch(tag)
	if m == nil {
		return Version{}, fmt.Errorf("tag %q is not of the form vMAJOR.MINOR.PATCH", tag)
	}
	var v Version
	var err error
	for i, p := range []*int{&v.Major, &v.Minor, &v.Patch} {
		if *p, err = strconv.Atoi(m[i+1]); err != nil {
			return Version{}, fmt.Errorf("tag %q: %w", tag, err)
		}
	}
	return v, nil
}

func (v Version) String() string { return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch) }

func (v Version) Less(w Version) bool {
	if v.Major != w.Major {
		return v.Major < w.Major
	}
	if v.Minor != w.Minor {
		return v.Minor < w.Minor
	}
	return v.Patch < w.Patch
}

func (v Version) Apply(b Bump) Version {
	switch b {
	case Major:
		if v.Major == 0 {
			return Version{0, v.Minor + 1, 0}
		}
		return Version{v.Major + 1, 0, 0}
	case Minor:
		return Version{v.Major, v.Minor + 1, 0}
	case Patch:
		return Version{v.Major, v.Minor, v.Patch + 1}
	}
	return v
}

var (
	headerRE   = regexp.MustCompile(`^([a-zA-Z]+)(\([^()]*\))?(!)?: \S`)
	breakingRE = regexp.MustCompile(`(?m)^BREAKING[ -]CHANGE: \S`)
	noRelease  = map[string]bool{"docs": true, "chore": true, "ci": true, "test": true, "style": true}
)

func Classify(message string) Bump {
	message = strings.TrimSpace(strings.ReplaceAll(message, "\r\n", "\n"))
	if message == "" {
		return None
	}
	subject, _, _ := strings.Cut(message, "\n")
	m := headerRE.FindStringSubmatch(subject)
	if m != nil && m[3] == "!" || breakingRE.MatchString(message) {
		return Major
	}
	if m == nil {
		return Patch
	}
	typ := strings.ToLower(m[1])
	switch {
	case typ == "feat":
		return Minor
	case noRelease[typ]:
		return None
	}
	return Patch
}

func Max(messages []string) Bump {
	b := None
	for _, m := range messages {
		if c := Classify(m); c > b {
			b = c
		}
	}
	return b
}

func Next(last string, messages []string, force Bump) (Version, bool, error) {
	v, err := ParseTag(last)
	if err != nil {
		return Version{}, false, err
	}
	b := force
	if b == None {
		b = Max(messages)
	}
	if b == None {
		return v, false, nil
	}
	return v.Apply(b), true, nil
}
