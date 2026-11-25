package view

import (
	"sort"
	"strings"
	"time"

	"ai-usage/internal/history"
	"ai-usage/internal/pricing"
	"ai-usage/internal/snapshot"
	"ai-usage/internal/usage"
)

type APIRow struct {
	Group                   Group
	Range, Month, PrevMonth float64
	Projected               float64
	Top                     []Named
}

type APICost struct {
	Rows                    []APIRow
	Range, Month, PrevMonth float64
	Projected               float64
	MonthName, PrevName     string
	PrevPartial             string
	Unpriced                []string
	UnpricedTokens          int64
	AsOf                    string
	Sources                 []string
	Sub                     string
}

func priced(r history.HourRow) bool { return r.Tool != usage.ToolProxy }

func addRow(e *pricing.Estimate, r history.HourRow) {
	if r.Tool == usage.ToolCopilot && r.CopilotAIU > 0 {
		e.AddCredits(r.CopilotAIU, r.Metrics)
		return
	}
	e.Add(r.Model, r.Metrics)
}

func (d *Dashboard) buildAPICost(s *snapshot.Snapshot, rows []history.HourRow, from, now time.Time) {
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	prevStart := monthStart.AddDate(0, -1, 0)
	monthEnd := monthStart.AddDate(0, 1, 0)
	c := APICost{MonthName: monthStart.Format("Jan"), PrevName: prevStart.Format("Jan"), AsOf: pricing.AsOf,
		Sources: pricing.Sources}
	if since := s.Meta.Since; !since.IsZero() && since.After(prevStart) {
		c.PrevPartial = "from " + since.Local().Format("Jan 2")
	}
	type acc struct {
		g                Group
		rng, month, prev pricing.Estimate
		models           map[string]float64
	}
	by := map[string]*acc{}
	var unpriced pricing.Estimate
	for _, r := range rows {
		if !priced(r) {
			continue
		}
		g := GroupOf(r.Tool, r.Profile)
		a := by[g.Key]
		if a == nil {
			a = &acc{g: g, models: map[string]float64{}}
			by[g.Key] = a
		}
		var e pricing.Estimate
		addRow(&e, r)
		if !r.T.Before(from) {
			a.rng.Merge(e)
			a.models[r.Model] += e.USD
			unpriced.Merge(pricing.Estimate{Unpriced: e.Unpriced})
		}
		switch {
		case !r.T.Before(monthStart) && r.T.Before(monthEnd):
			a.month.Merge(e)
		case !r.T.Before(prevStart) && r.T.Before(monthStart):
			a.prev.Merge(e)
		}
	}
	elapsed := now.Sub(monthStart).Hours()
	days := monthEnd.Sub(monthStart).Hours()
	project := func(v float64) float64 {
		if elapsed < 24 {
			return 0
		}
		return v / elapsed * days
	}
	for _, a := range by {
		row := APIRow{Group: a.g, Range: a.rng.USD, Month: a.month.USD, PrevMonth: a.prev.USD, Projected: project(a.month.USD)}
		for m, v := range a.models {
			if v > 0 {
				row.Top = append(row.Top, Named{m, v})
			}
		}
		sort.Slice(row.Top, func(i, j int) bool { return row.Top[i].Value > row.Top[j].Value })
		row.Top = row.Top[:min(len(row.Top), 3)]
		if row.Range == 0 && row.Month == 0 && row.PrevMonth == 0 {
			continue
		}
		c.Rows = append(c.Rows, row)
		c.Range += row.Range
		c.Month += row.Month
		c.PrevMonth += row.PrevMonth
	}
	c.Projected = project(c.Month)
	sort.Slice(c.Rows, func(i, j int) bool {
		if c.Rows[i].Group.order() != c.Rows[j].Group.order() {
			return c.Rows[i].Group.order() < c.Rows[j].Group.order()
		}
		return c.Rows[i].Group.Key < c.Rows[j].Group.Key
	})
	c.Unpriced, c.UnpricedTokens = unpriced.UnpricedModels(), unpriced.UnpricedTokens()
	c.Sub = "token usage at public pay-as-you-go API prices · follows the tab and profile filters · not a bill"
	d.APICost = c
}

func (c APICost) UnpricedList() string { return strings.Join(c.Unpriced, ", ") }
