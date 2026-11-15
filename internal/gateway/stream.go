package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxSSELine bounds one SSE line; a longer line ends the stream rather than growing without limit.
const maxSSELine = 16 << 20

// readSSE calls fn for every line of an SSE body (newline included). data is the trimmed payload of
// a "data:" line and nil for any other line. fn returns false to stop early.
func readSSE(r io.Reader, fn func(line, data []byte) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			var data []byte
			trimmed := bytes.TrimRight(line, "\r\n")
			if d, ok := bytes.CutPrefix(trimmed, []byte("data:")); ok {
				data = bytes.TrimSpace(d)
			}
			if !fn(line, data) {
				return nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxSSELine {
			return nil, fmt.Errorf("sse line exceeds %d bytes", maxSSELine)
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

// ---------------------------------------------------------------------------------------------
// OpenAI chunks -> Anthropic Messages events
// ---------------------------------------------------------------------------------------------

type anthropicStream struct {
	w        io.Writer
	flush    func()
	alias    string
	usage    *tokens
	id       string
	started  bool
	finished bool
	open     int // index of the open content block, -1 when none
	openText bool
	next     int
	tools    map[int]int // OpenAI tool_call index -> Anthropic block index
	stop     string
}

func newAnthropicStream(w http.ResponseWriter, alias string, usage *tokens) *anthropicStream {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return &anthropicStream{w: w, flush: flusher(w), alias: alias, usage: usage, open: -1, tools: map[int]int{}}
}

func (s *anthropicStream) event(name string, v any) bool {
	b, _ := json.Marshal(v)
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return false
	}
	s.flush()
	return true
}

func (s *anthropicStream) start() {
	if s.started {
		return
	}
	s.started = true
	if s.id == "" {
		s.id = anthropicID("")
	}
	s.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": s.id, "type": "message", "role": "assistant", "model": s.alias,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int64{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (s *anthropicStream) closeBlock() {
	if s.open < 0 {
		return
	}
	s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.open})
	s.open, s.openText = -1, false
}

type oaiChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Delta        oaiOutMsg `json:"delta"`
		FinishReason *string   `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaiUsage `json:"usage"`
	Error *apiError `json:"error"`
}

// openAIChunk handles one Chat Completions stream payload. It returns false to stop reading.
func (s *anthropicStream) openAIChunk(data []byte) bool {
	var ch oaiChunk
	if err := json.Unmarshal(data, &ch); err != nil {
		return true
	}
	if ch.Error != nil {
		s.start()
		s.closeBlock()
		s.event("error", map[string]any{"type": "error", "error": apiError{Type: "api_error", Message: ch.Error.Message}})
		s.finished = true
		return false
	}
	if s.id == "" && ch.ID != "" {
		s.id = anthropicID(ch.ID)
	}
	s.start()
	if ch.Usage != nil {
		s.usage.merge(fromOpenAIUsage(ch.Usage))
	}
	for _, choice := range ch.Choices {
		d := choice.Delta
		if d.Content != nil && *d.Content != "" {
			if !s.openText {
				s.closeBlock()
				s.open, s.openText = s.next, true
				s.next++
				s.event("content_block_start", map[string]any{
					"type": "content_block_start", "index": s.open,
					"content_block": map[string]string{"type": "text", "text": ""},
				})
			}
			if !s.event("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": s.open,
				"delta": map[string]string{"type": "text_delta", "text": *d.Content},
			}) {
				return false
			}
		}
		for _, tc := range d.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			blk, ok := s.tools[idx]
			if !ok {
				s.closeBlock()
				blk = s.next
				s.next++
				s.tools[idx] = blk
				s.open = blk
				id := tc.ID
				if id == "" {
					id = "toolu_" + newID()
				}
				s.event("content_block_start", map[string]any{
					"type": "content_block_start", "index": blk,
					"content_block": map[string]any{"type": "tool_use", "id": id, "name": tc.Function.Name, "input": map[string]any{}},
				})
			}
			if tc.Function.Arguments != "" {
				if !s.event("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": blk,
					"delta": map[string]string{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				}) {
					return false
				}
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			s.stop = anthropicStopReason(*choice.FinishReason)
		}
	}
	return true
}

func (s *anthropicStream) finish() {
	if s.finished {
		return
	}
	s.finished = true
	s.start()
	s.closeBlock()
	stop := s.stop
	if stop == "" {
		stop = "end_turn"
	}
	s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": anthropicUsageFrom(*s.usage),
	})
	s.event("message_stop", map[string]string{"type": "message_stop"})
}

// ---------------------------------------------------------------------------------------------
// Anthropic Messages events -> OpenAI chunks
// ---------------------------------------------------------------------------------------------

type openAIStream struct {
	w            io.Writer
	flush        func()
	alias        string
	includeUsage bool
	usage        *tokens
	id           string
	created      int64
	finishReason string
	tools        map[int]int // Anthropic block index -> OpenAI tool_call index
	nextTool     int
	finished     bool
}

func newOpenAIStream(w http.ResponseWriter, alias string, includeUsage bool, usage *tokens) *openAIStream {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return &openAIStream{w: w, flush: flusher(w), alias: alias, includeUsage: includeUsage, usage: usage,
		id: openAIID(""), created: time.Now().Unix(), tools: map[int]int{}}
}

func (s *openAIStream) write(v any) bool {
	b, _ := json.Marshal(v)
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return false
	}
	s.flush()
	return true
}

func (s *openAIStream) chunk(delta oaiOutMsg, finish *string) bool {
	return s.write(oaiResponse{
		ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.alias,
		Choices: []oaiChoice{{Index: 0, Delta: &delta, FinishReason: finish}},
	})
}

// anthropicEvent handles one Messages stream payload. It returns false to stop reading.
func (s *openAIStream) anthropicEvent(data []byte) bool {
	var ev antStreamEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return true
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			if ev.Message.ID != "" {
				s.id = openAIID(ev.Message.ID)
			}
			u := ev.Message.Usage
			s.usage.merge(fromAnthropicUsage(&u))
		}
		empty := ""
		return s.chunk(oaiOutMsg{Role: "assistant", Content: &empty}, nil)
	case "content_block_start":
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			idx := s.nextTool
			s.nextTool++
			s.tools[ev.Index] = idx
			return s.chunk(oaiOutMsg{ToolCalls: []oaiToolCall{{
				Index: &idx, ID: ev.ContentBlock.ID, Type: "function",
				Function: oaiFunctionCall{Name: ev.ContentBlock.Name, Arguments: ""},
			}}}, nil)
		}
	case "content_block_delta":
		if ev.Delta == nil {
			return true
		}
		switch ev.Delta.Type {
		case "text_delta":
			text := ev.Delta.Text
			return s.chunk(oaiOutMsg{Content: &text}, nil)
		case "input_json_delta":
			idx, ok := s.tools[ev.Index]
			if !ok || ev.Delta.PartialJSON == "" {
				return true
			}
			return s.chunk(oaiOutMsg{ToolCalls: []oaiToolCall{{Index: &idx, Function: oaiFunctionCall{Arguments: ev.Delta.PartialJSON}}}}, nil)
		}
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != nil {
			s.finishReason = openAIFinishReason(*ev.Delta.StopReason)
		}
		if ev.Usage != nil {
			s.usage.merge(fromAnthropicUsage(ev.Usage))
		}
	case "message_stop":
		s.finish()
		return false
	case "error":
		msg := "upstream stream error"
		if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		}
		s.write(map[string]any{"error": apiError{Type: "server_error", Message: msg}})
		s.finished = true
		_, _ = io.WriteString(s.w, "data: [DONE]\n\n")
		s.flush()
		return false
	}
	return true
}

func (s *openAIStream) finish() {
	if s.finished {
		return
	}
	s.finished = true
	reason := s.finishReason
	if reason == "" {
		reason = "stop"
	}
	s.chunk(oaiOutMsg{}, &reason)
	if s.includeUsage {
		s.write(oaiResponse{
			ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.alias,
			Choices: []oaiChoice{}, Usage: openAIUsageFrom(*s.usage),
		})
	}
	_, _ = io.WriteString(s.w, "data: [DONE]\n\n")
	s.flush()
}
