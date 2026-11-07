// Package proxy forwards authenticated loopback requests to Azure AI Foundry with an Entra bearer token.
package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"foundry-entra-proxy/internal/token"
)

const (
	apiPrefix        = "/openai/v1/"
	readyzTimeout    = 35 * time.Second
	headerProxyError = "X-Proxy-Error"
	headerCorrelate  = "X-Correlation-ID"
)

var strippedRequestHeaders = []string{
	"Authorization",
	"Api-Key",
	"X-Api-Key",
	"Proxy-Authorization",
	"Cookie",
}

type Options struct {
	Upstream           *url.URL
	ClientKey          string
	AllowedDeployments []string
	Tokens             token.Source
	Transport          http.RoundTripper
	StreamIdleTimeout  time.Duration
	MaxRequestBytes    int64
	Logger             *slog.Logger
}

type Proxy struct {
	opts    Options
	reverse *httputil.ReverseProxy
	log     *slog.Logger
}

type requestInfo struct {
	id                string
	model             string
	upstreamRequestID string
	retried           bool
	idleTimedOut      atomic.Bool
	usage             usage
}

type infoKey struct{}

func infoFrom(ctx context.Context) *requestInfo {
	info, _ := ctx.Value(infoKey{}).(*requestInfo)
	return info
}

func New(opts Options) *Proxy {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Transport == nil {
		opts.Transport = http.DefaultTransport
	}
	p := &Proxy{opts: opts, log: opts.Logger}
	p.reverse = &httputil.ReverseProxy{
		Rewrite:        p.rewrite,
		Transport:      &authTransport{base: opts.Transport, tokens: opts.Tokens, idle: opts.StreamIdleTimeout, log: opts.Logger},
		FlushInterval:  -1,
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.handleUpstreamError,
		ErrorLog:       slog.NewLogLogger(opts.Logger.Handler(), slog.LevelDebug),
	}
	return p
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	info := &requestInfo{id: newID()}
	w.Header().Set(headerCorrelate, info.id)
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()

	defer func() {
		level := slog.LevelInfo
		if rec.status >= 500 || info.idleTimedOut.Load() {
			level = slog.LevelWarn
		}
		attrs := []any{
			"id", info.id,
			"method", r.Method,
			"path", r.URL.Path,
			"model", info.model,
			"status", rec.status,
			"proxy_error", rec.Header().Get(headerProxyError),
			"upstream_request_id", info.upstreamRequestID,
			"auth_retried", info.retried,
			"stream_idle_timeout", info.idleTimedOut.Load(),
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"client", clientName(r.UserAgent()),
		}
		p.log.Log(r.Context(), level, "request", append(attrs, info.usage.logAttrs()...)...)
	}()

	if !isLoopback(r.RemoteAddr) {
		writeError(rec, http.StatusForbidden, "non_loopback", "proxy only accepts loopback traffic")
		return
	}

	switch r.URL.Path {
	case "/healthz":
		p.healthz(rec, r)
		return
	case "/readyz":
		p.readyz(rec, r)
		return
	}

	if !p.authorized(r) {
		writeError(rec, http.StatusUnauthorized, "client_unauthorized", "missing or invalid proxy client key")
		return
	}

	rule, ok := matchRoute(r)
	if !ok {
		writeError(rec, http.StatusForbidden, "route_not_allowed", "route is not on the proxy allowlist")
		return
	}

	var body []byte
	if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodDelete {
		var err error
		body, err = io.ReadAll(http.MaxBytesReader(rec, r.Body, p.opts.MaxRequestBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(rec, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds proxy limit")
				return
			}
			writeError(rec, http.StatusBadRequest, "request_unreadable", "could not read request body")
			return
		}
	}

	model := rule.pathModel
	if model == "" && len(body) > 0 {
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &payload); err != nil && rule.requiresModel {
			writeError(rec, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
			return
		}
		model = payload.Model
	}
	info.model = model
	if model == "" && rule.requiresModel {
		writeError(rec, http.StatusBadRequest, "model_required", "request must name a model (Foundry deployment)")
		return
	}
	if model != "" && !slices.Contains(p.opts.AllowedDeployments, model) {
		writeError(rec, http.StatusForbidden, "deployment_not_allowed", "model is not on the proxy deployment allowlist")
		return
	}

	out := r.WithContext(context.WithValue(r.Context(), infoKey{}, info))
	if body != nil {
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.ContentLength = int64(len(body))
		out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	} else {
		out.Body = http.NoBody
		out.ContentLength = 0
		out.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
	}
	p.reverse.ServeHTTP(rec, out)
}

func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	pr.Out.URL.Scheme = p.opts.Upstream.Scheme
	pr.Out.URL.Host = p.opts.Upstream.Host
	pr.Out.URL.Path = pr.In.URL.Path
	pr.Out.URL.RawPath = ""
	pr.Out.URL.RawQuery = pr.In.URL.RawQuery
	pr.Out.Host = p.opts.Upstream.Host
	for _, h := range strippedRequestHeaders {
		pr.Out.Header.Del(h)
	}
	pr.Out.Header.Del(headerCorrelate)
	// Let the transport negotiate and decode compression so usage can be read from the body;
	// the hop to the client is loopback, where compression buys nothing.
	pr.Out.Header.Del("Accept-Encoding")
	if info := infoFrom(pr.In.Context()); info != nil {
		pr.Out.Header.Set("X-Ms-Client-Request-Id", info.id)
	}
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	if info := infoFrom(resp.Request.Context()); info != nil {
		info.upstreamRequestID = firstNonEmpty(resp.Header.Get("Apim-Request-Id"), resp.Header.Get("X-Request-Id"))
		resp.Body = newUsageBody(resp, &info.usage)
	}
	return nil
}

func (p *Proxy) handleUpstreamError(w http.ResponseWriter, r *http.Request, err error) {
	var tokErr *tokenError
	switch {
	case errors.Is(err, token.ErrLoginRequired):
		p.log.Error("entra token unavailable: az login required", "error", err)
		writeError(w, http.StatusServiceUnavailable, "az_login_required", "Azure CLI session missing or expired; run `az login` and retry")
	case errors.As(err, &tokErr):
		p.log.Error("entra token unavailable", "error", err)
		writeError(w, http.StatusServiceUnavailable, "token_unavailable", "proxy could not acquire an Entra token; check `az account get-access-token`")
	case r.Context().Err() != nil:
		if rec, ok := w.(*recorder); ok {
			rec.status = 499
		}
	case isTimeout(err):
		p.log.Warn("upstream timeout", "error", err)
		writeError(w, http.StatusGatewayTimeout, "upstream_timeout", "Foundry did not respond in time")
	default:
		p.log.Warn("upstream unreachable", "error", err)
		writeError(w, http.StatusBadGateway, "upstream_unreachable", "proxy could not reach Foundry")
	}
}

func (p *Proxy) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (p *Proxy) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
	defer cancel()
	if _, err := p.opts.Tokens.Token(ctx); err != nil {
		code, msg := "token_unavailable", "proxy could not acquire an Entra token"
		if errors.Is(err, token.ErrLoginRequired) {
			code, msg = "az_login_required", "Azure CLI session missing or expired; run `az login`"
		}
		p.log.Warn("readiness check failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, code, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (p *Proxy) authorized(r *http.Request) bool {
	want := []byte(p.opts.ClientKey)
	match := func(got string) bool {
		return got != "" && subtle.ConstantTimeCompare([]byte(got), want) == 1
	}
	for _, v := range r.Header.Values("Authorization") {
		scheme, cred, ok := strings.Cut(strings.TrimSpace(v), " ")
		if ok && strings.EqualFold(scheme, "Bearer") && match(strings.TrimSpace(cred)) {
			return true
		}
	}
	for _, h := range []string{"Api-Key", "X-Api-Key"} {
		for _, v := range r.Header.Values(h) {
			if match(strings.TrimSpace(v)) {
				return true
			}
		}
	}
	return false
}

type routeRule struct {
	requiresModel bool
	pathModel     string
}

// matchRoute is the route allowlist: only the OpenAI v1 surfaces Codex and CLIProxyAPI use.
func matchRoute(r *http.Request) (routeRule, bool) {
	p := r.URL.Path
	if r.URL.RawPath != "" || path.Clean(p) != p || !strings.HasPrefix(p, apiPrefix) {
		return routeRule{}, false
	}
	seg := strings.Split(strings.TrimPrefix(p, apiPrefix), "/")
	m := r.Method

	switch {
	case m == http.MethodPost && len(seg) == 1 && (seg[0] == "responses" || seg[0] == "embeddings"):
		return routeRule{requiresModel: true}, true
	case m == http.MethodPost && len(seg) == 2 && seg[0] == "chat" && seg[1] == "completions":
		return routeRule{requiresModel: true}, true
	case m == http.MethodPost && len(seg) == 2 && seg[0] == "responses" && (seg[1] == "compact" || seg[1] == "input_tokens"):
		return routeRule{requiresModel: true}, true
	case m == http.MethodGet && len(seg) == 1 && seg[0] == "models":
		return routeRule{}, true
	case m == http.MethodGet && len(seg) == 2 && seg[0] == "models":
		return routeRule{pathModel: seg[1]}, true
	case (m == http.MethodGet || m == http.MethodDelete) && len(seg) == 2 && seg[0] == "responses":
		return routeRule{}, true
	case m == http.MethodGet && len(seg) == 3 && seg[0] == "responses" && seg[2] == "input_items":
		return routeRule{}, true
	case m == http.MethodPost && len(seg) == 3 && seg[0] == "responses" && seg[2] == "cancel":
		return routeRule{}, true
	}
	return routeRule{}, false
}

type tokenError struct{ err error }

func (e *tokenError) Error() string { return "entra token: " + e.err.Error() }
func (e *tokenError) Unwrap() error { return e.err }

// authTransport injects the Entra bearer, retries once on upstream 401, and bounds idle streams.
type authTransport struct {
	base   http.RoundTripper
	tokens token.Source
	idle   time.Duration
	log    *slog.Logger
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.attempt(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}

	resp.Body.Close()
	t.tokens.Invalidate()
	info := infoFrom(req.Context())
	if info != nil {
		info.retried = true
	}

	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		if retry.Body, err = req.GetBody(); err != nil {
			return nil, err
		}
	}
	resp, err = t.attempt(retry)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		t.log.Error("Foundry rejected a freshly acquired Entra token; check RBAC on the resource and FEP_TOKEN_SCOPE")
		resp.Header.Set(headerProxyError, "upstream_unauthorized")
	}
	return resp, err
}

func (t *authTransport) attempt(req *http.Request) (*http.Response, error) {
	tok, err := t.tokens.Token(req.Context())
	if err != nil {
		return nil, &tokenError{err: err}
	}

	ctx, cancel := context.WithCancel(req.Context())
	out := req.Clone(ctx)
	out.Header.Set("Authorization", "Bearer "+tok)

	resp, err := t.base.RoundTrip(out)
	if err != nil {
		cancel()
		return nil, err
	}
	if t.idle > 0 {
		resp.Body = newIdleBody(resp.Body, t.idle, cancel, infoFrom(req.Context()))
	} else {
		resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	}
	return resp, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// idleBody cancels the upstream request when no bytes arrive within the idle window.
type idleBody struct {
	io.ReadCloser
	idle   time.Duration
	timer  *time.Timer
	cancel context.CancelFunc
}

func newIdleBody(rc io.ReadCloser, idle time.Duration, cancel context.CancelFunc, info *requestInfo) *idleBody {
	b := &idleBody{ReadCloser: rc, idle: idle, cancel: cancel}
	b.timer = time.AfterFunc(idle, func() {
		if info != nil {
			info.idleTimedOut.Store(true)
		}
		cancel()
	})
	return b
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(b.idle)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set(headerProxyError, code)
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": msg, "type": "proxy_error", "code": code},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// clientName keeps the product token of a User-Agent (for example "codex_cli_rs") so usage can be
// attributed per tool without logging the full header.
func clientName(ua string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(ua), " ")
	name, _, _ = strings.Cut(name, "/")
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
