package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

const chatBody = `{"model":"gpt-fast","messages":[{"role":"user","content":"hi"}]}`

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := newHarness(t, unused, unused)
	for _, rec := range []struct {
		name string
		code int
		hdr  http.Header
	}{
		{"healthz", 0, h.do(t, http.MethodGet, "/healthz", "", "").Header()},
		{"unauthorized", 0, h.do(t, http.MethodGet, "/v1/models", "", "").Header()},
		{"bad host", 0, h.do(t, http.MethodGet, "/healthz", "", "", "Host", "evil.example").Header()},
	} {
		for k, want := range map[string]string{
			"X-Content-Type-Options":       "nosniff",
			"Cache-Control":                "no-store",
			"X-Frame-Options":              "DENY",
			"Referrer-Policy":              "no-referrer",
			"Cross-Origin-Resource-Policy": "same-origin",
		} {
			if got := rec.hdr.Get(k); got != want {
				t.Errorf("%s: %s = %q, want %q", rec.name, k, got, want)
			}
		}
		if !strings.Contains(rec.hdr.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q", rec.name, rec.hdr.Get("Content-Security-Policy"))
		}
		if rec.hdr.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: unexpected ACAO", rec.name)
		}
	}
}

func TestHostHeaderBlocksDNSRebinding(t *testing.T) {
	h := newHarness(t, jsonResponse(200, oaiCompletion), unused)
	for host, want := range map[string]int{
		"127.0.0.1:8789":      200,
		"localhost:8789":      200,
		"LOCALHOST":           200,
		"[::1]:8789":          200,
		"gw.localhost:8789":   200,
		"evil.example:8789":   http.StatusMisdirectedRequest,
		"127.0.0.1.nip.io":    http.StatusMisdirectedRequest,
		"localhost.evil.com":  http.StatusMisdirectedRequest,
		"":                    http.StatusMisdirectedRequest,
		"192.168.1.10:8789":   http.StatusMisdirectedRequest,
		"[::ffff:8.8.8.8]:80": http.StatusMisdirectedRequest,
	} {
		rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, chatBody, "Host", host)
		if rec.Code != want {
			t.Errorf("Host %q: %d, want %d (%s)", host, rec.Code, want, rec.Body)
		}
	}
}

func TestBrowserRequestsRefusedByDefault(t *testing.T) {
	h := newHarness(t, jsonResponse(200, oaiCompletion), unused)
	cases := []struct {
		name string
		hdr  []string
		want int
		code string
	}{
		{"foreign origin", []string{"Origin", "https://evil.example"}, 403, "origin_not_allowed"},
		{"null origin", []string{"Origin", "null"}, 403, "origin_not_allowed"},
		{"empty origin", []string{"Origin", ""}, 403, "origin_not_allowed"},
		{"loopback origin not listed", []string{"Origin", "http://localhost:3000"}, 403, "origin_not_allowed"},
		{"cross-site no origin", []string{"Sec-Fetch-Site", "cross-site"}, 403, "cross_site"},
		{"same-site no origin", []string{"Sec-Fetch-Site", "same-site"}, 403, "cross_site"},
		{"same-origin no origin", []string{"Sec-Fetch-Site", "same-origin"}, 403, "cross_site"},
		{"text/plain body", []string{"Content-Type", "text/plain"}, 415, "unsupported_media_type"},
		{"form body", []string{"Content-Type", "application/x-www-form-urlencoded"}, 415, "unsupported_media_type"},
		{"user typed URL", []string{"Sec-Fetch-Site", "none"}, 200, ""},
		{"json with charset", []string{"Content-Type", "application/json; charset=utf-8"}, 200, ""},
	}
	for _, c := range cases {
		rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, chatBody, c.hdr...)
		if rec.Code != c.want || rec.Header().Get(headerGatewayError) != c.code {
			t.Errorf("%s: %d %q, want %d %q", c.name, rec.Code, rec.Header().Get(headerGatewayError), c.want, c.code)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: ACAO must not be set", c.name)
		}
	}
	// A refused browser request never reaches the upstream.
	h.oai.mu.Lock()
	n := len(h.oai.requests)
	h.oai.mu.Unlock()
	if n != 2 {
		t.Fatalf("upstream saw %d calls, want 2", n)
	}

	rec := h.do(t, http.MethodOptions, "/v1/chat/completions", "", "",
		"Origin", "https://evil.example", "Access-Control-Request-Method", "POST")
	if rec.Code != 403 || rec.Header().Get("Access-Control-Allow-Methods") != "" {
		t.Fatalf("preflight from foreign origin: %d %v", rec.Code, rec.Header())
	}
}

func TestAllowedOriginCORS(t *testing.T) {
	h := newHarness(t, jsonResponse(200, oaiCompletion), unused)
	const origin = "http://localhost:5173"
	h.gw.cfg.AllowedOrigins = []string{origin}

	pre := h.do(t, http.MethodOptions, "/v1/chat/completions", "", "",
		"Origin", origin, "Access-Control-Request-Method", "POST",
		"Access-Control-Request-Headers", "Authorization, Content-Type, X-Stainless-OS",
		"Access-Control-Request-Private-Network", "true")
	hdr := pre.Header()
	if pre.Code != 204 || hdr.Get("Access-Control-Allow-Origin") != origin || hdr.Get("Access-Control-Allow-Methods") != "GET, POST" ||
		hdr.Get("Access-Control-Max-Age") != corsMaxAge || hdr.Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatalf("preflight: %d %v", pre.Code, hdr)
	}
	if hdr.Get("Access-Control-Allow-Credentials") != "" || strings.Contains(hdr.Get("Access-Control-Allow-Origin"), "*") {
		t.Fatalf("credentials or wildcard leaked: %v", hdr)
	}

	for name, extra := range map[string][]string{
		"method": {"Access-Control-Request-Method", "DELETE"},
		"header": {"Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "content-type, x-evil"},
		"none":   {},
	} {
		rec := h.do(t, http.MethodOptions, "/v1/chat/completions", "", "", append([]string{"Origin", origin}, extra...)...)
		if rec.Code != 403 || rec.Header().Get("Access-Control-Allow-Methods") != "" {
			t.Errorf("preflight %s: %d %v", name, rec.Code, rec.Header())
		}
	}

	rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, chatBody, "Origin", origin, "Sec-Fetch-Site", "cross-site")
	if rec.Code != 200 || rec.Header().Get("Access-Control-Allow-Origin") != origin ||
		!strings.Contains(rec.Header().Get("Access-Control-Expose-Headers"), headerRequestID) {
		t.Fatalf("allowed origin request: %d %v", rec.Code, rec.Header())
	}
	// Allowed origin still needs a key.
	if rec := h.do(t, http.MethodPost, "/v1/chat/completions", "", chatBody, "Origin", origin); rec.Code != 401 {
		t.Fatalf("keyless allowed origin: %d", rec.Code)
	}
	// Only the exact origin is allowed.
	if rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, chatBody, "Origin", "http://localhost:5174"); rec.Code != 403 {
		t.Fatalf("near-miss origin: %d", rec.Code)
	}
}

func TestCheckOrigin(t *testing.T) {
	for _, ok := range []string{"http://localhost:5173", "http://127.0.0.1:3000", "https://app.example", "https://app.example:8443", "http://[::1]:8080"} {
		if err := checkOrigin(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"*", "null", "", "localhost:5173", "http://evil.example", "https://app.example/", "https://app.example/path",
		"https://App.example", "https://app.example:443", "http://localhost:80", "https://u:p@app.example", "https://app.example?x", "ftp://app.example", "https://app.example#f"} {
		if err := checkOrigin(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := ParseConfig([]byte(strings.Replace(validConfig(), `"clients"`, `"allowed_origins":["*"],"clients"`, 1))); err == nil ||
		!strings.Contains(err.Error(), "allowed_origins[0]") {
		t.Fatalf("wildcard origin in config: %v", err)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	h := newHarness(t, jsonResponse(200, oaiCompletion), unused)
	h.gw.slots = make(chan struct{}, 1)
	h.gw.slots <- struct{}{} // the only slot is taken
	rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, chatBody)
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	<-h.gw.slots
	if rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, chatBody); rec.Code != 200 {
		t.Fatalf("after release: %d", rec.Code)
	}
	if len(h.gw.slots) != 0 {
		t.Fatal("slot not released")
	}
}

func TestPassthroughNeverReflectsUpstreamContentType(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(502)
		_, _ = io.WriteString(w, "<script>alert(1)</script>")
	}, unused)
	rec := h.do(t, http.MethodPost, "/v1/chat/completions", caidoKey, chatBody)
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%v", rec.Header())
	}
}

func TestAnthropicHeaderSanitizing(t *testing.T) {
	if got := anthropicBetas([]string{"a-1, b.2", "bad value", "A-1", "x;y", "c\x00"}); got != "a-1,b.2" {
		t.Fatalf("betas %q", got)
	}
	h := newHarness(t, unused, jsonResponse(200, antMessageBody))
	h.do(t, http.MethodPost, "/v1/messages", "", `{"model":"sonnet","max_tokens":10,"messages":[]}`,
		"X-Api-Key", caidoKey, "Anthropic-Version", "latest; drop", "Anthropic-Beta", "ok-1, not ok")
	up := h.ant.last(t)
	if up.Header.Get("Anthropic-Version") != DefaultAnthropicVersion || up.Header.Get("Anthropic-Beta") != "ok-1" {
		t.Fatalf("headers %v", up.Header)
	}
}
