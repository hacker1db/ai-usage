// Package view turns a snapshot plus the viewer's filters into typed chart models. The web and
// terminal renderers draw the same models, so both show the same numbers.
package view

import (
	"net/url"
	"regexp"
	"strings"

	"ai-usage/internal/history"
	"ai-usage/internal/usage"
)

// Group is a tool:profile series. Slot is its fixed categorical color slot (1-8; 0 = Other).
// Color follows the entity, never its rank, so filtering never repaints a series.
type Group struct {
	Key   string
	Label string
	Slot  int
}

// Groups in their fixed palette order (validated adjacent-pair CVD safe in both modes).
var Groups = []Group{
	{"claude-code:personal", "Claude · personal", 1},
	{"claude-code:work-subscription", "Claude · work", 2},
	{"claude-code:work-foundry", "Claude · Foundry", 3},
	{"codex:personal-chatgpt", "Codex · personal", 4},
	{"codex:work-foundry", "Codex · Foundry", 5},
	{"copilot:github-copilot", "Copilot", 6},
	{"claude-code:legacy", "Claude · legacy", 7},
	{"claude-code:no-profile", "Claude · no profile", 8},
}

// Other collects any tool:profile without a slot.
var Other = Group{"other", "Other", 0}

var groupByKey = func() map[string]Group {
	m := map[string]Group{}
	for _, g := range Groups {
		m[g.Key] = g
	}
	return m
}()

// GroupOf returns the series a tool/profile belongs to.
func GroupOf(tool, profile string) Group {
	if g, ok := groupByKey[tool+":"+profile]; ok {
		return g
	}
	return Other
}

// order returns a group's position in the fixed order (Other last).
func (g Group) order() int {
	if g.Slot == 0 {
		return len(Groups)
	}
	return g.Slot - 1
}

// Option is a selectable control value.
type Option struct{ ID, Label string }

// Tab filters rows to a tool or profile family.
type Tab struct {
	Option
	Match func(tool, profile string) bool
}

// Tabs in display order.
var Tabs = []Tab{
	{Option{"all", "All"}, func(t, _ string) bool { return t != usage.ToolProxy }},
	{Option{"claude", "Claude"}, func(t, _ string) bool { return t == usage.ToolClaude }},
	{Option{"codex", "Codex"}, func(t, _ string) bool { return t == usage.ToolCodex }},
	{Option{"copilot", "Copilot"}, func(t, _ string) bool { return t == usage.ToolCopilot }},
	{Option{"foundry", "Foundry"}, func(t, p string) bool { return p == "work-foundry" && t != usage.ToolProxy }},
}

// Metric is what the charts count.
type Metric struct {
	Option
	Unit string
}

// Metrics in display order.
var Metrics = []Metric{
	{Option{"total", "Total tokens"}, "tokens"},
	{Option{"uncached", "Uncached"}, "uncached tokens"},
	{Option{"output", "Output"}, "output tokens"},
	{Option{"requests", "Requests"}, "requests"},
}

// Row returns the metric's value for an hourly row.
func (m Metric) Row(r history.HourRow) float64 { return m.Of(r.Metrics) }

// Of returns the metric's value for a set of counters.
func (m Metric) Of(x usage.Metrics) float64 {
	switch m.ID {
	case "uncached":
		return float64(x.Uncached())
	case "output":
		return float64(x.Output)
	case "requests":
		return float64(x.Requests)
	}
	return float64(x.Total)
}

// Session returns the metric's value for a session summary.
func (m Metric) Session(s usage.SessionRow) float64 {
	switch m.ID {
	case "uncached":
		return float64(s.Uncached)
	case "output":
		return float64(s.Output)
	case "requests":
		return float64(s.Requests)
	}
	return float64(s.Total)
}

// Range is a time window and its bucket size in hours.
type Range struct {
	Option
	Hours  int
	Bucket int
}

// Ranges in display order.
var Ranges = []Range{
	{Option{"24h", "24h"}, 24, 1},
	{Option{"7d", "7d"}, 24 * 7, 4},
	{Option{"30d", "30d"}, 24 * 30, 24},
}

// Burn is a trailing window for burn rates.
type Burn struct {
	Option
	Hours int
}

// Burns in display order.
var Burns = []Burn{{Option{"1", "1h"}, 1}, {Option{"3", "3h"}, 3}, {Option{"6", "6h"}, 6}}

// SessionSorts in display order.
var SessionSorts = []Option{{"recent", "Recent"}, {"tokens", "Tokens"}, {"active", "Active time"}}

// Filters are the viewer's selections. Unknown values fall back to the defaults.
type Filters struct {
	Tab, Metric, Range, Burn, Sort, Query string
	Own                                   string // ownership label filter for the Work section; "" = all
	Profile                               string // profile filter: a profiles: set name or a tool:profile key; "" = all
}

var (
	ownRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)
	profileRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,63}$`)
)

// Defaults are the initial selections.
var Defaults = Filters{Tab: "all", Metric: "total", Range: "7d", Burn: "3", Sort: "recent"}

// FromQuery reads filters from URL query parameters.
func FromQuery(q url.Values) Filters {
	f := Filters{Tab: q.Get("tab"), Metric: q.Get("metric"), Range: q.Get("range"), Burn: q.Get("burn"),
		Sort: q.Get("sort"), Query: strings.TrimSpace(q.Get("q")), Own: q.Get("own"), Profile: q.Get("profile")}
	return f.Normalize()
}

// Link encodes non-default filters, with overrides applied, for links.
func (f Filters) Link(override ...string) string {
	g := f
	for i := 0; i+1 < len(override); i += 2 {
		v := override[i+1]
		switch override[i] {
		case "tab":
			g.Tab = v
		case "metric":
			g.Metric = v
		case "range":
			g.Range = v
		case "burn":
			g.Burn = v
		case "sort":
			g.Sort = v
		case "own":
			g.Own = v
		case "profile":
			g.Profile = v
		}
	}
	q := url.Values{}
	set := func(k, v, d string) {
		if v != d {
			q.Set(k, v)
		}
	}
	set("tab", g.Tab, Defaults.Tab)
	set("metric", g.Metric, Defaults.Metric)
	set("range", g.Range, Defaults.Range)
	set("burn", g.Burn, Defaults.Burn)
	set("sort", g.Sort, Defaults.Sort)
	set("profile", g.Profile, "")
	set("own", g.Own, "")
	set("q", g.Query, "")
	if len(q) == 0 {
		return "?"
	}
	return "?" + q.Encode()
}

// Normalize replaces unknown values with defaults.
func (f Filters) Normalize() Filters {
	pick := func(v string, opts []Option, d string) string {
		for _, o := range opts {
			if o.ID == v {
				return v
			}
		}
		return d
	}
	f.Tab = pick(f.Tab, options(Tabs, func(t Tab) Option { return t.Option }), Defaults.Tab)
	f.Metric = pick(f.Metric, options(Metrics, func(m Metric) Option { return m.Option }), Defaults.Metric)
	f.Range = pick(f.Range, options(Ranges, func(r Range) Option { return r.Option }), Defaults.Range)
	f.Burn = pick(f.Burn, options(Burns, func(b Burn) Option { return b.Option }), Defaults.Burn)
	f.Sort = pick(f.Sort, SessionSorts, Defaults.Sort)
	if f.Own = strings.ToLower(f.Own); !ownRe.MatchString(f.Own) {
		f.Own = "" // labels come from projects.yaml; Build drops one that isn't configured
	}
	if f.Profile = strings.ToLower(f.Profile); !profileRe.MatchString(f.Profile) {
		f.Profile = "" // Build drops a name that isn't a configured set or a profile with data
	}
	if len(f.Query) > 200 {
		f.Query = f.Query[:200]
	}
	return f
}

func options[T any](xs []T, get func(T) Option) []Option {
	out := make([]Option, len(xs))
	for i, x := range xs {
		out[i] = get(x)
	}
	return out
}

func find[T any](xs []T, id string, get func(T) Option) T {
	for _, x := range xs {
		if get(x).ID == id {
			return x
		}
	}
	return xs[0]
}

// TabOf, MetricOf, RangeOf, and BurnOf resolve the selected values.
func (f Filters) TabOf() Tab { return find(Tabs, f.Tab, func(t Tab) Option { return t.Option }) }
func (f Filters) MetricOf() Metric {
	return find(Metrics, f.Metric, func(m Metric) Option { return m.Option })
}
func (f Filters) RangeOf() Range {
	return find(Ranges, f.Range, func(r Range) Option { return r.Option })
}
func (f Filters) BurnOf() Burn { return find(Burns, f.Burn, func(b Burn) Option { return b.Option }) }

// Cycle returns the option after current (wrapping), for keyboard controls.
func Cycle(opts []Option, current string, step int) string {
	for i, o := range opts {
		if o.ID == current {
			return opts[((i+step)%len(opts)+len(opts))%len(opts)].ID
		}
	}
	return opts[0].ID
}

// TabOptions, MetricOptions, RangeOptions, and BurnOptions list the controls.
func TabOptions() []Option    { return options(Tabs, func(t Tab) Option { return t.Option }) }
func MetricOptions() []Option { return options(Metrics, func(m Metric) Option { return m.Option }) }
func RangeOptions() []Option  { return options(Ranges, func(r Range) Option { return r.Option }) }
func BurnOptions() []Option   { return options(Burns, func(b Burn) Option { return b.Option }) }
