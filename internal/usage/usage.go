// Package usage aggregates token usage from AI tool logs into rows keyed by
// (bucket, tool, profile, model) and per-session summaries.
package usage

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// SlotMinutes is the activity granularity: active time counts distinct 5-minute slots with
// activity, so idle gaps inside a long session don't count.
const SlotMinutes = 5

// Caps on per-session detail, matching what the dashboard shows.
const (
	MaxPrompts  = 8
	MaxCommands = 10
	MaxRefs     = 10
)

// Tool names as they appear in rows.
const (
	ToolClaude  = "claude-code"
	ToolCodex   = "codex"
	ToolCopilot = "copilot"
	ToolProxy   = "foundry-proxy" // cross-check only; overlaps Codex work-foundry
)

// Slot returns the activity slot of t.
func Slot(t time.Time) int64 { return t.Unix() / (SlotMinutes * 60) }

// Metrics are the token counters of a row. Input includes cached and cache-write tokens for every
// tool, so Total = Input + Output. Reasoning is a subset of Output.
type Metrics struct {
	Requests   int64 `json:"requests"`
	Input      int64 `json:"input"`
	Cached     int64 `json:"cached"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
	Total      int64 `json:"total"`
}

// Add accumulates o into m.
func (m *Metrics) Add(o Metrics) {
	m.Requests += o.Requests
	m.Input += o.Input
	m.Cached += o.Cached
	m.CacheWrite += o.CacheWrite
	m.Output += o.Output
	m.Reasoning += o.Reasoning
	m.Total += o.Total
}

// Uncached is input that wasn't a cache read, plus output.
func (m Metrics) Uncached() int64 { return max(m.Input-m.Cached, 0) + m.Output }

// Record is one model response's usage as read from a tool log.
type Record struct {
	Model      string
	Time       time.Time
	Session    string // "" when the source has no sessions (proxy log)
	Input      int64  // includes cached and cache-write tokens
	Cached     int64
	CacheWrite int64
	Output     int64
	Reasoning  int64
	CopilotAIU float64
}

// Granularity is the row bucket size.
type Granularity int

const (
	Daily Granularity = iota
	Hourly
)

// Key identifies an aggregated row. Bucket is a local date (2006-01-02) or hour (2006-01-02T15:00).
type Key struct {
	Bucket  string
	Tool    string
	Profile string
	Model   string
}

// Row is an aggregated usage row.
type Row struct {
	Key
	Metrics
	Sessions   int
	CopilotAIU float64
}

type rowAcc struct {
	m        Metrics
	sessions map[SessionKey]struct{}
	aiu      float64
}

// SessionKey identifies a session across tools and profiles.
type SessionKey struct {
	Tool    string
	Profile string
	ID      string
}

// String is the "tool|profile|id" form used by the history store and URLs.
func (k SessionKey) String() string { return k.Tool + "|" + k.Profile + "|" + k.ID }

// Meta is session metadata merged from tool logs. For scalar fields the first non-empty value
// wins; lists keep the first unique values up to their caps; counters add up.
type Meta struct {
	Title, TitleSrc, Prompt, Cwd, Repo, Branch string
	Slots                                      map[int64]struct{}
	Prompts, Commands, Refs                    []string
	Files, Tools                               Counter
}

type session struct {
	requests, total, uncached, output int64
	models                            Counter
	slots                             map[int64]struct{}
	first, last                       time.Time
	meta                              Meta
}

// Aggregator collects records newer than Cutoff into rows and sessions.
type Aggregator struct {
	Cutoff   time.Time
	Bucket   Granularity
	rows     map[Key]*rowAcc
	sessions map[SessionKey]*session
}

// NewAggregator returns an empty aggregator.
func NewAggregator(cutoff time.Time, bucket Granularity) *Aggregator {
	return &Aggregator{Cutoff: cutoff, Bucket: bucket, rows: map[Key]*rowAcc{}, sessions: map[SessionKey]*session{}}
}

func (a *Aggregator) session(k SessionKey) *session {
	s := a.sessions[k]
	if s == nil {
		s = &session{slots: map[int64]struct{}{}}
		a.sessions[k] = s
	}
	return s
}

// HasSession reports whether a session has usage in the window.
func (a *Aggregator) HasSession(k SessionKey) bool {
	_, ok := a.sessions[k]
	return ok
}

// Add records one response; it reports false when the record is outside the window.
func (a *Aggregator) Add(tool, profile string, r Record) bool {
	if r.Time.IsZero() || r.Time.Before(a.Cutoff) {
		return false
	}
	local := r.Time.Local()
	bucket := local.Format("2006-01-02")
	if a.Bucket == Hourly {
		bucket = local.Format("2006-01-02T15") + ":00"
	}
	model := r.Model
	if model == "" {
		model = "unknown"
	}
	k := Key{Bucket: bucket, Tool: tool, Profile: profile, Model: model}
	acc := a.rows[k]
	if acc == nil {
		acc = &rowAcc{sessions: map[SessionKey]struct{}{}}
		a.rows[k] = acc
	}
	acc.m.Add(Metrics{Requests: 1, Input: r.Input, Cached: r.Cached, CacheWrite: r.CacheWrite,
		Output: r.Output, Reasoning: r.Reasoning, Total: r.Input + r.Output})
	acc.aiu += r.CopilotAIU
	if r.Session != "" {
		sk := SessionKey{tool, profile, r.Session}
		acc.sessions[sk] = struct{}{}
		s := a.session(sk)
		s.requests++
		s.total += r.Input + r.Output
		s.uncached += max(r.Input-r.Cached, 0) + r.Output
		s.output += r.Output
		s.models.Add(model, 1)
		s.slots[Slot(r.Time)] = struct{}{}
		if s.first.IsZero() || r.Time.Before(s.first) {
			s.first = r.Time
		}
		if s.last.IsZero() || r.Time.After(s.last) {
			s.last = r.Time
		}
	}
	return true
}

// Note merges metadata into a session (creating it if needed).
func (a *Aggregator) Note(k SessionKey, m Meta) {
	if k.ID == "" {
		return
	}
	s := a.session(k)
	for x := range m.Slots {
		s.slots[x] = struct{}{}
	}
	s.meta.Prompts = appendUnique(s.meta.Prompts, m.Prompts, MaxPrompts)
	s.meta.Commands = appendUnique(s.meta.Commands, m.Commands, MaxCommands)
	s.meta.Refs = appendUnique(s.meta.Refs, m.Refs, MaxRefs)
	s.meta.Files.Merge(&m.Files)
	s.meta.Tools.Merge(&m.Tools)
	first(&s.meta.Title, m.Title)
	first(&s.meta.TitleSrc, m.TitleSrc)
	first(&s.meta.Prompt, m.Prompt)
	first(&s.meta.Cwd, m.Cwd)
	first(&s.meta.Repo, m.Repo)
	first(&s.meta.Branch, m.Branch)
}

func first(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}

func appendUnique(dst, src []string, limit int) []string {
outer:
	for _, v := range src {
		if v == "" || len(dst) >= limit {
			continue
		}
		for _, have := range dst {
			if have == v {
				continue outer
			}
		}
		dst = append(dst, v)
	}
	return dst
}

// Rows returns the aggregated rows sorted by key.
func (a *Aggregator) Rows() []Row {
	out := make([]Row, 0, len(a.rows))
	for k, acc := range a.rows {
		out = append(out, Row{Key: k, Metrics: acc.m, Sessions: len(acc.sessions), CopilotAIU: acc.aiu})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key.Less(out[j].Key) })
	return out
}

// Less orders keys by bucket, tool, profile, model.
func (k Key) Less(o Key) bool {
	if k.Bucket != o.Bucket {
		return k.Bucket < o.Bucket
	}
	if k.Tool != o.Tool {
		return k.Tool < o.Tool
	}
	if k.Profile != o.Profile {
		return k.Profile < o.Profile
	}
	return k.Model < o.Model
}

// SessionSet returns the distinct sessions behind a group of rows, for rollups.
func (a *Aggregator) SessionSet(match func(Key) bool) map[SessionKey]struct{} {
	out := map[SessionKey]struct{}{}
	for k, acc := range a.rows {
		if match(k) {
			for s := range acc.sessions {
				out[s] = struct{}{}
			}
		}
	}
	return out
}

// Count is a (name, n) pair, encoded as a two-element JSON array like Python's Counter.most_common.
type Count struct {
	Name string
	N    int
}

func (c Count) MarshalJSON() ([]byte, error) { return json.Marshal([2]any{c.Name, c.N}) }

func (c *Count) UnmarshalJSON(b []byte) error {
	var pair []json.RawMessage
	if err := json.Unmarshal(b, &pair); err != nil || len(pair) != 2 {
		return fmt.Errorf("count: want [name, n], got %s", b)
	}
	if err := json.Unmarshal(pair[0], &c.Name); err != nil {
		return err
	}
	var n Int
	_ = n.UnmarshalJSON(pair[1])
	c.N = int(n)
	return nil
}

// Detail is what happened in a session: prompts, files changed, commands, tools, refs, models.
// Every string was redacted when it was read.
type Detail struct {
	Prompts    []string `json:"prompts"`
	Files      []Count  `json:"files"`
	FilesTotal int      `json:"files_total"`
	Commands   []string `json:"commands"`
	Tools      []Count  `json:"tools"`
	Refs       []string `json:"refs"`
	Models     []Count  `json:"models"`
}

// SessionRow is a flat per-session summary. The JSON layout matches the history store written by
// the earlier Python dashboard, so existing data/hourly.json files keep working.
type SessionRow struct {
	Tool      string  `json:"tool"`
	Profile   string  `json:"profile"`
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	TitleSrc  string  `json:"title_src"`
	Project   string  `json:"project"`
	Path      string  `json:"path"`
	Repo      string  `json:"repo"`
	Branch    string  `json:"branch"`
	First     string  `json:"first"`
	Last      string  `json:"last"`
	ActiveMin int     `json:"active_min"`
	Requests  int64   `json:"requests"`
	Total     int64   `json:"total"`
	Uncached  int64   `json:"uncached"`
	Output    int64   `json:"output"`
	Model     string  `json:"model"`
	Owner     string  `json:"owner,omitempty"` // repository owner, set by the ownership classifier
	Class     string  `json:"class,omitempty"` // ownership label (personal, work, ...), not stored in history
	Detail    *Detail `json:"detail,omitempty"`
}

// Key returns the session's key.
func (s SessionRow) Key() SessionKey { return SessionKey{s.Tool, s.Profile, s.ID} }

// LastTime parses Last.
func (s SessionRow) LastTime() time.Time { return ParseTime(s.Last) }

// SessionRows returns sessions with usage in the window, newest activity first.
func (a *Aggregator) SessionRows(withDetail bool) []SessionRow {
	out := make([]SessionRow, 0, len(a.sessions))
	for k, s := range a.sessions {
		if s.requests == 0 {
			continue
		}
		title, src := s.meta.Title, s.meta.TitleSrc
		if title == "" && s.meta.Prompt != "" {
			title, src = s.meta.Prompt, "prompt"
		}
		path := ""
		if s.meta.Cwd != "" {
			path = Tilde(s.meta.Cwd)
		}
		model := ""
		if top := s.models.MostCommon(1); len(top) > 0 {
			model = top[0].Name
		}
		row := SessionRow{
			Tool: k.Tool, Profile: k.Profile, ID: k.ID, Title: title, TitleSrc: src,
			Project: ProjectOf(s.meta.Cwd, s.meta.Repo), Path: path, Repo: s.meta.Repo, Branch: s.meta.Branch,
			First: s.first.Format(time.RFC3339), Last: s.last.Format(time.RFC3339),
			ActiveMin: len(s.slots) * SlotMinutes,
			Requests:  s.requests, Total: s.total, Uncached: s.uncached, Output: s.output, Model: model,
		}
		if withDetail {
			row.Detail = &Detail{
				Prompts: nonNil(s.meta.Prompts), Files: s.meta.Files.MostCommon(25), FilesTotal: s.meta.Files.Len(),
				Commands: nonNil(s.meta.Commands), Tools: s.meta.Tools.MostCommon(12), Refs: nonNil(s.meta.Refs),
				Models: s.models.MostCommon(4),
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Last != out[j].Last {
			return out[i].Last > out[j].Last
		}
		return out[i].Key().String() < out[j].Key().String()
	})
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Counter counts names and remembers first-seen order for stable ties, like collections.Counter.
// The zero value is ready to use.
type Counter struct {
	order []string
	n     map[string]int
}

// Add adds n to name.
func (c *Counter) Add(name string, n int) {
	if c.n == nil {
		c.n = map[string]int{}
	}
	if _, ok := c.n[name]; !ok {
		c.order = append(c.order, name)
	}
	c.n[name] += n
}

// Merge adds every count of o.
func (c *Counter) Merge(o *Counter) {
	if o == nil {
		return
	}
	for _, k := range o.order {
		c.Add(k, o.n[k])
	}
}

// Len is the number of distinct names.
func (c *Counter) Len() int { return len(c.order) }

// MostCommon returns up to limit counts, largest first; ties keep first-seen order.
func (c *Counter) MostCommon(limit int) []Count {
	out := make([]Count, 0, len(c.order))
	for _, k := range c.order {
		out = append(out, Count{k, c.n[k]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].N > out[j].N })
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
