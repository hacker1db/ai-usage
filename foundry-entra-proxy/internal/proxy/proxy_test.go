package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"foundry-entra-proxy/internal/token"
)

const testKey = "client-key-0123456789abcdef0123456789"

type fakeTokens struct {
	mu          sync.Mutex
	generation  int
	invalidated int
	err         error
}

func (f *fakeTokens) Token(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	return fmt.Sprintf("entra-%d", f.generation), nil
}

func (f *fakeTokens) Invalidate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.generation++
	f.invalidated++
}

type captured struct {
	mu      sync.Mutex
	headers []http.Header
	paths   []string
}

func (c *captured) add(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.headers = append(c.headers, r.Header.Clone())
	c.paths = append(c.paths, r.URL.RequestURI())
}

func (c *captured) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.paths)
}

func (c *captured) last() (http.Header, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.headers) == 0 {
		return nil, ""
	}
	return c.headers[len(c.headers)-1], c.paths[len(c.paths)-1]
}

type harness struct {
	proxy    *httptest.Server
	tokens   *fakeTokens
	upstream *captured
	logs     *strings.Builder
}

func newHarness(t *testing.T, upstream http.HandlerFunc, mutate func(*Options)) *harness {
	t.Helper()
	cap := &captured{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.add(r)
		upstream(w, r)
	}))
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)

	logs := &strings.Builder{}
	var logMu sync.Mutex
	tokens := &fakeTokens{}
	opts := Options{
		Upstream:           u,
		ClientKey:          testKey,
		AllowedDeployments: []string{"gpt-5.5", "gpt-5-mini"},
		Tokens:             tokens,
		StreamIdleTimeout:  2 * time.Second,
		MaxRequestBytes:    1 << 20,
		Logger:             slog.New(slog.NewJSONHandler(lockedWriter{&logMu, logs}, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if mutate != nil {
		mutate(&opts)
	}
	front := httptest.NewServer(New(opts))
	t.Cleanup(front.Close)
	return &harness{proxy: front, tokens: tokens, upstream: cap, logs: logs}
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (h *harness) do(t *testing.T, method, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.proxy.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func okUpstream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Apim-Request-Id", "apim-123")
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true}`)
}

func bearer(k string) map[string]string { return map[string]string{"Authorization": "Bearer " + k} }

func TestClientAuth(t *testing.T) {
	h := newHarness(t, okUpstream, nil)
	body := `{"model":"gpt-5.5"}`
	tests := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{name: "no credentials", headers: nil, want: http.StatusUnauthorized},
		{name: "wrong bearer", headers: bearer("nope"), want: http.StatusUnauthorized},
		{name: "basic scheme", headers: map[string]string{"Authorization": "Basic " + testKey}, want: http.StatusUnauthorized},
		{name: "bearer", headers: bearer(testKey), want: http.StatusOK},
		{name: "lowercase bearer", headers: map[string]string{"Authorization": "bearer " + testKey}, want: http.StatusOK},
		{name: "api-key", headers: map[string]string{"api-key": testKey}, want: http.StatusOK},
		{name: "x-api-key", headers: map[string]string{"x-api-key": testKey}, want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.do(t, http.MethodPost, "/openai/v1/responses", body, tt.headers)
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
			if tt.want == http.StatusUnauthorized && resp.Header.Get(headerProxyError) != "client_unauthorized" {
				t.Fatalf("missing proxy error header: %v", resp.Header)
			}
		})
	}
}

func TestHeadersStrippedAndBearerInjected(t *testing.T) {
	h := newHarness(t, okUpstream, nil)
	headers := map[string]string{
		"Authorization":       "Bearer " + testKey,
		"api-key":             testKey,
		"x-api-key":           testKey,
		"Proxy-Authorization": "Basic Zm9v",
		"Cookie":              "session=1",
		"OpenAI-Beta":         "responses=v1",
	}
	resp := h.do(t, http.MethodPost, "/openai/v1/responses?api-version=preview", `{"model":"gpt-5.5"}`, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	got, uri := h.upstream.last()
	if uri != "/openai/v1/responses?api-version=preview" {
		t.Fatalf("upstream uri = %q", uri)
	}
	if v := got.Get("Authorization"); v != "Bearer entra-0" {
		t.Fatalf("upstream Authorization = %q", v)
	}
	for _, name := range []string{"Api-Key", "X-Api-Key", "Proxy-Authorization", "Cookie"} {
		if v := got.Get(name); v != "" {
			t.Errorf("upstream received %s = %q", name, v)
		}
	}
	if got.Get("OpenAI-Beta") != "responses=v1" {
		t.Errorf("non-auth header dropped")
	}
	if got.Get("X-Ms-Client-Request-Id") == "" || got.Get("X-Ms-Client-Request-Id") != resp.Header.Get(headerCorrelate) {
		t.Errorf("correlation id not propagated")
	}
}

func TestRouteAndDeploymentAllowlist(t *testing.T) {
	h := newHarness(t, okUpstream, nil)
	tests := []struct {
		name      string
		method    string
		path      string
		body      string
		want      int
		wantError string
	}{
		{name: "responses allowed", method: "POST", path: "/openai/v1/responses", body: `{"model":"gpt-5.5"}`, want: 200},
		{name: "chat allowed", method: "POST", path: "/openai/v1/chat/completions", body: `{"model":"gpt-5-mini"}`, want: 200},
		{name: "compact allowed", method: "POST", path: "/openai/v1/responses/compact", body: `{"model":"gpt-5.5"}`, want: 200},
		{name: "list models", method: "GET", path: "/openai/v1/models", want: 200},
		{name: "get allowed model", method: "GET", path: "/openai/v1/models/gpt-5.5", want: 200},
		{name: "get disallowed model", method: "GET", path: "/openai/v1/models/o3-pro", want: 403, wantError: "deployment_not_allowed"},
		{name: "get response", method: "GET", path: "/openai/v1/responses/resp_123", want: 200},
		{name: "cancel response", method: "POST", path: "/openai/v1/responses/resp_123/cancel", want: 200},
		{name: "disallowed deployment", method: "POST", path: "/openai/v1/responses", body: `{"model":"gpt-4o"}`, want: 403, wantError: "deployment_not_allowed"},
		{name: "missing model", method: "POST", path: "/openai/v1/responses", body: `{"input":"hi"}`, want: 400, wantError: "model_required"},
		{name: "invalid json", method: "POST", path: "/openai/v1/responses", body: `{`, want: 400, wantError: "invalid_json"},
		{name: "outside prefix", method: "GET", path: "/openai/deployments/x/chat/completions", want: 403, wantError: "route_not_allowed"},
		{name: "unknown v1 route", method: "POST", path: "/openai/v1/files", body: `{}`, want: 403, wantError: "route_not_allowed"},
		{name: "wrong method", method: "DELETE", path: "/openai/v1/models", want: 403, wantError: "route_not_allowed"},
		{name: "dot segments", method: "GET", path: "/openai/v1/models/../../admin", want: 403, wantError: "route_not_allowed"},
		{name: "encoded slash", method: "GET", path: "/openai/v1/models/a%2Fb", want: 403, wantError: "route_not_allowed"},
		{name: "root", method: "GET", path: "/", want: 403, wantError: "route_not_allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := h.upstream.count()
			req, _ := http.NewRequest(tt.method, h.proxy.URL, strings.NewReader(tt.body))
			req.URL.Opaque = tt.path
			req.Header.Set("Authorization", "Bearer "+testKey)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
			if got := resp.Header.Get(headerProxyError); got != tt.wantError {
				t.Fatalf("proxy error = %q, want %q", got, tt.wantError)
			}
			forwarded := h.upstream.count() > before
			if forwarded != (tt.want == 200) {
				t.Fatalf("forwarded = %v for status %d", forwarded, tt.want)
			}
		})
	}
}

func TestRequestTooLarge(t *testing.T) {
	h := newHarness(t, okUpstream, func(o *Options) { o.MaxRequestBytes = 64 })
	body := `{"model":"gpt-5.5","input":"` + strings.Repeat("x", 128) + `"}`
	resp := h.do(t, http.MethodPost, "/openai/v1/responses", body, bearer(testKey))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestNonLoopbackRejected(t *testing.T) {
	p := New(Options{ClientKey: testKey, Tokens: &fakeTokens{}})
	for _, path := range []string{"/healthz", "/openai/v1/models"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "10.1.2.3:5555"
		req.Header.Set("Authorization", "Bearer "+testKey)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || rec.Header().Get(headerProxyError) != "non_loopback" {
			t.Fatalf("%s: status = %d, error = %q", path, rec.Code, rec.Header().Get(headerProxyError))
		}
	}
}

func TestSSEStreamsWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"n\":1}\n\n")
		fl.Flush()
		<-release
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"n\":2}\n\n")
		fl.Flush()
	}, nil)
	defer close(release)

	resp := h.do(t, http.MethodPost, "/openai/v1/responses", `{"model":"gpt-5.5","stream":true}`, bearer(testKey))
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	lines := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	select {
	case l := <-lines:
		if l != "event: response.created" {
			t.Fatalf("first line = %q", l)
		}
	case <-time.After(time.Second):
		t.Fatal("first SSE event was buffered instead of streamed")
	}
}

func TestStreamIdleTimeoutCutsStalledUpstream(t *testing.T) {
	stall := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}, func(o *Options) { o.StreamIdleTimeout = 200 * time.Millisecond })
	defer close(stall)

	resp := h.do(t, http.MethodPost, "/openai/v1/responses", `{"model":"gpt-5.5","stream":true}`, bearer(testKey))
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stalled stream was not cut by idle timeout")
	}
}

func TestUpstream401ForcesOneReacquire(t *testing.T) {
	tests := []struct {
		name          string
		acceptToken   string
		wantStatus    int
		wantProxyErr  string
		wantInvalid   int
		wantUpstreams int
	}{
		{name: "fresh token succeeds", acceptToken: "Bearer entra-1", wantStatus: 200, wantInvalid: 1, wantUpstreams: 2},
		{name: "fresh token also rejected", acceptToken: "never", wantStatus: 401, wantProxyErr: "upstream_unauthorized", wantInvalid: 1, wantUpstreams: 2},
		{name: "first token accepted", acceptToken: "Bearer entra-0", wantStatus: 200, wantInvalid: 0, wantUpstreams: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var bodies []string
			var mu sync.Mutex
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, string(b))
				mu.Unlock()
				if r.Header.Get("Authorization") != tt.acceptToken {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				okUpstream(w, r)
			}, nil)

			body := `{"model":"gpt-5.5","input":"hi"}`
			resp := h.do(t, http.MethodPost, "/openai/v1/responses", body, bearer(testKey))
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if got := resp.Header.Get(headerProxyError); got != tt.wantProxyErr {
				t.Fatalf("proxy error = %q, want %q", got, tt.wantProxyErr)
			}
			if h.tokens.invalidated != tt.wantInvalid || len(bodies) != tt.wantUpstreams {
				t.Fatalf("invalidated = %d upstream calls = %d", h.tokens.invalidated, len(bodies))
			}
			for _, b := range bodies {
				if b != body {
					t.Fatalf("retry body = %q, want %q", b, body)
				}
			}
		})
	}
}

func TestTokenFailuresMapTo503(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "login required", err: fmt.Errorf("%w: run az login", token.ErrLoginRequired), code: "az_login_required"},
		{name: "other", err: fmt.Errorf("az exploded"), code: "token_unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, okUpstream, nil)
			h.tokens.err = tt.err

			resp := h.do(t, http.MethodPost, "/openai/v1/responses", `{"model":"gpt-5.5"}`, bearer(testKey))
			if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get(headerProxyError) != tt.code {
				t.Fatalf("proxy: status = %d, error = %q", resp.StatusCode, resp.Header.Get(headerProxyError))
			}

			ready := h.do(t, http.MethodGet, "/readyz", "", nil)
			if ready.StatusCode != http.StatusServiceUnavailable || ready.Header.Get(headerProxyError) != tt.code {
				t.Fatalf("readyz: status = %d, error = %q", ready.StatusCode, ready.Header.Get(headerProxyError))
			}

			health := h.do(t, http.MethodGet, "/healthz", "", nil)
			if health.StatusCode != http.StatusOK {
				t.Fatalf("healthz status = %d", health.StatusCode)
			}
		})
	}
}

func TestReadyzOK(t *testing.T) {
	h := newHarness(t, okUpstream, nil)
	resp := h.do(t, http.MethodGet, "/readyz", "", nil)
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusOK || body["status"] != "ready" {
		t.Fatalf("readyz = %d %v", resp.StatusCode, body)
	}
}

func TestUpstreamUnreachable(t *testing.T) {
	h := newHarness(t, okUpstream, func(o *Options) {
		o.Upstream = &url.URL{Scheme: "http", Host: "127.0.0.1:1"}
	})
	resp := h.do(t, http.MethodPost, "/openai/v1/responses", `{"model":"gpt-5.5"}`, bearer(testKey))
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get(headerProxyError) != "upstream_unreachable" {
		t.Fatalf("status = %d, error = %q", resp.StatusCode, resp.Header.Get(headerProxyError))
	}
}

func TestLogsNeverContainCredentials(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}, nil)
	h.do(t, http.MethodPost, "/openai/v1/responses", `{"model":"gpt-5.5"}`, bearer(testKey))
	h.do(t, http.MethodPost, "/openai/v1/responses", `{"model":"gpt-5.5"}`, bearer(testKey[:20]+"wrong-suffix-xxxxxxxx"))

	logs := h.logs.String()
	if logs == "" {
		t.Fatal("expected request logs")
	}
	for _, secret := range []string{testKey, testKey[:8], "entra-0", "entra-1"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs contain credential material %q:\n%s", secret, logs)
		}
	}
	if !strings.Contains(logs, `"upstream_request_id"`) || !strings.Contains(logs, `"id"`) {
		t.Fatalf("logs missing correlation fields:\n%s", logs)
	}
}
