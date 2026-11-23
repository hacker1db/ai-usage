package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBoundedRequestLogs(t *testing.T) {
	h := newHarness(t, nil, nil)
	huge := strings.Repeat("x", 60000)
	for range 10 {
		h.do(t, "GET", "/"+huge, "", "")
	}
	if got := h.logBuf.raw(); got != "" {
		t.Fatalf("unauthenticated INFO log: %d bytes", len(got))
	}
	h.do(t, "POST", "/v1/chat/completions", caidoKey, `{"model":"`+huge+`"}`)
	var entry map[string]any
	if err := json.Unmarshal([]byte(h.logBuf.raw()), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["level"] != "INFO" || len(entry["model"].(string)) > 256 {
		t.Fatal(entry)
	}
	var buf bytes.Buffer
	h.gw.log = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := &call{id: huge, client: huge, model: huge, provider: huge, upstreamModel: huge, upstreamReqID: huge}
	req := httptest.NewRequest("GET", "/"+huge, nil)
	req.Method = huge
	h.gw.logCall(req, c, &recorder{ResponseWriter: httptest.NewRecorder(), status: 200}, time.Now())
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	for k, v := range entry {
		if s, ok := v.(string); ok && k != "time" && len(s) > 256 {
			t.Fatalf("unbounded %s", k)
		}
	}
}

func TestDefaultBodyLimit(t *testing.T) {
	h := newHarness(t, nil, nil)
	if h.gw.cfg.MaxRequestBytes != 4<<20 {
		t.Fatal(h.gw.cfg.MaxRequestBytes)
	}
	rec := h.do(t, "POST", "/v1/chat/completions", caidoKey, strings.Repeat(" ", (4<<20)+1))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatal(rec.Code)
	}
}

func TestCredentialEchoErrors(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, route := range []struct{ path, model string }{
			{"/v1/chat/completions", "gpt-fast"}, {"/v1/messages", "sonnet"},
			{"/v1/messages", "gpt-fast"}, {"/v1/chat/completions", "sonnet"},
		} {
			t.Run(fmt.Sprintf("%v%s%s", stream, route.path, route.model), func(t *testing.T) {
				handler := func(w http.ResponseWriter, r *http.Request) {
					secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
					if secret == "" {
						secret = r.Header.Get("X-Api-Key")
					}

					msg := secret + strings.Repeat("!", 2000)
					raw, _ := json.Marshal(map[string]any{"type": "error", "error": apiError{Message: msg}})
					raw = bytes.ReplaceAll(raw, []byte("-"), []byte(`\u002d`))
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", raw)
					} else {
						w.WriteHeader(401)
						_, _ = w.Write(raw)
					}
				}
				h := newHarness(t, handler, handler)
				rec := h.do(t, "POST", route.path, caidoKey, fmt.Sprintf(`{"model":%q,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, route.model, stream))
				got := rec.Body.String()
				if strings.Contains(got, "entra-token") || strings.Contains(got, "sk-ant-upstream") || !strings.Contains(got, "[REDACTED]") || len(got) > 2200 {
					t.Fatalf("unsafe response (%d): %.150s", len(got), got)
				}
			})
		}
	}
	c := &call{credentials: []string{"old-token", "new-token"}}
	if got := c.safeError("old-token new-token"); got != "[REDACTED] [REDACTED]" {
		t.Fatal(got)
	}
	if got := c.safeError(strings.Repeat("x", 2000)); len(got) != maxErrorMessage {
		t.Fatal(len(got))
	}
	if got := clipLog(strings.Repeat("界", 200)); len(got) > 256 || !strings.HasSuffix(got, "...") {
		t.Fatal(got)
	}
}

type shortDeadlineWriter struct {
	http.ResponseWriter
	t *testing.T
}

func (w shortDeadlineWriter) SetReadDeadline(d time.Time) error {
	if !d.IsZero() {
		if left := time.Until(d); left < 29*time.Second || left > 31*time.Second {
			w.t.Errorf("read window %s", left)
		}
		d = time.Now().Add(50 * time.Millisecond)
	}
	return http.NewResponseController(w.ResponseWriter).SetReadDeadline(d)
}
func (w shortDeadlineWriter) SetWriteDeadline(d time.Time) error {
	if left := time.Until(d); left < 29*time.Second || left > 31*time.Second {
		w.t.Errorf("write window %s", left)
	}
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
}
func (w shortDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestStalledBodyReleasesSlot(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.gw.slots = make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.gw.ServeHTTP(shortDeadlineWriter{w, t}, r)
		done <- struct{}{}
	}))
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Type: application/json\r\nAuthorization: Bearer %s\r\nContent-Length: 1000\r\n\r\n", caidoKey)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("body pinned slot")
	}
	if len(h.gw.slots) != 0 {
		t.Fatal("slot not released")
	}
	h.gw.slots <- struct{}{}
	if rec := h.do(t, "GET", "/healthz", "", ""); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	<-h.gw.slots
}

func TestStalledReaderReleasesSlot(t *testing.T) {
	h := newHarness(t, jsonResponse(200, strings.Repeat("x", 8<<20)), nil)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.gw.ServeHTTP(shortDeadlineWriter{w, t}, r)
		close(done)
	}))
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := `{"model":"gpt-fast"}`
	_, err = fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Type: application/json\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s", caidoKey, len(body), body)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reader pinned slot")
	}
	if len(h.gw.slots) != 0 {
		t.Fatal("slot not released")
	}
}

func TestStreamingWriteWindowRenews(t *testing.T) {
	h := newHarness(t, sseResponse("data: {\"choices\":[]}\n\n", "data: [DONE]\n\n"), nil)
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest("POST", "http://127.0.0.1/v1/chat/completions", strings.NewReader(`{"model":"gpt-fast","stream":true}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer "+caidoKey)
	req.Header.Set("Content-Type", "application/json")
	h.gw.ServeHTTP(w, req)
	if w.writes < 4 {
		t.Fatal("stream did not renew deadlines", w.writes)
	}
	if w.reads != 2 {
		t.Fatal("read deadline not set and cleared", w.reads)
	}

	if err := readSSE(bufio.NewReader(w.Body), func(_, data []byte) bool { return true }); err != nil {
		t.Fatal(err)
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	reads, writes int
}

func (w *deadlineRecorder) SetReadDeadline(time.Time) error  { w.reads++; return nil }
func (w *deadlineRecorder) SetWriteDeadline(time.Time) error { w.writes++; return nil }
