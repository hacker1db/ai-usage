package ownership

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"ai-usage/internal/usage"
)

const (
	Personal = "personal"
	Org      = "org"
)

const MaxLabels = 8

const Me = "me"

const (
	ByRepo    = "repo"
	ByPath    = "path"
	ByOwner   = "owner"
	ByDir     = "dir"
	ByType    = "type"
	ByDefault = "default"
)

type Result struct {
	Owner string
	Label string
	By    string
}

type Classifier struct {
	Default string
	Labels  []string
	Source  string
	Warning string
	Auto    bool
	ByType  bool

	Profiles []ProfileSet

	types  *Types
	owners map[string]string
	repos  []repoRule
	paths  []pathRule

	mu      sync.Mutex
	remotes map[string]string
}

type repoRule struct{ pattern, label string }
type pathRule struct{ prefix, label string }

var labelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

func normLabel(s string) (string, bool) {
	s = strings.ToLower(strings.Join(strings.Fields(s), "-"))
	return s, labelRe.MatchString(s)
}

func newClassifier(t *Types) *Classifier {
	return &Classifier{types: t, owners: map[string]string{}, remotes: map[string]string{}}
}

const typeRuleDesc = "GitHub user repos are personal, org repos and everything else are org"

func Load(cfgPath string, t *Types) *Classifier {
	src, err := os.ReadFile(cfgPath)
	if errors.Is(err, fs.ErrNotExist) || cfgPath == "" {
		return Auto(t)
	}
	if err != nil {
		c := Auto(t)
		c.Warning = fmt.Sprintf("%s: %v; using the built-in rule", usage.Tilde(cfgPath), err)
		return c
	}
	c, err := Parse(string(src), GHAccounts(), t)
	if err != nil {
		a := Auto(t)
		a.Warning = fmt.Sprintf("%s: %v; using the built-in rule", usage.Tilde(cfgPath), err)
		return a
	}
	c.Source = usage.Tilde(cfgPath)
	if c.ByType {
		c.Source += " (then " + typeRuleDesc + ")"
	}
	return c
}

func Auto(t *Types) *Classifier {
	c := newClassifier(t)
	c.Auto, c.ByType = true, true
	c.Default = Org
	c.Labels = []string{Personal, Org}
	c.Source = "built-in: " + typeRuleDesc
	return c
}

func Parse(src string, me []string, t *Types) (*Classifier, error) {
	doc, err := ParseYAML(src)
	if err != nil {
		return nil, err
	}
	root, ok := doc.(*Map)
	if doc == nil {
		root, ok = &Map{Vals: map[string]any{}}, true
	}
	if !ok {
		return nil, errors.New("the top level must be a mapping with the keys default, labels, and profiles")
	}
	c := newClassifier(t)
	var warn []string
	for _, k := range root.Keys {
		if k != "default" && k != "labels" && k != "profiles" {
			warn = append(warn, "unknown key "+k)
		}
	}

	if c.Profiles, err = parseProfiles(root.Get("profiles")); err != nil {
		warn = append(warn, err.Error()+" (profile filters ignored)")
	}
	c.Default, c.ByType = Org, true
	if v, ok := root.Get("default").(string); ok {
		c.ByType = false
		l, valid := normLabel(v)
		if !valid {
			return nil, fmt.Errorf("default: %q isn't a usable label (letters, digits, . _ -, up to 32)", v)
		}
		c.Default = l
	}
	labels, ok := root.Get("labels").(*Map)
	if !ok && root.Get("labels") != nil {
		return nil, errors.New("labels: must map each label to owners, repos, or paths")
	}
	if labels != nil {
		for _, raw := range labels.Keys {
			l, valid := normLabel(raw)
			if !valid {
				return nil, fmt.Errorf("labels: %q isn't a usable label (letters, digits, . _ -, up to 32)", raw)
			}
			if slices.Contains(c.Labels, l) {
				return nil, fmt.Errorf("labels: %q is listed twice", l)
			}
			c.Labels = append(c.Labels, l)
			w, err := c.addRules(l, labels.Get(raw), me)
			if err != nil {
				return nil, fmt.Errorf("labels.%s: %w", raw, err)
			}
			warn = append(warn, w...)
		}
	}
	extra := []string{c.Default}
	if c.ByType {
		extra = []string{Personal, Org}
	}
	for _, l := range extra {
		if !slices.Contains(c.Labels, l) {
			c.Labels = append(c.Labels, l)
		}
	}
	if len(c.Labels) > MaxLabels {
		return nil, fmt.Errorf("%d labels; use at most %d", len(c.Labels), MaxLabels)
	}

	sort.SliceStable(c.paths, func(i, j int) bool { return len(c.paths[i].prefix) > len(c.paths[j].prefix) })
	c.Warning = strings.Join(warn, "; ")
	return c, nil
}

func (c *Classifier) addRules(label string, v any, me []string) ([]string, error) {
	lists := map[string][]string{}
	switch x := v.(type) {
	case nil:
		return nil, nil
	case []any, string:
		s, err := strs(x)
		if err != nil {
			return nil, err
		}
		lists["owners"] = s
	case *Map:
		for _, k := range x.Keys {
			if k != "owners" && k != "repos" && k != "paths" {
				return nil, fmt.Errorf("unknown key %q (use owners, repos, or paths)", k)
			}
			s, err := strs(x.Get(k))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			lists[k] = s
		}
	default:
		return nil, errors.New("expected a list of owners or a mapping of owners/repos/paths")
	}
	var warn []string
	for _, o := range lists["owners"] {
		names := []string{o}
		if strings.EqualFold(o, Me) {
			names = me
			if len(me) == 0 {
				warn = append(warn, label+": \"me\" matched no gh login")
			}
		}
		for _, n := range names {
			k := strings.ToLower(n)
			if prev, dup := c.owners[k]; dup && prev != label {
				warn = append(warn, fmt.Sprintf("owner %s is under both %s and %s; using %s", n, prev, label, prev))
				continue
			}
			c.owners[k] = label
		}
	}
	for _, r := range lists["repos"] {
		p := strings.ToLower(strings.TrimSuffix(r, ".git"))
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("repos: bad pattern %q", r)
		}
		c.repos = append(c.repos, repoRule{p, label})
	}
	for _, p := range lists["paths"] {
		abs := filepath.Clean(usage.ExpandHome(p))
		if !filepath.IsAbs(abs) {
			return nil, fmt.Errorf("paths: %q must be absolute or start with ~/", p)
		}
		c.paths = append(c.paths, pathRule{abs, label})
	}
	return warn, nil
}

func strs(v any) ([]string, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{x}, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, errors.New("list items must be plain strings")
			}
			out = append(out, strings.TrimSpace(s))
		}
		return out, nil
	}
	return nil, errors.New("expected a list")
}

func (c *Classifier) Classify(dir, repo string) Result {
	abs := ""
	if dir != "" {
		abs = filepath.Clean(usage.ExpandHome(dir))
	}
	if repo == "" && abs != "" {
		repo = c.remoteOf(abs)
	}
	host, owner, name := ParseRemote(repo)
	r := Result{Owner: owner, Label: c.Default, By: ByDefault}
	if name != "" {
		slug := strings.ToLower(owner + "/" + name)
		for _, rr := range c.repos {
			if ok, _ := path.Match(rr.pattern, slug); ok {
				r.Label, r.By = rr.label, ByRepo
				return r
			}
			if ok, _ := path.Match(rr.pattern, strings.ToLower(name)); ok && !strings.Contains(rr.pattern, "/") {
				r.Label, r.By = rr.label, ByRepo
				return r
			}
		}
	}
	if abs != "" {
		for _, pr := range c.paths {
			if abs == pr.prefix || strings.HasPrefix(abs, pr.prefix+string(filepath.Separator)) {
				r.Label, r.By = pr.label, ByPath
				return r
			}
		}
	}
	if l, ok := c.owners[strings.ToLower(owner)]; ok && owner != "" {
		r.Label, r.By = l, ByOwner
		return r
	}

	if rel, err := filepath.Rel(usage.Home, abs); abs != "" && err == nil && !strings.HasPrefix(rel, "..") {
		for _, seg := range strings.Split(rel, string(filepath.Separator)) {
			if l, ok := c.owners[strings.ToLower(seg)]; ok {
				r.Label, r.By = l, ByDir
				return r
			}
		}
	}
	if c.ByType && owner != "" && IsGitHub(host) {
		switch c.types.Of(owner) {
		case TypeUser:
			r.Label, r.By = Personal, ByType
		case TypeOrg:
			r.Label, r.By = Org, ByType
		}
	}
	return r
}

func (c *Classifier) Apply(s *usage.SessionRow) {
	r := c.Classify(s.Path, s.Repo)
	s.Owner, s.Class = r.Owner, r.Label
}

func (c *Classifier) remoteOf(dir string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u, ok := c.remotes[dir]; ok {
		return u
	}
	u := ""
	for p := dir; p != "/" && p != "." && p != usage.Home && len(p) > 1; p = filepath.Dir(p) {
		if gd := gitDir(p); gd != "" {
			u = remoteURL(filepath.Join(gd, "config"))
			break
		}
	}
	c.remotes[dir] = u
	return u
}

func gitDir(p string) string {
	dotgit := filepath.Join(p, ".git")
	st, err := os.Stat(dotgit)
	if err != nil {
		return ""
	}
	if st.IsDir() {
		return dotgit
	}
	b, err := os.ReadFile(dotgit)
	if err != nil {
		return ""
	}
	gd, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return ""
	}
	gd = strings.TrimSpace(gd)
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(p, gd)
	}
	if cd, err := os.ReadFile(filepath.Join(gd, "commondir")); err == nil {
		common := strings.TrimSpace(string(cd))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gd, common)
		}
		return filepath.Clean(common)
	}
	return gd
}

func remoteURL(cfg string) string {
	b, err := os.ReadFile(cfg)
	if err != nil {
		return ""
	}
	section, first := "", ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if !strings.HasPrefix(section, `[remote "`) {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "url" {
			continue
		}
		v = strings.TrimSpace(v)
		if section == `[remote "origin"]` {
			return v
		}
		if first == "" {
			first = v
		}
	}
	return first
}

func ParseRemote(u string) (host, owner, name string) {
	s := strings.TrimSpace(u)
	if s == "" {
		return "", "", ""
	}
	var p string
	slug := false
	switch {
	case strings.Contains(s, "://"):
		s = s[strings.Index(s, "://")+3:]
		host, p, _ = strings.Cut(s, "/")
	case strings.Contains(s, ":") && !strings.Contains(s[:strings.Index(s, ":")], "/"):
		host, p, _ = strings.Cut(s, ":")
	default:
		p, slug = s, true
	}
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	p = strings.Trim(strings.TrimSuffix(strings.Trim(p, "/"), ".git"), "/")
	segs := strings.Split(p, "/")
	if len(segs) > 0 && segs[0] == "v3" {
		segs = segs[1:]
	}
	if len(segs) < 2 || segs[0] == "" || segs[len(segs)-1] == "" {
		return "", "", ""
	}
	if slug && len(segs) > 2 {
		host = "dev.azure.com"
	}
	return host, segs[0], segs[len(segs)-1]
}

func GHAccounts() []string {
	dir := os.Getenv("GH_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(usage.Home, ".config", "gh")
	}
	b, err := os.ReadFile(filepath.Join(dir, "hosts.yml"))
	if err != nil {
		return nil
	}
	doc, err := ParseYAML(string(b))
	if err != nil {
		return nil
	}
	hosts, _ := doc.(*Map)
	var out []string
	add := func(s string) {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if hosts == nil {
		return nil
	}
	for _, h := range hosts.Keys {
		hm, _ := hosts.Get(h).(*Map)
		if hm == nil {
			continue
		}
		if u, ok := hm.Get("user").(string); ok {
			add(u)
		}
		if us, ok := hm.Get("users").(*Map); ok {
			for _, u := range us.Keys {
				add(u)
			}
		}
	}
	return out
}
