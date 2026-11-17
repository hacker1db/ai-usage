package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-usage/internal/proxylog"
)

const (
	caidoKey  = "aigw_test-key-for-caido-0123456789abcdef"  // gitleaks:allow (test fixture, not a credential)
	scriptKey = "aigw_test-key-for-script-0123456789abcdef" // gitleaks:allow (test fixture, not a credential)
)

// fakeUpstream records what the gateway sent and answers with a canned handler.
type fakeUpstream struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
	handler  http.HandlerFunc
}

type recordedRequest struct {
	Path   string
	Header http.Header
	Body   map[string]any
}

func newFakeUpstream(t *testing.T, h http.HandlerFunc) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{handler: h}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
		f.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		f.handler(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeUpstream) last(t *testing.T) recordedRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("upstream received no request")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

type harness struct {
	gw     *Gateway
	logBuf *syncBuffer
	oai    *fakeUpstream
	ant    *fakeUpstream
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) lines(t *testing.T) []proxylog.Request {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []proxylog.Request
	sc := bufio.NewScanner(bytes.NewReader(b.buf.Bytes()))
	for sc.Scan() {
		var r proxylog.Request
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("log line is not JSON: %s", sc.Text())
		}
		if r.Msg == proxylog.Message {
			out = append(out, r)
		}
	}
	return out
}

func (b *syncBuffer) raw() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newHarness(t *testing.T, oai, ant http.HandlerFunc) *harness {
	t.Helper()
	h := &harness{logBuf: &syncBuffer{}}
	h.oai = newFakeUpstream(t, oai)
	h.ant = newFakeUpstream(t, ant)
	cfgJSON := fmt.Sprintf(`{
		"clients": [
			{"name": "caido", "key_sha256": %q},
			{"name": "script", "key_sha256": %q, "models": ["gpt-fast"]}
		],
		"providers": {
			"foundry": {"kind": "openai", "base_url": %q, "auth": {"type": "env", "env": "FOUNDRY_TOKEN"}},
			"anthropic": {"kind": "anthropic", "base_url": %q, "auth": {"type": "env", "env": "ANTHROPIC_KEY", "header": "x-api-key"},
				"headers": {"X-Team": "red"}}
		},
		"models": {
			"gpt-fast": {"provider": "foundry", "model": "gpt-6-luna"},
			"sonnet": {"provider": "anthropic", "model": "claude-sonnet-5-5"}
		}
	}`, HashClientKey(caidoKey), HashClientKey(scriptKey), h.oai.URL+"/openai/v1", h.ant.URL)
	cfg, err := ParseConfig([]byte(cfgJSON))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"FOUNDRY_TOKEN": "entra-token", "ANTHROPIC_KEY": "sk-ant-upstream"}
	h.gw = New(Options{
		Config: cfg, Version: "test",
		Logger: slog.New(slog.NewJSONHandler(h.logBuf, nil)),
		Getenv: func(k string) string { return env[k] },
	})
	return h
}

func (h *harness) do(t *testing.T, method, path, key string, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Host = "127.0.0.1:8789"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Caido/0.50.1 (darwin)")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			req.Host = hdr[i+1]
			continue
		}
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.gw.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return m
}

func jsonResponse(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "up-req-1")
		w.Header().Set("X-Ratelimit-Remaining-Requests", "99")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func sseResponse(events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, e := range events {
			_, _ = io.WriteString(w, e)
			w.(http.Flusher).Flush()
		}
	}
}

const oaiCompletion = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-6-luna",
	"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],
	"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":1}}}`

const antMessageBody = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5-5",
	"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","stop_sequence":null,
	"usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":10,"cache_creation_input_tokens":3}}`

func unused(http.ResponseWriter, *http.Request) {}

// ---- access control ----

func TestRejectsNonLoopback(t *testing.T) {
	h := newHarness(t, unused, unused)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = "192.168.1.5:1234"
	req.Header.Set("Authorization", "Bearer "+caidoKey)
	rec := httptest.NewRecorder()
	h.gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || rec.Header().Get(headerGatewayError) != "non_loopback" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestHealthzNeedsNoKeyAndIsNotLogged(t *testing.T) {
	h := newHarness(t, unused, unused)
	if rec := h.do(t, http.MethodGet, "/healthz", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
	if n := len(h.logBuf.lines(t)); n != 0 {
		t.Fatalf("healthz logged %d request lines", n)
	}
}

func TestClientAuth(t *testing.T) {
	h := newHarness(t, unused, unused)
	cases := []struct {
		name string
		hdr  []string
		want int
	}{
		{"missing", nil, http.StatusUnauthorized},
		{"wrong bearer", []string{"Authorization", "Bearer aigw_nope"}, http.StatusUnauthorized},
		{"bearer", []string{"Authorization", "Bearer " + caidoKey}, http.StatusOK},
		{"x-api-key", []string{"X-Api-Key", caidoKey}, http.StatusOK},
		{"api-key", []string{"Api-Key", caidoKey}, http.StatusOK},
		{"basic is not bearer", []string{"Authorization", "Basic " + caidoKey}, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do(t, http.MethodGet, "/v1/models", "", "", tc.hdr...)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestUnauthorizedErrorShapeFollowsRoute(t *testing.T) {
	h := newHarness(t, unused, unused)
	rec := h.do(t, http.MethodPost, "/v1/messages", "", `{}`)
	m := decode(t, rec)
	if m["type"] != "error" || m["error"].(map[string]any)["type"] != "authentication_error" {
		t.Fatalf("anthropic shape: %v", m)
	}
	rec = h.do(t, http.MethodPost, "/v1/chat/completions", "", `{}`)
	m = decode(t, rec)
	if e := m["error"].(map[string]any); e["code"] != "client_unauthorized" || e["type"] != "authentication_error" {
		t.Fatalf("openai shape: %v", m)
	}
}

func TestModelsListRespectsClientAllowlist(t *testing.T) {
	h := newHarness(t, unused, unused)
	ids := func(key string) []string {
		m := decode(t, h.do(t, http.MethodGet, "/models", key, ""))
		var out []string
		for _, d := range m["data"].([]any) {
			out = append(out, d.(map[string]any)["id"].(string))
		}
		return out
	}
	if got := fmt.Sprint(ids(caidoKey)); got != "[gpt-fast sonnet]" {
		t.Fatalf("caido: %s", got)
	}
	if got := fmt.Sprint(ids(scriptKey)); got != "[gpt-fast]" {
		t.Fatalf("script: %s", got)
	}
}

func TestRequestValidation(t *testing.T) {
	h := newHarness(t, unused, unused)
	cases := []struct {
		name, key, body, code string
		status                int
	}{
		{"not json", caidoKey, `[1,2]`, "invalid_request", 400},
		{"no model", caidoKey, `{"messages":[]}`, "invalid_model", 400},
		{"model not string", caidoKey, `{"model":5}`, "invalid_model", 400},
		{"case variant", caidoKey, `{"model":"gpt-fast","Model":"sonnet"}`, "invalid_model", 400},
		{"unknown model", caidoKey, `{"model":"gpt-9"}`, "model_not_found", 404},
		{"client not allowed", scriptKey, `{"model":"sonnet"}`, "model_not_found", 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do(t, http.MethodPost, "/v1/chat/completions", tc.key, tc.body)
			if rec.Code != tc.status || rec.Header().Get(headerGatewayError) != tc.code {
				t.Fatalf("got %d %q: %s", rec.Code, rec.Header().Get(headerGatewayError), rec.Body)
			}
		})
	}
	if h.oai.count()+h.ant.count() != 0 {
		t.Fatal("invalid requests reached an upstream")
	}
}

func TestUnknownRoute(t *testing.T) {
	h := newHarness(t, unused, unused)
	for _, p := range []string{"/v1/embeddings", "/v1/responses", "/admin"} {
		if rec := h.do(t, http.MethodPost, p, caidoKey, `{}`); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", p, rec.Code)
		}
	}
}

func TestBodyLimit(t *testing.T) {
	h := newHarness(t, unused, unused)
	h.gw.cfg.MaxRequestBytes = 64
	rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, `{"model":"gpt-fast","messages":[{"role":"user","content":"`+strings.Repeat("x", 100)+`"}]}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d", rec.Code)
	}
}

// ---- passthrough ----

func TestOpenAIPassthrough(t *testing.T) {
	h := newHarness(t, jsonResponse(200, oaiCompletion), unused)
	rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey,
		`{"model":"gpt-fast","messages":[{"role":"user","content":"ping"}],"temperature":0.1,"vendor_extra":{"keep":true}}`,
		"Cookie", "session=1", "X-Api-Key", caidoKey)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"content":"pong"`) {
		t.Fatalf("body: %s", rec.Body)
	}
	if rec.Header().Get("X-Ratelimit-Remaining-Requests") != "99" || rec.Header().Get("X-Request-Id") != "up-req-1" {
		t.Fatalf("relayed headers missing: %v", rec.Header())
	}
	up := h.oai.last(t)
	if up.Path != "/openai/v1/chat/completions" {
		t.Fatalf("path %s", up.Path)
	}
	if up.Body["model"] != "gpt-6-luna" || up.Body["vendor_extra"] == nil {
		t.Fatalf("body not rewritten faithfully: %v", up.Body)
	}
	if got := up.Header.Get("Authorization"); got != "Bearer entra-token" {
		t.Fatalf("upstream auth %q", got)
	}
	for _, hdr := range []string{"Cookie", "X-Api-Key"} {
		if up.Header.Get(hdr) != "" {
			t.Fatalf("client header %s leaked upstream", hdr)
		}
	}
	if !strings.HasPrefix(up.Header.Get("User-Agent"), "ai-gateway/") {
		t.Fatalf("UA %q", up.Header.Get("User-Agent"))
	}

	lines := h.logBuf.lines(t)
	if len(lines) != 1 {
		t.Fatalf("log lines: %d", len(lines))
	}
	l := lines[0]
	if l.Client != "caido" || l.Model != "gpt-fast" || l.UpstreamModel != "gpt-6-luna" || l.Provider != "foundry" || l.Status != 200 {
		t.Fatalf("log: %+v", l)
	}
	if l.InputTokens != 12 || l.OutputTokens != 3 || l.CachedTokens != 4 || l.ReasoningTokens != 1 || l.TotalTokens != 15 {
		t.Fatalf("usage: %+v", l)
	}
	if strings.Contains(h.logBuf.raw(), caidoKey) || strings.Contains(h.logBuf.raw(), "entra-token") {
		t.Fatal("a secret reached the log")
	}
}

func TestOpenAIPassthroughStreamHidesInjectedUsageChunk(t *testing.T) {
	stream := sseResponse(
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"po\"}}]}\n\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ng\"},\"finish_reason\":\"stop\"}]}\n\n",
		"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n",
		"data: [DONE]\n\n",
	)
	h := newHarness(t, stream, unused)

	rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, `{"model":"gpt-fast","stream":true,"messages":[]}`)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"usage"`) || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("usage chunk should be hidden: %s", rec.Body)
	}
	so := h.oai.last(t).Body["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("include_usage not forced: %v", so)
	}

	rec = h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, `{"model":"gpt-fast","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)
	if !strings.Contains(rec.Body.String(), `"usage"`) {
		t.Fatalf("usage chunk should be kept when asked for: %s", rec.Body)
	}
	for _, l := range h.logBuf.lines(t) {
		if l.InputTokens != 7 || l.OutputTokens != 2 {
			t.Fatalf("stream usage not logged: %+v", l)
		}
	}
}

func TestAnthropicPassthrough(t *testing.T) {
	h := newHarness(t, unused, jsonResponse(200, antMessageBody))
	rec := h.do(t, http.MethodPost, "/v1/messages", "", `{"model":"sonnet","max_tokens":10,"messages":[{"role":"user","content":"ping"}]}`,
		"X-Api-Key", caidoKey, "Anthropic-Version", "2023-06-01", "Anthropic-Beta", "prompt-caching-2024-07-31")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	up := h.ant.last(t)
	if up.Path != "/v1/messages" || up.Body["model"] != "claude-sonnet-5-5" {
		t.Fatalf("upstream %s %v", up.Path, up.Body)
	}
	if up.Header.Get("X-Api-Key") != "sk-ant-upstream" || up.Header.Get("Authorization") != "" {
		t.Fatalf("anthropic auth header wrong: %v", up.Header)
	}
	if up.Header.Get("Anthropic-Beta") != "prompt-caching-2024-07-31" || up.Header.Get("Anthropic-Version") != "2023-06-01" || up.Header.Get("X-Team") != "red" {
		t.Fatalf("headers: %v", up.Header)
	}
	l := h.logBuf.lines(t)[0]
	if l.InputTokens != 18 || l.CachedTokens != 10 || l.CacheWriteTokens != 3 || l.OutputTokens != 2 {
		t.Fatalf("anthropic usage: %+v", l)
	}
}

func TestAnthropicPassthroughStreamUsage(t *testing.T) {
	stream := sseResponse(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":9,\"cache_read_input_tokens\":100,\"output_tokens\":1}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":42}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	h := newHarness(t, unused, stream)
	rec := h.do(t, http.MethodPost, "/v1/messages", caidoKey, `{"model":"sonnet","stream":true,"max_tokens":5,"messages":[]}`)
	if !strings.Contains(rec.Body.String(), "event: message_stop") {
		t.Fatalf("stream not relayed: %s", rec.Body)
	}
	l := h.logBuf.lines(t)[0]
	if l.InputTokens != 109 || l.CachedTokens != 100 || l.OutputTokens != 42 || l.TotalTokens != 151 {
		t.Fatalf("usage: %+v", l)
	}
}

func TestCountTokens(t *testing.T) {
	h := newHarness(t, unused, jsonResponse(200, `{"input_tokens":17}`))
	rec := h.do(t, http.MethodPost, "/v1/messages/count_tokens", caidoKey, `{"model":"sonnet","messages":[]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"input_tokens":17`) || h.ant.last(t).Path != "/v1/messages/count_tokens" {
		t.Fatalf("anthropic count: %d %s", rec.Code, rec.Body)
	}
	body := `{"model":"gpt-fast","messages":[{"role":"user","content":"` + strings.Repeat("a", 400) + `"}]}`
	rec = h.do(t, http.MethodPost, "/v1/messages/count_tokens", caidoKey, body)
	if rec.Code != 200 || decode(t, rec)["input_tokens"].(float64) != float64(len(body)/4) || h.oai.count() != 0 {
		t.Fatalf("estimate: %d %s", rec.Code, rec.Body)
	}
}

// ---- translation ----

func TestAnthropicClientToOpenAIUpstream(t *testing.T) {
	h := newHarness(t, jsonResponse(200, oaiCompletion), unused)
	rec := h.do(t, http.MethodPost, "/v1/messages", caidoKey, `{"model":"gpt-fast","max_tokens":64,"system":"sys","messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	m := decode(t, rec)
	if m["type"] != "message" || m["model"] != "gpt-fast" || m["stop_reason"] != "end_turn" {
		t.Fatalf("response: %v", m)
	}
	if c := m["content"].([]any)[0].(map[string]any); c["text"] != "pong" {
		t.Fatalf("content: %v", c)
	}
	up := h.oai.last(t)
	if up.Path != "/openai/v1/chat/completions" || up.Body["model"] != "gpt-6-luna" || up.Body["max_completion_tokens"] != float64(64) {
		t.Fatalf("upstream: %s %v", up.Path, up.Body)
	}
	if l := h.logBuf.lines(t)[0]; !strings.Contains(h.logBuf.raw(), `"translated":true`) || l.InputTokens != 12 {
		t.Fatalf("log: %+v", l)
	}
}

func TestOpenAIClientToAnthropicUpstream(t *testing.T) {
	h := newHarness(t, unused, jsonResponse(200, antMessageBody))
	rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, `{"model":"sonnet","messages":[{"role":"system","content":"sys"},{"role":"user","content":"ping"}]}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	m := decode(t, rec)
	choice := m["choices"].([]any)[0].(map[string]any)
	if choice["message"].(map[string]any)["content"] != "pong" || choice["finish_reason"] != "stop" {
		t.Fatalf("choice: %v", choice)
	}
	if u := m["usage"].(map[string]any); u["prompt_tokens"] != float64(18) || u["completion_tokens"] != float64(2) {
		t.Fatalf("usage: %v", u)
	}
	up := h.ant.last(t)
	if up.Path != "/v1/messages" || up.Body["system"] != "sys" || up.Body["max_tokens"] != float64(defaultAnthropicMaxTokens) {
		t.Fatalf("upstream: %v", up.Body)
	}
	if up.Header.Get("Anthropic-Version") != DefaultAnthropicVersion {
		t.Fatalf("anthropic-version %q", up.Header.Get("Anthropic-Version"))
	}
}

func TestAnthropicClientStreamFromOpenAI(t *testing.T) {
	stream := sseResponse(
		"data: {\"id\":\"chatcmpl-9\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n",
		"data: {\"id\":\"chatcmpl-9\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n",
		"data: {\"id\":\"chatcmpl-9\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"}}]}\n\n",
		"data: {\"id\":\"chatcmpl-9\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"scan\",\"arguments\":\"\"}}]}}]}\n\n",
		"data: {\"id\":\"chatcmpl-9\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"h\\\":\"}}]}}]}\n\n",
		"data: {\"id\":\"chatcmpl-9\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]}}]}\n\n",
		"data: {\"id\":\"chatcmpl-9\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
		"data: {\"id\":\"chatcmpl-9\",\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":8,\"total_tokens\":28,\"prompt_tokens_details\":{\"cached_tokens\":5}}}\n\n",
		"data: [DONE]\n\n",
	)
	h := newHarness(t, stream, unused)
	rec := h.do(t, http.MethodPost, "/v1/messages", caidoKey, `{"model":"gpt-fast","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"go"}]}`)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	events := parseSSE(t, rec.Body.String())
	var types []string
	for _, e := range events {
		types = append(types, e.name)
	}
	want := "message_start content_block_start content_block_delta content_block_delta content_block_stop content_block_start content_block_delta content_block_delta content_block_stop message_delta message_stop"
	if got := strings.Join(types, " "); got != want {
		t.Fatalf("events:\n got %s\nwant %s", got, want)
	}
	for _, e := range events {
		if e.name != e.data["type"] {
			t.Fatalf("event name %q does not match data type %v", e.name, e.data["type"])
		}
	}
	text := events[2].data["delta"].(map[string]any)["text"].(string) + events[3].data["delta"].(map[string]any)["text"].(string)
	if text != "Hello" {
		t.Fatalf("text %q", text)
	}
	tool := events[5].data["content_block"].(map[string]any)
	if tool["type"] != "tool_use" || tool["id"] != "call_a" || tool["name"] != "scan" || events[5].data["index"] != float64(1) {
		t.Fatalf("tool block: %v", events[5].data)
	}
	args := events[6].data["delta"].(map[string]any)["partial_json"].(string) + events[7].data["delta"].(map[string]any)["partial_json"].(string)
	if args != `{"h":1}` {
		t.Fatalf("args %q", args)
	}
	md := events[9].data
	if md["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("stop: %v", md)
	}
	if u := md["usage"].(map[string]any); u["output_tokens"] != float64(8) || u["input_tokens"] != float64(15) || u["cache_read_input_tokens"] != float64(5) {
		t.Fatalf("usage: %v", u)
	}
	if l := h.logBuf.lines(t)[0]; l.InputTokens != 20 || l.OutputTokens != 8 {
		t.Fatalf("log usage: %+v", l)
	}
}

func TestOpenAIClientStreamFromAnthropic(t *testing.T) {
	stream := sseResponse(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_7\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":11,\"output_tokens\":1}}}\n\n",
		"event: ping\ndata: {\"type\":\"ping\"}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_z\",\"name\":\"scan\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"h\\\":2}\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":6}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	h := newHarness(t, unused, stream)
	rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, `{"model":"sonnet","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"go"}]}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	events := parseSSE(t, rec.Body.String())
	if last := events[len(events)-1]; last.raw != "[DONE]" {
		t.Fatalf("stream must end with [DONE], got %q", last.raw)
	}
	var content, args, finish string
	var usage map[string]any
	for _, e := range events[:len(events)-1] {
		if e.data["object"] != "chat.completion.chunk" || e.data["id"] != "chatcmpl-7" || e.data["model"] != "sonnet" {
			t.Fatalf("chunk envelope: %v", e.data)
		}
		if u, ok := e.data["usage"].(map[string]any); ok {
			usage = u
			continue
		}
		ch := e.data["choices"].([]any)[0].(map[string]any)
		d := ch["delta"].(map[string]any)
		if c, ok := d["content"].(string); ok {
			content += c
		}
		if tcs, ok := d["tool_calls"].([]any); ok {
			tc := tcs[0].(map[string]any)
			if tc["index"] != float64(0) {
				t.Fatalf("tool index: %v", tc)
			}
			fn := tc["function"].(map[string]any)
			args += fn["arguments"].(string)
			if id, ok := tc["id"]; ok && id != "toolu_z" {
				t.Fatalf("tool id %v", id)
			}
		}
		if f, ok := ch["finish_reason"].(string); ok {
			finish = f
		}
	}
	if content != "Hi" || args != `{"h":2}` || finish != "tool_calls" {
		t.Fatalf("content %q args %q finish %q", content, args, finish)
	}
	if usage == nil || usage["prompt_tokens"] != float64(11) || usage["completion_tokens"] != float64(6) {
		t.Fatalf("usage chunk: %v", usage)
	}
}

func TestTranslatedUpstreamErrorKeepsStatusAndCallerShape(t *testing.T) {
	h := newHarness(t,
		jsonResponse(429, `{"error":{"message":"slow down","type":"rate_limit"}}`),
		jsonResponse(400, `{"type":"error","error":{"type":"invalid_request_error","message":"bad thing"}}`))

	rec := h.do(t, http.MethodPost, "/v1/messages", caidoKey, `{"model":"gpt-fast","max_tokens":5,"messages":[]}`)
	m := decode(t, rec)
	if rec.Code != 429 || m["error"].(map[string]any)["type"] != "rate_limit_error" || !strings.Contains(m["error"].(map[string]any)["message"].(string), "slow down") {
		t.Fatalf("%d %v", rec.Code, m)
	}
	if rec.Header().Get("X-Ratelimit-Remaining-Requests") != "99" {
		t.Fatal("rate-limit header not relayed")
	}

	rec = h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, `{"model":"sonnet","messages":[]}`)
	m = decode(t, rec)
	if rec.Code != 400 || !strings.Contains(m["error"].(map[string]any)["message"].(string), "bad thing") {
		t.Fatalf("%d %v", rec.Code, m)
	}
}

func TestPassthroughUpstreamErrorIsRelayedVerbatim(t *testing.T) {
	body := `{"error":{"message":"deployment missing","code":"DeploymentNotFound"}}`
	h := newHarness(t, jsonResponse(404, body), unused)
	rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, `{"model":"gpt-fast","messages":[]}`)
	if rec.Code != 404 || rec.Body.String() != body {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestRetriesOnceOnUpstream401(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		jsonResponse(200, oaiCompletion)(w, r)
	}, unused)
	rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, `{"model":"gpt-fast","messages":[]}`)
	if rec.Code != 200 || calls.Load() != 2 || !strings.Contains(h.logBuf.raw(), `"auth_retried":true`) {
		t.Fatalf("%d calls=%d", rec.Code, calls.Load())
	}
}

func TestCredentialUnavailable(t *testing.T) {
	h := newHarness(t, unused, unused)
	h.gw.providers["foundry"].cred = envCred{name: "MISSING", getenv: func(string) string { return "" }}
	rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, `{"model":"gpt-fast","messages":[]}`)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get(headerGatewayError) != "credential_unavailable" || h.oai.count() != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "MISSING") {
		t.Fatal("credential details leaked to the client")
	}
}

func TestUpstreamUnreachable(t *testing.T) {
	h := newHarness(t, unused, unused)
	h.oai.Close()
	rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, `{"model":"gpt-fast","messages":[]}`)
	if rec.Code != http.StatusBadGateway || rec.Header().Get(headerGatewayError) != "upstream_unreachable" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestStreamIdleTimeoutCutsStream(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}, unused)
	h.gw.cfg.StreamIdleTimeout = Duration(50 * time.Millisecond)
	rec := h.do(t, http.MethodPost, "/v1/messages", caidoKey, `{"model":"gpt-fast","stream":true,"max_tokens":5,"messages":[]}`)
	if !strings.Contains(rec.Body.String(), "message_stop") || !strings.Contains(h.logBuf.raw(), `"stream_idle_timeout":true`) {
		t.Fatalf("idle stream not cut cleanly: %s", rec.Body)
	}
}

func TestClientCancelIsLogged499(t *testing.T) {
	arrived := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-r.Context().Done()
	}, unused)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-fast","messages":[]}`)).WithContext(ctx)
	req.RemoteAddr = "127.0.0.1:1"
	req.Host = "127.0.0.1:8789"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+caidoKey)
	done := make(chan struct{})
	go func() {
		h.gw.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	select {
	case <-arrived:
	case <-done:
		t.Fatal("request finished before reaching the upstream")
	}
	cancel()
	<-done
	if l := h.logBuf.lines(t)[0]; l.Status != 499 {
		t.Fatalf("status %d", l.Status)
	}
}

// ---- helpers ----

type sseEvent struct {
	name string
	raw  string
	data map[string]any
}

func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, block := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var e sseEvent
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				e.name = v
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				e.raw = v
			}
		}
		if e.raw != "[DONE]" {
			if err := json.Unmarshal([]byte(e.raw), &e.data); err != nil {
				t.Fatalf("bad SSE data %q", e.raw)
			}
		}
		out = append(out, e)
	}
	return out
}

func TestCaidoShiftPrefixedModel(t *testing.T) {
	h := newHarness(t, jsonResponse(200, oaiCompletion), unused)
	rec := h.do(t, http.MethodPost, "/chat/completions", caidoKey, `{"model":"openai/gpt-fast","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || h.oai.last(t).Body["model"] != "gpt-6-luna" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if l := h.logBuf.lines(t)[0]; l.Model != "gpt-fast" {
		t.Fatalf("logged alias %q", l.Model)
	}
}
