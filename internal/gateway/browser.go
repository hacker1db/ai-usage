package gateway

import (
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// The gateway is called by local programs (Caido, SDKs, scripts), not by web pages. A browser can
// still reach 127.0.0.1, so every request passes browserGuard first:
//
//   - Host must name a loopback address. This defeats DNS rebinding, where a page on a hostile
//     domain resolves its own name to 127.0.0.1 so the browser treats the gateway as same-origin.
//   - Any request carrying an Origin header is refused unless that exact origin is listed in
//     allowed_origins (empty by default). Requests without Origin but with a Sec-Fetch-Site other
//     than "none" (navigations, <img>, <script>, form posts in older browsers) are refused too.
//   - CORS preflights are answered only for allowed origins, and only for the methods and headers
//     the API uses. Access-Control-Allow-Credentials is never sent; the key travels in a header.
//   - POST bodies must be application/json, so no "simple" cross-site form or text/plain request
//     can reach a handler even if a browser omitted Origin.
//   - Every response carries headers that stop sniffing, framing, caching and cross-origin embedding.

// corsAllowHeaders are the request headers an allowed origin may send.
var corsAllowHeaders = []string{
	"authorization", "x-api-key", "api-key", "content-type",
	"anthropic-version", "anthropic-beta", "anthropic-dangerous-direct-browser-access",
	"x-stainless-arch", "x-stainless-lang", "x-stainless-os", "x-stainless-package-version",
	"x-stainless-retry-count", "x-stainless-runtime", "x-stainless-runtime-version",
	"x-stainless-timeout", "x-stainless-helper-method", "openai-organization", "openai-project",
}

// corsExposeHeaders are the response headers an allowed origin may read.
var corsExposeHeaders = strings.Join([]string{
	headerRequestID, headerGatewayError, "request-id", "x-request-id", "retry-after",
	"x-ratelimit-limit-requests", "x-ratelimit-limit-tokens", "x-ratelimit-remaining-requests",
	"x-ratelimit-remaining-tokens", "x-ratelimit-reset-requests", "x-ratelimit-reset-tokens",
	"anthropic-ratelimit-requests-limit", "anthropic-ratelimit-requests-remaining",
	"anthropic-ratelimit-requests-reset", "anthropic-ratelimit-tokens-limit",
	"anthropic-ratelimit-tokens-remaining", "anthropic-ratelimit-tokens-reset",
}, ", ")

const corsMaxAge = "600"

// setSecurityHeaders sets the headers every response carries.
func setSecurityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Add("Vary", "Origin")
}

// browserGuard applies the browser-facing policy. It returns false when it has already written the
// response (a refusal, or a preflight answer).
func (g *Gateway) browserGuard(w http.ResponseWriter, r *http.Request, c *call) bool {
	h := w.Header()
	if !isLoopbackHostHeader(r.Host) {
		g.fail(w, c, http.StatusMisdirectedRequest, "host_not_allowed", "the Host header must name a loopback address such as 127.0.0.1 or localhost")
		return false
	}

	origin, hasOrigin := originHeader(r)
	allowed := hasOrigin && slices.Contains(g.cfg.AllowedOrigins, origin)
	switch {
	case hasOrigin && !allowed:
		g.fail(w, c, http.StatusForbidden, "origin_not_allowed", "browser origins are refused unless listed in allowed_origins")
		return false
	case !hasOrigin && !secFetchSiteOK(r.Header.Get("Sec-Fetch-Site")):
		g.fail(w, c, http.StatusForbidden, "cross_site", "cross-site browser requests are refused")
		return false
	}

	if allowed {
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
	}

	if r.Method == http.MethodOptions {
		g.preflight(w, r, c, allowed)
		return false
	}

	if r.Method == http.MethodPost && !isJSONContentType(r.Header.Get("Content-Type")) {
		g.fail(w, c, http.StatusUnsupportedMediaType, "unsupported_media_type", "request body must be Content-Type: application/json")
		return false
	}
	return true
}

// preflight answers a CORS preflight. Only allowed origins reach it with allowed=true.
func (g *Gateway) preflight(w http.ResponseWriter, r *http.Request, c *call, allowed bool) {
	method := r.Header.Get("Access-Control-Request-Method")
	if !allowed || (method != http.MethodGet && method != http.MethodPost) {
		g.fail(w, c, http.StatusForbidden, "preflight_refused", "CORS preflight refused")
		return
	}
	for _, name := range splitHeaderList(r.Header.Values("Access-Control-Request-Headers")) {
		if !slices.Contains(corsAllowHeaders, name) {
			g.fail(w, c, http.StatusForbidden, "preflight_refused", fmt.Sprintf("CORS preflight refused: header %q is not allowed", name))
			return
		}
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", "GET, POST")
	h.Set("Access-Control-Allow-Headers", strings.Join(corsAllowHeaders, ", "))
	h.Set("Access-Control-Max-Age", corsMaxAge)
	h.Add("Vary", "Access-Control-Request-Method")
	h.Add("Vary", "Access-Control-Request-Headers")
	if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
		h.Set("Access-Control-Allow-Private-Network", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

// originHeader reports the Origin header. Several Origin headers, or an empty one, count as present
// so they are refused rather than ignored.
func originHeader(r *http.Request) (string, bool) {
	values, ok := r.Header["Origin"]
	if !ok {
		return "", false
	}
	if len(values) != 1 {
		return "", true
	}
	return values[0], true
}

// secFetchSiteOK accepts requests without Sec-Fetch-Site (non-browser clients) and user-initiated
// ones ("none": typed into the address bar). The gateway serves no pages, so "same-origin" can
// only come from a rebinding attempt the Host check missed, and is refused with the rest.
func secFetchSiteOK(v string) bool {
	return v == "" || v == "none"
}

func isJSONContentType(v string) bool {
	mt, _, err := mime.ParseMediaType(v)
	return err == nil && mt == "application/json"
}

// isLoopbackHostHeader accepts "localhost", "*.localhost" and loopback IP literals, with or without
// a port.
func isLoopbackHostHeader(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	} else if strings.HasPrefix(hostport, "[") && strings.HasSuffix(hostport, "]") {
		host = hostport[1 : len(hostport)-1]
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func splitHeaderList(values []string) []string {
	var out []string
	for _, v := range values {
		for part := range strings.SplitSeq(v, ",") {
			if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// checkOrigin validates one allowed_origins entry: an exact, serialized browser origin.
func checkOrigin(o string) error {
	if o == "*" || o == "null" {
		return fmt.Errorf("%q is not allowed; list exact origins such as \"http://localhost:5173\"", o)
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return fmt.Errorf("%q must be an origin such as \"https://app.example\"", o)
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(o, "?") || strings.HasSuffix(o, "#") {
		return fmt.Errorf("%q must be scheme://host[:port] with no path, query, or credentials", o)
	}
	if o != strings.ToLower(o) {
		return fmt.Errorf("%q must be lowercase, as browsers send it", o)
	}
	if (u.Scheme == "http" && u.Port() == "80") || (u.Scheme == "https" && u.Port() == "443") {
		return fmt.Errorf("%q: browsers omit the default port; drop it", o)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHostHeader(u.Host) {
			return errors.New("http origins are only allowed for loopback hosts; use https")
		}
	default:
		return fmt.Errorf("%q: scheme must be http or https", o)
	}
	return nil
}
