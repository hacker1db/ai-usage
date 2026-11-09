package web

import (
	"fmt"
	"net/http"
	"strings"

	"ai-usage/internal/palette"
)

// tokensCSS renders palette.go as CSS custom properties: light on :root, dark under the OS
// preference (unless the viewer picked light) and under an explicit data-theme="dark".
func tokensCSS() string {
	vars := func(m palette.Mode) string {
		var b strings.Builder
		ink := palette.Inks[m]
		scheme := "light"
		if m == palette.Dark {
			scheme = "dark"
		}
		fmt.Fprintf(&b, "  color-scheme: %s;\n", scheme)
		for k, v := range map[string]string{"surface": ink.Surface, "page": ink.Page, "ink": ink.Primary,
			"ink-2": ink.Secondary, "muted": ink.Muted, "grid": ink.Grid, "axis": ink.Axis} {
			fmt.Fprintf(&b, "  --%s: %s;\n", k, v)
		}
		for i, v := range palette.Series[m] {
			fmt.Fprintf(&b, "  --s%d: %s;\n", i, v)
		}
		for i := 1; i < len(palette.Sequential); i++ {
			fmt.Fprintf(&b, "  --q%d: %s;\n", i, palette.SeqColor(m, i))
		}
		for i, v := range palette.Mix[m] {
			fmt.Fprintf(&b, "  --mix%d: %s;\n", i, v)
		}
		for i, v := range palette.Accent[m] {
			fmt.Fprintf(&b, "  --a%d: %s;\n", i, v)
		}
		for k, v := range palette.Status {
			fmt.Fprintf(&b, "  --%s: %s;\n", k, v)
		}
		return b.String()
	}
	return ":root {\n" + vars(palette.Light) + "}\n" +
		"@media (prefers-color-scheme: dark) {\n  :root:not([data-theme=\"light\"]) {\n" + vars(palette.Dark) + "  }\n}\n" +
		":root[data-theme=\"dark\"] {\n" + vars(palette.Dark) + "}\n"
}

func (s *Server) tokens(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write([]byte(s.tokenCSS))
}
