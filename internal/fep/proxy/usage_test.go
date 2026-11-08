package proxy

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ai-usage/internal/proxylog"
)

// requestLog waits for the proxy's "request" log line (written after the response finishes) and decodes it.
func requestLog(t *testing.T, h *harness) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(h.logText(), "\n") {
			var entry map[string]any
			if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "request" {
				return entry
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no request log line:\n%s", h.logText())
	return nil
}

func wantTokens(t *testing.T, entry map[string]any, want map[string]float64) {
	t.Helper()
	for k, v := range want {
		if got, ok := entry[k].(float64); !ok || got != v {
			t.Errorf("%s = %v, want %v (log: %v)", k, entry[k], v, entry)
		}
	}
}

func TestUsageFromResponsesJSON(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","output":[],"usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":100},"output_tokens":30,"output_tokens_details":{"reasoning_tokens":12},"total_tokens":150}}`)
	}, nil)
	resp := h.do(t, http.MethodPost, "/openai/v1/responses", `{"model":"gpt-5.5"}`,
		map[string]string{"Authorization": "Bearer " + testKey, "User-Agent": "codex_cli_rs/0.166.0 (Mac OS 26.0; arm64)"})
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"resp_1"`) {
		t.Fatalf("body altered: %s", body)
	}

	entry := requestLog(t, h)
	wantTokens(t, entry, map[string]float64{
		"input_tokens": 120, "output_tokens": 30, "cached_tokens": 100, "reasoning_tokens": 12, "total_tokens": 150,
	})
	if entry["client"] != "codex_cli_rs" {
		t.Errorf("client = %v, want codex_cli_rs", entry["client"])
	}
}

func TestUsageFromResponsesStream(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"usage\":null}}\n\n")
		fl.Flush()
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"usage\"}\n\n")
		// Split the final event across writes to exercise line reassembly.
		_, _ = io.WriteString(w, "event: response.completed\r\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":")
		fl.Flush()
		_, _ = io.WriteString(w, "40,\"output_tokens\":8,\"total_tokens\":48}}}\r\n\r\n")
	}, nil)
	resp := h.do(t, http.MethodPost, "/openai/v1/responses", `{"model":"gpt-5.5","stream":true}`, bearer(testKey))
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "response.completed") {
		t.Fatalf("stream altered: %s", body)
	}
	wantTokens(t, requestLog(t, h), map[string]float64{"input_tokens": 40, "output_tokens": 8, "total_tokens": 48})
}

func TestUsageFromChatCompletionsStream(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3,\"total_tokens\":12,\"prompt_tokens_details\":{\"cached_tokens\":4}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}, nil)
	resp := h.do(t, http.MethodPost, "/openai/v1/chat/completions", `{"model":"gpt-5.5","stream":true}`, bearer(testKey))
	_, _ = io.ReadAll(resp.Body)
	wantTokens(t, requestLog(t, h), map[string]float64{"input_tokens": 9, "output_tokens": 3, "cached_tokens": 4, "total_tokens": 12})
}

func TestUsageReadThroughUpstreamGzip(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("transport should negotiate gzip itself, got Accept-Encoding %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = io.WriteString(gz, `{"usage":{"input_tokens":5,"output_tokens":2}}`)
		_ = gz.Close()
	}, nil)
	resp := h.do(t, http.MethodPost, "/openai/v1/responses", `{"model":"gpt-5.5"}`,
		map[string]string{"Authorization": "Bearer " + testKey, "Accept-Encoding": "br"})
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"input_tokens":5`) {
		t.Fatalf("client should receive decoded JSON, got %q", body)
	}
	wantTokens(t, requestLog(t, h), map[string]float64{"input_tokens": 5, "output_tokens": 2, "total_tokens": 7})
}

func TestNoUsageOnUpstreamError(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"429"},"usage":{"input_tokens":1}}`)
	}, nil)
	resp := h.do(t, http.MethodPost, "/openai/v1/responses", `{"model":"gpt-5.5"}`, bearer(testKey))
	_, _ = io.ReadAll(resp.Body)
	if _, ok := requestLog(t, h)["input_tokens"]; ok {
		t.Fatal("usage must only be logged for 2xx responses")
	}
}

func TestClientName(t *testing.T) {
	for ua, want := range map[string]string{
		"codex_cli_rs/0.166.0 (Mac OS 26.0; arm64) iTerm": "codex_cli_rs",
		"OpenAI/Go 1.2.3":        "OpenAI",
		"":                       "",
		strings.Repeat("x", 100): strings.Repeat("x", 64),
	} {
		if got := clientName(ua); got != want {
			t.Errorf("clientName(%q) = %q, want %q", ua, got, want)
		}
	}
}

// The ai-usage collector decodes request lines into proxylog.Request; this pins that contract.
func TestRequestLogMatchesCollectorContract(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":100},"output_tokens":30,"output_tokens_details":{"reasoning_tokens":12},"total_tokens":150}}`)
	}, nil)
	resp := h.do(t, http.MethodPost, "/openai/v1/responses", `{"model":"gpt-5.5"}`,
		map[string]string{"Authorization": "Bearer " + testKey, "User-Agent": "codex_cli_rs/0.166.0"})
	_, _ = io.ReadAll(resp.Body)
	requestLog(t, h) // wait for the line

	var got proxylog.Request
	for _, line := range strings.Split(h.logText(), "\n") {
		var r proxylog.Request
		if json.Unmarshal([]byte(line), &r) == nil && r.Msg == proxylog.Message {
			got = r
		}
	}
	want := proxylog.Request{Time: got.Time, Msg: proxylog.Message, Model: "gpt-5.5", Client: "codex_cli_rs", Status: 200,
		InputTokens: 120, OutputTokens: 30, CachedTokens: 100, ReasoningTokens: 12, TotalTokens: 150}
	if got != want || got.Time == "" {
		t.Fatalf("decoded request line = %+v, want %+v", got, want)
	}
}
