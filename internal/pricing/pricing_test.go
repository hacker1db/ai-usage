package pricing

import (
	"math"
	"testing"

	"ai-usage/internal/usage"
)

func TestLookupVariants(t *testing.T) {
	for _, m := range []string{"claude-opus-5-5", "claude-fable-5[1m]", "anthropic.claude-opus-4-8", "claude-opus-4.8",
		"claude-sonnet-4-5-20250929", "openai/gpt-6.1-sol", "GPT-6-LUNA"} {
		if _, ok := Lookup(m); !ok {
			t.Errorf("Lookup(%q) not found", m)
		}
	}
	for _, m := range []string{"opus", "<synthetic>", "codex-auto-review", ""} {
		if _, ok := Lookup(m); ok {
			t.Errorf("Lookup(%q) found an ambiguous or unknown model", m)
		}
	}
}

func TestCostSplitsInputByCacheKind(t *testing.T) {
	p, _ := Lookup("claude-opus-5-5") // $4 in, $5 5m write, $8 1h write, $0.20 read, $20 out
	x := usage.Metrics{Input: 4_000_000, Cached: 1_000_000, CacheWrite: 2_000_000, CacheWrite1h: 1_000_000,
		Output: 1_000_000, Total: 5_000_000}
	// fresh 1M*4 + read 1M*0.20 + 5m write 1M*5 + 1h write 1M*8 + out 1M*20
	if got, want := p.Cost(x), 4+0.20+5+8+20.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

func TestEstimateTracksUnpriced(t *testing.T) {
	var e Estimate
	e.Add("gpt-6.1-sol", usage.Metrics{Input: 1_000_000, Total: 1_000_000})
	e.Add("opus", usage.Metrics{Input: 10, Total: 10})
	e.Add("codex-auto-review", usage.Metrics{Input: 50, Total: 50})
	if math.Abs(e.USD-2) > 1e-9 || e.Priced != 1_000_000 || e.UnpricedTokens() != 60 {
		t.Fatalf("estimate = %+v", e)
	}
	if got := e.UnpricedModels(); got[0] != "codex-auto-review" {
		t.Fatalf("unpriced = %v", got)
	}
}
