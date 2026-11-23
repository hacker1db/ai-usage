package proxylog

const Message = "request"

const (
	KeyModel           = "model"
	KeyClient          = "client"
	KeyInputTokens     = "input_tokens"
	KeyOutputTokens    = "output_tokens"
	KeyCachedTokens    = "cached_tokens" //nolint:gosec // G101: a log attribute name, not a credential.
	KeyReasoningTokens = "reasoning_tokens"
	KeyTotalTokens     = "total_tokens"

	KeyCacheWriteTokens = "cache_write_tokens" //nolint:gosec // G101: a log attribute name, not a credential.
	KeyProvider         = "provider"
	KeyUpstreamModel    = "upstream_model"
)

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

	CacheWriteTokens int64  `json:"cache_write_tokens"`
	Provider         string `json:"provider"`
	UpstreamModel    string `json:"upstream_model"`
}
