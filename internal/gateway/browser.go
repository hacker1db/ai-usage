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

var corsAllowHeaders = []string{
	"authorization", "x-api-key", "api-key", "content-type",
	"anthropic-version", "anthropic-beta", "anthropic-dangerous-direct-browser-access",
	"x-stainless-arch", "x-stainless-lang", "x-stainless-os", "x-stainless-package-version",
	"x-stainless-retry-count", "x-stainless-runtime", "x-stainless-runtime-version",
	"x-stainless-timeout", "x-stainless-helper-method", "openai-organization", "openai-project",
}

var corsExposeHeaders = strings.Join([]string{
	headerRequestID, headerGatewayError, "request-id", "x-request-id", "retry-after",
	"x-ratelimit-limit-requests", "x-ratelimit-limit-tokens", "x-ratelimit-remaining-requests",
	"x-ratelimit-remaining-tokens", "x-ratelimit-reset-requests", "x-ratelimit-reset-tokens",
	"anthropic-ratelimit-requests-limit", "anthropic-ratelimit-requests-remaining",
	"anthropic-ratelimit-requests-reset", "anthropic-ratelimit-tokens-limit",
	"anthropic-ratelimit-tokens-remaining", "anthropic-ratelimit-tokens-reset",
}, ", ")

const corsMaxAge = "600"

func setSecurityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Add("Vary", "Origin")
}

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

func secFetchSiteOK(v string) bool {
	return v == "" || v == "none"
}

func isJSONContentType(v string) bool {
	mt, _, err := mime.ParseMediaType(v)
	return err == nil && mt == "application/json"
}

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
