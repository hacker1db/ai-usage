package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const defaultAnthropicMaxTokens = 8192

var errBadContent = errors.New("unsupported message content")

func anthropicToOpenAIRequest(in *antRequest, upstreamModel, maxTokensField string) (*oaiRequest, error) {
	out := &oaiRequest{
		Model:       upstreamModel,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stream:      in.Stream,
	}
	if in.Stream {
		out.StreamOptions = &oaiStreamOptions{IncludeUsage: true}
	}
	if in.MaxTokens > 0 {
		n := in.MaxTokens
		if maxTokensField == "max_tokens" {
			out.MaxTokens = &n
		} else {
			out.MaxCompletionTokens = &n
		}
	}
	if len(in.StopSequences) > 0 {
		out.Stop, _ = json.Marshal(in.StopSequences)
	}
	if in.Metadata != nil {
		out.User = in.Metadata.UserID
	}

	sys, err := anthropicSystemText(in.System)
	if err != nil {
		return nil, err
	}
	if sys != "" {
		out.Messages = append(out.Messages, oaiMessage{Role: "system", Content: jsonString(sys)})
	}

	for i, m := range in.Messages {
		msgs, err := anthropicMessageToOpenAI(m)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out.Messages = append(out.Messages, msgs...)
	}

	for _, t := range in.Tools {
		if len(t.InputSchema) == 0 {

			continue
		}
		out.Tools = append(out.Tools, oaiTool{Type: "function", Function: oaiFunction{
			Name: t.Name, Description: t.Description, Parameters: t.InputSchema,
		}})
	}
	if in.ToolChoice != nil && len(out.Tools) > 0 {
		switch in.ToolChoice.Type {
		case "auto":
			out.ToolChoice = jsonString("auto")
		case "any":
			out.ToolChoice = jsonString("required")
		case "none":
			out.ToolChoice = jsonString("none")
		case "tool":
			out.ToolChoice, _ = json.Marshal(map[string]any{
				"type": "function", "function": map[string]string{"name": in.ToolChoice.Name},
			})
		}
	}
	return out, nil
}

func anthropicSystemText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var blocks []antBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("system: %w", errBadContent)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

func anthropicBlocks(raw json.RawMessage) ([]antBlock, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil, nil
		}
		return []antBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []antBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errBadContent
	}
	return blocks, nil
}

func anthropicMessageToOpenAI(m antMessage) ([]oaiMessage, error) {
	blocks, err := anthropicBlocks(m.Content)
	if err != nil {
		return nil, err
	}
	var out []oaiMessage
	var parts []oaiPart
	var calls []oaiToolCall
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, oaiPart{Type: "text", Text: b.Text})
			}
		case "image":
			if b.Source == nil {
				continue
			}
			url := b.Source.URL
			if b.Source.Type == "base64" {
				url = "data:" + b.Source.MediaType + ";base64," + b.Source.Data
			}
			parts = append(parts, oaiPart{Type: "image_url", ImageURL: &oaiImageURL{URL: url}})
		case "tool_use":
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			calls = append(calls, oaiToolCall{ID: b.ID, Type: "function", Function: oaiFunctionCall{Name: b.Name, Arguments: args}})
		case "tool_result":
			text, err := toolResultText(b.Content)
			if err != nil {
				return nil, err
			}
			if b.IsError && !strings.HasPrefix(text, "Error") {
				text = "Error: " + text
			}
			out = append(out, oaiMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: jsonString(text)})
		default:

		}
	}

	switch m.Role {
	case "assistant":
		msg := oaiMessage{Role: "assistant", ToolCalls: calls}
		if text := joinText(parts); text != "" || len(calls) == 0 {
			msg.Content = jsonString(text)
		} else {
			msg.Content = json.RawMessage("null")
		}
		out = append(out, msg)
	case "user":
		if len(parts) > 0 {
			out = append(out, oaiMessage{Role: "user", Content: userContent(parts)})
		}
	default:
		return nil, fmt.Errorf("unknown role %q", m.Role)
	}
	return out, nil
}

func toolResultText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	blocks, err := anthropicBlocks(raw)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}

func joinText(parts []oaiPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func userContent(parts []oaiPart) json.RawMessage {
	if len(parts) == 1 && parts[0].Type == "text" {
		return jsonString(parts[0].Text)
	}
	raw, _ := json.Marshal(parts)
	return raw
}

func openAIToAnthropicRequest(in *oaiRequest, upstreamModel string) (*antRequest, error) {
	out := &antRequest{
		Model:       upstreamModel,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stream:      in.Stream,
		MaxTokens:   defaultAnthropicMaxTokens,
	}
	switch {
	case in.MaxCompletionTokens != nil && *in.MaxCompletionTokens > 0:
		out.MaxTokens = *in.MaxCompletionTokens
	case in.MaxTokens != nil && *in.MaxTokens > 0:
		out.MaxTokens = *in.MaxTokens
	}
	if len(in.Stop) > 0 && string(in.Stop) != "null" {
		var one string
		if json.Unmarshal(in.Stop, &one) == nil {
			out.StopSequences = []string{one}
		} else if err := json.Unmarshal(in.Stop, &out.StopSequences); err != nil {
			return nil, errors.New("stop must be a string or an array of strings")
		}
	}
	if in.User != "" {
		out.Metadata = &antMetadata{UserID: in.User}
	}

	var system []string
	var msgs []antMessage
	appendBlocks := func(role string, blocks []antBlock) {
		if len(blocks) == 0 {
			return
		}

		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			var prev []antBlock
			_ = json.Unmarshal(msgs[n-1].Content, &prev)
			msgs[n-1].Content, _ = json.Marshal(append(prev, blocks...))
			return
		}
		raw, _ := json.Marshal(blocks)
		msgs = append(msgs, antMessage{Role: role, Content: raw})
	}

	for i, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			text, err := openAIText(m.Content)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			if text != "" {
				system = append(system, text)
			}
		case "user":
			blocks, err := openAIUserBlocks(m.Content)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			appendBlocks("user", blocks)
		case "assistant":
			text, err := openAIText(m.Content)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			var blocks []antBlock
			if text != "" {
				blocks = append(blocks, antBlock{Type: "text", Text: text})
			}
			for _, c := range m.ToolCalls {
				blocks = append(blocks, antBlock{Type: "tool_use", ID: c.ID, Name: c.Function.Name, Input: toolInput(c.Function.Arguments)})
			}
			appendBlocks("assistant", blocks)
		case "tool":
			text, err := openAIText(m.Content)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			appendBlocks("user", []antBlock{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: jsonString(text)}})
		default:
			return nil, fmt.Errorf("messages[%d]: unknown role %q", i, m.Role)
		}
	}
	if len(system) > 0 {
		out.System = jsonString(strings.Join(system, "\n\n"))
	}
	out.Messages = msgs

	for _, t := range in.Tools {
		if t.Type != "function" {
			continue
		}
		schema := t.Function.Parameters
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, antTool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema})
	}
	if len(in.ToolChoice) > 0 && len(out.Tools) > 0 {
		var s string
		if json.Unmarshal(in.ToolChoice, &s) == nil {
			switch s {
			case "auto":
				out.ToolChoice = &antToolChoice{Type: "auto"}
			case "required":
				out.ToolChoice = &antToolChoice{Type: "any"}
			case "none":
				out.ToolChoice = &antToolChoice{Type: "none"}
			}
		} else {
			var obj struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(in.ToolChoice, &obj) == nil && obj.Function.Name != "" {
				out.ToolChoice = &antToolChoice{Type: "tool", Name: obj.Function.Name}
			}
		}
	}
	return out, nil
}

func toolInput(args string) json.RawMessage {
	args = strings.TrimSpace(args)
	if args == "" {
		return json.RawMessage("{}")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &obj) == nil {
		return json.RawMessage(args)
	}
	raw, _ := json.Marshal(map[string]string{"arguments": args})
	return raw
}

func openAIParts(raw json.RawMessage) ([]oaiPart, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil, nil
		}
		return []oaiPart{{Type: "text", Text: s}}, nil
	}
	var parts []oaiPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errBadContent
	}
	return parts, nil
}

func openAIText(raw json.RawMessage) (string, error) {
	parts, err := openAIParts(raw)
	if err != nil {
		return "", err
	}
	return joinText(parts), nil
}

func openAIUserBlocks(raw json.RawMessage) ([]antBlock, error) {
	parts, err := openAIParts(raw)
	if err != nil {
		return nil, err
	}
	var out []antBlock
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, antBlock{Type: "text", Text: p.Text})
			}
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			out = append(out, antBlock{Type: "image", Source: imageSource(p.ImageURL.URL)})
		}
	}
	return out, nil
}

func imageSource(url string) *antImageSource {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		if meta, data, ok := strings.Cut(rest, ","); ok {
			if mediaType, ok := strings.CutSuffix(meta, ";base64"); ok {
				return &antImageSource{Type: "base64", MediaType: mediaType, Data: data}
			}
		}
	}
	return &antImageSource{Type: "url", URL: url}
}

func openAIToAnthropicResponse(in *oaiResponse, alias string) *antResponse {
	out := &antResponse{ID: anthropicID(in.ID), Type: "message", Role: "assistant", Model: alias, Content: []antBlock{}}
	reason := "end_turn"
	if len(in.Choices) > 0 {
		c := in.Choices[0]
		if c.Message != nil {
			if c.Message.Content != nil && *c.Message.Content != "" {
				out.Content = append(out.Content, antBlock{Type: "text", Text: *c.Message.Content})
			}
			for _, tc := range c.Message.ToolCalls {
				out.Content = append(out.Content, antBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: toolInput(tc.Function.Arguments)})
			}
		}
		if c.FinishReason != nil {
			reason = anthropicStopReason(*c.FinishReason)
		}
	}
	out.StopReason = &reason
	if in.Usage != nil {
		out.Usage = anthropicUsageFrom(fromOpenAIUsage(in.Usage))
	}
	return out
}

func anthropicToOpenAIResponse(in *antResponse, alias string) *oaiResponse {
	msg := &oaiOutMsg{Role: "assistant"}
	var text strings.Builder
	for _, b := range in.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			input := string(b.Input)
			if input == "" {
				input = "{}"
			}
			msg.ToolCalls = append(msg.ToolCalls, oaiToolCall{ID: b.ID, Type: "function", Function: oaiFunctionCall{Name: b.Name, Arguments: input}})
		}
	}
	if s := text.String(); s != "" || len(msg.ToolCalls) == 0 {
		msg.Content = &s
	}
	reason := "stop"
	if in.StopReason != nil {
		reason = openAIFinishReason(*in.StopReason)
	}
	u := fromAnthropicUsage(&in.Usage)
	return &oaiResponse{
		ID: openAIID(in.ID), Object: "chat.completion", Created: time.Now().Unix(), Model: alias,
		Choices: []oaiChoice{{Index: 0, Message: msg, FinishReason: &reason}},
		Usage:   openAIUsageFrom(u),
	}
}

func anthropicStopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	return "end_turn"
}

func openAIFinishReason(stop string) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	return "stop"
}

func anthropicID(id string) string {
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	if id == "" {
		id = newID()
	}
	return "msg_" + strings.TrimPrefix(id, "chatcmpl-")
}

func openAIID(id string) string {
	if strings.HasPrefix(id, "chatcmpl-") {
		return id
	}
	if id == "" {
		id = newID()
	}
	return "chatcmpl-" + strings.TrimPrefix(id, "msg_")
}

func jsonString(s string) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimRight(buf.Bytes(), "\n")
}
