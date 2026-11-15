package gateway

import (
	"encoding/json"
	"reflect"
	"testing"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAnthropicToOpenAIRequest(t *testing.T) {
	var in antRequest
	raw := `{
		"model": "alias",
		"system": [{"type":"text","text":"be brief"},{"type":"text","text":"be kind"}],
		"max_tokens": 256,
		"temperature": 0.2,
		"stop_sequences": ["END"],
		"stream": true,
		"metadata": {"user_id": "u1"},
		"tools": [
			{"name":"lookup","description":"find a host","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}},
			{"type":"web_search_20250305","name":"web_search"}
		],
		"tool_choice": {"type":"tool","name":"lookup"},
		"messages": [
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"checking"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"a"}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"10.0.0.1"}]},
				{"type":"tool_result","tool_use_id":"toolu_2","is_error":true,"content":"boom"},
				{"type":"text","text":"and?"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
			]}
		]
	}`
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	out, err := anthropicToOpenAIRequest(&in, "gpt-up", "max_completion_tokens")
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "gpt-up" || !out.Stream || out.StreamOptions == nil || !out.StreamOptions.IncludeUsage {
		t.Fatalf("model/stream not mapped: %+v", out)
	}
	if out.MaxCompletionTokens == nil || *out.MaxCompletionTokens != 256 || out.MaxTokens != nil {
		t.Fatalf("max tokens: %+v %+v", out.MaxCompletionTokens, out.MaxTokens)
	}
	if string(out.Stop) != `["END"]` || out.User != "u1" {
		t.Fatalf("stop/user: %s %q", out.Stop, out.User)
	}
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "lookup" {
		t.Fatalf("server tool should be dropped, function kept: %+v", out.Tools)
	}
	if string(out.ToolChoice) != `{"function":{"name":"lookup"},"type":"function"}` {
		t.Fatalf("tool_choice: %s", out.ToolChoice)
	}

	got := mustJSON(t, out.Messages)
	want := `[` +
		`{"role":"system","content":"be brief\n\nbe kind"},` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"checking","tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"a\"}"}}]},` +
		`{"role":"tool","content":"10.0.0.1","tool_call_id":"toolu_1"},` +
		`{"role":"tool","content":"Error: boom","tool_call_id":"toolu_2"},` +
		`{"role":"user","content":[{"type":"text","text":"and?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}` +
		`]`
	if got != want {
		t.Fatalf("messages:\n got %s\nwant %s", got, want)
	}
}

func TestAnthropicToOpenAIRequestMaxTokensField(t *testing.T) {
	in := antRequest{MaxTokens: 10, Messages: []antMessage{{Role: "user", Content: jsonString("x")}}}
	out, err := anthropicToOpenAIRequest(&in, "m", "max_tokens")
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxTokens == nil || *out.MaxTokens != 10 || out.MaxCompletionTokens != nil {
		t.Fatalf("got %+v / %+v", out.MaxTokens, out.MaxCompletionTokens)
	}
	if out.StreamOptions != nil {
		t.Fatal("stream_options must be absent when not streaming")
	}
}

func TestAnthropicToOpenAIRequestToolUseOnlyAssistantHasNullContent(t *testing.T) {
	in := antRequest{Messages: []antMessage{{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"t","name":"f","input":{}}]`)}}}
	out, err := anthropicToOpenAIRequest(&in, "m", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := mustJSON(t, out.Messages); got != `[{"role":"assistant","content":null,"tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":"{}"}}]}]` {
		t.Fatal(got)
	}
}

func TestAnthropicToOpenAIRequestErrors(t *testing.T) {
	for name, msgs := range map[string][]antMessage{
		"bad role":    {{Role: "system", Content: jsonString("x")}},
		"bad content": {{Role: "user", Content: json.RawMessage(`42`)}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := anthropicToOpenAIRequest(&antRequest{Messages: msgs}, "m", ""); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	if _, err := anthropicToOpenAIRequest(&antRequest{System: json.RawMessage(`42`)}, "m", ""); err == nil {
		t.Fatal("expected a system error")
	}
}

func TestOpenAIToAnthropicRequest(t *testing.T) {
	var in oaiRequest
	raw := `{
		"model": "alias",
		"max_completion_tokens": 300,
		"max_tokens": 100,
		"stop": "STOP",
		"user": "u9",
		"stream": true,
		"tools": [{"type":"function","function":{"name":"scan","description":"scan a host"}}],
		"tool_choice": "required",
		"messages": [
			{"role":"system","content":"sys one"},
			{"role":"developer","content":[{"type":"text","text":"sys two"}]},
			{"role":"user","content":"first"},
			{"role":"user","content":[{"type":"text","text":"second"},{"type":"image_url","image_url":{"url":"https://example.test/a.png"}}]},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"scan","arguments":"{\"host\":\"h\"}"}},{"id":"call_2","type":"function","function":{"name":"scan","arguments":"not json"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"open: 443"},
			{"role":"tool","tool_call_id":"call_2","content":[{"type":"text","text":"bad"}]},
			{"role":"assistant","content":"done"}
		]
	}`
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	out, err := openAIToAnthropicRequest(&in, "claude-up")
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "claude-up" || out.MaxTokens != 300 || !out.Stream {
		t.Fatalf("scalars: %+v", out)
	}
	if !reflect.DeepEqual(out.StopSequences, []string{"STOP"}) || out.Metadata == nil || out.Metadata.UserID != "u9" {
		t.Fatalf("stop/user: %+v %+v", out.StopSequences, out.Metadata)
	}
	if string(out.System) != `"sys one\n\nsys two"` {
		t.Fatalf("system: %s", out.System)
	}
	if out.ToolChoice == nil || out.ToolChoice.Type != "any" {
		t.Fatalf("tool_choice: %+v", out.ToolChoice)
	}
	if len(out.Tools) != 1 || string(out.Tools[0].InputSchema) != `{"type":"object","properties":{}}` {
		t.Fatalf("tools: %+v", out.Tools)
	}
	got := mustJSON(t, out.Messages)
	want := `[` +
		`{"role":"user","content":[{"type":"text","text":"first"},{"type":"text","text":"second"},{"type":"image","source":{"type":"url","url":"https://example.test/a.png"}}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"scan","input":{"host":"h"}},{"type":"tool_use","id":"call_2","name":"scan","input":{"arguments":"not json"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"open: 443"},{"type":"tool_result","tool_use_id":"call_2","content":"bad"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"done"}]}` +
		`]`
	if got != want {
		t.Fatalf("messages:\n got %s\nwant %s", got, want)
	}
}

func TestOpenAIToAnthropicRequestDefaults(t *testing.T) {
	in := oaiRequest{
		Messages:   []oaiMessage{{Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,QUJD"}}]`)}},
		Stop:       json.RawMessage(`["a","b"]`),
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"x"}}`),
		Tools:      []oaiTool{{Type: "function", Function: oaiFunction{Name: "x", Parameters: json.RawMessage(`{"type":"object"}`)}}},
	}
	out, err := openAIToAnthropicRequest(&in, "m")
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxTokens != defaultAnthropicMaxTokens {
		t.Fatalf("max_tokens default: %d", out.MaxTokens)
	}
	if !reflect.DeepEqual(out.StopSequences, []string{"a", "b"}) {
		t.Fatalf("stop: %v", out.StopSequences)
	}
	if out.ToolChoice == nil || out.ToolChoice.Type != "tool" || out.ToolChoice.Name != "x" {
		t.Fatalf("tool_choice: %+v", out.ToolChoice)
	}
	if got := mustJSON(t, out.Messages); got != `[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"QUJD"}}]}]` {
		t.Fatal(got)
	}

	for _, bad := range []oaiRequest{
		{Stop: json.RawMessage(`42`)},
		{Messages: []oaiMessage{{Role: "wizard"}}},
		{Messages: []oaiMessage{{Role: "user", Content: json.RawMessage(`{}`)}}},
	} {
		if _, err := openAIToAnthropicRequest(&bad, "m"); err == nil {
			t.Fatalf("expected error for %+v", bad)
		}
	}
}

func TestOpenAIToAnthropicResponse(t *testing.T) {
	var in oaiResponse
	raw := `{"id":"chatcmpl-abc","choices":[{"index":0,"message":{"role":"assistant","content":"hello","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40}}}`
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	out := openAIToAnthropicResponse(&in, "alias")
	got := mustJSON(t, out)
	want := `{"id":"msg_abc","type":"message","role":"assistant","model":"alias","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"call_1","name":"f","input":{"a":1}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":60,"output_tokens":20,"cache_read_input_tokens":40}}`
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

func TestAnthropicToOpenAIResponse(t *testing.T) {
	var in antResponse
	raw := `{"id":"msg_xyz","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"..."},{"type":"text","text":"hi "},{"type":"text","text":"there"},{"type":"tool_use","id":"toolu_1","name":"f","input":{"b":2}}],
		"stop_reason":"max_tokens","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":30,"cache_creation_input_tokens":7}}`
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	out := anthropicToOpenAIResponse(&in, "alias")
	if out.ID != "chatcmpl-xyz" || out.Object != "chat.completion" || out.Model != "alias" {
		t.Fatalf("envelope: %+v", out)
	}
	c := out.Choices[0]
	if *c.FinishReason != "length" || *c.Message.Content != "hi there" || len(c.Message.ToolCalls) != 1 || c.Message.ToolCalls[0].Function.Arguments != `{"b":2}` {
		t.Fatalf("choice: %s", mustJSON(t, c))
	}
	if got := mustJSON(t, out.Usage); got != `{"prompt_tokens":47,"completion_tokens":5,"total_tokens":52,"prompt_tokens_details":{"cached_tokens":30}}` {
		t.Fatal(got)
	}
}

func TestStopReasonMapping(t *testing.T) {
	for finish, stop := range map[string]string{"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use", "content_filter": "refusal", "": "end_turn"} {
		if got := anthropicStopReason(finish); got != stop {
			t.Errorf("anthropicStopReason(%q) = %q, want %q", finish, got, stop)
		}
	}
	for stop, finish := range map[string]string{"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length", "tool_use": "tool_calls", "refusal": "content_filter", "pause_turn": "stop"} {
		if got := openAIFinishReason(stop); got != finish {
			t.Errorf("openAIFinishReason(%q) = %q, want %q", stop, got, finish)
		}
	}
}

func TestIDs(t *testing.T) {
	if anthropicID("msg_1") != "msg_1" || anthropicID("chatcmpl-2") != "msg_2" || len(anthropicID("")) < 10 {
		t.Fatal("anthropicID")
	}
	if openAIID("chatcmpl-1") != "chatcmpl-1" || openAIID("msg_2") != "chatcmpl-2" {
		t.Fatal("openAIID")
	}
}

func TestJSONStringDoesNotEscapeHTML(t *testing.T) {
	if got := string(jsonString(`<a href="x">&</a>`)); got != `"<a href=\"x\">&</a>"` {
		t.Fatal(got)
	}
}
