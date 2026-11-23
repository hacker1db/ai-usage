package view

import (
	"net/url"
	"regexp"
	"strings"

	"ai-usage/internal/history"
	"ai-usage/internal/usage"
)

type Group struct {
	Key   string
	Label string
	Slot  int
}

var Groups = []Group{
	{"claude-code:personal", "Claude · personal", 1},
	{"claude-code:work-subscription", "Claude · work", 2},
	{"claude-code:work-foundry", "Claude · Foundry", 3},
	{"codex:personal-chatgpt", "Codex · personal", 4},
	{"codex:work-foundry", "Codex · Foundry", 5},
	{"copilot:github-copilot", "Copilot", 6},
	{"claude-code:legacy", "Claude · legacy", 7},
	{"antigravity:google", "Antigravity", 8},
	{"claude-code:no-profile", "Claude · no profile", 0},
}

var Other = Group{"other", "Other", 0}

var groupByKey = func() map[string]Group {
	m := map[string]Group{}
	for _, g := range Groups {
		m[g.Key] = g
	}
	return m
}()

func GroupOf(tool, profile string) Group {
	if g, ok := groupByKey[tool+":"+profile]; ok {
		return g
	}
	return Other
}

func DetailGroupOf(tool, profile string) Group {
	g := GroupOf(tool, profile)
	if g.Key != Other.Key {
		return g
	}
	label := tool
	if tool == usage.ToolGateway {
		label = "Gateway"
	}
	if profile != "" {
		label += " · " + profile
	}
	return Group{Key: tool + ":" + profile, Label: label, Slot: Other.Slot}
}

func (g Group) order() int {
	if g.Slot == 0 {
		return len(Groups)
	}
	return g.Slot - 1
}

type Option struct{ ID, Label string }

type Tab struct {
	Option
	Match func(tool, profile string) bool
}

var Tabs = []Tab{
	{Option{"all", "All"}, func(t, _ string) bool { return t != usage.ToolProxy }},
	{Option{"claude", "Claude"}, func(t, _ string) bool { return t == usage.ToolClaude }},
	{Option{"codex", "Codex"}, func(t, _ string) bool { return t == usage.ToolCodex }},
	{Option{"copilot", "Copilot"}, func(t, _ string) bool { return t == usage.ToolCopilot }},
	{Option{"antigravity", "Antigravity"}, func(t, _ string) bool { return t == usage.ToolAntigravity }},
	{Option{"foundry", "Foundry"}, func(t, p string) bool { return p == "work-foundry" && t != usage.ToolProxy }},
	{Option{"gateway", "Gateway"}, func(t, _ string) bool { return t == usage.ToolGateway }},
}

type Metric struct {
	Option
	Unit string
}

var Metrics = []Metric{
	{Option{"total", "Total tokens"}, "tokens"},
	{Option{"uncached", "Uncached"}, "uncached tokens"},
	{Option{"output", "Output"}, "output tokens"},
	{Option{"requests", "Requests"}, "requests"},
}

func (m Metric) Row(r history.HourRow) float64 { return m.Of(r.Metrics) }

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

type Range struct {
	Option
	Hours  int
	Bucket int
}

var Ranges = []Range{
	{Option{"24h", "24h"}, 24, 1},
	{Option{"7d", "7d"}, 24 * 7, 4},
	{Option{"30d", "30d"}, 24 * 30, 24},
}

type Burn struct {
	Option
	Hours int
}

var Burns = []Burn{{Option{"1", "1h"}, 1}, {Option{"3", "3h"}, 3}, {Option{"6", "6h"}, 6}}

var SessionSorts = []Option{{"recent", "Recent"}, {"tokens", "Tokens"}, {"active", "Active time"}}

type Filters struct {
	Tab, Metric, Range, Burn, Sort, Query string
	Own                                   string
	Profile                               string
}

var (
	ownRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)
	profileRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,63}$`)
)

var Defaults = Filters{Tab: "all", Metric: "total", Range: "7d", Burn: "3", Sort: "recent"}

func FromQuery(q url.Values) Filters {
	f := Filters{Tab: q.Get("tab"), Metric: q.Get("metric"), Range: q.Get("range"), Burn: q.Get("burn"),
		Sort: q.Get("sort"), Query: strings.TrimSpace(q.Get("q")), Own: q.Get("own"), Profile: q.Get("profile")}
	return f.Normalize()
}

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
		f.Own = ""
	}
	if f.Profile = strings.ToLower(f.Profile); !profileRe.MatchString(f.Profile) {
		f.Profile = ""
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

func (f Filters) TabOf() Tab { return find(Tabs, f.Tab, func(t Tab) Option { return t.Option }) }
func (f Filters) MetricOf() Metric {
	return find(Metrics, f.Metric, func(m Metric) Option { return m.Option })
}
func (f Filters) RangeOf() Range {
	return find(Ranges, f.Range, func(r Range) Option { return r.Option })
}
func (f Filters) BurnOf() Burn { return find(Burns, f.Burn, func(b Burn) Option { return b.Option }) }

func Cycle(opts []Option, current string, step int) string {
	for i, o := range opts {
		if o.ID == current {
			return opts[((i+step)%len(opts)+len(opts))%len(opts)].ID
		}
	}
	return opts[0].ID
}

func TabOptions() []Option    { return options(Tabs, func(t Tab) Option { return t.Option }) }
func MetricOptions() []Option { return options(Metrics, func(m Metric) Option { return m.Option }) }
func RangeOptions() []Option  { return options(Ranges, func(r Range) Option { return r.Option }) }
func BurnOptions() []Option   { return options(Burns, func(b Burn) Option { return b.Option }) }
