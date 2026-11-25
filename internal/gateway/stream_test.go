package gateway

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestStreamErrorEventsAreTranslated(t *testing.T) {
	h := newHarness(t,
		sseResponse("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n", "data: {\"error\":{\"message\":\"overloaded\"}}\n\n"),
		sseResponse("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_e\",\"usage\":{\"input_tokens\":1}}}\n\n",
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"))

	rec := h.do(t, http.MethodPost, "/v1/messages", caidoKey, `{"model":"gpt-fast","stream":true,"max_tokens":5,"messages":[]}`)
	events := parseSSE(t, rec.Body.String())
	last := events[len(events)-1]
	if last.name != "error" || last.data["error"].(map[string]any)["message"] != "overloaded" {
		t.Fatalf("anthropic error event: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "message_stop") {
		t.Fatal("an errored stream must not also report message_stop")
	}

	rec = h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, `{"model":"sonnet","stream":true,"messages":[]}`)
	body := rec.Body.String()
	if !strings.Contains(body, `"message":"busy"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") || strings.Count(body, "[DONE]") != 1 {
		t.Fatalf("openai error chunk: %s", body)
	}
}

func TestStreamWithoutTerminatorStillFinishes(t *testing.T) {
	h := newHarness(t, unused,
		sseResponse("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_t\",\"usage\":{\"input_tokens\":3}}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n"))
	rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, `{"model":"sonnet","stream":true,"messages":[]}`)
	if !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) || !strings.HasSuffix(rec.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("truncated stream not closed: %s", rec.Body)
	}
}

func TestReadSSE(t *testing.T) {
	in := "event: a\r\ndata: {\"x\":1}\r\n\r\n: comment\ndata:[DONE]"
	var datas []string
	var lines int
	if err := readSSE(strings.NewReader(in), func(line, data []byte) bool {
		lines++
		if data != nil {
			datas = append(datas, string(data))
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if lines != 5 || strings.Join(datas, "|") != `{"x":1}|[DONE]` {
		t.Fatalf("lines=%d datas=%v", lines, datas)
	}
	stop := 0
	_ = readSSE(strings.NewReader("data: 1\ndata: 2\n"), func(_, _ []byte) bool { stop++; return false })
	if stop != 1 {
		t.Fatal("fn returning false must stop reading")
	}
	boom := errors.New("boom")
	if err := readSSE(io.MultiReader(strings.NewReader("data: 1\n"), errReader{boom}), func(_, _ []byte) bool { return true }); !errors.Is(err, boom) {
		t.Fatalf("read error: %v", err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestUpstreamErrorMessage(t *testing.T) {

	cases := [][2]string{
		{`{"error":{"message":"m1"}}`, "m1"},
		{`{"error":"m2"}`, "m2"},
		{`{"message":"m3"}`, "m3"},
		{`<html>bad gateway</html>`, "<html>bad gateway</html>"},
		{`{"type":"error","error":{"type":"x","message":"m4"}}`, "m4"},
	}
	for _, c := range cases {
		if got := upstreamErrorMessage([]byte(c[0])); got != c[1] {
			t.Errorf("%s: got %q want %q", c[0], got, c[1])
		}
	}
}

func TestErrorTypes(t *testing.T) {
	for status, want := range map[int]string{400: "invalid_request_error", 401: "authentication_error", 403: "permission_error", 404: "not_found_error", 413: "request_too_large", 429: "rate_limit_error", 500: "api_error", 529: "overloaded_error"} {
		if got := anthropicErrorType(status); got != want {
			t.Errorf("anthropic %d: %s", status, got)
		}
	}
	for status, want := range map[int]string{400: "invalid_request_error", 401: "authentication_error", 403: "permission_error", 429: "rate_limit_error", 502: "server_error"} {
		if got := openAIErrorType(status); got != want {
			t.Errorf("openai %d: %s", status, got)
		}
	}
}
