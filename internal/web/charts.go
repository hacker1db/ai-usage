package web

import (
	"encoding/json"
	"html/template"
	"math"
	"strconv"
	"time"

	"ai-usage/internal/palette"
	"ai-usage/internal/view"
)

// Charts draw on a 960-unit-wide viewBox and scale with their container.
const chartW = 960.0

type margin struct{ top, right, bottom, left float64 }

func slotClass(prefix string, slot int) string { return prefix + strconv.Itoa(slot) }

func groupSlot(key string) int {
	for _, g := range view.Groups {
		if g.Key == key {
			return g.Slot
		}
	}
	return 0
}

func groupLabel(key string) string {
	for _, g := range view.Groups {
		if g.Key == key {
			return g.Label
		}
	}
	return view.Other.Label
}

// yGrid draws recessive hairline gridlines with clean tick labels.
func yGrid(s *svg, m margin, h, yMax float64, y func(float64) float64) {
	for i, v := range view.Ticks(yMax, 4) {
		yy := y(v)
		class := "grid"
		if i == 0 {
			class = "axis"
		}
		s.el("line", "class", class, "x1", num(m.left), "x2", num(chartW-m.right), "y1", num(yy), "y2", num(yy))
		s.text(m.left-8, yy+4, "tick", "end", view.Compact(v))
	}
}

// cumulativeChart is the period-to-date line with its projection and optional budget. app.js
// reads data-cum to drive the crosshair.
func cumulativeChart(c view.Cumulative) template.HTML {
	const h = 260.0
	m := margin{24, 18, 30, 60}
	hours := float64(max(c.Hours, 1))
	x := func(i float64) float64 { return m.left + i/hours*(chartW-m.left-m.right) }
	y := func(v float64) float64 { return h - m.bottom - v/c.YMax*(h-m.top-m.bottom) }

	type payload struct {
		L, R, T, B, W, H float64
		YMax             float64   `json:"ymax"`
		Start            int64     `json:"start"`
		Now              int64     `json:"now"`
		Hours            int       `json:"hours"`
		Cum              []float64 `json:"cum"`
		By               []float64 `json:"by"`
		Run              float64   `json:"run"`
		Rate             float64   `json:"rate"`
		Budget           float64   `json:"budget"`
	}
	round := func(xs []float64) []float64 {
		out := make([]float64, len(xs))
		for i, v := range xs {
			out[i] = math.Round(v)
		}
		return out
	}
	data, _ := json.Marshal(payload{m.left, m.right, m.top, m.bottom, chartW, h, c.YMax, c.Start.UnixMilli(), c.Now.UnixMilli(),
		c.Hours, round(c.Cum), round(c.ByHour), math.Round(c.Run), c.ProjRate, c.Budget})

	s := newSVG(chartW, h, "chart cum", c.Desc, "data-cum", string(data))
	yGrid(s, m, h, c.YMax, y)

	days := int(math.Round(hours / 24))
	step := 1
	if days > 20 {
		step = 3
	} else if days > 10 {
		step = 2
	}
	for d := 0; d <= days; d += step {
		at := time.Date(c.Start.Year(), c.Start.Month(), c.Start.Day()+d, 0, 0, 0, 0, c.Start.Location())
		i := at.Sub(c.Start).Hours()
		if i > hours {
			break
		}
		s.text(x(i), h-10, "tick", "middle", view.ShortDay(at))
	}
	if c.Budget > 0 {
		by := y(c.Budget)
		s.el("line", "class", "budget-line", "x1", num(m.left), "x2", num(chartW-m.right), "y1", num(by), "y2", num(by))
		s.text(chartW-m.right, by-6, "budget-label", "end", "Budget "+view.Compact(c.Budget))
	}
	if n := len(c.Cum); n > 0 {
		line := "M" + num(x(0)) + "," + num(y(0))
		for i, v := range c.Cum {
			line += " L" + num(x(float64(i+1))) + "," + num(y(v))
		}
		xn := x(float64(n))
		s.el("path", "class", "cum-area", "d", line+" L"+num(xn)+","+num(y(0))+" Z")
		s.el("path", "class", "cum-line", "d", line)
		s.el("path", "class", "cum-proj", "d", "M"+num(xn)+","+num(y(c.Run))+" L"+num(x(hours))+","+num(y(c.ProjEnd)))
		s.el("line", "class", "now-line", "x1", num(xn), "x2", num(xn), "y1", num(m.top-6), "y2", num(h-m.bottom))
		s.text(xn, m.top-10, "now-label", "middle", "Now")
		s.el("circle", "class", "end-dot", "cx", num(xn), "cy", num(y(c.Run)), "r", "4.5")
		s.text(x(hours)-4, y(c.ProjEnd)-9, "value-label", "end", view.Compact(c.ProjEnd))
	}
	s.el("line", "class", "xh", "x1", "0", "x2", "0", "y1", num(m.top), "y2", num(h-m.bottom), "visibility", "hidden")
	s.el("circle", "class", "xh-dot", "cx", "0", "cy", "0", "r", "4", "visibility", "hidden")
	s.el("rect", "class", "hit cum-hit", "x", num(m.left), "y", num(m.top), "width", num(chartW-m.left-m.right), "height", num(h-m.top-m.bottom))
	s.end("svg")
	return s.html()
}

// stackedChart is usage per bucket stacked by profile: thin bars (<= 24 units), 2-unit surface
// gaps between segments, a rounded data-end on the top segment only.
func stackedChart(st view.Stacked) template.HTML {
	const h = 240.0
	m := margin{22, 18, 30, 60}
	s := newSVG(chartW, h, "chart bars", st.Sub)
	y := func(v float64) float64 { return h - m.bottom - v/st.YMax*(h-m.top-m.bottom) }
	yGrid(s, m, h, st.YMax, y)
	n := max(st.N, 1)
	band := (chartW - m.left - m.right) / float64(n)
	barW := math.Max(math.Min(band*0.7, 24), 2)
	x := func(i int) float64 { return m.left + float64(i)*band + (band-barW)/2 }
	every := max(1, int(math.Ceil(float64(n)/12)))
	for i := 0; i < n; i += every {
		s.text(x(i)+barW/2, h-10, "tick", "middle", st.BucketLabel(i))
	}
	const gap = 2.0
	for i := 0; i < st.N; i++ {
		var segs []view.Series
		for _, se := range st.Series {
			if se.Values[i] > 0 {
				segs = append(segs, se)
			}
		}
		base := 0.0
		for si, se := range segs {
			v := se.Values[i]
			y0, y1 := y(base), y(base+v)
			g := 0.0
			if si > 0 {
				g = gap
			}
			hh := math.Max(y0-y1-g, 1)
			top := y0 - g - hh
			r := 0.0
			if si == len(segs)-1 {
				r = 4
			}
			s.el("path", "class", slotClass("s", se.Group.Slot), "d", roundedTop(x(i), top, barW, hh, r))
			base += v
		}
	}
	for i := 0; i < st.N; i++ {
		t := tip{H: st.BucketTitle(i)}
		for j := len(st.Series) - 1; j >= 0; j-- {
			if v := st.Series[j].Values[i]; v > 0 {
				t.R = append(t.R, row(st.Series[j].Group.Label, view.Exact(v), st.Series[j].Group.Slot))
			}
		}
		t.R = append(t.R, row("Total", view.Exact(st.Totals[i]), -1))
		s.el("rect", "class", "hit", "x", num(m.left+float64(i)*band), "y", num(m.top), "width", num(band),
			"height", num(h-m.top-m.bottom), "data-tip", t.attr())
	}
	s.end("svg")
	return s.html()
}

// sparkline is a profile's series over the range, in its own color.
func sparkline(values []float64, slot int) template.HTML {
	const w, h = 280.0, 42.0
	s := newSVG(w, h, "spark", "trend", "preserveAspectRatio", "none", "aria-hidden", "true")
	mx := 1.0
	for _, v := range values {
		mx = math.Max(mx, v)
	}
	pts := ""
	for i, v := range values {
		px := float64(i) / math.Max(float64(len(values)-1), 1) * w
		pts += num(px) + "," + num(h-3-v/mx*(h-8)) + " "
	}
	s.el("polyline", "class", slotClass("line s", slot), "points", pts, "vector-effect", "non-scaling-stroke")
	s.end("svg")
	return s.html()
}

// meter is a thin budget bar: the fill carries the status, the track is a lighter wash.
func meter(pct float64, level view.Level) template.HTML {
	const w, h = 280.0, 8.0
	s := newSVG(w, h, "meter", view.Pct(pct/100)+" used", "preserveAspectRatio", "none")
	s.el("rect", "class", "meter-track", "x", "0", "y", "0", "width", num(w), "height", num(h), "rx", "4")
	if fill := math.Min(pct, 100) / 100 * w; fill > 0 {
		s.el("rect", "class", "status-fill "+string(level), "x", "0", "y", "0", "width", num(math.Max(fill, h)), "height", num(h), "rx", "4")
	}
	s.end("svg")
	return s.html()
}

// heatmap is usage by weekday and hour on the sequential ramp (one hue, light to dark).
func heatmap(hm view.Heatmap) template.HTML {
	// Drawn on its own narrower canvas: it sits in a half-width card, and a 960-wide viewBox
	// would shrink its labels below legibility.
	const w = 600.0
	m := margin{20, 4, 4, 34}
	cell := (w - m.left - m.right) / 24
	ch := cell
	h := m.top + 7*ch + m.bottom
	s := newSVG(w, h, "chart heat", hm.Peak)
	for hr := 0; hr < 24; hr += 3 {
		at := time.Date(2000, 1, 1, hr, 0, 0, 0, time.Local)
		s.text(m.left+float64(hr)*cell+cell/2, m.top-7, "tick", "middle", view.HourLabel(at))
	}
	for wd := 0; wd < 7; wd++ {
		yy := m.top + float64(wd)*ch
		s.text(m.left-8, yy+ch/2+4, "tick", "end", view.Weekdays[wd])
		for hr := 0; hr < 24; hr++ {
			v := hm.Cells[wd][hr]
			at := time.Date(2000, 1, 1, hr, 0, 0, 0, time.Local)
			t := tip{H: view.Weekdays[wd] + " " + view.HourLabel(at) + "–" + view.HourLabel(at.Add(time.Hour)),
				R: [][]any{row("Last "+strconv.Itoa(hm.Days)+" days", view.Exact(v), -1)}}
			s.el("rect", "class", slotClass("q", palette.SeqStep(v, hm.Max)), "x", num(m.left+float64(hr)*cell+1),
				"y", num(yy+1), "width", num(cell-2), "height", num(ch-2), "rx", "3", "data-tip", t.attr())
		}
	}
	s.end("svg")
	return s.html()
}

var mixParts = []string{"Cache read", "Fresh input", "Output"}

// mixBar is one profile's token composition as a tall segmented bar: each part its own block with
// 4-unit corners and its share printed inside when it fits.
func mixBar(mx view.Mix) template.HTML {
	const w, h = 640.0, 28.0
	total := mx.Total()
	s := newSVG(w, h, "mixbar", mx.Group.Label+" token mix")
	parts := []float64{mx.Cached, mx.Fresh, mx.Output}
	var shown []int
	for i, v := range parts {
		if v > 0 {
			shown = append(shown, i)
		}
	}
	const gap = 2.0
	avail := w - gap*float64(max(len(shown)-1, 0))
	xx := 0.0
	for _, i := range shown {
		bw := parts[i] / total * avail
		t := tip{H: mx.Group.Label + " · " + mixParts[i], R: [][]any{row("Tokens", view.Exact(parts[i]), -1), row("Share", view.Pct(parts[i]/total), -1)}}
		s.el("rect", "class", slotClass("mix", i), "x", num(xx), "y", "0", "width", num(math.Max(bw, 1)), "height", num(h), "rx", "4", "data-tip", t.attr())
		label := view.Pct(parts[i] / total)
		if bw > float64(len(label))*8+16 {
			s.text(xx+8, h/2+4, slotClass("on-mix", i), "start", label)
		}
		xx += bw + gap
	}
	s.end("svg")
	return s.html()
}

// shareBar is a thin fully rounded bar showing a share of the whole.
func shareBar(share float64) template.HTML {
	const w, h = 120.0, 6.0
	s := newSVG(w, h, "share", view.Pct(share), "preserveAspectRatio", "none")
	s.el("rect", "class", "meter-track", "x", "0", "y", "0", "width", num(w), "height", num(h), "rx", "3")
	if share > 0 {
		s.el("rect", "class", "share-fill", "x", "0", "y", "0", "width", num(math.Max(share*w, h)), "height", num(h), "rx", "3")
	}
	s.end("svg")
	return s.html()
}

// projectsChart is usage per repository, stacked by profile, with value, active time, and sessions.
func projectsChart(ps []view.Project, unit string) template.HTML {
	const rowH = 30.0
	m := margin{6, 250, 20, 200}
	h := math.Max(float64(len(ps))*rowH+m.top+m.bottom, 80)
	s := newSVG(chartW, h, "chart projects", unit+" by project")
	if len(ps) == 0 {
		s.text(10, 30, "tick", "start", "No sessions in this tab and range.")
		s.end("svg")
		return s.html()
	}
	mx := 1.0
	for _, p := range ps {
		mx = math.Max(mx, p.Total)
	}
	mx = view.NiceMax(mx)
	x := func(v float64) float64 { return m.left + v/mx*(chartW-m.left-m.right) }
	for i, p := range ps {
		yy := m.top + float64(i)*rowH
		label := p.Name
		if r := []rune(label); len(r) > 28 {
			label = "…" + string(r[len(r)-27:])
		}
		s.text(m.left-10, yy+rowH/2+4, "label", "end", label)
		base := 0.0
		for si, part := range p.Parts {
			x0 := x(base)
			if si > 0 {
				x0 += 2
			}
			bw := math.Max(x(base+part.Value)-x0, 1)
			r := 0.0
			if si == len(p.Parts)-1 {
				r = 4
			}
			s.el("path", "class", slotClass("s", groupSlot(part.Name)), "d", roundedRight(x0, yy+7, bw, rowH-14, r))
			base += part.Value
		}
		value := view.Compact(p.Total) + " · " + view.Duration(p.Active) + " · " + strconv.Itoa(p.Sessions) + " sess."
		if p.Class != "" {
			value += " · " + p.Class
		}
		s.text(x(p.Total)+8, yy+rowH/2+4, "value-label", "start", value)
		t := tip{H: p.Name}
		if p.Path != "" {
			t.H += " (" + p.Path + ")"
		}
		for _, part := range p.Parts {
			t.R = append(t.R, row(groupLabel(part.Name), view.Exact(part.Value), groupSlot(part.Name)))
		}
		t.R = append(t.R, row("Active time", view.Duration(p.Active), -1), row("Sessions", strconv.Itoa(p.Sessions), -1))
		if p.Class != "" {
			own := p.Class
			if p.Owner != "" {
				own += " (" + p.Owner + ")"
			}
			t.R = append(t.R, row("Ownership", own, -1))
		}
		s.el("rect", "class", "hit", "x", "0", "y", num(yy), "width", num(chartW), "height", num(rowH), "data-tip", t.attr())
	}
	s.end("svg")
	return s.html()
}

// crossCheckChart pairs the proxy's daily tokens with Codex Foundry's, which should track closely.
func crossCheckChart(days []view.DayPair) template.HTML {
	const h = 170.0
	m := margin{14, 18, 28, 60}
	mx := 0.0
	for _, d := range days {
		mx = math.Max(mx, math.Max(d.Proxy, d.Codex))
	}
	yMax := view.NiceMax(mx * 1.05)
	y := func(v float64) float64 { return h - m.bottom - v/yMax*(h-m.top-m.bottom) }
	s := newSVG(chartW, h, "chart xcheck", "Proxy vs Codex Foundry tokens per day")
	yGrid(s, m, h, yMax, y)
	n := max(len(days), 1)
	band := (chartW - m.left - m.right) / float64(n)
	bw := math.Max(math.Min((band-6)/2, 12), 1.5)
	every := max(1, int(math.Ceil(float64(n)/10)))
	codexSlot := groupSlot("codex:work-foundry")
	for i, d := range days {
		cx := m.left + float64(i)*band + band/2
		for j, v := range []float64{d.Proxy, d.Codex} {
			if v <= 0 {
				continue
			}
			xx := cx - bw - 1
			class := "s0"
			if j == 1 {
				xx = cx + 1
				class = slotClass("s", codexSlot)
			}
			s.el("path", "class", class, "d", roundedTop(xx, y(v), bw, y(0)-y(v), 3))
		}
		if i%every == 0 {
			s.text(cx, h-10, "tick", "middle", view.ShortDay(d.Day))
		}
		t := tip{H: view.DayLabel(d.Day), R: [][]any{row("foundry-entra-proxy", view.Exact(d.Proxy), 0),
			row("Codex · Foundry", view.Exact(d.Codex), codexSlot)}}
		s.el("rect", "class", "hit", "x", num(m.left+float64(i)*band), "y", num(m.top), "width", num(band),
			"height", num(h-m.top-m.bottom), "data-tip", t.attr())
	}
	s.end("svg")
	return s.html()
}
