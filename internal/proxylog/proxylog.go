// Package proxylog is the contract between foundry-entra-proxy, which writes one JSON "request"
// line per proxied call, and the ai-usage collector, which reads those lines back. Both sides use
// these keys, so a rename on one side breaks the build or the round-trip test instead of silently
// dropping usage.
package proxylog

// Message is the slog message of a per-request line.
const Message = "request"

// Attribute keys written by the proxy (slog adds "time", "level", and "msg").
const (
	KeyModel           = "model"
	KeyClient          = "client"
	KeyInputTokens     = "input_tokens"
	KeyOutputTokens    = "output_tokens"
	KeyCachedTokens    = "cached_tokens"
	KeyReasoningTokens = "reasoning_tokens"
	KeyTotalTokens     = "total_tokens"
)

// Request is the subset of a request line the collector reads. Token fields are absent when the
// response carried no usage, which decodes as zero.
type Request struct {
	Time            string `json:"time"`
	Msg             string `json:"msg"`
	Model           string `json:"model"`
	Client          string `json:"client"`
	Status          int    `json:"status"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	CachedTokens    int64  `json:"cached_tokens"`
	ReasoningTokens int64  `json:"reasoning_tokens"`
	TotalTokens     int64  `json:"total_tokens"`
}
