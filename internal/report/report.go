// Package report writes the dated per tool/profile/model/day report (report.md, usage.json,
// usage.csv), replacing the earlier collect.py.
package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"ai-usage/internal/sources"
	"ai-usage/internal/usage"
)

// Meta describes a report run.
type Meta struct {
	Generated string `json:"generated"`
	Since     string `json:"since"`
	Until     string `json:"until"`
	TZ        string `json:"tz"`
	Days      int    `json:"days"`
}

// FlatRow is one usage.json / usage.csv row.
type FlatRow struct {
	Tool       string  `json:"tool"`
	Profile    string  `json:"profile"`
	Model      string  `json:"model"`
	Day        string  `json:"day"`
	Sessions   int     `json:"sessions"`
	Requests   int64   `json:"requests"`
	Input      int64   `json:"input"`
	Cached     int64   `json:"cached"`
	CacheWrite int64   `json:"cache_write"`
	Output     int64   `json:"output"`
	Reasoning  int64   `json:"reasoning"`
	Total      int64   `json:"total"`
	CopilotAIU float64 `json:"copilot_aiu"`
}

// Document is usage.json.
type Document struct {
	Meta    Meta             `json:"meta"`
	Sources []sources.Status `json:"sources"`
	Rows    []FlatRow        `json:"rows"`
}

// Cutoff is local midnight days-1 days before now, so "7 days" means today plus six full days.
func Cutoff(now time.Time, days int) time.Time {
	d := now.AddDate(0, 0, -(days - 1))
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, now.Location())
}

// Build collects daily rows for the window.
func Build(now time.Time, days int, opts sources.Options) (Document, *usage.Aggregator) {
	cutoff := Cutoff(now, days)
	agg := usage.NewAggregator(cutoff, usage.Daily)
	status, _ := sources.CollectAll(agg, opts, nil)
	doc := Document{
		Meta: Meta{Generated: now.Format(time.RFC3339), Since: cutoff.Format("2006-01-02"),
			Until: now.Format("2006-01-02"), TZ: now.Format("MST"), Days: days},
		Sources: status,
	}
	rows := agg.Rows()
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Bucket < rows[j].Bucket })
	for _, r := range rows {
		doc.Rows = append(doc.Rows, FlatRow{r.Tool, r.Profile, r.Model, r.Bucket, r.Sessions, r.Requests, r.Input,
			r.Cached, r.CacheWrite, r.Output, r.Reasoning, r.Total, math.Round(r.CopilotAIU*1e4) / 1e4})
	}
	return doc, agg
}

// Write stores report.md, usage.json, and usage.csv in outRoot/<today>/ and returns that folder
// and the markdown.
func Write(doc Document, agg *usage.Aggregator, outRoot string) (string, string, error) {
	dir := filepath.Join(usage.ExpandHome(outRoot), doc.Meta.Until)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	js, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "usage.json"), js, 0o644); err != nil {
		return "", "", err
	}
	if err := writeCSV(filepath.Join(dir, "usage.csv"), doc.Rows); err != nil {
		return "", "", err
	}
	md := Markdown(doc, agg)
	return dir, md, os.WriteFile(filepath.Join(dir, "report.md"), []byte(md), 0o644)
}

func writeCSV(path string, rows []FlatRow) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"tool", "profile", "model", "day", "sessions", "requests", "input", "cached",
		"cache_write", "output", "reasoning", "total", "copilot_aiu"})
	i := func(n int64) string { return strconv.FormatInt(n, 10) }
	for _, r := range rows {
		_ = w.Write([]string{r.Tool, r.Profile, r.Model, r.Day, strconv.Itoa(r.Sessions), i(r.Requests), i(r.Input),
			i(r.Cached), i(r.CacheWrite), i(r.Output), i(r.Reasoning), i(r.Total), strconv.FormatFloat(r.CopilotAIU, 'f', -1, 64)})
	}
	w.Flush()
	return w.Error()
}

type group struct {
	key      []string
	m        usage.Metrics
	sessions int
	aiu      float64
}

// rollup groups non-proxy (or proxy-only) rows by keyFn, counting distinct sessions per group.
func rollup(agg *usage.Aggregator, proxy bool, keyFn func(usage.Key) []string) []group {
	byKey := map[string]*group{}
	var order []string
	for _, r := range agg.Rows() {
		if (r.Tool == usage.ToolProxy) != proxy {
			continue
		}
		k := keyFn(r.Key)
		id := strings.Join(k, "\x00")
		g := byKey[id]
		if g == nil {
			g = &group{key: k}
			byKey[id] = g
			order = append(order, id)
		}
		g.m.Add(r.Metrics)
		g.aiu += r.CopilotAIU
	}
	out := make([]group, 0, len(order))
	for _, id := range order {
		g := byKey[id]
		g.sessions = len(agg.SessionSet(func(k usage.Key) bool {
			return (k.Tool == usage.ToolProxy) == proxy && strings.Join(keyFn(k), "\x00") == id
		}))
		out = append(out, *g)
	}
	return out
}

func n(v int64) string {
	s := strconv.FormatInt(v, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func table(title string, groups []group, headers []string) []string {
	lines := []string{"## " + title, "",
		"| " + strings.Join(append(append([]string{}, headers...), "Sessions", "Requests", "Input", "Cached", "Output", "Reasoning", "Total"), " | ") + " |",
		"|" + strings.Repeat("---|", len(headers)+7)}
	sort.SliceStable(groups, func(i, j int) bool {
		if len(headers) < 3 {
			return groups[i].m.Total > groups[j].m.Total
		}
		return strings.Join(groups[i].key, "\x00") < strings.Join(groups[j].key, "\x00")
	})
	for _, g := range groups {
		cells := append(append([]string{}, g.key...), n(int64(g.sessions)), n(g.m.Requests), n(g.m.Input),
			n(g.m.Cached), n(g.m.Output), n(g.m.Reasoning), n(g.m.Total))
		lines = append(lines, "| "+strings.Join(cells, " | ")+" |")
	}
	return append(lines, "")
}

// Markdown renders report.md.
func Markdown(doc Document, agg *usage.Aggregator) string {
	out := []string{fmt.Sprintf("# AI usage report (%s to %s)", doc.Meta.Since, doc.Meta.Until), "",
		fmt.Sprintf("Generated %s by `ai-usage report`. Days are local time (%s).", doc.Meta.Generated, doc.Meta.TZ), "",
		"## Sources", "", "| Source | Path | Status | Requests in window |", "|---|---|---|---|"}
	for _, s := range doc.Sources {
		out = append(out, fmt.Sprintf("| %s | `%s` | %s | %s |", s.Source, s.Path, s.Status, n(int64(s.Requests))))
	}
	out = append(out, "")
	if all := rollup(agg, false, func(usage.Key) []string { return []string{"all"} }); len(all) > 0 {
		g := all[0]
		out = append(out, fmt.Sprintf("**Total (excluding proxy cross-check):** %s tokens (%s input, of which %s cached; %s output), %s requests, %s sessions.",
			n(g.m.Total), n(g.m.Input), n(g.m.Cached), n(g.m.Output), n(g.m.Requests), n(int64(g.sessions))), "")
	}
	out = append(out, table("By tool and profile", rollup(agg, false, func(k usage.Key) []string { return []string{k.Tool, k.Profile} }), []string{"Tool", "Profile"})...)
	out = append(out, table("By model", rollup(agg, false, func(k usage.Key) []string { return []string{k.Tool, k.Profile, k.Model} }), []string{"Tool", "Profile", "Model"})...)
	out = append(out, table("By day", rollup(agg, false, func(k usage.Key) []string { return []string{k.Bucket, k.Tool, k.Profile} }), []string{"Day", "Tool", "Profile"})...)

	var cop []group
	for _, g := range rollup(agg, false, func(k usage.Key) []string { return []string{k.Model} }) {
		if g.aiu > 0 {
			cop = append(cop, g)
		}
	}
	if len(cop) > 0 {
		sort.SliceStable(cop, func(i, j int) bool { return cop[i].aiu > cop[j].aiu })
		out = append(out, "## Copilot AI units (local estimate, total_nano_aiu / 1e9)", "", "| Model | AIU |", "|---|---|")
		for _, g := range cop {
			out = append(out, fmt.Sprintf("| %s | %s |", g.key[0], aiu(g.aiu)))
		}
		out = append(out, "")
	}
	if proxy := rollup(agg, true, func(k usage.Key) []string { return []string{k.Profile, k.Model} }); len(proxy) > 0 {
		out = append(out, table("foundry-entra-proxy (cross-check; overlaps Codex work-foundry)", proxy, []string{"Client", "Model"})...)
	}
	out = append(out, "## Notes", "",
		"- Input includes cached and cache-write tokens for every tool, so Total = Input + Output.",
		"- Reasoning tokens are a subset of Output (OpenAI and Copilot); Claude logs do not report them.",
		"- Copilot premium-request billing is only authoritative in GitHub billing; see README.", "")
	return strings.Join(out, "\n")
}

func aiu(v float64) string {
	whole := int64(v)
	return n(whole) + "." + fmt.Sprintf("%02d", int64(math.Round((v-float64(whole))*100))%100)
}
