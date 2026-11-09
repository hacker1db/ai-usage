// Package web serves the dashboard: server-rendered HTML with inline SVG charts and a small
// vanilla script for tooltips and live refresh. No framework and no build step.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ai-usage/internal/palette"
	"ai-usage/internal/snapshot"
	"ai-usage/internal/usage"
	"ai-usage/internal/view"
	"ai-usage/internal/workreport"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// securityHeaders go on every response. SVG colors are classes, so no inline styles are needed.
var securityHeaders = map[string]string{
	"Content-Security-Policy": "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; " +
		"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
	"X-Content-Type-Options": "nosniff",
	"Referrer-Policy":        "no-referrer",
	"Cache-Control":          "no-cache",
}

// reportCSP serves the skill's self-contained report sandboxed (opaque origin), so its inline
// scripts can't call this dashboard's API.
const reportCSP = "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline'; img-src data:; " +
	"style-src 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com"

// Server is the dashboard's HTTP handler.
type Server struct {
	c             *snapshot.Collector
	allowedHosts  map[string]bool
	workReportDir string
	tmpl          *template.Template
	log           *slog.Logger
	mux           *http.ServeMux
	tokenCSS      string
}

// New returns the handler for a collector listening on port.
func New(c *snapshot.Collector, port int, workReportDir string, log *slog.Logger) (*Server, error) {
	t, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	p := strconv.Itoa(port)
	s := &Server{c: c, workReportDir: workReportDir, tmpl: t, log: log, mux: http.NewServeMux(),
		allowedHosts: map[string]bool{"127.0.0.1:" + p: true, "localhost:" + p: true, "[::1]:" + p: true}, tokenCSS: tokensCSS()}
	static, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	s.mux.HandleFunc("GET /tokens.css", s.tokens)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	s.mux.HandleFunc("GET /api/usage", s.apiUsage)
	s.mux.HandleFunc("GET /api/etag", s.apiETag)
	s.mux.HandleFunc("GET /api/session", s.apiSession)
	s.mux.HandleFunc("POST /api/refresh", s.apiRefresh)
	s.mux.HandleFunc("GET /report", s.report)
	s.mux.HandleFunc("GET /session", s.session)
	s.mux.HandleFunc("GET /{$}", s.page)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for k, v := range securityHeaders {
		w.Header().Set(k, v)
	}
	// Loopback peers only, and only our own Host header (blocks DNS rebinding).
	if !isLoopback(r.RemoteAddr) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "loopback only"})
		return
	}
	if !s.allowedHosts[strings.ToLower(r.Host)] {
		writeJSON(w, http.StatusMisdirectedRequest, map[string]string{"error": "unexpected Host header"})
		return
	}
	s.mux.ServeHTTP(w, r)
}

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) apiUsage(w http.ResponseWriter, r *http.Request) {
	snap, body, etag, _ := s.c.Current()
	if snap == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]bool{"pending": true})
		return
	}
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (s *Server) apiETag(w http.ResponseWriter, _ *http.Request) {
	snap, _, etag, err := s.c.Current()
	out := map[string]any{"etag": etag, "pending": snap == nil}
	if snap != nil {
		out["generated"] = snap.Meta.Generated.UnixMilli()
		out["interval"] = snap.Meta.Interval
	}
	if err != nil {
		out["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiSession(w http.ResponseWriter, r *http.Request) {
	x, ok := s.c.Session(r.URL.Query().Get("key"))
	if !ok || x.Detail == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown session"})
		return
	}
	writeJSON(w, http.StatusOK, x.Detail)
}

func (s *Server) apiRefresh(w http.ResponseWriter, r *http.Request) {
	// A custom header can't be sent cross-origin without a CORS preflight this server never approves.
	if r.Header.Get("X-Requested-With") != "ai-usage" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	s.c.Wake()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "refreshing"})
}

func (s *Server) report(w http.ResponseWriter, _ *http.Request) {
	raw, err := os.ReadFile(filepath.Join(s.workReportDir, workreport.ReportName))
	if s.workReportDir == "" || err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no ai-work-report.html in the work report folder yet"})
		return
	}
	w.Header().Set("Content-Security-Policy", reportCSP)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(raw)
}

type pageData struct {
	D                    view.Dashboard
	ETag                 string
	Pending              bool
	Days                 int
	Tabs, Metrics, Burns []view.Option
	Ranges, Sorts        []view.Option
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	snap, _, etag, _ := s.c.Current()
	pending := snap == nil
	if pending {
		snap = &snapshot.Snapshot{Meta: snapshot.Meta{Generated: time.Now(), Since: time.Now(), Interval: 5}}
	}
	data := pageData{D: view.Build(snap, view.FromQuery(r.URL.Query()), time.Now()), ETag: etag, Pending: pending,
		Days: snap.Meta.Days, Tabs: view.TabOptions(), Metrics: view.MetricOptions(), Burns: view.BurnOptions(),
		Ranges: view.RangeOptions(), Sorts: view.SessionSorts}
	name := "page"
	if r.Header.Get("X-Partial") == "1" {
		name = "main"
	}
	s.render(w, name, data)
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	x, ok := s.c.Session(key)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown session"})
		return
	}
	back := view.FromQuery(r.URL.Query()).Link()
	s.render(w, "session", map[string]any{"S": x, "Group": view.GroupOf(x.Tool, x.Profile), "Back": back})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("render", "template", name, "error", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

var funcs = template.FuncMap{
	"compact":  view.Compact,
	"exact":    view.Exact,
	"compacti": func(n int64) string { return view.Compact(float64(n)) },
	"exacti":   func(n any) string { return view.Exact(toFloat(n)) },
	"pct":      view.Pct,
	"dur":      func(min any) string { return view.Duration(toFloat(min)) },
	"when":     view.WhenLabel,
	"whenStr":  func(s string) string { return view.WhenLabel(usage.ParseTime(s).Local()) },
	"dayStr":   func(s string) string { return view.DayLabel(usage.ParseTime(s).Local()) },
	"shortday": view.ShortDay,
	"add":      func(a, b int) int { return a + b },
	"mod":      func(a, b int) int { return a % b },
	"pchip":    func(o view.ProfileOpt, f view.Filters) map[string]any { return map[string]any{"O": o, "F": f} },
	"memberLabels": func(gs []view.Group) string {
		names := make([]string, len(gs))
		for i, g := range gs {
			names[i] = g.Label
		}
		return strings.Join(names, ", ")
	},
	"div":        func(a, b float64) float64 { return a / b },
	"cumulative": cumulativeChart,
	"stacked":    stackedChart,
	"spark":      sparkline,
	"meter":      meter,
	"heatmap":    heatmap,
	"mixbar":     mixBar,
	"share":      shareBar,
	"projects":   projectsChart,
	"crosscheck": crossCheckChart,
	"countBars":  countBars,
	"swatch": func(slot int) template.HTML {
		return template.HTML(`<span class="swatch s` + strconv.Itoa(slot) + `" aria-hidden="true"></span>`) //nolint:gosec // int only
	},
	"ramp": func() []int {
		out := make([]int, len(palette.Sequential)-1)
		for i := range out {
			out[i] = i + 1
		}
		return out
	},
	"revIdx": func(n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = n - 1 - i
		}
		return out
	},
	"branch": func(b string) string {
		if b == "" {
			return ""
		}
		return "⎇ " + b
	},
	"join": func(parts ...string) string {
		var keep []string
		for _, p := range parts {
			if p != "" {
				keep = append(keep, p)
			}
		}
		return strings.Join(keep, " · ")
	},
	"usedGroups": func(ps []view.Project) []view.Group {
		used := map[string]bool{}
		for _, p := range ps {
			for _, part := range p.Parts {
				used[part.Name] = true
			}
		}
		var out []view.Group
		for _, g := range append(append([]view.Group(nil), view.Groups...), view.Other) {
			if used[g.Key] {
				out = append(out, g)
			}
		}
		return out
	},
	"hasCross": func(ds []view.DayPair) bool {
		for _, d := range ds {
			if d.Proxy > 0 || d.Codex > 0 {
				return true
			}
		}
		return false
	},
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	}
	return math.NaN()
}

// countBars draws a ranked list of counts (files, tools, models) as thin horizontal bars.
func countBars(counts []usage.Count, unit string) template.HTML {
	if len(counts) == 0 {
		return template.HTML(`<p class="empty">None recorded.</p>`)
	}
	// A narrow canvas: these sit in half-width cards, where a 960-wide viewBox shrinks the text.
	const w, rowH, labelW = 560.0, 24.0, 300.0
	h := float64(len(counts)) * rowH
	s := newSVG(w, h, "chart counts", unit)
	mx := 1
	for _, c := range counts {
		mx = max(mx, c.N)
	}
	for i, c := range counts {
		yy := float64(i) * rowH
		label := c.Name
		if r := []rune(label); len(r) > 44 {
			label = "…" + string(r[len(r)-43:])
		}
		s.text(labelW-10, yy+rowH/2+4, "label", "end", label)
		bw := math.Max(float64(c.N)/float64(mx)*(w-labelW-80), 2)
		s.el("path", "class", "count-bar", "d", roundedRight(labelW, yy+8, bw, rowH-16, 3))
		s.text(labelW+bw+8, yy+rowH/2+4, "value-label", "start", strconv.Itoa(c.N)+" "+unit)
	}
	s.end("svg")
	return s.html()
}
