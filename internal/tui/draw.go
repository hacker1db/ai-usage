package tui

import (
	"math"
	"strings"
	"time"

	"ai-usage/internal/palette"
	"ai-usage/internal/view"
)

// Status is the live-connection state shown in the header.
type Status struct {
	Source string // "live via dashboard :8790" or "local collect"
	Stale  bool
	Notice string // error text, if any
}

// Frame is a rendered dashboard: a fixed header and a scrollable body.
type Frame struct {
	Header []Line
	Body   []Line
}

const chartRows = 10

// Draw renders every section of d at width w.
func Draw(d *view.Dashboard, th Theme, w int, st Status) Frame {
	w = max(w, 40)
	h := &Doc{W: w}
	drawHeader(h, d, th, st)
	b := &Doc{W: w}
	if d.Empty {
		b.Blank()
		b.Text("No usage for this tab yet.", th.Muted())
	}
	sections := []func(*Doc, *view.Dashboard, Theme){
		drawHero, drawCumulative, drawStack, drawCards, drawHeat, drawMix, drawTop, drawOwnership,
		drawProjects, drawSessions, drawModels, drawSources,
	}
	for _, s := range sections {
		s(b, d, th)
	}
	b.Blank()
	b.Text(strings.Join(d.Foot, " · "), th.Muted())
	return Frame{Header: h.Lines, Body: b.Lines}
}

func section(doc *Doc, th Theme, title, sub string) {
	doc.Blank()
	b := new(B).T(title, th.Title())
	if sub != "" {
		b.T("  "+Truncate(sub, doc.W-Width(title)-2), th.Muted())
	}
	doc.Add(b)
}

func drawHeader(doc *Doc, d *view.Dashboard, th Theme, st Status) {
	age := d.Now.Sub(d.Generated).Round(time.Second)
	level, text := "good", "live"
	if st.Stale || age > time.Duration(max(d.Interval, 5)*3)*time.Second {
		level, text = "warning", "stale"
	}
	right := text + " · updated " + age.String() + " ago · " + st.Source
	b := new(B).T("ai-usage", th.Title()).T(" · Token usage", th.Primary())
	b.To(doc.W-Width(right)-2).C('●', th.Status(level)).Sp(1).T(right, th.Secondary())
	doc.Add(b)
	doc.Text(d.Subtitle, th.Muted())
	if st.Notice != "" {
		doc.Add(new(B).T("! ", th.Status("warning")).T(st.Notice, th.Secondary()))
	}

	tabs := new(B)
	for i, t := range view.Tabs {
		label := " " + t.Label + " " + view.Exact(float64(d.TabCounts[t.ID])) + " "
		s := th.Secondary()
		if t.ID == d.Tab.ID {
			s = Style{FG: th.Ink.Primary, Bold: true, Inverse: true}
		}
		if i > 0 {
			tabs.T("│", th.Muted())
		}
		tabs.T(label, s)
	}
	tabs.T("  tab/1-5", th.Muted())
	doc.Add(tabs)

	ctl := new(B)
	group := func(name string, opts []view.Option, cur, key string) {
		g := new(B).T(name+" ", th.Muted())
		for _, o := range opts {
			s := th.Muted()
			if o.ID == cur {
				s = Style{FG: th.Ink.Primary, Bold: true, Underline: true}
			}
			g.T(o.Label, s).Sp(1)
		}
		g.T("("+key+")  ", th.Muted())
		if ctl.Len() > 0 && ctl.Len()+g.Len() > doc.W {
			doc.Add(ctl)
			ctl = new(B)
		}
		ctl.l = append(ctl.l, g.l...)
	}
	group("metric", view.MetricOptions(), d.Filters.Metric, "m")
	group("range", view.RangeOptions(), d.Filters.Range, "r")
	group("burn", view.BurnOptions(), d.Filters.Burn, "b")
	doc.Add(ctl)
	doc.Text(strings.Repeat("─", doc.W), Style{FG: th.Ink.Axis})
}

func drawHero(doc *Doc, d *view.Dashboard, th Theme) {
	section(doc, th, d.Hero.Title, "")
	doc.Add(new(B).T(view.Compact(d.Hero.Value), Style{FG: th.Ink.Primary, Bold: true}).Sp(1).T(d.Hero.Unit, th.Secondary()).
		T("  ("+view.Exact(d.Hero.Value)+")", th.Muted()))
	b := new(B)
	for _, s := range d.Hero.Stats {
		item := s.Label + " " + s.Value
		if b.Len() > 0 && b.Len()+Width(item)+3 > doc.W {
			doc.Add(b)
			b = new(B)
		}
		if b.Len() > 0 {
			b.T("  ·  ", th.Muted())
		}
		b.T(s.Label+" ", th.Muted()).T(s.Value, th.Primary())
	}
	doc.Add(b)
}

// yGutter is the width of y-axis labels for ticks up to top.
func yGutter(top float64) int {
	g := 0
	for _, t := range view.Ticks(top, 4) {
		g = max(g, Width(view.Compact(t)))
	}
	return g + 1
}

// tickRows maps the 5 axis ticks to canvas rows (0 = top).
func tickRows(top float64, rows int) map[int]string {
	out := map[int]string{}
	for _, t := range view.Ticks(top, 4) {
		r := int(math.Round((1 - t/top) * float64(rows-1)))
		out[r] = view.Compact(t)
	}
	return out
}

func drawCumulative(doc *Doc, d *view.Dashboard, th Theme) {
	c := d.Cum
	label := "this month"
	if c.Budget > 0 {
		label = "budget period"
	}
	section(doc, th, "Cumulative "+c.Unit, label+", projected at the 7-day average rate")
	g := yGutter(c.YMax)
	pw := doc.W - g - 1
	if pw < 10 || c.Hours <= 0 {
		return
	}
	cv := NewBraille(pw, chartRows)
	dw, dh := pw*2-1, chartRows*4-1
	x := func(i float64) int { return int(math.Round(i / float64(c.Hours) * float64(dw))) }
	y := func(v float64) int { return int(math.Round((1 - v/c.YMax) * float64(dh))) }

	nowX := x(float64(len(c.Cum)))
	for yy := 0; yy <= dh; yy += 3 {
		cv.Set(nowX, yy, th.Ink.Axis)
	}
	if c.Budget > 0 {
		by := y(c.Budget)
		for xx := 0; xx <= dw; xx += 3 {
			cv.Set(xx, by, palette.Status["warning"])
		}
	}
	if len(c.Cum) > 0 {
		cv.Line(nowX, y(c.Run), x(float64(c.Hours)), y(c.ProjEnd), th.Ink.Muted, true)
		px, py := x(0), y(0)
		for i, v := range c.Cum {
			nx, ny := x(float64(i+1)), y(v)
			cv.Line(px, py, nx, ny, th.Ink.Secondary, false)
			px, py = nx, ny
		}
	}
	ticks := tickRows(c.YMax, chartRows)
	budgetRow := -1
	if c.Budget > 0 {
		budgetRow = y(c.Budget) / 4
	}
	for r := range chartRows {
		b := new(B).T(PadLeft(ticks[r], g-1), th.Muted()).T(" ", Style{}).C('│', Style{FG: th.Ink.Axis})
		for cx := range pw {
			ch, col := cv.Cell(cx, r)
			b.C(ch, Style{FG: col})
		}
		line := b.Line(doc.W)
		if r == budgetRow {
			overlay(line, doc.W-1, "Budget "+view.Compact(c.Budget), th.Secondary(), true)
		}
		if r == y(c.ProjEnd)/4 && len(c.Cum) > 0 {
			overlay(line, doc.W-1, view.Compact(c.ProjEnd), th.Secondary(), true)
		}
		doc.AddLine(line)
	}
	doc.Add(new(B).Sp(g).C('└', Style{FG: th.Ink.Axis}).T(strings.Repeat("─", pw), Style{FG: th.Ink.Axis}))
	axis := new(B).Sp(g + 1)
	labels := make([]rune, pw)
	for i := range labels {
		labels[i] = ' '
	}
	days := int(math.Round(c.End.Sub(c.Start).Hours() / 24))
	step := 1
	for days/step*8 > pw {
		step++
	}
	last := -2
	for k := 0; k <= days; k += step {
		at := c.Start.AddDate(0, 0, k)
		col := x(at.Sub(c.Start).Hours()) / 2
		s := []rune(view.ShortDay(at))
		col = min(col, pw-len(s))
		if col <= last+1 || col < 0 {
			continue
		}
		copy(labels[col:], s)
		last = col + len(s)
	}
	axis.T(string(labels), th.Muted())
	doc.Add(axis)
	key := new(B).Sp(g+1).T("━", Style{FG: th.Ink.Secondary}).T(" recorded  ", th.Muted()).
		T("┅", Style{FG: th.Ink.Muted}).T(" projected  ", th.Muted()).
		T("┆", Style{FG: th.Ink.Axis}).T(" now", th.Muted())
	if c.Budget > 0 {
		key.T("  ┄", Style{FG: palette.Status["warning"]}).T(" budget", th.Muted())
	}
	doc.Add(key)
	doc.Text(Truncate(c.Desc, doc.W), th.Muted())
}

// overlay writes s into line ending at column end (right-aligned) when rightAlign is true.
func overlay(line Line, end int, s string, st Style, rightAlign bool) {
	rs := []rune(" " + s)
	start := end - len(rs) + 1
	if !rightAlign {
		start = end
	}
	for i, r := range rs {
		if p := start + i; p >= 0 && p < len(line) {
			line[p] = Cell{r, st}
		}
	}
}

func drawStack(doc *Doc, d *view.Dashboard, th Theme) {
	s := d.Stack
	section(doc, th, "Usage by profile", s.Sub)
	if len(s.Series) == 0 {
		doc.Text("No usage in range.", th.Muted())
		return
	}
	// aggregate buckets when there are more than columns available
	g0 := yGutter(s.YMax)
	pw := doc.W - g0 - 1
	n, k := s.N, 1
	for n > pw {
		k++
		n = int(math.Ceil(float64(s.N) / float64(k)))
	}
	vals := make([][]float64, len(s.Series))
	colors := make([]string, len(s.Series))
	totals := make([]float64, n)
	for si, se := range s.Series {
		colors[si] = th.Series(se.Group.Slot)
		vals[si] = make([]float64, n)
		for i, v := range se.Values {
			vals[si][i/k] += v
			totals[i/k] += v
		}
	}
	top := s.YMax
	if k > 1 {
		mx := 0.0
		for _, t := range totals {
			mx = math.Max(mx, t)
		}
		top = view.NiceMax(mx * 1.05)
	}
	g := yGutter(top)
	pw = doc.W - g - 1
	colW, gap := 1, 0
	switch {
	case n*3 <= pw:
		colW, gap = 2, 1
	case n*2 <= pw:
		colW, gap = 1, 1
	}
	stride := colW + gap
	stacks := make([][]Segment, n)
	for i := range n {
		col := make([]float64, len(vals))
		for si := range vals {
			col[si] = vals[si][i]
		}
		stacks[i] = Stack(col, colors, top, chartRows)
	}
	ticks := tickRows(top, chartRows)
	for r := range chartRows {
		cell := chartRows - 1 - r
		b := new(B).T(PadLeft(ticks[r], g-1), th.Muted()).Sp(1).C('│', Style{FG: th.Ink.Axis})
		for i := range n {
			fill, fg, bg := StackCell(stacks[i], cell)
			for range colW {
				b.C(lowerBlocks[fill], Style{FG: fg, BG: bg})
			}
			b.Sp(gap)
		}
		doc.Add(b)
	}
	doc.Add(new(B).Sp(g).C('└', Style{FG: th.Ink.Axis}).T(strings.Repeat("─", pw), Style{FG: th.Ink.Axis}))
	labels := make([]rune, pw)
	for i := range labels {
		labels[i] = ' '
	}
	// candidates: every bucket, or only day starts for multi-hour buckets; then label every m-th
	// candidate so labels sit at a uniform spacing with at least one space between them
	var cand []int
	widest := 0
	for i := range n {
		if s.BucketHours > 1 && s.BucketHours < 24 && s.BucketStart(i*k).Hour() != 0 {
			continue
		}
		cand = append(cand, i)
		widest = max(widest, Width(s.BucketLabel(i*k)))
	}
	m := 1
	if len(cand) > 1 {
		gapCols := (cand[1] - cand[0]) * stride
		m = max(int(math.Ceil(float64(widest+1)/float64(gapCols))), 1)
	}
	for ci := 0; ci < len(cand); ci += m {
		i := cand[ci]
		lab := []rune(s.BucketLabel(i * k))
		if col := i * stride; col+len(lab) <= pw {
			copy(labels[col:], lab)
		}
	}
	doc.Add(new(B).Sp(g+1).T(string(labels), th.Muted()))
	legend(doc, th, s.Series)
}

func legend(doc *Doc, th Theme, series []view.Series) {
	b := new(B)
	for _, se := range series {
		item := se.Group.Label + " " + view.Compact(se.Total)
		if b.Len() > 0 && b.Len()+Width(item)+4 > doc.W {
			doc.Add(b)
			b = new(B)
		}
		if b.Len() > 0 {
			b.Sp(2)
		}
		b.C('█', Style{FG: th.Series(se.Group.Slot)}).Sp(1).T(se.Group.Label, th.Primary()).Sp(1).
			T(view.Compact(se.Total), th.Secondary())
	}
	doc.Add(b)
}

func drawCards(doc *Doc, d *view.Dashboard, th Theme) {
	section(doc, th, "Profiles", "last "+d.Range.Label+" · burn over "+d.Burn.Label+" · share of tab")
	if len(d.Cards) == 0 {
		doc.Text("No usage for this tab yet.", th.Muted())
		return
	}
	sparkW := min(28, max(doc.W-92, 10))
	for _, c := range d.Cards {
		b := new(B).C('█', Style{FG: th.Series(c.Group.Slot)}).Sp(1).T(PadRight(c.Group.Label, 20), th.Primary()).
			T(PadLeft(view.Compact(c.Total), 7), Style{FG: th.Ink.Primary, Bold: true}).
			T(PadLeft(view.Compact(c.Requests)+" req", 11), th.Secondary()).
			T(PadLeft(view.Compact(c.Burn)+"/h", 9), th.Secondary()).
			T(PadLeft(view.Pct(c.Share), 6), th.Secondary()).Sp(2)
		spark := Spark(c.Spark, sparkW)
		for _, r := range spark {
			b.C(r, Style{FG: th.Series(c.Group.Slot)})
		}
		b.Sp(sparkW - len(spark) + 2)
		var models []string
		for _, m := range c.TopModels {
			models = append(models, m.Name+" "+view.Compact(m.Value))
		}
		b.T(Truncate(strings.Join(models, " · "), max(doc.W-b.Len(), 0)), th.Muted())
		doc.Add(b)
		if c.Budget != nil {
			drawBudget(doc, th, c.Budget)
		}
	}
}

func drawBudget(doc *Doc, th Theme, bb *view.BudgetBar) {
	const barW = 20
	filled := int(math.Round(math.Min(bb.Pct(), 100) / 100 * barW))
	st := th.Status(string(bb.Level))
	b := new(B).Sp(2).T(strings.Repeat("█", filled), st).T(strings.Repeat("░", barW-filled), Style{FG: th.Ink.Axis}).Sp(1).
		T(bb.Level.Icon()+" "+bb.Status, st).
		T(" · "+string(bb.Period)+" "+view.Compact(bb.Used)+" of "+view.Compact(bb.Tokens)+" "+string(bb.Metric)+
			" ("+view.Pct(bb.Pct()/100)+") · projected "+view.Compact(bb.Projected)+" by "+view.ShortDay(bb.End.Add(-time.Second)), th.Secondary())
	doc.Add(b)
}

func drawHeat(doc *Doc, d *view.Dashboard, th Theme) {
	h := d.Heat
	section(doc, th, "When you work", d.Metric.Unit+" by weekday and hour, last "+view.Exact(float64(h.Days))+" days")
	head := new(B).Sp(5)
	for hr := 0; hr < 24; hr += 3 {
		head.T(PadRight(view.HourLabel(time.Date(2000, 1, 1, hr, 0, 0, 0, time.Local)), 6), th.Muted())
	}
	doc.Add(head)
	for wd, name := range view.Weekdays {
		b := new(B).T(PadRight(name, 5), th.Muted())
		for hr := range 24 {
			v := h.Cells[wd][hr]
			if v <= 0 {
				b.T(" ·", th.Muted())
				continue
			}
			b.T("██", Style{FG: palette.SeqColor(th.Mode, palette.SeqStep(v, h.Max))})
		}
		doc.Add(b)
	}
	ramp := new(B).Sp(5).T("less ", th.Muted())
	steps := len(palette.Sequential) - 1
	for i := 1; i <= steps; i += 2 {
		ramp.T("█", Style{FG: palette.SeqColor(th.Mode, i)})
	}
	ramp.T(" more", th.Muted())
	if h.Peak != "" {
		ramp.T("   "+h.Peak, th.Secondary())
	}
	doc.Add(ramp)
}

func drawMix(doc *Doc, d *view.Dashboard, th Theme) {
	section(doc, th, "Token mix", "cache reads → fresh input → output, last "+d.Range.Label+" (cost order)")
	if len(d.Mix) == 0 {
		doc.Text("No token usage in range.", th.Muted())
		return
	}
	colors := palette.Mix[th.Mode]
	barW := min(40, max(doc.W-50, 10))
	row := func(label string, slot int, m view.Mix, swatch bool) {
		b := new(B)
		if swatch {
			b.C('█', Style{FG: th.Series(slot)}).Sp(1)
		} else {
			b.Sp(2)
		}
		b.T(PadRight(label, 20), th.Primary())
		segs := Stack([]float64{m.Cached, m.Fresh, m.Output}, colors[:], m.Total(), barW)
		for i := range barW {
			// horizontal bar: reuse the stacking math one cell at a time, filled from the left
			fill, fg, bg := StackCell(segs, i)
			b.C(leftBlocks[fill], Style{FG: fg, BG: bg})
		}
		b.T("  cache hit "+PadLeft(view.Pct(m.CacheHit()), 4), th.Secondary()).T("  "+view.Compact(m.Total()), th.Muted())
		doc.Add(b)
	}
	for _, m := range d.Mix {
		row(m.Group.Label, m.Group.Slot, m, true)
	}
	if len(d.Mix) > 1 {
		row(d.MixAll.Group.Label, 0, d.MixAll, false)
	}
	doc.Add(new(B).Sp(22).C('█', Style{FG: colors[0]}).T(" cache read  ", th.Muted()).
		C('█', Style{FG: colors[1]}).T(" fresh input  ", th.Muted()).C('█', Style{FG: colors[2]}).T(" output", th.Muted()))
}

func drawTop(doc *Doc, d *view.Dashboard, th Theme) {
	section(doc, th, "Where the tokens went", d.TopSub)
	if len(d.Top) == 0 {
		doc.Text("No sessions in this tab and range.", th.Muted())
		return
	}
	wide := doc.W >= 110
	shareW, projW, profW := 12, 17, 21
	if !wide {
		shareW, projW, profW = 6, 13, 2 // narrow: swatch only for the profile
	}
	for i, x := range d.Top {
		right := new(B).Sp(1).T(PadRight(orDash(x.Project), projW), th.Secondary()).Sp(1).
			C('█', Style{FG: th.Series(x.Group.Slot)}).Sp(1)
		if wide {
			right.T(PadRight(x.Group.Label, profW-2), th.Secondary())
		}
		right.T(PadLeft(view.Compact(x.Value), 8), Style{FG: th.Ink.Primary, Bold: true}).Sp(1)
		hbar(right, x.Share, shareW, Style{FG: th.Ink.Secondary}, Style{FG: th.Ink.Axis})
		right.T(PadLeft(view.Pct(x.Share), 5), th.Muted())
		row(doc, new(B).T(PadLeft(view.Exact(float64(i+1)), 2)+". ", th.Muted()), sessionTitle(x), th.Primary(), right)
	}
}

// row lays out prefix, a flexible title column, and right-hand cells across the width.
func row(doc *Doc, prefix *B, title string, titleSt Style, right *B) {
	w := max(doc.W-prefix.Len()-right.Len(), 4)
	prefix.T(PadRight(title, w), titleSt)
	prefix.l = append(prefix.l, right.l...)
	doc.Add(prefix)
}

func sessionTitle(x view.SessionView) string {
	if x.Title == "" {
		return "(untitled)"
	}
	return x.Title
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// hbar draws a single-color horizontal bar of share (0-1) in w cells with eighth precision.
func hbar(b *B, share float64, w int, st, empty Style) {
	units := int(math.Round(math.Min(math.Max(share, 0), 1) * float64(w*8)))
	for i := range w {
		n := min(max(units-i*8, 0), 8)
		if n == 0 {
			b.C('·', empty)
			continue
		}
		b.C(leftBlocks[n], st)
	}
}

func groupByKey(key string) view.Group {
	for _, g := range view.Groups {
		if g.Key == key {
			return g
		}
	}
	return view.Other
}

// ownSuffix names the ownership filter in a section title.
func ownSuffix(d *view.Dashboard) string {
	if d.Filters.Own == "" {
		return ""
	}
	return " · " + d.Filters.Own
}

func drawOwnership(doc *Doc, d *view.Dashboard, th Theme) {
	o := d.Ownership
	filter := "all"
	if d.Filters.Own != "" {
		filter = d.Filters.Own
	}
	section(doc, th, "Ownership", d.Metric.Unit+" by repository owner · last "+d.Range.Label+" · showing "+filter+" (o)")
	if o.Total <= 0 {
		doc.Text("No sessions in this tab and range.", th.Muted())
	} else {
		const labelW, rightW = 14, 44
		barW := max(min(doc.W-labelW-rightW-2, 50), 8)
		for _, r := range o.Rows {
			st := th.Secondary()
			mark := "  "
			if r.Label == d.Filters.Own {
				st, mark = Style{FG: th.Ink.Primary, Bold: true}, "▸ "
			}
			b := new(B).T(mark, th.Primary()).T(PadRight(Truncate(r.Label, labelW-3), labelW-2), st).Sp(1)
			hbar(b, r.Share, barW, th.Secondary(), th.Muted())
			b.Sp(1).T(Truncate(PadLeft(view.Pct(r.Share), 4)+" · "+view.Compact(r.Value)+" · "+
				view.Exact(float64(r.Sessions))+" sess. · "+view.Exact(float64(r.Projects))+" proj.", rightW), th.Primary())
			doc.Add(b)
		}
	}
	doc.Text("Rules: "+o.Source, th.Muted())
	if o.Warning != "" {
		doc.Text("! "+o.Warning, th.Status("warning"))
	}
}

func drawProjects(doc *Doc, d *view.Dashboard, th Theme) {
	section(doc, th, "Projects"+ownSuffix(d), d.Metric.Unit+" by project · last "+d.Range.Label+" · active time = 5-minute slots with activity")
	if len(d.Projects) == 0 {
		doc.Text("No sessions in this tab and range.", th.Muted())
		return
	}
	const nameW, rightW = 24, 40
	barW := max(doc.W-nameW-rightW-2, 10)
	top := 0.0
	for _, p := range d.Projects {
		top = math.Max(top, p.Total)
	}
	used := map[string]bool{}
	for _, p := range d.Projects {
		vals := make([]float64, len(p.Parts))
		colors := make([]string, len(p.Parts))
		for i, part := range p.Parts {
			vals[i] = part.Value
			colors[i] = th.Series(groupByKey(part.Name).Slot)
			used[part.Name] = true
		}
		segs := Stack(vals, colors, top, barW)
		name := p.Name
		if Width(name) > nameW-1 {
			r := []rune(name)
			name = "…" + string(r[len(r)-(nameW-2):])
		}
		b := new(B).T(PadLeft(name, nameW-1), th.Secondary()).Sp(1)
		for i := range barW {
			fill, fg, bg := StackCell(segs, i)
			b.C(leftBlocks[fill], Style{FG: fg, BG: bg})
		}
		right := view.Compact(p.Total) + " · " + view.Duration(p.Active) + " · " + view.Exact(float64(p.Sessions)) + " sess."
		if p.Class != "" {
			right += " · " + p.Class
		}
		b.Sp(1).T(Truncate(right, rightW), th.Primary())
		doc.Add(b)
	}
	var series []view.Series
	for _, g := range append(append([]view.Group(nil), view.Groups...), view.Other) {
		if used[g.Key] {
			series = append(series, view.Series{Group: g})
		}
	}
	b := new(B).Sp(nameW)
	for _, se := range series {
		b.C('█', Style{FG: th.Series(se.Group.Slot)}).Sp(1).T(se.Group.Label, th.Muted()).Sp(2)
	}
	doc.Add(b)
}

func drawSessions(doc *Doc, d *view.Dashboard, th Theme) {
	sub := view.Exact(float64(d.SessionN)) + " sessions in the last " + d.Range.Label + " · sorted by " + sortLabel(d.Filters.Sort) + " (s)"
	section(doc, th, "Sessions"+ownSuffix(d), sub)
	if len(d.Sessions) == 0 {
		doc.Text("No sessions match.", th.Muted())
		return
	}
	wide := doc.W >= 110
	lastW, projW, profW, actW, valW := 15, 16, 22, 7, 8
	if !wide {
		projW, profW = 12, 2 // narrow: swatch only for the profile
	}
	cells := func(project string, slot int, label, active, value string, st, vst Style, swatch bool) *B {
		r := new(B).Sp(1).T(PadRight(project, projW), st).Sp(1)
		if swatch {
			r.C('█', Style{FG: th.Series(slot)}).Sp(1)
		} else {
			r.Sp(2)
		}
		if wide {
			r.T(PadRight(label, profW-2), st)
		}
		return r.Sp(1).T(PadLeft(active, actW), st).T(PadLeft(value, valW+1), vst)
	}
	row(doc, new(B).T(PadRight("Last active", lastW), th.Muted()), "Session", th.Muted(),
		cells("Project", 0, "Profile", "Active", shortMetric(d.Metric), th.Muted(), th.Muted(), false))
	for _, x := range d.Sessions {
		title := sessionTitle(x)
		if x.SrcLabel != "" {
			title += " (" + x.SrcLabel + ")"
		}
		row(doc, new(B).T(PadRight(view.WhenLabel(x.Last), lastW), th.Muted()), title, th.Primary(),
			cells(orDash(x.Project), x.Group.Slot, x.Group.Label, view.Duration(float64(x.ActiveMin)), view.Compact(x.Value),
				th.Secondary(), Style{FG: th.Ink.Primary, Bold: true}, true))
	}
	if d.SessionN > len(d.Sessions) {
		doc.Text("showing "+view.Exact(float64(len(d.Sessions)))+" of "+view.Exact(float64(d.SessionN)), th.Muted())
	}
}

// shortMetric is a column-sized metric name.
func shortMetric(m view.Metric) string {
	if m.ID == "total" {
		return "Tokens"
	}
	return m.Label
}

func sortLabel(id string) string {
	for _, o := range view.SessionSorts {
		if o.ID == id {
			return strings.ToLower(o.Label)
		}
	}
	return id
}

func drawModels(doc *Doc, d *view.Dashboard, th Theme) {
	section(doc, th, "Models", "last "+d.Range.Label+" · "+view.Exact(float64(len(d.Models)))+" profile/model pairs")
	if len(d.Models) == 0 {
		doc.Text("No usage in range.", th.Muted())
		return
	}
	type col struct {
		name string
		get  func(view.ModelRow) int64
	}
	cols := []col{{"Requests", func(m view.ModelRow) int64 { return m.Requests }}, {"Input", func(m view.ModelRow) int64 { return m.Input }},
		{"Cached", func(m view.ModelRow) int64 { return m.Cached }}, {"Output", func(m view.ModelRow) int64 { return m.Output }}}
	if doc.W >= 110 {
		cols = append(cols, col{"Reasoning", func(m view.ModelRow) int64 { return m.Reasoning }})
	}
	cols = append(cols, col{"Total", func(m view.ModelRow) int64 { return m.Total }})
	profW, numW := 22, 10
	if doc.W < 110 {
		numW = 8
	}
	modelW := max(doc.W-profW-numW*len(cols), 10)
	h := new(B).T(PadRight("Profile", profW), th.Muted()).T(PadRight("Model", modelW), th.Muted())
	for _, c := range cols {
		h.T(PadLeft(c.name, numW), th.Muted())
	}
	doc.Add(h)
	for _, m := range d.Models {
		b := new(B).C('█', Style{FG: th.Series(m.Group.Slot)}).Sp(1).T(PadRight(m.Group.Label, profW-2), th.Secondary()).
			T(PadRight(m.Model, modelW), th.Primary())
		for _, c := range cols {
			b.T(PadLeft(view.Compact(float64(c.get(m))), numW), th.Secondary())
		}
		doc.Add(b)
	}
}

func drawSources(doc *Doc, d *view.Dashboard, th Theme) {
	s := d.Sources
	section(doc, th, "Sources", s.Sub)
	const srcW, stW, reqW = 30, 14, 10
	doc.Add(new(B).T(PadRight("Source", srcW), th.Muted()).T(PadRight("Status", stW), th.Muted()).
		T(PadLeft("Requests", reqW), th.Muted()).T("  Path", th.Muted()))
	for _, r := range s.Status {
		st := th.Status(string(r.Level))
		doc.Add(new(B).T(PadRight(r.Source, srcW), th.Primary()).T(PadRight(r.Level.Icon()+" "+r.Label, stW), st).
			T(PadLeft(view.Exact(float64(r.Requests)), reqW), th.Secondary()).T("  "+r.Path, th.Muted()))
	}
	for _, n := range s.Notes {
		wrap(doc, n, th.Muted())
	}
	for _, w := range s.Warnings {
		doc.Add(new(B).T("! ", th.Status("warning")).T(Truncate(w, doc.W-2), th.Secondary()))
	}
}

// wrap word-wraps text to the document width.
func wrap(doc *Doc, text string, st Style) {
	line := ""
	for _, word := range strings.Fields(text) {
		if line != "" && Width(line)+1+Width(word) > doc.W {
			doc.Text(line, st)
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	if line != "" {
		doc.Text(Truncate(line, doc.W), st)
	}
}
