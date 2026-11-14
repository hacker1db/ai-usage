// Package pricing estimates what token usage would cost at the providers' public pay-as-you-go API
// prices. It is an API-equivalent figure, not a bill: subscriptions (Claude Pro/Max, ChatGPT) are
// flat-rate, and provider-reported billing lives in the billing package.
package pricing

import (
	"sort"
	"strings"

	"ai-usage/internal/usage"
)

// AsOf is when the table was copied from the sources below.
const AsOf = "2026-11-12"

// Sources are the public price pages the table comes from.
var Sources = []string{
	"https://platform.claude.com/docs/en/about-claude/pricing",
	"https://developers.openai.com/api/docs/pricing",
	"https://docs.github.com/en/copilot/reference/copilot-billing/models-and-pricing",
}

// CopilotCreditUSD is GitHub's published rate for one Copilot AI credit
// (https://docs.github.com/en/copilot/concepts/billing-and-usage/individuals/billing). Copilot's
// local logs record credits per request, priced from the same per-token model rates.
const CopilotCreditUSD = 0.01

// Price is USD per million tokens at standard (non-batch, non-fast, global) rates.
type Price struct {
	Input, CacheWrite, CacheWrite1h, CacheRead, Output float64
}

// anthropic builds a Claude price: cache writes are 1.25x (5 minutes) and 2x (1 hour) input.
func anthropic(in, read, out float64) Price {
	return Price{Input: in, CacheWrite: in * 1.25, CacheWrite1h: in * 2, CacheRead: read, Output: out}
}

// openai builds an OpenAI price; Codex reports one kind of cache write (30-minute, 1.25x input).
func openai(in, read, out float64) Price {
	return Price{Input: in, CacheWrite: in * 1.25, CacheWrite1h: in * 1.25, CacheRead: read, Output: out}
}

// Table maps model IDs to prices. Claude Haiku 5.5 uses its prompts-up-to-100K price (the higher
// tier needs per-request prompt sizes, which the hourly rows don't keep).
var Table = map[string]Price{
	"claude-fable-5-2":  anthropic(8, 0.20, 40),
	"claude-mythos-5-2": anthropic(8, 0.20, 40),
	"claude-fable-5-1":  anthropic(10, 0.25, 50),
	"claude-mythos-5-1": anthropic(10, 0.25, 50),
	"claude-fable-5":    anthropic(10, 1, 50),
	"claude-mythos-5":   anthropic(10, 1, 50),
	"claude-opus-5-5":   anthropic(4, 0.20, 20),
	"claude-opus-5":     anthropic(5, 0.50, 25),
	"claude-opus-4-8":   anthropic(5, 0.50, 25),
	"claude-opus-4-7":   anthropic(5, 0.50, 25),
	"claude-opus-4-6":   anthropic(5, 0.50, 25),
	"claude-opus-4-5":   anthropic(5, 0.50, 25),
	"claude-opus-4-1":   anthropic(15, 1.50, 75),
	"claude-opus-4":     anthropic(15, 1.50, 75),
	"claude-sonnet-5-5": anthropic(2, 0.10, 10),
	"claude-sonnet-5":   anthropic(2, 0.20, 10),
	"claude-sonnet-4-6": anthropic(3, 0.30, 15),
	"claude-sonnet-4-5": anthropic(3, 0.30, 15),
	"claude-sonnet-4":   anthropic(3, 0.30, 15),
	"claude-haiku-5-5":  anthropic(0.10, 0.01, 0.50),
	"claude-haiku-4-5":  anthropic(1, 0.10, 5),

	"gpt-6-astra":   openai(10, 1, 50),
	"gpt-6.1-sol":   openai(2, 0.10, 10),
	"gpt-6-sol":     openai(2, 0.20, 10),
	"gpt-6.1-terra": openai(1, 0.05, 5),
	"gpt-6-luna":    openai(0.10, 0.01, 0.50),
}

// Lookup finds a model's price. It accepts the ID variants logs use: a "[1m]" context suffix, a
// provider prefix ("anthropic.", "openai/"), dotted Claude versions ("claude-opus-4.8"), and a
// trailing date snapshot ("-20260401").
func Lookup(model string) (Price, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.IndexByte(m, '['); i >= 0 {
		m = m[:i]
	}
	if i := strings.LastIndexAny(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	m = strings.TrimPrefix(m, "anthropic.")
	if p, ok := Table[m]; ok {
		return p, true
	}
	if strings.HasPrefix(m, "claude-") {
		m = strings.ReplaceAll(m, ".", "-")
	}
	if i := strings.LastIndexByte(m, '-'); i > 0 && len(m)-i-1 == 8 && isDigits(m[i+1:]) {
		m = m[:i]
	}
	p, ok := Table[m]
	return p, ok
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// Cost is the USD cost of x at p. Input includes cache reads and writes, which are priced
// separately; a 1-hour cache write is priced at its own rate.
func (p Price) Cost(x usage.Metrics) float64 {
	w1h := min(x.CacheWrite1h, x.CacheWrite)
	fresh := max(x.Input-x.Cached-x.CacheWrite, 0)
	usd := float64(fresh)*p.Input + float64(x.Cached)*p.CacheRead +
		float64(x.CacheWrite-w1h)*p.CacheWrite + float64(w1h)*p.CacheWrite1h + float64(x.Output)*p.Output
	return usd / 1e6
}

// Estimate is an accumulated API-price estimate.
type Estimate struct {
	USD      float64
	Priced   int64            // tokens with a known price
	Unpriced map[string]int64 // tokens per model with no known price
}

// Add prices x for model; tools whose rows are a cross-check of other rows are the caller's to skip.
func (e *Estimate) Add(model string, x usage.Metrics) {
	if p, ok := Lookup(model); ok {
		e.USD += p.Cost(x)
		e.Priced += x.Total
		return
	}
	if x.Total == 0 {
		return
	}
	if e.Unpriced == nil {
		e.Unpriced = map[string]int64{}
	}
	e.Unpriced[model] += x.Total
}

// AddCredits adds Copilot AI credits (already priced by GitHub) for tokens x of model.
func (e *Estimate) AddCredits(credits float64, x usage.Metrics) {
	e.USD += credits * CopilotCreditUSD
	e.Priced += x.Total
}

// Merge adds o into e.
func (e *Estimate) Merge(o Estimate) {
	e.USD += o.USD
	e.Priced += o.Priced
	for k, v := range o.Unpriced {
		if e.Unpriced == nil {
			e.Unpriced = map[string]int64{}
		}
		e.Unpriced[k] += v
	}
}

// UnpricedModels lists the unpriced models, most tokens first.
func (e Estimate) UnpricedModels() []string {
	out := make([]string, 0, len(e.Unpriced))
	for k := range e.Unpriced {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if e.Unpriced[out[i]] != e.Unpriced[out[j]] {
			return e.Unpriced[out[i]] > e.Unpriced[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// UnpricedTokens is the total of tokens with no known price.
func (e Estimate) UnpricedTokens() int64 {
	var n int64
	for _, v := range e.Unpriced {
		n += v
	}
	return n
}
