package sources

import (
	"encoding/json"
	"os"

	"ai-usage/internal/proxylog"
	"ai-usage/internal/usage"
)

// CollectProxy reads foundry-entra-proxy request lines. They are a cross-check, not added to
// totals, because they overlap Codex work-foundry sessions.
func CollectProxy(agg *usage.Aggregator, logPath string) Status {
	logPath = usage.ExpandHome(logPath)
	st := Status{Source: usage.ToolProxy, Path: usage.Tilde(logPath)}
	if _, err := os.Stat(logPath); err != nil {
		st.Status = "not found"
		return st
	}
	err := usage.EachJSONLine(logPath, func(line []byte) {
		var r proxylog.Request
		if json.Unmarshal(line, &r) != nil || r.Msg != proxylog.Message {
			return
		}
		client := r.Client
		if client == "" {
			client = "unknown"
		}
		if agg.Add(usage.ToolProxy, client, usage.Record{
			Model: r.Model, Time: usage.ParseTime(r.Time),
			Input: r.InputTokens, Cached: r.CachedTokens, Output: r.OutputTokens, Reasoning: r.ReasoningTokens,
		}) {
			st.Requests++
		}
	})
	if err != nil {
		st.Status = "error: " + err.Error()
		return st
	}
	st.Status = "ok"
	return st
}
