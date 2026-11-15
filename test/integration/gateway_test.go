//go:build integration

// Package integration runs the real ai-gateway and ai-usage binaries end to end: fake OpenAI and
// Anthropic upstreams on loopback, the gateway as a subprocess on a real TCP port, real HTTP
// clients (and, when test/sdk has node_modules, the official OpenAI and Anthropic SDKs), then
// `ai-usage report` over the gateway's log to prove the usage reaches the cost report.
//
// Run with: go test -tags integration -v ./test/integration/
package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func build(t *testing.T, root, dir, pkg string) string {
	t.Helper()
	out := filepath.Join(dir, filepath.Base(pkg))
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
	return out
}

// upstreamLog records what each fake upstream received.
type upstreamLog struct {
	mu   sync.Mutex
	hits []string
}

func (u *upstreamLog) add(s string) {
	u.mu.Lock()
	u.hits = append(u.hits, s)
	u.mu.Unlock()
}

func fakeOpenAI(t *testing.T, log *upstreamLog) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.Header.Get("Authorization") != "Bearer upstream-openai-token" {
			http.Error(w, `{"error":{"message":"bad upstream token"}}`, http.StatusUnauthorized)
			return
		}
		log.add(fmt.Sprintf("openai %s model=%v stream=%v", r.URL.Path, req["model"], req["stream"]))
		if req["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, part := range []string{"Hello", " from", " Foundry"} {
				fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-it\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", req["model"], part)
				w.(http.Flusher).Flush()
			}
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-it\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", req["model"])
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-it\",\"object\":\"chat.completion.chunk\",\"created\":1,\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":200,\"total_tokens\":1200,\"prompt_tokens_details\":{\"cached_tokens\":400}}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl-it","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":200,"total_tokens":1200}}`, req["model"])
	}))
}

func fakeAnthropic(t *testing.T, log *upstreamLog) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.Header.Get("X-Api-Key") != "sk-ant-upstream" || r.Header.Get("Anthropic-Version") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`)
			return
		}
		log.add(fmt.Sprintf("anthropic %s model=%v stream=%v", r.URL.Path, req["model"], req["stream"]))
		if req["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			ev := func(name, data string) {
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
				w.(http.Flusher).Flush()
			}
			ev("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_it","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":300,"cache_read_input_tokens":700,"output_tokens":1}}}`, req["model"]))
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello from Claude"}}`)
			ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
			ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":150}}`)
			ev("message_stop", `{"type":"message_stop"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"msg_it","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":300,"output_tokens":150,"cache_read_input_tokens":700}}`, req["model"])
	}))
}

type gatewayProc struct {
	cmd  *exec.Cmd
	base string
	key  string
	log  string
}

func startGateway(t *testing.T, bin, dir, openaiURL, anthropicURL string) *gatewayProc {
	t.Helper()
	keyFile := filepath.Join(dir, "caido.key")
	out, err := exec.Command(bin, "keygen", "-name", "caido", "-key-file", keyFile).CombinedOutput()
	if err != nil {
		t.Fatalf("keygen: %v\n%s", err, out)
	}
	var hash string
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.Index(line, `"key_sha256": "`); i >= 0 {
			hash = strings.TrimSuffix(line[i+len(`"key_sha256": "`):], `"}`)
		}
	}
	keyRaw, err := os.ReadFile(keyFile)
	if err != nil || len(hash) != 64 {
		t.Fatalf("keygen output: %v %q\n%s", err, hash, out)
	}
	if info, _ := os.Stat(keyFile); info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", info.Mode().Perm())
	}

	logPath := filepath.Join(dir, "gateway.log")
	cfg := fmt.Sprintf(`{
		"listen": "127.0.0.1:0",
		"log_file": %q,
		"clients": [{"name": "caido", "key_sha256": %q}],
		"providers": {
			"work-foundry": {"kind": "openai", "base_url": %q, "auth": {"type": "env", "env": "IT_OPENAI_TOKEN"}},
			"anthropic-api": {"kind": "anthropic", "base_url": %q, "auth": {"type": "file", "file": %q, "header": "x-api-key"}}
		},
		"models": {
			"gpt-6-luna": {"provider": "work-foundry"},
			"claude-sonnet": {"provider": "anthropic-api", "model": "claude-sonnet-5-5"}
		}
	}`, logPath, hash, openaiURL+"/openai/v1", anthropicURL, filepath.Join(dir, "anthropic.key"))
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "anthropic.key"), []byte("sk-ant-upstream\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "IT_OPENAI_TOKEN=upstream-openai-token")

	check := exec.Command(bin, "check", "-config", cfgPath)
	check.Env = env
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("check: %v\n%s", err, out)
	} else {
		t.Logf("ai-gateway check:\n%s", out)
	}

	cmd := exec.Command(bin, "-config", cfgPath)
	cmd.Env = env
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "ai-gateway listening on "); ok {
				addr <- v
			}
		}
	}()
	select {
	case base := <-addr:
		return &gatewayProc{cmd: cmd, base: base, key: strings.TrimSpace(string(keyRaw)), log: logPath}
	case <-time.After(15 * time.Second):
		t.Fatal("gateway did not report its address")
	}
	return nil
}

// post sends body and returns the status and the whole response body (the response is closed).
func (g *gatewayProc) post(t *testing.T, path string, body string, hdr ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, g.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestGatewayEndToEnd(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	gwBin := build(t, root, dir, "./cmd/ai-gateway")
	usageBin := build(t, root, dir, "./cmd/ai-usage")

	var hits upstreamLog
	oai := fakeOpenAI(t, &hits)
	defer oai.Close()
	ant := fakeAnthropic(t, &hits)
	defer ant.Close()

	gw := startGateway(t, gwBin, dir, oai.URL, ant.URL)
	bearer := []string{"Authorization", "Bearer " + gw.key}

	t.Run("health and models", func(t *testing.T) {
		resp, err := http.Get(gw.base + "/healthz")
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("healthz: %v %v", err, resp)
		}
		resp.Body.Close()
		req, _ := http.NewRequest(http.MethodGet, gw.base+"/v1/models", nil)
		req.Header.Set("X-Api-Key", gw.key)
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), `"claude-sonnet"`) || !strings.Contains(string(b), `"gpt-6-luna"`) {
			t.Fatalf("models: %s", b)
		}
	})

	t.Run("rejects a wrong key", func(t *testing.T) {
		status, _ := gw.post(t, "/v1/chat/completions", `{"model":"gpt-6-luna","messages":[]}`, "Authorization", "Bearer aigw_wrong")
		if status != 401 {
			t.Fatalf("status %d", status)
		}
	})

	t.Run("openai client to openai upstream", func(t *testing.T) {
		status, body := gw.post(t, "/v1/chat/completions", `{"model":"gpt-6-luna","messages":[{"role":"user","content":"ping"}]}`, bearer...)
		if status != 200 || !strings.Contains(body, "pong") {
			t.Fatalf("%d %s", status, body)
		}
		status, body = gw.post(t, "/v1/chat/completions", `{"model":"gpt-6-luna","stream":true,"messages":[{"role":"user","content":"ping"}]}`, bearer...)
		if status != 200 || !strings.Contains(body, "Foundry") || strings.Contains(body, `"usage"`) {
			t.Fatalf("stream %d %s", status, body)
		}
	})

	t.Run("anthropic client to openai upstream (translated stream)", func(t *testing.T) {
		status, body := gw.post(t, "/v1/messages", `{"model":"gpt-6-luna","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"ping"}]}`,
			"X-Api-Key", gw.key, "Anthropic-Version", "2023-06-01")
		if status != 200 || !strings.Contains(body, "event: message_stop") || !strings.Contains(body, `"text":" Foundry"`) {
			t.Fatalf("%d %s", status, body)
		}
	})

	t.Run("openai client to anthropic upstream (translated)", func(t *testing.T) {
		status, body := gw.post(t, "/v1/chat/completions", `{"model":"claude-sonnet","messages":[{"role":"user","content":"ping"}]}`, bearer...)
		if status != 200 || !strings.Contains(body, `"content":"pong"`) || !strings.Contains(body, `"prompt_tokens":1000`) {
			t.Fatalf("%d %s", status, body)
		}
		status, body = gw.post(t, "/v1/chat/completions", `{"model":"claude-sonnet","stream":true,"messages":[{"role":"user","content":"ping"}]}`, bearer...)
		if status != 200 || !strings.Contains(body, "Hello from Claude") || !strings.HasSuffix(body, "data: [DONE]\n\n") {
			t.Fatalf("stream %d %s", status, body)
		}
	})

	t.Run("official SDKs", func(t *testing.T) {
		sdkDir := filepath.Join(root, "test", "sdk")
		if _, err := os.Stat(filepath.Join(sdkDir, "node_modules")); err != nil {
			t.Skip("test/sdk/node_modules missing; run `npm ci --prefix test/sdk` to include the SDK compatibility check")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "node", "compat.mjs")
		cmd.Dir = sdkDir
		cmd.Env = append(os.Environ(), "GATEWAY_URL="+gw.base, "GATEWAY_KEY="+gw.key)
		out, err := cmd.CombinedOutput()
		t.Logf("SDK compatibility:\n%s", out)
		if err != nil {
			t.Fatalf("sdk compat: %v", err)
		}
	})

	// Stop the gateway so its log is complete, then check the log and the report.
	_ = gw.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- gw.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("gateway exited uncleanly on SIGTERM: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("gateway did not stop on SIGTERM")
	}

	t.Run("log is costable and holds no secrets", func(t *testing.T) {
		raw, err := os.ReadFile(gw.log)
		if err != nil {
			t.Fatal(err)
		}
		if info, _ := os.Stat(gw.log); info.Mode().Perm() != 0o600 {
			t.Fatalf("log mode %v", info.Mode().Perm())
		}
		for _, secret := range []string{gw.key, "upstream-openai-token", "sk-ant-upstream"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatalf("secret %q found in log", secret[:6])
			}
		}
		if n := bytes.Count(raw, []byte(`"msg":"request"`)); n < 6 {
			t.Fatalf("only %d request lines:\n%s", n, raw)
		}
	})

	t.Run("ai-usage report counts gateway usage", func(t *testing.T) {
		home := t.TempDir()
		missing := filepath.Join(home, "missing")
		cmd := exec.Command(usageBin, "report", "-print", "-days", "1", "-out", filepath.Join(home, "reports"),
			"-gateway-log", gw.log, "-proxy-log", missing, "-codex-home", missing, "-copilot-db", missing, "-claude", missing+"=none")
		cmd.Env = append(os.Environ(), "HOME="+home, "CODEX_HOME="+missing)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("report: %v\n%s", err, out)
		}
		report := string(out)
		t.Logf("ai-usage report:\n%s", report)
		for _, want := range []string{"ai-gateway", "work-foundry", "anthropic-api", "gpt-6-luna", "claude-sonnet-5-5"} {
			if !strings.Contains(report, want) {
				t.Errorf("report is missing %q", want)
			}
		}
	})

	hits.mu.Lock()
	t.Logf("upstream calls:\n  %s", strings.Join(hits.hits, "\n  "))
	hits.mu.Unlock()
}
