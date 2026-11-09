package ownership

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// MaxProfileSets caps the named profile filters; more than this crowds the filter bar.
const MaxProfileSets = 12

// ProfileSet is a named dashboard filter over tool:profile series, from the profiles: key of
// projects.yaml. A pattern is "tool:profile" or a bare "profile" (any tool); both take * globs.
type ProfileSet struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Patterns []string `json:"patterns"`
}

// MatchProfile reports whether a tool/profile pair matches one pattern.
func MatchProfile(pattern, tool, profile string) bool {
	p := strings.ToLower(pattern)
	if strings.Contains(p, ":") {
		ok, _ := path.Match(p, strings.ToLower(tool+":"+profile))
		return ok
	}
	ok, _ := path.Match(p, strings.ToLower(profile))
	return ok
}

// Match reports whether a tool/profile pair belongs to the set.
func (s ProfileSet) Match(tool, profile string) bool {
	for _, p := range s.Patterns {
		if MatchProfile(p, tool, profile) {
			return true
		}
	}
	return false
}

// parseProfiles reads the profiles: mapping. Each entry is a list of patterns, or a mapping with
// match: (the patterns) and an optional label: (the display name).
//
//	profiles:
//	  personal: [claude-code:personal, codex:personal-chatgpt]
//	  foundry:
//	    label: Azure Foundry
//	    match: [work-foundry]
func parseProfiles(v any) ([]ProfileSet, error) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(*Map)
	if !ok {
		return nil, errors.New("profiles: must map each filter name to a list of tool:profile patterns")
	}
	var out []ProfileSet
	seen := map[string]bool{}
	for _, raw := range m.Keys {
		name, valid := normLabel(raw)
		if !valid {
			return nil, fmt.Errorf("profiles: %q isn't a usable name (letters, digits, . _ -, up to 32)", raw)
		}
		if seen[name] {
			return nil, fmt.Errorf("profiles: %q is listed twice", name)
		}
		seen[name] = true
		set := ProfileSet{Name: name, Label: strings.TrimSpace(raw)}
		var pats any
		switch x := m.Get(raw).(type) {
		case *Map:
			for _, k := range x.Keys {
				if k != "label" && k != "match" {
					return nil, fmt.Errorf("profiles.%s: unknown key %q (use label or match)", raw, k)
				}
			}
			if l, ok := x.Get("label").(string); ok && strings.TrimSpace(l) != "" {
				set.Label = strings.TrimSpace(l)
			}
			pats = x.Get("match")
		default:
			pats = x
		}
		list, err := strs(pats)
		if err != nil {
			return nil, fmt.Errorf("profiles.%s: %w", raw, err)
		}
		if len(list) == 0 {
			return nil, fmt.Errorf("profiles.%s: list at least one tool:profile pattern", raw)
		}
		for _, p := range list {
			if _, err := path.Match(strings.ToLower(p), ""); err != nil || strings.Count(p, ":") > 1 {
				return nil, fmt.Errorf("profiles.%s: bad pattern %q", raw, p)
			}
		}
		if r := []rune(set.Label); len(r) > 40 {
			set.Label = string(r[:40])
		}
		set.Patterns = list
		out = append(out, set)
	}
	if len(out) > MaxProfileSets {
		return nil, fmt.Errorf("profiles: %d filters; use at most %d", len(out), MaxProfileSets)
	}
	return out, nil
}
