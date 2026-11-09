package view

import (
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"ai-usage/internal/budget"
	"ai-usage/internal/history"
	"ai-usage/internal/ownership"
	"ai-usage/internal/snapshot"
	"ai-usage/internal/usage"
	"ai-usage/internal/workreport"
)

const hour = time.Hour

// SessionLimit caps the session list.
const SessionLimit = 150

// Stat is a labeled figure.
type Stat struct{ Label, Value string }

// Hero is the headline: the range total plus today, burn, period-to-date, and projection.
type Hero struct {
	Title string
	Value float64
	Unit  string
	Stats []Stat
}

// Cumulative is usage summed through the budget period (or month) with a projection to its end
// at the trailing 7-day average rate.
type Cumulative struct {
	Start, End time.Time
	Now        time.Time
	Hours      int       // hours in the period
	ByHour     []float64 // per-hour usage, len Hours
	Cum        []float64 // running total through each elapsed hour, len elapsed
	Run        float64   // total so far
	ProjRate   float64   // per hour
	ProjEnd    float64   // projected total at End
	Budget     float64   // 0 when no budget applies
	YMax       float64
	Unit       string
	Desc       string
}

// Series is one group's values per bucket.
type Series struct {
	Group  Group
	Values []float64
	Total  float64
}

// Stacked is usage per time bucket, stacked by group.
type Stacked struct {
	Start       time.Time
	N           int
	BucketHours int
	Series      []Series // groups with usage, fixed order
	Totals      []float64
	YMax        float64
	Sub         string
}

// BucketStart returns bucket i's start time.
func (s Stacked) BucketStart(i int) time.Time {
	return s.Start.Add(time.Duration(i*s.BucketHours) * hour)
}

// BucketLabel is the axis label for bucket i.
func (s Stacked) BucketLabel(i int) string {
	at := s.BucketStart(i)
	switch {
	case s.BucketHours == 1:
		return HourLabel(at)
	case s.BucketHours == 24 || at.Hour() == 0:
		return ShortDay(at)
	}
	return HourLabel(at)
}

// BucketTitle is the tooltip heading for bucket i.
func (s Stacked) BucketTitle(i int) string {
	at := s.BucketStart(i)
	if s.BucketHours == 24 {
		return DayLabel(at)
	}
	return HourFull(at) + " – " + HourLabel(at.Add(time.Duration(s.BucketHours)*hour))
}

// Level is a status: good, warning, serious, critical. Never a series color.
type Level string

const (
	Good     Level = "good"
	Warning  Level = "warning"
	Serious  Level = "serious"
	Critical Level = "critical"
)

// Icon is the status glyph that accompanies its color.
func (l Level) Icon() string {
	switch l {
	case Critical:
		return "✕"
	case Serious:
		return "▲"
	case Warning:
		return "!"
	}
	return "✓"
}

// BudgetBar is a profile's budget meter.
type BudgetBar struct {
	Period    budget.Period
	Metric    budget.Metric
	Used      float64
	Tokens    float64
	Projected float64
	End       time.Time
	Level     Level
	Status    string
}

// Pct is used/tokens as 0-100.
func (b BudgetBar) Pct() float64 { return b.Used / b.Tokens * 100 }

// Card is one profile's summary.
type Card struct {
	Group     Group
	Total     float64
	Requests  float64
	Burn      float64
	Share     float64
	Spark     []float64
	TopModels []Named
	Budget    *BudgetBar
}

// Named is a labeled value.
type Named struct {
	Name  string
	Value float64
}

// Heatmap is usage by weekday (Mon-Sun) and hour of day, showing when work happens.
type Heatmap struct {
	Cells [7][24]float64
	Max   float64
	Days  int
	Peak  string
}

// Weekdays labels Heatmap rows.
var Weekdays = [7]string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// Mix is a group's token composition: cache reads, fresh input, and output (cost order).
type Mix struct {
	Group                 Group
	Cached, Fresh, Output float64
}

// Total is the composition's whole.
func (m Mix) Total() float64 { return m.Cached + m.Fresh + m.Output }

// CacheHit is the share of input served from cache.
func (m Mix) CacheHit() float64 {
	if in := m.Cached + m.Fresh; in > 0 {
		return m.Cached / in
	}
	return math.NaN()
}

// SessionView is a session row decorated for display.
type SessionView struct {
	usage.SessionRow
	Group     Group
	Value     float64
	Share     float64 // of the tab's range total
	Last      time.Time
	Workstrm  string // ai-work-report theme, if classified
	Basis     string
	SrcLabel  string // "first prompt" when the title is the first prompt
	SearchKey string
}

// Project is usage by repository, split by group.
type Project struct {
	Name, Path string
	Class      string // ownership label with the most usage
	Owner      string // repository owner, when known
	Total      float64
	Active     float64 // minutes
	Sessions   int
	Parts      []Named // by group key in fixed order
}

// OwnRow is one ownership label's usage in range.
type OwnRow struct {
	Label    string
	Value    float64
	Share    float64 // of all sessions' value in the tab and range
	Sessions int
	Projects int
	Active   float64 // minutes
}

// Ownership is usage split by repository ownership (personal, work, ...). It is computed from
// sessions, so it covers the Work section; the hourly charts above aren't split by repository.
type Ownership struct {
	Rows    []OwnRow // every configured label, in config order
	Total   float64
	Source  string
	Auto    bool
	Default string
	Warning string
}

// ModelRow is a profile/model pair's counters in range.
type ModelRow struct {
	Group Group
	Model string
	usage.Metrics
}

// Theme is a workstream row.
type Theme struct {
	Name, Desc                   string
	Sessions                     int
	Value, Active, Manual, Saved float64
}

// Workstreams is the ai-work-report overlay.
type Workstreams struct {
	Available  bool
	Classified string // when classified
	Matched    int
	InRange    int
	Title      string
	Thesis     string
	Themes     []Theme
	Total      float64
	ReportTime string
	Error      string
	Dir        string
}

// DayPair is the proxy cross-check for one day.
type DayPair struct {
	Day          time.Time
	Proxy, Codex float64
}

// Sources is collector status plus the foundry-entra-proxy cross-check.
type Sources struct {
	Status       []StatusRow
	ProxyTotal   float64
	CodexFoundry float64
	Daily        []DayPair
	Sub          string
	Notes        []string
	Warnings     []string
}

// StatusRow is one collector's status with its level.
type StatusRow struct {
	Source, Path, Label string
	Level               Level
	Requests            int
}

// Dashboard is every chart model for one view.
type Dashboard struct {
	Filters     Filters
	Tab         Tab
	Metric      Metric
	Range       Range
	Burn        Burn
	Now         time.Time
	Generated   time.Time
	Interval    int
	Subtitle    string
	TabCounts   map[string]int
	Hero        Hero
	Cum         Cumulative
	Stack       Stacked
	Cards       []Card
	Heat        Heatmap
	Mix         []Mix
	MixAll      Mix
	Top         []SessionView
	TopSub      string
	Projects    []Project
	Ownership   Ownership
	Sessions    []SessionView
	SessionN    int
	Models      []ModelRow
	Work        Workstreams
	Sources     Sources
	Foot        []string
	Empty       bool
	Profiles    []ProfileOpt // filter choices: configured sets, then single profiles with data
	Profile     ProfileOpt   // the selection; ID "" = all profiles
	ProfileHint string       // how to configure profile sets, shown when none are configured

	match func(tool, profile string) bool // tab and profile filters combined
}

// ProfileOpt is one profile filter choice: a named set from projects.yaml or a single series.
type ProfileOpt struct {
	ID, Label string
	Set       bool    // a configured profiles: set
	Members   []Group // series with data the choice covers, fixed order
	Value     float64 // the metric in range, within the current tab
	patterns  []string
}

// Match reports whether a tool/profile pair passes the filter.
func (o ProfileOpt) Match(tool, profile string) bool {
	switch {
	case o.ID == "":
		return true
	case o.Set:
		return ownership.ProfileSet{Patterns: o.patterns}.Match(tool, profile)
	}
	return GroupOf(tool, profile).Key == o.ID
}

// Slot is the color slot for a single-profile choice (sets have none).
func (o ProfileOpt) Slot() int {
	if o.Set || len(o.Members) != 1 {
		return -1
	}
	return o.Members[0].Slot
}

func floorHour(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func sumSince(rows []history.HourRow, get func(history.HourRow) float64, from, to time.Time) float64 {
	s := 0.0
	for _, r := range rows {
		if !r.T.Before(from) && (to.IsZero() || r.T.Before(to)) {
			s += get(r)
		}
	}
	return s
}

// rate is the hourly rate over the trailing window; the current partial hour counts toward it.
func rate(rows []history.HourRow, get func(history.HourRow) float64, now time.Time, hours int) float64 {
	return sumSince(rows, get, floorHour(now).Add(-time.Duration(hours-1)*hour), time.Time{}) / float64(hours)
}

func metricByID(id budget.Metric) Metric {
	return find(Metrics, string(id), func(m Metric) Option { return m.Option })
}

// Build computes the dashboard for a snapshot, filters, and the current time.
func Build(s *snapshot.Snapshot, f Filters, now time.Time) Dashboard {
	f = f.Normalize()
	d := Dashboard{Filters: f, Tab: f.TabOf(), Metric: f.MetricOf(), Range: f.RangeOf(), Burn: f.BurnOf(),
		Now: now, Generated: s.Meta.Generated, Interval: s.Meta.Interval, TabCounts: map[string]int{}}
	d.Subtitle = s.Meta.TZ + " · since " + DayLabel(s.Meta.Since) + " · data from " + HourFull(s.Meta.Generated)
	from := floorHour(now).Add(-time.Duration(d.Range.Hours-1) * hour)
	d.buildProfiles(s, from)
	d.match = func(t, p string) bool { return d.Tab.Match(t, p) && d.Profile.Match(t, p) }

	for _, t := range Tabs {
		seen := map[string]bool{}
		for _, r := range s.Rows {
			if t.Match(r.Tool, r.Profile) && d.Profile.Match(r.Tool, r.Profile) {
				seen[GroupOf(r.Tool, r.Profile).Key] = true
			}
		}
		d.TabCounts[t.ID] = len(seen)
	}
	var rows []history.HourRow
	groupSet := map[string]bool{}
	for _, r := range s.Rows {
		if d.match(r.Tool, r.Profile) {
			rows = append(rows, r)
			groupSet[GroupOf(r.Tool, r.Profile).Key] = true
		}
	}
	d.Empty = len(rows) == 0
	var groupKeys []string
	for k := range groupSet {
		groupKeys = append(groupKeys, k)
	}

	d.buildHero(s, rows, groupKeys, from)
	d.buildStack(rows)
	d.buildCards(s, rows, from)
	d.buildHeat(rows)
	d.buildMix(rows, from)
	d.buildModels(rows, from)
	sessions := d.sessionsInRange(s, from)
	d.buildTop(sessions, rows, from)
	work := d.buildOwnership(s, sessions)
	d.buildProjects(work)
	d.buildSessions(s, work)
	d.buildWork(s, work)
	d.buildSources(s, from)
	cfg := "No budgets set (see dashboard.example.json)"
	if s.Meta.ConfigExists {
		cfg = "Budgets: " + s.Meta.Config
	}
	d.Foot = []string{"Timezone " + s.Meta.TZ, "History " + itoa(s.Meta.Days) + " days", cfg}
	return d
}

func itoa(n int) string { return Exact(float64(n)) }

// buildProfiles lists the profile filter choices and resolves the selection. Sets come from
// projects.yaml's profiles: key in config order; single profiles follow in palette order, only
// those with data. Values are the metric in range within the current tab.
func (d *Dashboard) buildProfiles(s *snapshot.Snapshot, from time.Time) {
	value := map[string]float64{}
	pairs := map[string][][2]string{} // every tool/profile pair per series with data, to test set patterns
	for _, r := range s.Rows {
		if r.Tool == usage.ToolProxy {
			continue
		}
		k := GroupOf(r.Tool, r.Profile).Key
		if p := [2]string{r.Tool, r.Profile}; !slices.Contains(pairs[k], p) {
			pairs[k] = append(pairs[k], p)
		}
		if d.Tab.Match(r.Tool, r.Profile) && !r.T.Before(from) {
			value[k] += d.Metric.Row(r)
		}
	}
	have := func(k string) bool { _, ok := pairs[k]; return ok }
	order := append(append([]Group(nil), Groups...), Other)
	all := ProfileOpt{Label: "All profiles"}
	for _, g := range order {
		if have(g.Key) {
			all.Members = append(all.Members, g)
			all.Value += value[g.Key]
		}
	}
	d.Profiles = append(d.Profiles, all)
	setNames := map[string]bool{}
	for _, ps := range s.Meta.Ownership.Profiles {
		setNames[ps.Name] = true
		o := ProfileOpt{ID: ps.Name, Label: ps.Label, Set: true, patterns: ps.Patterns}
		// Sum per row, not per series: "Other" can hold pairs a set only partly matches.
		for _, r := range s.Rows {
			if r.Tool != usage.ToolProxy && ps.Match(r.Tool, r.Profile) && d.Tab.Match(r.Tool, r.Profile) && !r.T.Before(from) {
				o.Value += d.Metric.Row(r)
			}
		}
		for _, g := range order {
			if slices.ContainsFunc(pairs[g.Key], func(p [2]string) bool { return ps.Match(p[0], p[1]) }) {
				o.Members = append(o.Members, g)
			}
		}
		d.Profiles = append(d.Profiles, o)
	}
	for _, g := range order {
		// Set names can't contain ":", so only the "other" key can collide; the set wins.
		if have(g.Key) && !setNames[g.Key] {
			d.Profiles = append(d.Profiles, ProfileOpt{ID: g.Key, Label: g.Label, Members: []Group{g}, Value: value[g.Key]})
		}
	}
	d.Profile = all
	for _, o := range d.Profiles {
		if o.ID != "" && o.ID == d.Filters.Profile {
			d.Profile = o
			break
		}
	}
	d.Filters.Profile = d.Profile.ID
	if len(s.Meta.Ownership.Profiles) == 0 {
		d.ProfileHint = "Group profiles into named filters with a profiles: section in projects.yaml (see projects.example.yaml)."
	}
}

// tabBudget sums budgets only when every group in the tab has one with the same period and metric.
func tabBudget(s *snapshot.Snapshot, keys []string) *budget.Budget {
	if len(keys) == 0 {
		return nil
	}
	var sum *budget.Budget
	for _, k := range keys {
		b, ok := s.Budgets[k]
		if !ok {
			return nil
		}
		if sum == nil {
			c := b
			sum = &c
			continue
		}
		if !sum.Same(b) {
			return nil
		}
		sum.Tokens += b.Tokens
	}
	return sum
}

func (d *Dashboard) buildHero(s *snapshot.Snapshot, rows []history.HourRow, keys []string, from time.Time) {
	m, now := d.Metric, d.Now
	b := tabBudget(s, keys)
	get := m.Row
	unit := m.Unit
	start, end := budget.Bounds(now, budget.Month, 1, 0)
	if b != nil {
		start, end = budget.Bounds(now, b.Period, b.ResetDay, b.ResetWeekday)
		if m.ID != "requests" {
			get = metricByID(b.Metric).Row
			unit = string(b.Metric)
		}
	}
	rangeTotal := sumSince(rows, m.Row, from, time.Time{})
	today := sumSince(rows, m.Row, startOfDay(now), time.Time{})
	burn := rate(rows, m.Row, now, d.Burn.Hours)
	projRate := rate(rows, get, now, 24*7)
	toDate := sumSince(rows, get, start, time.Time{})
	left := math.Max(end.Sub(now).Hours(), 0)
	projected := toDate + projRate*left

	title := d.Tab.Label
	if title == "All" {
		title = "All tools"
	}
	if d.Profile.ID != "" {
		title += " · " + d.Profile.Label
	}
	d.Hero = Hero{Title: title + " · last " + d.Range.Label, Value: rangeTotal, Unit: m.Unit}
	period := "Month to date"
	if b != nil {
		period = "Period to date"
	}
	d.Hero.Stats = []Stat{
		{"Today", Compact(today)},
		{"Burn, last " + d.Burn.Label, Compact(burn) + "/h"},
		{period, Compact(toDate)},
		{"Projected by " + ShortDay(end.Add(-time.Second)), Compact(projected)},
	}
	if b != nil {
		pct := toDate / float64(b.Tokens) * 100
		empty := "Not this period"
		switch {
		case toDate >= float64(b.Tokens):
			empty = "Now"
		case projRate > 0:
			at := now.Add(time.Duration((float64(b.Tokens) - toDate) / projRate * float64(hour)))
			if at.Before(end) {
				empty = DayLabel(at)
			}
		}
		d.Hero.Stats = append(d.Hero.Stats, Stat{"Budget used", Pct(pct/100) + " of " + Compact(float64(b.Tokens))},
			Stat{"Budget empty", empty})
	}

	// cumulative curve through the period
	hours := int(math.Ceil(end.Sub(start).Hours()))
	c := Cumulative{Start: start, End: end, Now: now, Hours: hours, ByHour: make([]float64, hours), ProjRate: projRate, Unit: unit}
	for _, r := range rows {
		if i := int(math.Floor(r.T.Sub(start).Hours())); i >= 0 && i < hours {
			c.ByHour[i] += get(r)
		}
	}
	nowIdx := min(int(math.Floor(floorHour(now).Sub(start).Hours())), hours-1)
	for i := 0; i <= nowIdx; i++ {
		c.Run += c.ByHour[i]
		c.Cum = append(c.Cum, c.Run)
	}
	c.ProjEnd = c.Run + projRate*left
	if b != nil {
		c.Budget = float64(b.Tokens)
	}
	c.YMax = NiceMax(math.Max(math.Max(c.ProjEnd, c.Budget), c.Run) * 1.05)
	c.Desc = "Cumulative " + unit + " from " + DayLabel(start) + ": " + Exact(c.Run) + " so far, projected " +
		Exact(c.ProjEnd) + " by " + DayLabel(end.Add(-time.Second)) + " at the 7-day average rate."
	d.Cum = c
}

func (d *Dashboard) buildStack(rows []history.HourRow) {
	r, now := d.Range, d.Now
	n := int(math.Ceil(float64(r.Hours) / float64(r.Bucket)))
	bh := time.Duration(r.Bucket) * hour
	end := floorHour(now).Add(hour)
	switch {
	case r.Bucket == 24:
		end = startOfDay(now).Add(24 * hour)
	case r.Bucket > 1:
		// align multi-hour buckets to local midnight so labels read naturally
		sod := startOfDay(now)
		end = sod.Add(time.Duration(math.Ceil(float64(now.Sub(sod))/float64(bh))) * bh)
	}
	start := end.Add(-time.Duration(n) * bh)
	all := append(append([]Group(nil), Groups...), Other)
	vals := make([][]float64, len(all))
	for i := range vals {
		vals[i] = make([]float64, n)
	}
	for _, row := range rows {
		i := int(math.Floor(float64(row.T.Sub(start)) / float64(bh)))
		if i < 0 || i >= n {
			continue
		}
		vals[GroupOf(row.Tool, row.Profile).order()][i] += d.Metric.Row(row)
	}
	st := Stacked{Start: start, N: n, BucketHours: r.Bucket, Totals: make([]float64, n)}
	for gi, g := range all {
		t := 0.0
		for i, v := range vals[gi] {
			t += v
			st.Totals[i] += v
		}
		if t > 0 {
			st.Series = append(st.Series, Series{g, vals[gi], t})
		}
	}
	mx := 0.0
	for _, t := range st.Totals {
		mx = math.Max(mx, t)
	}
	st.YMax = NiceMax(mx * 1.05)
	per := "hour"
	switch r.Bucket {
	case 1:
	case 24:
		per = "day"
	default:
		per = itoa(r.Bucket) + " hours"
	}
	st.Sub = d.Metric.Unit + " per " + per + ", last " + r.Label
	d.Stack = st
}

func (d *Dashboard) buildCards(s *snapshot.Snapshot, rows []history.HourRow, from time.Time) {
	byGroup := map[string][]history.HourRow{}
	for _, r := range rows {
		k := GroupOf(r.Tool, r.Profile).Key
		byGroup[k] = append(byGroup[k], r)
	}
	all := append(append([]Group(nil), Groups...), Other)
	sum := 0.0
	for _, g := range all {
		gr, ok := byGroup[g.Key]
		if !ok {
			continue
		}
		c := Card{Group: g, Total: sumSince(gr, d.Metric.Row, from, time.Time{}),
			Requests: sumSince(gr, func(r history.HourRow) float64 { return float64(r.Requests) }, from, time.Time{}),
			Burn:     rate(gr, d.Metric.Row, d.Now, d.Burn.Hours)}
		models := map[string]float64{}
		for _, r := range gr {
			if !r.T.Before(from) {
				models[r.Model] += d.Metric.Row(r)
			}
		}
		c.TopModels = topNamed(models, 3)
		for _, se := range d.Stack.Series {
			if se.Group.Key == g.Key {
				c.Spark = se.Values
			}
		}
		if b, ok := s.Budgets[g.Key]; ok {
			c.Budget = budgetBar(gr, d.Now, b)
		}
		sum += c.Total
		d.Cards = append(d.Cards, c)
	}
	for i := range d.Cards {
		if sum > 0 {
			d.Cards[i].Share = d.Cards[i].Total / sum
		} else {
			d.Cards[i].Share = math.NaN()
		}
	}
}

func topNamed(m map[string]float64, n int) []Named {
	out := make([]Named, 0, len(m))
	for k, v := range m {
		out = append(out, Named{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Value != out[j].Value {
			return out[i].Value > out[j].Value
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func budgetBar(rows []history.HourRow, now time.Time, b budget.Budget) *BudgetBar {
	get := metricByID(b.Metric).Row
	start, end := budget.Bounds(now, b.Period, b.ResetDay, b.ResetWeekday)
	used := sumSince(rows, get, start, time.Time{})
	proj := used + rate(rows, get, now, 24*7)*math.Max(end.Sub(now).Hours(), 0)
	bb := &BudgetBar{Period: b.Period, Metric: b.Metric, Used: used, Tokens: float64(b.Tokens), Projected: proj, End: end}
	switch {
	case used >= bb.Tokens:
		bb.Level, bb.Status = Critical, "Over budget"
	case proj > bb.Tokens:
		bb.Level, bb.Status = Serious, "On track to exceed"
	case bb.Pct() > 75:
		bb.Level, bb.Status = Warning, "Above 75%"
	default:
		bb.Level, bb.Status = Good, "Within budget"
	}
	return bb
}

func (d *Dashboard) buildHeat(rows []history.HourRow) {
	days := max(d.Range.Hours/24, 7)
	from := startOfDay(d.Now).AddDate(0, 0, -(days - 1))
	h := Heatmap{Days: days}
	for _, r := range rows {
		if r.T.Before(from) {
			continue
		}
		wd := (int(r.T.Weekday()) + 6) % 7 // Monday first
		h.Cells[wd][r.T.Hour()] += d.Metric.Row(r)
	}
	pw, ph := 0, 0
	for w := range h.Cells {
		for hr, v := range h.Cells[w] {
			if v > h.Max {
				h.Max, pw, ph = v, w, hr
			}
		}
	}
	if h.Max > 0 {
		at := time.Date(2000, 1, 1, ph, 0, 0, 0, time.Local)
		h.Peak = "Busiest: " + Weekdays[pw] + " " + HourLabel(at) + "–" + HourLabel(at.Add(hour)) + " (" + Compact(h.Max) + ")"
	}
	d.Heat = h
}

func (d *Dashboard) buildMix(rows []history.HourRow, from time.Time) {
	by := map[string]*Mix{}
	for _, r := range rows {
		if r.T.Before(from) {
			continue
		}
		g := GroupOf(r.Tool, r.Profile)
		m := by[g.Key]
		if m == nil {
			m = &Mix{Group: g}
			by[g.Key] = m
		}
		m.Cached += float64(r.Cached)
		m.Fresh += float64(max(r.Input-r.Cached, 0))
		m.Output += float64(r.Output)
	}
	d.MixAll = Mix{Group: Group{Key: "all", Label: "All in tab"}}
	for _, g := range append(append([]Group(nil), Groups...), Other) {
		if m, ok := by[g.Key]; ok && m.Total() > 0 {
			d.Mix = append(d.Mix, *m)
			d.MixAll.Cached += m.Cached
			d.MixAll.Fresh += m.Fresh
			d.MixAll.Output += m.Output
		}
	}
}

func (d *Dashboard) buildModels(rows []history.HourRow, from time.Time) {
	type key struct{ g, model string }
	by := map[key]*ModelRow{}
	for _, r := range rows {
		if r.T.Before(from) {
			continue
		}
		g := GroupOf(r.Tool, r.Profile)
		k := key{g.Key, r.Model}
		m := by[k]
		if m == nil {
			m = &ModelRow{Group: g, Model: r.Model}
			by[k] = m
		}
		m.Metrics.Add(r.Metrics)
	}
	for _, m := range by {
		d.Models = append(d.Models, *m)
	}
	sort.Slice(d.Models, func(i, j int) bool {
		if d.Models[i].Total != d.Models[j].Total {
			return d.Models[i].Total > d.Models[j].Total
		}
		return d.Models[i].Model < d.Models[j].Model
	})
}

var titleSrcLabel = map[string]string{"prompt": "first prompt"}

func (d *Dashboard) sessionsInRange(s *snapshot.Snapshot, from time.Time) []SessionView {
	var cls *workreport.Classification
	if s.WorkReport != nil {
		cls = s.WorkReport.Classified
	}
	var out []SessionView
	for _, x := range s.Sessions {
		last := x.LastTime()
		if !d.match(x.Tool, x.Profile) || last.Before(from) {
			continue
		}
		v := SessionView{SessionRow: x, Group: GroupOf(x.Tool, x.Profile), Value: d.Metric.Session(x),
			Last: last.Local(), SrcLabel: titleSrcLabel[x.TitleSrc]}
		if hit, ok := cls.Lookup(x.ID); ok {
			v.Basis = hit.Basis
			switch {
			case hit.Theme != "none":
				v.Workstrm = cls.ThemeName(hit.Theme)
			default:
				v.Workstrm = "housekeeping"
			}
		}
		v.SearchKey = strings.ToLower(strings.Join([]string{x.Title, x.Project, x.Path, x.Branch, x.Repo, x.Model, x.Profile,
			v.Workstrm, x.Class, x.Owner}, " "))
		out = append(out, v)
	}
	return out
}

func (d *Dashboard) buildTop(sessions []SessionView, rows []history.HourRow, from time.Time) {
	total := sumSince(rows, d.Metric.Row, from, time.Time{})
	top := append([]SessionView(nil), sessions...)
	sort.SliceStable(top, func(i, j int) bool { return top[i].Value > top[j].Value })
	if len(top) > 8 {
		top = top[:8]
	}
	covered := 0.0
	for i := range top {
		if total > 0 {
			top[i].Share = top[i].Value / total
		}
		covered += top[i].Value
	}
	d.Top = top
	if total > 0 && len(top) > 0 {
		d.TopSub = "Top " + itoa(len(top)) + " sessions account for " + Pct(math.Min(covered/total, 1)) + " of " +
			d.Metric.Unit + " in the last " + d.Range.Label
	}
}

// buildOwnership totals sessions per ownership label and returns the sessions the Work section
// shows: all of them, or only the selected label's.
func (d *Dashboard) buildOwnership(s *snapshot.Snapshot, sessions []SessionView) []SessionView {
	o := s.Meta.Ownership
	d.Ownership = Ownership{Source: o.Source, Auto: o.Auto, Default: o.Default, Warning: o.Warning}
	idx := map[string]int{}
	add := func(l string) int {
		if i, ok := idx[l]; ok {
			return i
		}
		idx[l] = len(d.Ownership.Rows)
		d.Ownership.Rows = append(d.Ownership.Rows, OwnRow{Label: l})
		return idx[l]
	}
	for _, l := range o.Labels {
		add(l)
	}
	projects := map[string]map[string]bool{}
	for _, x := range sessions {
		if x.Class == "" {
			continue // a snapshot from before ownership existed
		}
		r := &d.Ownership.Rows[add(x.Class)]
		r.Value += x.Value
		r.Sessions++
		r.Active += float64(x.ActiveMin)
		if projects[x.Class] == nil {
			projects[x.Class] = map[string]bool{}
		}
		projects[x.Class][x.Project] = true
		d.Ownership.Total += x.Value
	}
	for i := range d.Ownership.Rows {
		r := &d.Ownership.Rows[i]
		r.Projects = len(projects[r.Label])
		if d.Ownership.Total > 0 {
			r.Share = r.Value / d.Ownership.Total
		}
	}
	if _, ok := idx[d.Filters.Own]; !ok {
		d.Filters.Own = ""
	}
	if d.Filters.Own == "" {
		return sessions
	}
	out := make([]SessionView, 0, len(sessions))
	for _, x := range sessions {
		if x.Class == d.Filters.Own {
			out = append(out, x)
		}
	}
	return out
}

func (d *Dashboard) buildProjects(sessions []SessionView) {
	by := map[string]*Project{}
	parts := map[string]map[string]float64{}
	classes := map[string]map[string]float64{}
	for _, x := range sessions {
		name := x.Project
		if name == "" {
			name = "(unknown)"
		}
		p := by[name]
		if p == nil {
			p = &Project{Name: name, Path: x.Path}
			by[name] = p
			parts[name] = map[string]float64{}
			classes[name] = map[string]float64{}
		}
		if p.Owner == "" {
			p.Owner = x.Owner
		}
		if x.Class != "" {
			classes[name][x.Class] += x.Value + 1e-9 // + epsilon so zero-value sessions still count
		}
		p.Total += x.Value
		p.Active += float64(x.ActiveMin)
		p.Sessions++
		parts[name][x.Group.Key] += x.Value
	}
	list := make([]*Project, 0, len(by))
	for _, p := range by {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Total != list[j].Total {
			return list[i].Total > list[j].Total
		}
		return list[i].Name < list[j].Name
	})
	var rest *Project
	restParts := map[string]float64{}
	for i, p := range list {
		if i < 12 {
			continue
		}
		if rest == nil {
			rest = &Project{}
		}
		rest.Total += p.Total
		rest.Active += p.Active
		rest.Sessions += p.Sessions
		for k, v := range parts[p.Name] {
			restParts[k] += v
		}
	}
	if len(list) > 12 {
		list = list[:12]
	}
	order := append(append([]Group(nil), Groups...), Other)
	split := func(m map[string]float64) []Named {
		var out []Named
		for _, g := range order {
			if v := m[g.Key]; v > 0 {
				out = append(out, Named{g.Key, v})
			}
		}
		return out
	}
	for _, p := range list {
		p.Parts = split(parts[p.Name])
		best := -1.0
		for _, r := range d.Ownership.Rows { // config order breaks ties
			if v, ok := classes[p.Name][r.Label]; ok && v > best {
				p.Class, best = r.Label, v
			}
		}
		d.Projects = append(d.Projects, *p)
	}
	if rest != nil {
		rest.Name = itoa(len(by)-12) + " other projects"
		rest.Parts = split(restParts)
		d.Projects = append(d.Projects, *rest)
	}
}

func (d *Dashboard) buildSessions(s *snapshot.Snapshot, sessions []SessionView) {
	q := strings.ToLower(d.Filters.Query)
	list := make([]SessionView, 0, len(sessions))
	for _, x := range sessions {
		if q == "" || strings.Contains(x.SearchKey, q) {
			list = append(list, x)
		}
	}
	switch d.Filters.Sort {
	case "tokens":
		sort.SliceStable(list, func(i, j int) bool { return list[i].Value > list[j].Value })
	case "active":
		sort.SliceStable(list, func(i, j int) bool { return list[i].ActiveMin > list[j].ActiveMin })
	default:
		sort.SliceStable(list, func(i, j int) bool { return list[i].Last.After(list[j].Last) })
	}
	d.SessionN = len(list)
	if len(list) > SessionLimit {
		list = list[:SessionLimit]
	}
	d.Sessions = list
}

var (
	tagRe         = regexp.MustCompile(`<[^>]*>`)
	placeholderRe = regexp.MustCompile(`\{(pct|tokens|claude_pct):([a-z0-9-]+)\}`)
	anyBraceRe    = regexp.MustCompile(`\{[^}]*\}`)
)

func (d *Dashboard) buildWork(s *snapshot.Snapshot, sessions []SessionView) {
	w := Workstreams{InRange: len(sessions), Dir: s.Meta.WorkReportDir}
	if w.Dir == "" {
		w.Dir = "data/work-report"
	}
	wr := s.WorkReport
	if wr == nil {
		d.Work = w
		return
	}
	w.Error, w.ReportTime = wr.Error, wr.ReportMtime
	cls := wr.Classified
	if cls == nil && wr.ReportMtime == "" {
		d.Work = w
		return
	}
	w.Available = true
	w.Classified = wr.ClassifiedMtime
	if w.Classified == "" {
		w.Classified = wr.ReportMtime
	}
	type acc struct {
		Theme
		id string
	}
	by := map[string]*acc{}
	for _, x := range sessions {
		hit, ok := cls.Lookup(x.ID)
		if !ok || hit.Theme == "none" {
			continue
		}
		w.Matched++
		t := by[hit.Theme]
		if t == nil {
			t = &acc{id: hit.Theme}
			by[hit.Theme] = t
		}
		ai := math.Max(float64(x.ActiveMin)/60*1.5, 0.1)
		manual := math.Min(float64(hit.ManualLikely), 16)
		t.Sessions++
		t.Value += x.Value
		t.Active += float64(x.ActiveMin)
		t.Manual += manual
		t.Saved += math.Max(manual-ai, 0)
		w.Total += x.Value
	}
	for _, t := range by {
		t.Name = t.id
		if cls != nil {
			t.Name = cls.ThemeName(t.id)
			for _, th := range cls.Themes {
				if th.ID == t.id {
					t.Desc = th.D
				}
			}
		}
		w.Themes = append(w.Themes, t.Theme)
	}
	sort.Slice(w.Themes, func(i, j int) bool { return w.Themes[i].Value > w.Themes[j].Value })
	if n := wr.Narrative; n != nil {
		fill := func(text string) string {
			text = tagRe.ReplaceAllString(text, "")
			text = placeholderRe.ReplaceAllStringFunc(text, func(m string) string {
				p := placeholderRe.FindStringSubmatch(m)
				t, ok := by[p[2]]
				if !ok {
					return "—"
				}
				if p[1] == "tokens" {
					return Compact(t.Value)
				}
				if w.Total == 0 {
					return "0%"
				}
				return Pct(t.Value / w.Total)
			})
			text = strings.NewReplacer("{total_tokens}", Compact(w.Total), "{claude_tokens}", Compact(w.Total),
				"{top_n}", itoa(w.Matched)).Replace(text)
			return anyBraceRe.ReplaceAllString(text, "")
		}
		w.Title, w.Thesis = fill(n.TitleHTML), fill(n.ThesisHTML)
	}
	d.Work = w
}

func (d *Dashboard) buildSources(s *snapshot.Snapshot, from time.Time) {
	src := Sources{}
	for _, x := range s.Sources {
		row := StatusRow{Source: x.Source, Path: x.Path, Requests: x.Requests, Label: x.Status, Level: Critical}
		switch x.Status {
		case "ok":
			row.Level = Good
		case "not found":
			row.Level = Warning
		}
		src.Status = append(src.Status, row)
	}
	days := max(d.Range.Hours/24, 1)
	first := startOfDay(d.Now).AddDate(0, 0, -(days - 1))
	daily := make([]DayPair, days)
	for i := range daily {
		daily[i].Day = first.AddDate(0, 0, i)
	}
	for _, r := range s.Rows {
		isProxy := r.Tool == usage.ToolProxy
		isCodex := r.Tool == usage.ToolCodex && r.Profile == "work-foundry"
		if !isProxy && !isCodex {
			continue
		}
		if !r.T.Before(from) {
			if isProxy {
				src.ProxyTotal += float64(r.Total)
			} else {
				src.CodexFoundry += float64(r.Total)
			}
		}
		// index by calendar day, which stays right across DST changes
		if i := daysBetween(first, r.T); i >= 0 && i < days {
			if isProxy {
				daily[i].Proxy += float64(r.Total)
			} else {
				daily[i].Codex += float64(r.Total)
			}
		}
	}
	src.Daily = daily
	m := s.Meta
	src.Sub = "collected in " + Exact(float64(m.CollectMS)) + " ms · " + itoa(m.CacheMisses) + " files re-read, " +
		itoa(m.CacheHits) + " cached · every " + itoa(m.Interval) + "s"
	src.Notes = append(src.Notes, "Foundry proxy cross-check (last "+d.Range.Label+"): proxy logged "+Compact(src.ProxyTotal)+
		" tokens; Codex Foundry sessions recorded "+Compact(src.CodexFoundry)+
		". The proxy is not added to totals because it overlaps Codex Foundry sessions.")
	if m.History != "" {
		since := "—"
		if m.HistorySince != "" {
			if t, err := time.ParseInLocation("2006-01-02T15:04", m.HistorySince, time.Local); err == nil {
				since = DayLabel(t)
			}
		}
		note := "History store " + m.History + ": " + itoa(m.HistoryRows) + " hourly rows since " + since + ". "
		if m.HistoryOnlyRows > 0 {
			note += itoa(m.HistoryOnlyRows) + " rows in this window come only from history (their transcripts are gone)."
		} else {
			note += "Every row in this window still has its source transcript."
		}
		src.Notes = append(src.Notes, note)
	}
	if m.ConfigError != "" {
		src.Warnings = append(src.Warnings, "Budget config: "+m.ConfigError)
	}
	if m.HistoryWarning != "" {
		src.Warnings = append(src.Warnings, "History: "+m.HistoryWarning)
	}
	d.Sources = src
}

// daysBetween counts calendar days from the local midnight first to t's local date.
func daysBetween(first, t time.Time) int {
	y, m, d := t.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, first.Location())
	return int(math.Round(day.Sub(first).Hours() / 24))
}
