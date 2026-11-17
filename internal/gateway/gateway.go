// Package gateway is a loopback API gateway: local tools (Caido, scripts, editors) call it with a
// gateway-issued key in either the OpenAI Chat Completions or the Anthropic Messages format, and it
// forwards to configured upstream accounts (Azure AI Foundry via Entra, GitHub Models via `gh auth
// token`, Anthropic or OpenAI API keys, another local proxy), translating between the two formats
// when the caller and the upstream differ. Every call is logged as a proxylog "request" line so
// ai-usage can count tokens and estimate cost.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"ai-usage/internal/fep/token"
	"ai-usage/internal/proxylog"
)

const (
	headerGatewayError = "X-Gateway-Error"
	headerRequestID    = "X-Gateway-Request-Id"

	// maxUpstreamBody bounds a buffered (non-streamed) upstream response.
	maxUpstreamBody = 64 << 20
	// maxErrorBody bounds what is read from an upstream error response.
	maxErrorBody    = 1 << 20
	clientIOTimeout = 30 * time.Second
	maxErrorMessage = 1024
)

// Options configures a Gateway. Getenv and Runner are injectable for tests.
type Options struct {
	Config    *Config
	Transport http.RoundTripper
	Logger    *slog.Logger
	Version   string
	Getenv    func(string) string
	Runner    token.Runner
}

// Gateway is the HTTP handler.
type Gateway struct {
	cfg       *Config
	log       *slog.Logger
	transport http.RoundTripper
	version   string
	providers map[string]*upstream
	slots     chan struct{}
}

type upstream struct {
	name string
	cfg  *Provider
	cred credential
}

// New builds a Gateway from a validated config.
func New(opts Options) *Gateway {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Transport == nil {
		opts.Transport = http.DefaultTransport
	}
	if opts.Getenv == nil {
		opts.Getenv = func(string) string { return "" }
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	g := &Gateway{cfg: opts.Config, log: opts.Logger, transport: opts.Transport, version: opts.Version, providers: map[string]*upstream{}, slots: make(chan struct{}, opts.Config.MaxConcurrentRequests)}
	for name, p := range opts.Config.Providers {
		g.providers[name] = &upstream{name: name, cfg: p, cred: newCredential(p.Auth, opts.Getenv, opts.Runner)}
	}
	return g
}

// format is the API dialect of the inbound request.
type format string

const (
	formatOpenAI    format = kindOpenAI
	formatAnthropic format = kindAnthropic
)

// call is the per-request state that ends up in the log line.
type call struct {
	credentials    []string // Tokens used by this call, including the pre-refresh token.
	id             string
	format         format
	client         string
	model          string
	provider       string
	upstreamModel  string
	translated     bool
	stream         bool
	upstreamStatus int
	upstreamReqID  string
	authRetried    bool
	idleTimedOut   atomic.Bool
	usage          tokens
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := &call{id: newID(), format: formatFor(r.URL.Path)}
	w.Header().Set(headerRequestID, c.id)
	setSecurityHeaders(w.Header())
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	defer func() { g.logCall(r, c, rec, start) }()

	if !isLoopbackAddr(r.RemoteAddr) {
		g.fail(rec, c, http.StatusForbidden, "non_loopback", "the gateway only accepts loopback traffic")
		return
	}
	if !g.browserGuard(rec, r, c) {
		return
	}
	if r.URL.Path == "/healthz" {
		writeJSON(rec, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	default:
		rec.Header().Set("Retry-After", "1")
		g.fail(rec, c, http.StatusServiceUnavailable, "too_many_requests", "the gateway is at its concurrent request limit")
		return
	}

	route, ok := matchRoute(r)
	if !ok {
		g.fail(rec, c, http.StatusNotFound, "route_not_found", "unknown route; use /v1/chat/completions, /v1/messages, or /v1/models")
		return
	}
	client := g.cfg.authenticate(r)
	if client == nil {
		g.fail(rec, c, http.StatusUnauthorized, "client_unauthorized", "missing or invalid gateway API key")
		return
	}
	c.client = client.Name

	if route == routeModels {
		g.listModels(rec, client)
		return
	}

	rc := http.NewResponseController(rec)
	_ = rc.SetReadDeadline(time.Now().Add(clientIOTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(rec, r.Body, g.cfg.MaxRequestBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			g.fail(rec, c, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the gateway limit")
			return
		}
		g.fail(rec, c, http.StatusBadRequest, "request_unreadable", "could not read the request body")
		return
	}
	_ = rc.SetReadDeadline(time.Time{})
	doc, err := decodeObject(body)
	if err != nil {
		g.fail(rec, c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	alias, err := modelOf(doc)
	if err != nil {
		g.fail(rec, c, http.StatusBadRequest, "invalid_model", err.Error())
		return
	}
	alias = g.cfg.resolveAlias(alias)
	c.model = alias
	m, ok := g.cfg.Models[alias]
	if !ok || (len(client.Models) > 0 && !slices.Contains(client.Models, alias)) {
		g.fail(rec, c, http.StatusNotFound, "model_not_found", "model is not available to this API key; GET /v1/models lists the ones that are")
		return
	}
	up := g.providers[m.Provider]
	c.provider, c.upstreamModel = up.name, m.Model
	c.stream = boolField(doc, "stream")

	switch {
	case route == routeCountTokens && up.cfg.Kind != kindAnthropic:
		// No OpenAI equivalent; Claude Code only uses this for context budgeting.
		writeJSON(rec, http.StatusOK, map[string]int{"input_tokens": len(body) / 4})
	case string(c.format) == up.cfg.Kind:
		g.passthrough(rec, r, c, up, route, doc)
	case c.format == formatAnthropic:
		c.translated = true
		g.anthropicViaOpenAI(rec, r, c, up, body)
	default:
		c.translated = true
		g.openAIViaAnthropic(rec, r, c, up, body)
	}
}

type routeID int

const (
	routeModels routeID = iota + 1
	routeChat
	routeMessages
	routeCountTokens
)

func formatFor(path string) format {
	if strings.Contains(path, "/messages") {
		return formatAnthropic
	}
	return formatOpenAI
}

// matchRoute accepts the paths with or without the /v1 prefix, since clients differ on whether the
// configured base URL already ends in /v1.
func matchRoute(r *http.Request) (routeID, bool) {
	p := strings.TrimPrefix(r.URL.Path, "/v1")
	switch {
	case r.Method == http.MethodGet && p == "/models":
		return routeModels, true
	case r.Method == http.MethodPost && p == "/chat/completions":
		return routeChat, true
	case r.Method == http.MethodPost && p == "/messages":
		return routeMessages, true
	case r.Method == http.MethodPost && p == "/messages/count_tokens":
		return routeCountTokens, true
	}
	return 0, false
}

func (g *Gateway) listModels(w http.ResponseWriter, client *Client) {
	type entry struct {
		ID          string `json:"id"`
		Object      string `json:"object"`
		Type        string `json:"type"`
		DisplayName string `json:"display_name"`
		OwnedBy     string `json:"owned_by"`
		Created     int64  `json:"created"`
		CreatedAt   string `json:"created_at"`
	}
	// One body serves both dialects: OpenAI clients read object/owned_by, Anthropic clients
	// read type/display_name/has_more.
	data := []entry{}
	for _, alias := range g.cfg.modelsFor(client) {
		data = append(data, entry{
			ID: alias, Object: "model", Type: "model", DisplayName: alias,
			OwnedBy: g.cfg.Models[alias].Provider, CreatedAt: "1970-01-01T00:00:00Z",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": false})
}

// ---- request body helpers ----

func decodeObject(body []byte) (map[string]json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil || doc == nil {
		return nil, errors.New("request body must be a JSON object")
	}
	return doc, nil
}

// modelOf reads the exact top-level "model" key. A key that only matches "model" ignoring case is
// refused: case-insensitive upstream parsers could read it instead of the checked value.
func modelOf(doc map[string]json.RawMessage) (string, error) {
	for k := range doc {
		if k != "model" && strings.EqualFold(k, "model") {
			return "", errors.New("request body has a wrongly cased model key")
		}
	}
	raw, ok := doc["model"]
	if !ok {
		return "", errors.New("request must name a model")
	}
	var m string
	if err := json.Unmarshal(raw, &m); err != nil || strings.TrimSpace(m) == "" {
		return "", errors.New("model must be a non-empty string")
	}
	return m, nil
}

func boolField(doc map[string]json.RawMessage, key string) bool {
	var v bool
	_ = json.Unmarshal(doc[key], &v)
	return v
}

// ---- upstream calls ----

// send posts body to the provider path with the upstream credential, retrying once with a fresh
// credential when the upstream answers 401.
func (g *Gateway) send(ctx context.Context, c *call, up *upstream, path string, body []byte, in http.Header) (*http.Response, error) {
	resp, used, err := g.attempt(ctx, c, up, path, body, in)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	drain(resp.Body)
	up.cred.Invalidate(used)
	c.authRetried = true
	resp, _, err = g.attempt(ctx, c, up, path, body, in)
	return resp, err
}

func (g *Gateway) attempt(ctx context.Context, c *call, up *upstream, path string, body []byte, in http.Header) (*http.Response, string, error) {
	tok, err := up.cred.Token(ctx)
	if err != nil {
		return nil, "", err
	}
	if tok != "" {
		c.credentials = append(c.credentials, tok)
	}
	u := *up.cfg.base
	u.Path = strings.TrimRight(u.Path, "/") + path

	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, tok, err
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	if c.stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	h.Set("User-Agent", "ai-gateway/"+g.version)
	h.Set("X-Client-Request-Id", c.id)
	for k, v := range up.cfg.Headers {
		h.Set(k, v)
	}
	if up.cfg.Kind == kindAnthropic {
		version := up.cfg.AnthropicVersion
		if v := in.Get("Anthropic-Version"); c.format == formatAnthropic && anthropicVersionRE.MatchString(v) {
			version = v
		}
		h.Set("Anthropic-Version", version)
		if c.format == formatAnthropic {
			if betas := anthropicBetas(in.Values("Anthropic-Beta")); betas != "" {
				h.Set("Anthropic-Beta", betas)
			}
		}
	}
	setAuthHeader(h, up.cfg.Auth.Header, tok)

	resp, err := g.transport.RoundTrip(req)
	if err != nil {
		cancel()
		return nil, tok, err
	}
	c.upstreamStatus = resp.StatusCode
	c.upstreamReqID = firstNonEmpty(resp.Header.Get("Request-Id"), resp.Header.Get("X-Request-Id"), resp.Header.Get("Apim-Request-Id"))
	resp.Body = newIdleBody(resp.Body, time.Duration(g.cfg.StreamIdleTimeout), cancel, c)
	return resp, tok, nil
}

// upstreamFailed answers a transport or credential error in the caller's format.
func (g *Gateway) upstreamFailed(w http.ResponseWriter, r *http.Request, c *call, err error) {
	switch {
	case r.Context().Err() != nil:
		if rec, ok := w.(*recorder); ok {
			rec.status = 499
		}
	case errors.Is(err, token.ErrLoginRequired):
		g.fail(w, c, http.StatusServiceUnavailable, "az_login_required", "Azure CLI session missing or expired; run `az login` and retry")
	case errors.Is(err, errNoCredential):
		g.log.Error("upstream credential unavailable", "provider", c.provider, "error", err)
		g.fail(w, c, http.StatusServiceUnavailable, "credential_unavailable", "the gateway could not load the credential for provider "+c.provider+"; see the gateway log")
	case isTimeout(err):
		g.fail(w, c, http.StatusGatewayTimeout, "upstream_timeout", "provider "+c.provider+" did not respond in time")
	default:
		g.log.Warn("upstream unreachable", "provider", c.provider, "error", err)
		g.fail(w, c, http.StatusBadGateway, "upstream_unreachable", "the gateway could not reach provider "+c.provider)
	}
}

// relayedHeaders are upstream response headers worth passing back: request IDs and rate limits.
func copyRelayedHeaders(dst, src http.Header) {
	for k, vs := range src {
		lk := strings.ToLower(k)
		if lk == "retry-after" || lk == "request-id" || lk == "x-request-id" || lk == "apim-request-id" ||
			strings.HasPrefix(lk, "x-ratelimit-") || strings.HasPrefix(lk, "anthropic-ratelimit-") {
			for _, v := range vs {
				dst.Add(k, v)
			}
		}
	}
}

// ---- passthrough: caller and upstream speak the same format ----

func (g *Gateway) passthrough(w http.ResponseWriter, r *http.Request, c *call, up *upstream, route routeID, doc map[string]json.RawMessage) {
	doc["model"] = jsonString(c.upstreamModel)
	wantUsage := true
	if c.stream && up.cfg.Kind == kindOpenAI {
		// Ask for the final usage chunk so the call can be costed; hide it again from a client that
		// did not ask for it.
		var so map[string]json.RawMessage
		_ = json.Unmarshal(doc["stream_options"], &so)
		if so == nil {
			so = map[string]json.RawMessage{}
		}
		wantUsage = boolField(so, "include_usage")
		so["include_usage"] = json.RawMessage("true")
		doc["stream_options"], _ = json.Marshal(so)
	}
	body, err := json.Marshal(doc)
	if err != nil {
		g.fail(w, c, http.StatusBadRequest, "invalid_request", "could not re-encode the request body")
		return
	}

	path := "/chat/completions"
	if up.cfg.Kind == kindAnthropic {
		path = "/v1/messages"
		if route == routeCountTokens {
			path = "/v1/messages/count_tokens"
		}
	}
	resp, err := g.send(r.Context(), c, up, path, body, r.Header)
	if err != nil {
		g.upstreamFailed(w, r, c, err)
		return
	}
	defer resp.Body.Close()

	copyRelayedHeaders(w.Header(), resp.Header)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		g.relayError(w, c, resp)
		return
	}
	if !isEventStream(resp) {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody+1))
		if err != nil {
			g.upstreamFailed(w, r, c, err)
			return
		}
		if len(raw) > maxUpstreamBody {
			g.fail(w, c, http.StatusBadGateway, "upstream_response_too_large", "upstream response exceeds the gateway limit")
			return
		}
		// Both APIs answer in JSON; never reflect an upstream-chosen type such as text/html.
		w.Header().Set("Content-Type", "application/json")
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 && route != routeCountTokens {
			scanUsage(up.cfg.Kind, raw, &c.usage)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	flush := flusher(w)
	_ = readSSE(resp.Body, func(line, data []byte) bool {
		if data != nil {
			if clean, ok := c.streamError(data); ok {
				line = append(append([]byte("data: "), clean...), '\n')
			}
			scanUsage(up.cfg.Kind, data, &c.usage)
			if !wantUsage && isUsageOnlyChunk(data) {
				return true
			}
		}
		if _, err := w.Write(line); err != nil {
			return false
		}
		if len(bytes.TrimSpace(line)) == 0 {
			flush()
		}
		return true
	})
	flush()
}

func isUsageOnlyChunk(data []byte) bool {
	var chunk struct {
		Choices []json.RawMessage `json:"choices"`
		Usage   json.RawMessage   `json:"usage"`
	}
	return json.Unmarshal(data, &chunk) == nil && len(chunk.Choices) == 0 && len(chunk.Usage) > 0 && string(chunk.Usage) != "null"
}

// ---- translation: Anthropic caller, OpenAI upstream ----

func (g *Gateway) anthropicViaOpenAI(w http.ResponseWriter, r *http.Request, c *call, up *upstream, body []byte) {
	var in antRequest
	if err := json.Unmarshal(body, &in); err != nil {
		g.fail(w, c, http.StatusBadRequest, "invalid_request", "request body is not a valid Messages request")
		return
	}
	out, err := anthropicToOpenAIRequest(&in, c.upstreamModel, up.cfg.MaxTokensField)
	if err != nil {
		g.fail(w, c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	payload, _ := json.Marshal(out)
	resp, err := g.send(r.Context(), c, up, "/chat/completions", payload, r.Header)
	if err != nil {
		g.upstreamFailed(w, r, c, err)
		return
	}
	defer resp.Body.Close()
	copyRelayedHeaders(w.Header(), resp.Header)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		g.relayError(w, c, resp)
		return
	}

	if isEventStream(resp) {
		s := newAnthropicStream(w, c.model, &c.usage)
		_ = readSSE(resp.Body, func(_, data []byte) bool {
			if data == nil || string(data) == "[DONE]" {
				return true
			}
			if clean, ok := c.streamError(data); ok {
				data = clean
			}
			return s.openAIChunk(data)
		})
		s.finish()
		return
	}

	var res oaiResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUpstreamBody)).Decode(&res); err != nil {
		g.fail(w, c, http.StatusBadGateway, "upstream_invalid", "provider returned an unreadable response")
		return
	}
	c.usage.merge(fromOpenAIUsage(res.Usage))
	writeJSON(w, http.StatusOK, openAIToAnthropicResponse(&res, c.model))
}

// ---- translation: OpenAI caller, Anthropic upstream ----

func (g *Gateway) openAIViaAnthropic(w http.ResponseWriter, r *http.Request, c *call, up *upstream, body []byte) {
	var in oaiRequest
	if err := json.Unmarshal(body, &in); err != nil {
		g.fail(w, c, http.StatusBadRequest, "invalid_request", "request body is not a valid Chat Completions request")
		return
	}
	out, err := openAIToAnthropicRequest(&in, c.upstreamModel)
	if err != nil {
		g.fail(w, c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	payload, _ := json.Marshal(out)
	resp, err := g.send(r.Context(), c, up, "/v1/messages", payload, r.Header)
	if err != nil {
		g.upstreamFailed(w, r, c, err)
		return
	}
	defer resp.Body.Close()
	copyRelayedHeaders(w.Header(), resp.Header)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		g.relayError(w, c, resp)
		return
	}

	if isEventStream(resp) {
		includeUsage := in.StreamOptions != nil && in.StreamOptions.IncludeUsage
		s := newOpenAIStream(w, c.model, includeUsage, &c.usage)
		_ = readSSE(resp.Body, func(_, data []byte) bool {
			if data == nil {
				return true
			}
			if clean, ok := c.streamError(data); ok {
				data = clean
			}
			return s.anthropicEvent(data)
		})
		s.finish()
		return
	}

	var res antResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUpstreamBody)).Decode(&res); err != nil {
		g.fail(w, c, http.StatusBadGateway, "upstream_invalid", "provider returned an unreadable response")
		return
	}
	c.usage.merge(fromAnthropicUsage(&res.Usage))
	writeJSON(w, http.StatusOK, anthropicToOpenAIResponse(&res, c.model))
}

// relayError re-shapes an upstream error into the caller's format, keeping the status code.
func (g *Gateway) relayError(w http.ResponseWriter, c *call, resp *http.Response) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	msg := c.safeError(upstreamErrorMessage(raw))
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	g.fail(w, c, resp.StatusCode, "upstream_error", "provider "+c.provider+": "+msg)
}

func upstreamErrorMessage(raw []byte) string {
	var env struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(raw, &env) != nil {
		s := strings.TrimSpace(string(raw))
		return s
	}
	var e apiError
	if json.Unmarshal(env.Error, &e) == nil && e.Message != "" {
		return e.Message
	}
	var s string
	if json.Unmarshal(env.Error, &s) == nil && s != "" {
		return s
	}
	return env.Message
}

// ---- errors ----

// fail writes a gateway error in the caller's format.
func (g *Gateway) fail(w http.ResponseWriter, c *call, status int, code, msg string) {
	w.Header().Set(headerGatewayError, code)
	if c.format == formatAnthropic {
		writeJSON(w, status, map[string]any{
			"type":  "error",
			"error": apiError{Type: anthropicErrorType(status), Message: msg},
		})
		return
	}
	writeJSON(w, status, map[string]any{
		"error": apiError{Type: openAIErrorType(status), Message: msg, Code: code},
	})
}

func anthropicErrorType(status int) string {
	switch status {
	case 400, 422:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	}
	return "api_error"
}

func openAIErrorType(status int) string {
	switch {
	case status == 401:
		return "authentication_error"
	case status == 403:
		return "permission_error"
	case status == 429:
		return "rate_limit_error"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	}
	return "server_error"
}

// ---- logging ----

func (g *Gateway) logCall(r *http.Request, c *call, rec *recorder, start time.Time) {
	if r.URL.Path == "/healthz" {
		return
	}
	level := slog.LevelInfo
	if c.client == "" {
		level = slog.LevelDebug
	} else if rec.status >= 500 || c.idleTimedOut.Load() {
		level = slog.LevelWarn
	}
	attrs := []any{
		"id", clipLog(c.id),
		"method", clipLog(r.Method),
		"path", clipLog(r.URL.Path),
		"format", clipLog(string(c.format)),
		proxylog.KeyClient, clipLog(c.client),
		"agent", clipLog(clientName(r.UserAgent())),
		proxylog.KeyModel, clipLog(c.model),
		proxylog.KeyProvider, clipLog(c.provider),
		proxylog.KeyUpstreamModel, clipLog(c.upstreamModel),
		"translated", c.translated,
		"stream", c.stream,
		"status", rec.status,
		"gateway_error", clipLog(rec.Header().Get(headerGatewayError)),
		"upstream_status", c.upstreamStatus,
		"upstream_request_id", clipLog(c.upstreamReqID),
		"auth_retried", c.authRetried,
		"stream_idle_timeout", c.idleTimedOut.Load(),
		"bytes", rec.bytes,
		"duration_ms", time.Since(start).Milliseconds(),
	}
	g.log.Log(r.Context(), level, proxylog.Message, append(attrs, c.usage.logAttrs()...)...)
}

// ---- plumbing ----

type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *recorder) WriteHeader(code int) {
	r.extendWriteDeadline()
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.extendWriteDeadline()
	r.wroteHeader = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) extendWriteDeadline() {
	_ = http.NewResponseController(r.ResponseWriter).SetWriteDeadline(time.Now().Add(clientIOTimeout))
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func flusher(w http.ResponseWriter) func() {
	rc := http.NewResponseController(w)
	return func() {
		_ = rc.SetWriteDeadline(time.Now().Add(clientIOTimeout))
		_ = rc.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func isEventStream(resp *http.Response) bool {
	ct, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	return strings.EqualFold(strings.TrimSpace(ct), "text/event-stream")
}

// idleBody cancels the upstream request when no bytes arrive within the idle window.
type idleBody struct {
	io.ReadCloser
	idle   time.Duration
	timer  *time.Timer
	cancel context.CancelFunc
}

func newIdleBody(rc io.ReadCloser, idle time.Duration, cancel context.CancelFunc, c *call) *idleBody {
	b := &idleBody{ReadCloser: rc, idle: idle, cancel: cancel}
	b.timer = time.AfterFunc(idle, func() {
		c.idleTimedOut.Store(true)
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

func drain(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, maxErrorBody))
	_ = rc.Close()
}

func isLoopbackAddr(remoteAddr string) bool {
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

// clientName keeps the product token of a User-Agent ("Caido/0.50" -> "Caido").
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
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var (
	anthropicVersionRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	anthropicBetaRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,79}$`)
)

// anthropicBetas keeps the well-formed beta tokens from the caller's Anthropic-Beta headers, so a
// client cannot smuggle arbitrary text upstream.
func anthropicBetas(values []string) string {
	var keep []string
	for _, tok := range splitHeaderList(values) {
		if anthropicBetaRE.MatchString(tok) && !slices.Contains(keep, tok) {
			keep = append(keep, tok)
		}
	}
	return strings.Join(keep, ",")
}

// clipLog bounds every string attribute, including the truncation marker.
func clipLog(s string) string { return clipString(s, 256) }

func clipString(s string, limit int) string {
	if len(s) > limit {
		return strings.ToValidUTF8(s[:limit-3], "") + "..."
	}
	return s
}

func (c *call) safeError(msg string) string {
	for _, secret := range c.credentials {
		msg = strings.ReplaceAll(msg, secret, "[REDACTED]")
	}
	return clipString(msg, maxErrorMessage)
}

// Normalize only error events; successful content and usage retain their original wire shape.
func (c *call) streamError(data []byte) ([]byte, bool) {
	var env map[string]json.RawMessage
	if json.Unmarshal(data, &env) != nil || len(env["error"]) == 0 {
		return nil, false
	}
	clean, _ := json.Marshal(map[string]any{"type": "error", "error": apiError{Type: "api_error", Message: c.safeError(upstreamErrorMessage(data))}})
	return clean, true
}
