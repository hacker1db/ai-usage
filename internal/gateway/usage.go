package gateway

import (
	"bytes"
	"encoding/json"

	"ai-usage/internal/proxylog"
)

type tokens struct {
	found      bool
	input      int64
	cached     int64
	cacheWrite int64
	output     int64
	reasoning  int64
}

func fromOpenAIUsage(u *oaiUsage) tokens {
	if u == nil {
		return tokens{}
	}
	t := tokens{found: true, input: u.PromptTokens, output: u.CompletionTokens}
	if u.PromptTokensDetails != nil {
		t.cached = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		t.reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	return t
}

func fromAnthropicUsage(u *antUsage) tokens {
	if u == nil {
		return tokens{}
	}
	return tokens{
		found:      true,
		input:      u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		cached:     u.CacheReadInputTokens,
		cacheWrite: u.CacheCreationInputTokens,
		output:     u.OutputTokens,
	}
}

func (t *tokens) merge(o tokens) {
	if !o.found {
		return
	}
	t.found = true
	if o.input > 0 {
		t.input = o.input
	}
	if o.cached > 0 {
		t.cached = o.cached
	}
	if o.cacheWrite > 0 {
		t.cacheWrite = o.cacheWrite
	}
	if o.output > 0 {
		t.output = o.output
	}
	if o.reasoning > 0 {
		t.reasoning = o.reasoning
	}
}

func anthropicUsageFrom(t tokens) antUsage {
	return antUsage{
		InputTokens:              max(t.input-t.cached-t.cacheWrite, 0),
		OutputTokens:             t.output,
		CacheReadInputTokens:     t.cached,
		CacheCreationInputTokens: t.cacheWrite,
	}
}

func openAIUsageFrom(t tokens) *oaiUsage {
	u := &oaiUsage{PromptTokens: t.input, CompletionTokens: t.output, TotalTokens: t.input + t.output}
	if t.cached > 0 {
		u.PromptTokensDetails = &oaiTokenDetails{CachedTokens: t.cached}
	}
	if t.reasoning > 0 {
		u.CompletionTokensDetails = &oaiTokenDetails{ReasoningTokens: t.reasoning}
	}
	return u
}

func scanUsage(kind string, data []byte, into *tokens) {
	if !bytes.Contains(data, []byte(`"usage"`)) {
		return
	}
	if kind == kindAnthropic {
		var ev antStreamEvent
		if json.Unmarshal(data, &ev) != nil {
			return
		}
		switch {
		case ev.Message != nil:
			u := ev.Message.Usage
			into.merge(fromAnthropicUsage(&u))
		case ev.Usage != nil:
			into.merge(fromAnthropicUsage(ev.Usage))
		default:
			var full antResponse
			if json.Unmarshal(data, &full) == nil {
				into.merge(fromAnthropicUsage(&full.Usage))
			}
		}
		return
	}
	var env struct {
		Usage *oaiUsage `json:"usage"`
	}
	if json.Unmarshal(data, &env) == nil && env.Usage != nil {
		into.merge(fromOpenAIUsage(env.Usage))
	}
}

func (t tokens) logAttrs() []any {
	if !t.found {
		return nil
	}
	return []any{
		proxylog.KeyInputTokens, t.input,
		proxylog.KeyOutputTokens, t.output,
		proxylog.KeyCachedTokens, t.cached,
		proxylog.KeyCacheWriteTokens, t.cacheWrite,
		proxylog.KeyReasoningTokens, t.reasoning,
		proxylog.KeyTotalTokens, t.input + t.output,
	}
}
