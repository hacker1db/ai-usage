package view

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"ai-usage/internal/billing"
	"ai-usage/internal/pricing"
	"ai-usage/internal/snapshot"
	"ai-usage/internal/usage"
)

type Amount struct {
	Currency string
	Value    float64
}

func (a Amount) String() string { return Money(a.Value, a.Currency) }

func Money(v float64, cur string) string {
	s := fmt.Sprintf("%.2f", math.Abs(v))
	if i := strings.IndexByte(s, '.'); i > 3 {
		var b strings.Builder
		for j, r := range s[:i] {
			if j > 0 && (i-j)%3 == 0 {
				b.WriteByte(',')
			}
			b.WriteRune(r)
		}
		s = b.String() + s[i:]
	}
	sign := ""
	if v < 0 && s != "0.00" {
		sign = "-"
	}
	if cur == "USD" || cur == "" {
		return sign + "$" + s
	}
	return sign + s + " " + cur
}

type CostRow struct {
	Label     string
	Provider  string
	Slot      int
	Mode      string
	Month     string
	MonthName string
	Current   bool
	Totals    []Amount
	Projected *Amount
	Daily     []float64
	DailyMax  float64
	Top       []Named
	Currency  string
	Items     int
	Level     Level
	Status    string
	Fix       string
	Basis     string
	Retrieved time.Time

	PrevName   string
	PrevTotals []Amount
	PrevNote   string

	Local, PrevLocal *Amount
}

func (r CostRow) Headline() string {
	if len(r.Totals) == 0 && r.Local != nil {
		return "≈" + r.Local.String()
	}
	return JoinAmounts(r.Totals)
}

func (r CostRow) Prev() string {
	switch {
	case r.PrevNote != "":
		return r.PrevNote
	case len(r.PrevTotals) == 0 && r.PrevLocal != nil:
		return "≈" + r.PrevLocal.String() + " (local logs)"
	}
	return JoinAmounts(r.PrevTotals)
}

type Costs struct {
	Rows       []CostRow
	MonthTotal []Amount
	Month      string
	Sub        string
	Hint       string
	Warning    string
	Configured bool
}

func (d *Dashboard) buildCosts(s *snapshot.Snapshot, now time.Time) {
	c := Costs{Warning: s.Meta.BillingWarning, Configured: len(s.Costs) > 0}
	if !c.Configured {
		c.Hint = "No billing accounts found. Azure Foundry spend appears when a Claude or Codex profile uses a Foundry endpoint and the az CLI is installed; Copilot spend appears when the gh CLI is installed and Copilot CLI has been used here."
	}
	months := billing.Months(now)
	c.Month = monthName(months[0])
	credits := copilotCredits(s)
	total := map[string]float64{}

	var ids []string
	byID := map[string]map[string]billing.Status{}
	for _, st := range s.Costs {
		if byID[st.ID] == nil {
			byID[st.ID] = map[string]billing.Status{}
			ids = append(ids, st.ID)
		}
		byID[st.ID][st.Month] = st
	}
	for _, id := range ids {
		cur, ok := byID[id][months[0]]
		prev, hasPrev := byID[id][months[1]]
		if !ok {
			cur, ok = prev, false
		}
		row := costRow(cur, now, months[0])
		if hasPrev && ok {
			row.PrevName = monthName(prev.Month)
			p := costRow(prev, now, months[0])
			switch {
			case prev.Report == nil && (prev.Error == "" || prev.Retrying):
				row.PrevNote = "pending"
			case prev.Report == nil:
				row.PrevNote = "unavailable: " + prev.Error
			default:
				row.PrevTotals = p.Totals
			}
		}
		if cur.Provider == "copilot" {
			if v := credits[months[0]]; v > 0 {
				row.Local = &Amount{"USD", v * pricing.CopilotCreditUSD}
			}
			if v := credits[months[1]]; v > 0 {
				row.PrevLocal = &Amount{"USD", v * pricing.CopilotCreditUSD}
			}
			if len(row.Totals) == 0 && cur.Report != nil {
				row.Status = strings.Replace(row.Status, " · no metered usage reported",
					" · no metered usage billed to this GitHub account; a seat from an organization is billed to the organization's pooled AI credits", 1)
			}
		}
		if row.Current && cur.Report != nil {
			for _, a := range row.Totals {
				total[a.Currency] += a.Value
			}
		}
		c.Rows = append(c.Rows, row)
	}
	c.MonthTotal = amounts(total)
	c.Sub = "provider-reported, UTC months · not filtered by tab · never added to token counts"
	d.Costs = c
}

func copilotCredits(s *snapshot.Snapshot) map[string]float64 {
	out := map[string]float64{}
	for _, r := range s.Rows {
		if r.Tool == usage.ToolCopilot {
			out[r.T.UTC().Format("2006-01")] += r.CopilotAIU
		}
	}
	return out
}

func costRow(st billing.Status, now time.Time, current string) CostRow {
	row := CostRow{Label: st.Label, Month: st.Month, MonthName: monthName(st.Month), Current: st.Month == current,
		Provider: providerName(st.Provider), Mode: modeName(st.Mode), Fix: st.Fix, Slot: GroupOf("copilot", "github-copilot").Slot}
	if st.Provider == "azure" {
		row.Slot = GroupOf("claude-code", "work-foundry").Slot
	}
	next := ""
	if !st.Next.IsZero() && st.Next.After(now) {
		next = " · next update " + WhenLabel(st.Next.Local())
	}
	switch {
	case st.Report == nil && st.Error == "":
		row.Level, row.Status = Warning, "querying provider…"
		return row
	case st.Report == nil && st.Retrying:

		row.Level, row.Status = Warning, "waiting for the provider's rate limit; next try "+WhenLabel(st.Next.Local())
		return row
	case st.Report == nil:
		row.Level, row.Status = Critical, st.Error
		return row
	case st.Error != "" && !st.Retrying:
		row.Level, row.Status = Serious, "showing data from "+WhenLabel(st.Report.Retrieved.Local())+"; last query failed: "+st.Error
	default:

		row.Level, row.Status = Good, "updated "+WhenLabel(st.Report.Retrieved.Local())+next
	}
	r := st.Report
	row.Basis, row.Retrieved, row.Items = r.Basis, r.Retrieved, len(r.Items)
	totals := map[string]float64{}
	top := map[string]float64{}
	first, _ := billing.Month(st.Month)
	days := first.AddDate(0, 1, 0).Sub(first).Hours() / 24
	daily := make([]float64, int(days))
	dated := false
	for _, it := range r.Items {
		v, err := it.Net.Float64()
		if err != nil {
			continue
		}
		totals[it.Currency] += v
		top[itemName(it)] += v
		if t, err := time.Parse("2006-01-02", it.Date); err == nil {
			if i := int(t.Sub(first).Hours() / 24); i >= 0 && i < len(daily) {
				daily[i] += v
				dated = true
			}
		}
	}
	row.Totals = amounts(totals)
	if len(totals) == 0 {
		row.Status += " · no metered usage reported"
	}
	if len(row.Totals) == 1 {
		row.Currency = row.Totals[0].Currency
		for k, v := range top {
			if k != "" && v != 0 {
				row.Top = append(row.Top, Named{k, v})
			}
		}
		sort.Slice(row.Top, func(i, j int) bool { return row.Top[i].Value > row.Top[j].Value })
		row.Top = row.Top[:min(len(row.Top), 4)]
		if dated {
			row.Daily = daily
			for _, v := range daily {
				row.DailyMax = math.Max(row.DailyMax, v)
			}
		}
		if row.Current {
			elapsed := now.UTC().Sub(first).Hours() / 24
			if elapsed >= 1 && elapsed < days {
				p := Amount{row.Currency, row.Totals[0].Value / elapsed * days}
				row.Projected = &p
			}
		}
	}
	return row
}

func itemName(it billing.Item) string {
	switch {
	case it.Model != "":
		return it.Model
	case it.SKU != "":
		return it.SKU
	case it.Resource != "":
		if i := strings.LastIndexByte(it.Resource, '/'); i >= 0 {
			return it.Resource[i+1:]
		}
		return it.Resource
	}
	return ""
}

func amounts(m map[string]float64) []Amount {
	out := make([]Amount, 0, len(m))
	for k, v := range m {
		out = append(out, Amount{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

func JoinAmounts(as []Amount) string {
	parts := make([]string, len(as))
	for i, a := range as {
		parts[i] = a.String()
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " · ")
}

func monthName(m string) string {
	t, err := billing.Month(m)
	if err != nil {
		return m
	}
	return t.Format("Jan 2006")
}

func providerName(p string) string {
	switch p {
	case "azure":
		return "Azure Foundry"
	case "copilot":
		return "GitHub Copilot"
	}
	return p
}

func modeName(m string) string {
	switch m {
	case "ai_credit":
		return "AI credits"
	case "premium_request":
		return "premium requests"
	}
	return m
}
