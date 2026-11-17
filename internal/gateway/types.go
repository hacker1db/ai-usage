package gateway

import "encoding/json"

// Wire types for the two API formats the gateway speaks. Only the fields the gateway reads or
// writes are modeled; translation is lossy by design for features one side lacks (see
// docs/ai-gateway.md, "Translation limits").

// ---- OpenAI Chat Completions ----

type oaiRequest struct {
	Model               string            `json:"model"`
	Messages            []oaiMessage      `json:"messages"`
	Tools               []oaiTool         `json:"tools,omitempty"`
	ToolChoice          json.RawMessage   `json:"tool_choice,omitempty"`
	MaxTokens           *int              `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int              `json:"max_completion_tokens,omitempty"`
	Temperature         *float64          `json:"temperature,omitempty"`
	TopP                *float64          `json:"top_p,omitempty"`
	Stop                json.RawMessage   `json:"stop,omitempty"`
	Stream              bool              `json:"stream,omitempty"`
	StreamOptions       *oaiStreamOptions `json:"stream_options,omitempty"`
	User                string            `json:"user,omitempty"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"` // string, []oaiPart, or null
	ToolCalls  []oaiToolCall   `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type oaiPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *oaiImageURL `json:"image_url,omitempty"`
}

type oaiImageURL struct {
	URL string `json:"url"`
}

type oaiTool struct {
	Type     string      `json:"type"`
	Function oaiFunction `json:"function"`
}

type oaiFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type oaiToolCall struct {
	Index    *int            `json:"index,omitempty"`
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type,omitempty"`
	Function oaiFunctionCall `json:"function"`
}

type oaiFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type oaiResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   *oaiUsage   `json:"usage,omitempty"`
}

type oaiChoice struct {
	Index        int        `json:"index"`
	Message      *oaiOutMsg `json:"message,omitempty"`
	Delta        *oaiOutMsg `json:"delta,omitempty"`
	FinishReason *string    `json:"finish_reason"`
}

// oaiOutMsg is an assistant message or stream delta. Content is a pointer so a delta can omit it
// while a full message can carry null.
type oaiOutMsg struct {
	Role      string        `json:"role,omitempty"`
	Content   *string       `json:"content,omitempty"`
	ToolCalls []oaiToolCall `json:"tool_calls,omitempty"`
}

type oaiUsage struct {
	PromptTokens            int64            `json:"prompt_tokens"`
	CompletionTokens        int64            `json:"completion_tokens"`
	TotalTokens             int64            `json:"total_tokens"`
	PromptTokensDetails     *oaiTokenDetails `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *oaiTokenDetails `json:"completion_tokens_details,omitempty"`
}

type oaiTokenDetails struct {
	CachedTokens    int64 `json:"cached_tokens,omitempty"`
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
}

// ---- Anthropic Messages ----

type antRequest struct {
	Model         string          `json:"model"`
	System        json.RawMessage `json:"system,omitempty"` // string or []antBlock
	Messages      []antMessage    `json:"messages"`
	MaxTokens     int             `json:"max_tokens"`
	Tools         []antTool       `json:"tools,omitempty"`
	ToolChoice    *antToolChoice  `json:"tool_choice,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Metadata      *antMetadata    `json:"metadata,omitempty"`
}

type antMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

type antMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []antBlock
}

type antBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *antImageSource `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result: string or []antBlock
	IsError   bool            `json:"is_error,omitempty"`
}

type antImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type antTool struct {
	Type        string          `json:"type,omitempty"` // set for server tools, which have no schema
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type antToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

type antResponse struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"`
	Role         string     `json:"role"`
	Model        string     `json:"model"`
	Content      []antBlock `json:"content"`
	StopReason   *string    `json:"stop_reason"`
	StopSequence *string    `json:"stop_sequence"`
	Usage        antUsage   `json:"usage"`
}

type antUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
}

// antStreamEvent covers every Anthropic SSE event type the gateway reads.
type antStreamEvent struct {
	Type         string       `json:"type"`
	Index        int          `json:"index"`
	Message      *antResponse `json:"message,omitempty"`
	ContentBlock *antBlock    `json:"content_block,omitempty"`
	Delta        *struct {
		Type         string  `json:"type"`
		Text         string  `json:"text"`
		PartialJSON  string  `json:"partial_json"`
		StopReason   *string `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	} `json:"delta,omitempty"`
	Usage *antUsage `json:"usage,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}
