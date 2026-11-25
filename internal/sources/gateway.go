package sources

import (
	"encoding/json"

	"ai-usage/internal/proxylog"
	"ai-usage/internal/usage"
)

func CollectGateway(agg *usage.Aggregator, logPath string) Status {
	logPath = usage.ExpandHome(logPath)
	st := Status{Source: usage.ToolGateway, Path: usage.Tilde(logPath)}
	if missing := statSource(logPath, false); missing != "" {
		st.Status = missing
		return st
	}
	err := usage.EachJSONLine(logPath, func(line []byte) {
		var r proxylog.Request
		if json.Unmarshal(line, &r) != nil || r.Msg != proxylog.Message || r.Provider == "" {
			return
		}
		if r.Status < 200 || r.Status > 299 {
			return
		}
		model := r.UpstreamModel
		if model == "" {
			model = r.Model
		}
		if agg.Add(usage.ToolGateway, r.Provider, usage.Record{
			Model: model, Time: usage.ParseTime(r.Time),
			Input: r.InputTokens, Cached: r.CachedTokens, CacheWrite: r.CacheWriteTokens,
			Output: r.OutputTokens, Reasoning: r.ReasoningTokens,
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
