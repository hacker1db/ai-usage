package plancost

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ai-usage/internal/usage"
)

func writePricing(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pricing.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodPricing = `{
  "currency": "USD",
  "unit": "per_million_tokens",
  "models": {
    "alpha": {"input": 10, "cached_input": 1, "cache_write": 12.5, "output": 50},
    "beta-*": {"input": 2, "cached_input": 0.2, "cache_write": 0, "output": 8},
    "beta-large-*": {"input": 4, "cached_input": 0.4, "cache_write": 0, "output": 16}
  }
}`

func TestLoadPricingMissingFileIsUnloaded(t *testing.T) {
	p, err := LoadPricing(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Loaded {
		t.Fatal("missing file reported as loaded")
	}
	if _, ok := p.Lookup("alpha"); ok {
		t.Fatal("missing file priced a model")
	}
}

func TestLoadPricingRejectsBadFiles(t *testing.T) {
	cases := map[string]string{
		"typo field":       `{"currency":"USD","unit":"per_million_tokens","models":{"a":{"input":1,"cached_input":1,"cache_write":1,"ouput":1}}}`, //nolint:misspell // the typo is the test case.
		"missing field":    `{"currency":"USD","unit":"per_million_tokens","models":{"a":{"input":1,"cached_input":1,"cache_write":1}}}`,
		"null field":       `{"currency":"USD","unit":"per_million_tokens","models":{"a":{"input":1,"cached_input":1,"cache_write":1,"output":null}}}`,
		"negative":         `{"currency":"USD","unit":"per_million_tokens","models":{"a":{"input":-1,"cached_input":1,"cache_write":1,"output":1}}}`,
		"string price":     `{"currency":"USD","unit":"per_million_tokens","models":{"a":{"input":"1","cached_input":1,"cache_write":1,"output":1}}}`,
		"wrong currency":   `{"currency":"EUR","unit":"per_million_tokens","models":{}}`,
		"wrong unit":       `{"currency":"USD","unit":"per_token","models":{}}`,
		"unknown top key":  `{"currency":"USD","unit":"per_million_tokens","models":{},"extra":1}`,
		"star everything":  `{"currency":"USD","unit":"per_million_tokens","models":{"*":{"input":1,"cached_input":1,"cache_write":1,"output":1}}}`,
		"star in middle":   `{"currency":"USD","unit":"per_million_tokens","models":{"a*b":{"input":1,"cached_input":1,"cache_write":1,"output":1}}}`,
		"trailing garbage": `{"currency":"USD","unit":"per_million_tokens","models":{}} {}`,
		"trailing bracket": `{"currency":"USD","unit":"per_million_tokens","models":{}}]`,
		"not json":         `nope`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadPricing(writePricing(t, body)); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestLookupPrecedence(t *testing.T) {
	p, err := LoadPricing(writePricing(t, goodPricing))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Loaded || len(p.SHA256) != 64 {
		t.Fatalf("loaded=%v sha=%q", p.Loaded, p.SHA256)
	}
	for model, wantIn := range map[string]float64{"alpha": 10, "beta-small": 2, "beta-large-2": 4} {
		got, ok := p.Lookup(model)
		if !ok || got.Input != wantIn {
			t.Errorf("%s: got %+v ok=%v, want input %v", model, got, ok, wantIn)
		}
	}
	for _, model := range []string{"alpha-2", "beta", "gamma", ""} {
		if _, ok := p.Lookup(model); ok {
			t.Errorf("%q should be unpriced", model)
		}
	}
}

func TestCostSplitsInputClasses(t *testing.T) {

	m := usage.Metrics{Input: 4_000_000, Cached: 2_000_000, CacheWrite: 1_000_000, Output: 1_000_000}
	got := Price{Input: 10, CachedInput: 1, CacheWrite: 12.5, Output: 50}.Cost(m)
	if want := 10 + 2 + 12.5 + 50.0; got != want {
		t.Fatalf("cost %v, want %v", got, want)
	}

	bad := usage.Metrics{Input: 1, Cached: 5, Output: 0}
	if c := (Price{Input: 100, CachedInput: 1}).Cost(bad); c != 5.0/1e6 {
		t.Fatalf("cost %v", c)
	}
}

type fakeSource struct {
	rows   []usage.SessionRow
	models map[usage.SessionKey]map[string]usage.Metrics
}

func (f fakeSource) SessionRows(bool) []usage.SessionRow { return f.rows }
func (f fakeSource) SessionModels(k usage.SessionKey) map[string]usage.Metrics {
	return f.models[k]
}

func newFake() fakeSource {
	a := usage.SessionKey{Tool: "claude-code", Profile: "personal", ID: "s1"}
	b := usage.SessionKey{Tool: "codex", Profile: "work", ID: "s2"}
	c := usage.SessionKey{Tool: "codex", Profile: "personal", ID: "s2"}
	row := func(k usage.SessionKey) usage.SessionRow {
		return usage.SessionRow{Tool: k.Tool, Profile: k.Profile, ID: k.ID, Title: k.ID + " title"}
	}
	return fakeSource{
		rows: []usage.SessionRow{row(a), row(b), row(c)},
		models: map[usage.SessionKey]map[string]usage.Metrics{
			a: {"alpha": {Requests: 2, Input: 2_000_000, Cached: 1_000_000, Output: 100_000, Total: 2_100_000}},
			b: {
				"beta-small": {Requests: 1, Input: 1_000_000, Output: 1_000_000, Total: 2_000_000},
				"gamma":      {Requests: 1, Input: 500, Output: 500, Total: 1000},
			},
			c: {"alpha": {Requests: 1, Input: 1_000_000, Total: 1_000_000}},
		},
	}
}

func TestResolve(t *testing.T) {
	rows := newFake().rows
	keys, missing, err := Resolve(rows, []string{"s1", " claude-code|personal|s1 ", "codex|work|s2", "zzz", "zzz", "codex|x|s2", ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].ID != "s1" || keys[1].Profile != "work" {
		t.Fatalf("keys %v", keys)
	}
	if strings.Join(missing, ",") != "zzz,codex|x|s2" {
		t.Fatalf("missing %v", missing)
	}
	if _, _, err := Resolve(rows, []string{"s2"}); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("want ambiguous, got %v", err)
	}
	for _, bad := range []string{"a|b", "a||c", "a|b|c|d", "|b|c"} {
		if _, _, err := Resolve(rows, []string{bad}); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestComputeComplete(t *testing.T) {
	p, err := LoadPricing(writePricing(t, goodPricing))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Compute(newFake(), []string{"s1", "codex|personal|s2"}, p, nil, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Complete || s.EstimatedCostUSD == nil {
		t.Fatalf("want complete: %+v", s)
	}

	if *s.EstimatedCostUSD != 26 || s.PricedCostUSD != 26 {
		t.Fatalf("cost %v / %v", *s.EstimatedCostUSD, s.PricedCostUSD)
	}
	if s.Totals.Total != 3_100_000 || s.Totals.Requests != 3 {
		t.Fatalf("totals %+v", s.Totals)
	}
	if len(s.Models) != 1 || s.Models[0].Model != "alpha" || *s.Models[0].CostUSD != 26 {
		t.Fatalf("models %+v", s.Models)
	}
}

func TestComputeIncompleteNeverReportsZero(t *testing.T) {
	p, err := LoadPricing(writePricing(t, goodPricing))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Compute(newFake(), []string{"codex|work|s2", "ghost"}, p, []string{"copilot: error: locked"}, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if s.Complete {
		t.Fatal("summary with missing session, unpriced model, and source error marked complete")
	}
	if s.EstimatedCostUSD != nil {
		t.Fatalf("estimate must be null with unpriced models, got %v", *s.EstimatedCostUSD)
	}
	if s.PricedCostUSD != 10 {
		t.Fatalf("priced %v", s.PricedCostUSD)
	}
	if strings.Join(s.UnpricedModels, ",") != "gamma" || strings.Join(s.MissingSessions, ",") != "ghost" {
		t.Fatalf("unpriced %v missing %v", s.UnpricedModels, s.MissingSessions)
	}
	if s.Sessions[0].CostUSD != nil || s.Sessions[0].PricedCostUSD != 10 {
		t.Fatalf("session cost %+v", s.Sessions[0])
	}

	b, _ := json.Marshal(s)
	for _, want := range []string{`"estimated_cost_usd":null`, `"version":1`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON lacks %s: %s", want, b)
		}
	}
}

func TestComputeMissingSessionNullsEstimate(t *testing.T) {
	p, err := LoadPricing(writePricing(t, goodPricing))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Compute(newFake(), []string{"s1", "ghost"}, p, nil, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if s.Complete || s.EstimatedCostUSD != nil || s.PricedCostUSD != 16 {
		t.Fatalf("missing session must null the estimate: complete=%v est=%v priced=%v", s.Complete, s.EstimatedCostUSD, s.PricedCostUSD)
	}
	if s.Sessions[0].CostUSD == nil || *s.Sessions[0].CostUSD != 16 {
		t.Fatalf("a fully priced session keeps its own cost: %+v", s.Sessions[0])
	}
}

func TestModelCostMatchesTotalAcrossSessions(t *testing.T) {
	p, err := LoadPricing(writePricing(t, goodPricing))
	if err != nil {
		t.Fatal(err)
	}
	a := usage.SessionKey{Tool: "t", Profile: "p", ID: "a"}
	b := usage.SessionKey{Tool: "t", Profile: "p", ID: "b"}
	src := fakeSource{
		rows: []usage.SessionRow{{Tool: "t", Profile: "p", ID: "a"}, {Tool: "t", Profile: "p", ID: "b"}},
		models: map[usage.SessionKey]map[string]usage.Metrics{
			a: {"alpha": {Requests: 1, Input: 1_000_000, Cached: 2_000_000, Total: 1_000_000}},
			b: {"alpha": {Requests: 1, Input: 1_000_000, Total: 1_000_000}},
		},
	}
	s, err := Compute(src, []string{"a", "b"}, p, nil, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}

	if *s.EstimatedCostUSD != 12 || *s.Models[0].CostUSD != 12 {
		t.Fatalf("total %v, model %v", *s.EstimatedCostUSD, *s.Models[0].CostUSD)
	}
}

func TestComputeWithoutPricingOrSessions(t *testing.T) {
	s, err := Compute(newFake(), []string{"ghost"}, &Pricing{Path: "x"}, nil, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if s.Complete {
		t.Fatal("nothing found must not be complete")
	}
	b, _ := json.Marshal(s)
	for _, want := range []string{`"sessions":[]`, `"models":[]`, `"unpriced_models":[]`, `"source_errors":[]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON lacks %s: %s", want, b)
		}
	}
}

func TestAggregatorSessionModels(t *testing.T) {
	agg := usage.NewAggregator(time.Time{}, usage.Daily)
	now := time.Now()
	agg.Add("claude-code", "p", usage.Record{Model: "alpha", Time: now, Session: "s", Input: 10, Cached: 4, Output: 2})
	agg.Add("claude-code", "p", usage.Record{Model: "alpha", Time: now, Session: "s", Input: 5, Output: 1})
	agg.Add("claude-code", "p", usage.Record{Model: "", Time: now, Session: "s", Input: 1})
	got := agg.SessionModels(usage.SessionKey{Tool: "claude-code", Profile: "p", ID: "s"})
	if a := got["alpha"]; a.Requests != 2 || a.Input != 15 || a.Cached != 4 || a.Output != 3 || a.Total != 18 {
		t.Fatalf("alpha %+v", a)
	}
	if got["unknown"].Requests != 1 {
		t.Fatalf("unknown model not tracked: %+v", got)
	}
	if agg.SessionModels(usage.SessionKey{Tool: "x", Profile: "p", ID: "s"}) != nil {
		t.Fatal("unknown session returned metrics")
	}
}
