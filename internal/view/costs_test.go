package view

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ai-usage/internal/billing"
	"ai-usage/internal/history"
	"ai-usage/internal/snapshot"
	"ai-usage/internal/usage"
)

func TestMoney(t *testing.T) {
	for v, want := range map[float64]string{0: "$0.00", 1234567.891: "$1,234,567.89", -3.5: "-$3.50", 999.999: "$1,000.00"} {
		if got := Money(v, "USD"); got != want {
			t.Errorf("Money(%v) = %q, want %q", v, got, want)
		}
	}
	if got := Money(4, "EUR"); got != "4.00 EUR" {
		t.Error(got)
	}
}

func TestBuildCosts(t *testing.T) {
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC) // 10 days into a 30-day month
	az := &billing.Report{Retrieved: now, Basis: "b", Items: []billing.Item{
		{Date: "2026-11-01", Resource: "/x/accounts/foundry", Currency: "USD", Net: json.Number("6")},
		{Date: "2026-11-03", Resource: "/x/accounts/foundry", Currency: "USD", Net: json.Number("4")},
	}}
	cp := &billing.Report{Retrieved: now, Items: []billing.Item{{Model: "gpt", Currency: "USD", Net: json.Number("1.5")}}}
	s := &snapshot.Snapshot{Costs: []billing.Status{
		{ID: "az", Label: "Foundry", Provider: "azure", Month: "2026-11", Report: az},
		{ID: "az", Label: "Foundry", Provider: "azure", Month: "2026-10"},
		{ID: "cp", Label: "Copilot", Provider: "copilot", Mode: "ai_credit", Month: "2026-11", Report: cp, Error: "403"},
		{ID: "cp", Label: "Copilot", Provider: "copilot", Mode: "ai_credit", Month: "2026-10", Error: "denied"},
	}}
	var d Dashboard
	d.buildCosts(s, now)
	c := d.Costs
	if !c.Configured || len(c.Rows) != 2 || JoinAmounts(c.MonthTotal) != "$11.50" {
		t.Fatalf("want one card per account: %+v", c)
	}
	a := c.Rows[0]
	if a.Level != Good || a.Projected == nil || a.Projected.String() != "$30.00" || len(a.Daily) != 30 || a.Daily[2] != 4 || a.Top[0].Name != "foundry" {
		t.Fatalf("azure row %+v", a)
	}
	if a.PrevName != "Oct 2026" || a.Prev() != "pending" {
		t.Fatalf("azure previous month %q %q", a.PrevName, a.Prev())
	}
	if cp := c.Rows[1]; cp.Level != Serious || cp.Prev() != "unavailable: denied" {
		t.Fatalf("copilot row %+v", cp)
	}
	d.buildCosts(&snapshot.Snapshot{Costs: []billing.Status{{Provider: "azure", Month: "2026-11", Error: "rate limited", Retrying: true, Next: now.Add(time.Hour)}}}, now)
	if r := d.Costs.Rows[0]; r.Level != Warning || !strings.Contains(r.Status, "next try") {
		t.Fatalf("a rate limit before any data should be a wait, got %s %q", r.Level, r.Status)
	}
	// A rate-limited refresh with data already shown is not an issue: the card stays good.
	d.buildCosts(&snapshot.Snapshot{Costs: []billing.Status{{Provider: "azure", Month: "2026-11", Report: az, Error: "rate limited",
		Retrying: true, Next: now.Add(time.Hour)}}}, now)
	if r := d.Costs.Rows[0]; r.Level != Good || strings.Contains(r.Status, "rate") || !strings.Contains(r.Status, "next update") {
		t.Fatalf("rate-limited refresh should look normal, got %s %q", r.Level, r.Status)
	}
	// An org-billed Copilot seat: the personal endpoint reports nothing, so the card shows the
	// locally logged AI credits instead of a misleading zero.
	empty := &billing.Report{Retrieved: now}
	org := &snapshot.Snapshot{
		Costs: []billing.Status{{ID: "cp", Provider: "copilot", Month: "2026-11", Report: empty}},
		Rows:  []history.HourRow{{Tool: usage.ToolCopilot, CopilotAIU: 1711.7, T: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}},
	}
	d.buildCosts(org, now)
	if r := d.Costs.Rows[0]; r.Headline() != "≈$17.12" || JoinAmounts(d.Costs.MonthTotal) != "—" {
		t.Fatalf("org seat headline %q, total %v", r.Headline(), d.Costs.MonthTotal)
	}
	d.buildCosts(&snapshot.Snapshot{}, now)
	if d.Costs.Configured || d.Costs.Hint == "" {
		t.Fatal("expected setup hint")
	}
}
