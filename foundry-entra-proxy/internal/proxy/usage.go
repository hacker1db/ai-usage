package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
)

// maxUsageScanBytes bounds what the usage scanner buffers: a whole JSON body, or one SSE line.
// Bodies or lines past the limit pass through untouched; only the token counts are lost.
const maxUsageScanBytes = 16 << 20

// usage is the normalized token accounting for one upstream response. Responses API
// (input/output_tokens) and Chat Completions or embeddings (prompt/completion_tokens) both map here.
type usage struct {
	found     bool
	input     int64
	output    int64
	cached    int64
	reasoning int64
	total     int64
}

type usageDetails struct {
	CachedTokens    int64 `json:"cached_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type rawUsage struct {
	InputTokens             int64         `json:"input_tokens"`
	OutputTokens            int64         `json:"output_tokens"`
	PromptTokens            int64         `json:"prompt_tokens"`
	CompletionTokens        int64         `json:"completion_tokens"`
	TotalTokens             int64         `json:"total_tokens"`
	InputTokensDetails      *usageDetails `json:"input_tokens_details"`
	OutputTokensDetails     *usageDetails `json:"output_tokens_details"`
	PromptTokensDetails     *usageDetails `json:"prompt_tokens_details"`
	CompletionTokensDetails *usageDetails `json:"completion_tokens_details"`
}

// usageEnvelope matches a non-streamed body ({"usage": ...}), a Chat Completions stream chunk
// ({"usage": ...}), and a Responses stream event ({"response": {"usage": ...}}).
type usageEnvelope struct {
	Usage    *rawUsage `json:"usage"`
	Response *struct {
		Usage *rawUsage `json:"usage"`
	} `json:"response"`
}

func (u *usage) absorb(data []byte) {
	if !bytes.Contains(data, []byte(`"usage"`)) {
		return
	}
	var env usageEnvelope
	if json.Unmarshal(data, &env) != nil {
		return
	}
	raw := env.Usage
	if env.Response != nil && env.Response.Usage != nil {
		raw = env.Response.Usage
	}
	if raw == nil {
		return
	}
	*u = usage{
		found:  true,
		input:  firstPositive(raw.InputTokens, raw.PromptTokens),
		output: firstPositive(raw.OutputTokens, raw.CompletionTokens),
		total:  raw.TotalTokens,
	}
	for _, d := range []*usageDetails{raw.InputTokensDetails, raw.PromptTokensDetails} {
		if d != nil && u.cached == 0 {
			u.cached = d.CachedTokens
		}
	}
	for _, d := range []*usageDetails{raw.OutputTokensDetails, raw.CompletionTokensDetails} {
		if d != nil && u.reasoning == 0 {
			u.reasoning = d.ReasoningTokens
		}
	}
	if u.total == 0 {
		u.total = u.input + u.output
	}
}

// logAttrs returns the slog key/value pairs for the request log line, or nil when nothing was found.
func (u *usage) logAttrs() []any {
	if u == nil || !u.found {
		return nil
	}
	return []any{
		"input_tokens", u.input,
		"output_tokens", u.output,
		"cached_tokens", u.cached,
		"reasoning_tokens", u.reasoning,
		"total_tokens", u.total,
	}
}

// usageBody passes the upstream body through unchanged while feeding a copy to the usage parser.
// SSE is parsed one "data:" line at a time; anything else is buffered and parsed at EOF.
type usageBody struct {
	io.ReadCloser
	into     *usage
	sse      bool
	buf      []byte
	overflow bool
	done     bool
}

func newUsageBody(resp *http.Response, into *usage) io.ReadCloser {
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return resp.Body
	case resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity":
		return resp.Body
	case mediaType == "text/event-stream":
		return &usageBody{ReadCloser: resp.Body, into: into, sse: true}
	case mediaType == "application/json":
		return &usageBody{ReadCloser: resp.Body, into: into}
	}
	return resp.Body
}

func (b *usageBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.feed(p[:n])
	}
	if err == io.EOF {
		b.finish()
	}
	return n, err
}

func (b *usageBody) Close() error {
	b.finish()
	return b.ReadCloser.Close()
}

func (b *usageBody) feed(chunk []byte) {
	if !b.sse {
		if !b.overflow && len(b.buf)+len(chunk) <= maxUsageScanBytes {
			b.buf = append(b.buf, chunk...)
		} else {
			b.overflow, b.buf = true, nil
		}
		return
	}
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			b.appendLine(chunk)
			return
		}
		b.appendLine(chunk[:i])
		b.flushLine()
		chunk = chunk[i+1:]
	}
}

func (b *usageBody) appendLine(part []byte) {
	if b.overflow {
		return
	}
	if len(b.buf)+len(part) > maxUsageScanBytes {
		b.overflow, b.buf = true, nil
		return
	}
	b.buf = append(b.buf, part...)
}

func (b *usageBody) flushLine() {
	if !b.overflow {
		line := bytes.TrimRight(b.buf, "\r")
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			b.into.absorb(bytes.TrimSpace(data))
		}
	}
	b.buf, b.overflow = b.buf[:0], false
}

func (b *usageBody) finish() {
	if b.done {
		return
	}
	b.done = true
	if b.sse {
		b.flushLine()
		return
	}
	if !b.overflow {
		b.into.absorb(b.buf)
	}
	b.buf = nil
}

func firstPositive(values ...int64) int64 {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}
