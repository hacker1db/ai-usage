// Package budget reads optional token budgets from dashboard.json and computes period bounds.
package budget

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Period is a budget reset cycle.
type Period string

const (
	Day   Period = "day"
	Week  Period = "week"
	Month Period = "month"
)

// Metric is what a budget counts.
type Metric string

const (
	Total    Metric = "total"    // input (incl. cached) + output
	Uncached Metric = "uncached" // input - cached + output
	Output   Metric = "output"
)

// Budget is one tool:profile budget.
type Budget struct {
	Tokens       int64  `json:"tokens"`
	Metric       Metric `json:"metric"`
	Period       Period `json:"period"`
	ResetDay     int    `json:"reset_day"`     // 1-28, for month
	ResetWeekday int    `json:"reset_weekday"` // 0=Mon..6=Sun, for week
}

// Same reports whether two budgets reset together and count the same metric, so they can be summed.
func (b Budget) Same(o Budget) bool {
	return b.Metric == o.Metric && b.Period == o.Period && b.ResetDay == o.ResetDay && b.ResetWeekday == o.ResetWeekday
}

type rawBudget struct {
	Tokens       *float64 `json:"tokens"`
	Metric       string   `json:"metric"`
	Period       string   `json:"period"`
	ResetDay     *int     `json:"reset_day"`
	ResetWeekday *int     `json:"reset_weekday"`
}

// Load reads budgets keyed by "tool:profile". A missing file is no budgets and no error; problems
// with individual entries are reported in warning while the valid ones still load.
func Load(path string) (budgets map[string]Budget, warning string) {
	budgets = map[string]Budget{}
	if path == "" {
		return budgets, ""
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return budgets, ""
	}
	name := filepath.Base(path)
	if err != nil {
		return budgets, fmt.Sprintf("%s: %v", name, err)
	}
	var f struct {
		Budgets map[string]json.RawMessage `json:"budgets"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return budgets, fmt.Sprintf("%s: %v", name, err)
	}
	var problems []string
	keys := make([]string, 0, len(f.Budgets))
	for k := range f.Budgets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.HasPrefix(key, "_") {
			continue
		}
		var b rawBudget
		if !strings.Contains(key, ":") || json.Unmarshal(f.Budgets[key], &b) != nil {
			problems = append(problems, fmt.Sprintf(`%s: expected "tool:profile": {...}`, key))
			continue
		}
		period := Period(b.Period)
		if period == "" {
			period = Month
		}
		if (period != Day && period != Week && period != Month) || b.Tokens == nil || *b.Tokens <= 0 {
			problems = append(problems, fmt.Sprintf("%s: needs tokens > 0 and period in (day, week, month)", key))
			continue
		}
		metric := Metric(b.Metric)
		if metric != Uncached && metric != Output {
			metric = Total
		}
		budgets[key] = Budget{Tokens: int64(*b.Tokens), Metric: metric, Period: period,
			ResetDay: clamp(deref(b.ResetDay, 1), 1, 28), ResetWeekday: clamp(deref(b.ResetWeekday, 0), 0, 6)}
	}
	return budgets, strings.Join(problems, "; ")
}

func deref(p *int, d int) int {
	if p == nil {
		return d
	}
	return *p
}

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }

// Bounds returns the local [start, end) of the period containing now.
func Bounds(now time.Time, p Period, resetDay, resetWeekday int) (time.Time, time.Time) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch p {
	case Day:
		return day, day.AddDate(0, 0, 1)
	case Week:
		back := (int(day.Weekday()) + 6 - resetWeekday) % 7 // Weekday: 0=Sun; resetWeekday: 0=Mon
		start := day.AddDate(0, 0, -back)
		return start, start.AddDate(0, 0, 7)
	}
	start := time.Date(now.Year(), now.Month(), resetDay, 0, 0, 0, 0, now.Location())
	if start.After(now) {
		start = start.AddDate(0, -1, 0)
	}
	return start, start.AddDate(0, 1, 0)
}
