package sources

import (
	"encoding/json"

	"ai-usage/internal/proxylog"
	"ai-usage/internal/usage"
)

func CollectProxy(agg *usage.Aggregator, logPath string) Status {
	logPath = usage.ExpandHome(logPath)
	st := Status{Source: usage.ToolProxy, Path: usage.Tilde(logPath)}
	if missing := statSource(logPath, false); missing != "" {
		st.Status = missing
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
