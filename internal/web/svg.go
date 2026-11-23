package web

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"strconv"
	"strings"
)

type svg struct {
	b strings.Builder
}

type attrs []string

func num(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

func (s *svg) open(tag string, a attrs, selfClose bool) {
	s.b.WriteByte('<')
	s.b.WriteString(tag)
	for i := 0; i+1 < len(a); i += 2 {
		if a[i+1] == "" {
			continue
		}
		s.b.WriteByte(' ')
		s.b.WriteString(a[i])
		s.b.WriteString(`="`)
		s.b.WriteString(html.EscapeString(a[i+1]))
		s.b.WriteByte('"')
	}
	if selfClose {
		s.b.WriteString("/>")
	} else {
		s.b.WriteByte('>')
	}
}

func (s *svg) el(tag string, a ...string)    { s.open(tag, a, true) }
func (s *svg) start(tag string, a ...string) { s.open(tag, a, false) }
func (s *svg) end(tag string)                { s.b.WriteString("</" + tag + ">") }

func (s *svg) text(x, y float64, class, anchor, body string, extra ...string) {
	s.open("text", append(attrs{"x", num(x), "y", num(y), "class", class, "text-anchor", anchor}, extra...), false)
	s.b.WriteString(html.EscapeString(body))
	s.end("text")
}

func (s *svg) html() template.HTML { return template.HTML(s.b.String()) } //nolint:gosec // all content escaped above

func newSVG(w, h float64, class, label string, extra ...string) *svg {
	s := &svg{}
	s.open("svg", append(attrs{"viewBox", fmt.Sprintf("0 0 %s %s", num(w), num(h)), "class", class,
		"role", "img", "aria-label", label, "preserveAspectRatio", "xMidYMid meet"}, extra...), false)
	return s
}

type tip struct {
	H string  `json:"h"`
	R [][]any `json:"r"`
}

func (t tip) attr() string {
	b, _ := json.Marshal(t)
	return string(b)
}

func row(label, value string, slot int) []any { return []any{label, value, slot} }

func roundedTop(x, y, w, h, r float64) string {
	r = min(r, w/2, h)
	if r <= 0 {
		return fmt.Sprintf("M%s,%s V%s H%s V%s Z", num(x), num(y+h), num(y), num(x+w), num(y+h))
	}
	return fmt.Sprintf("M%s,%s V%s Q%s,%s %s,%s H%s Q%s,%s %s,%s V%s Z",
		num(x), num(y+h), num(y+r), num(x), num(y), num(x+r), num(y),
		num(x+w-r), num(x+w), num(y), num(x+w), num(y+r), num(y+h))
}

func roundedRight(x, y, w, h, r float64) string {
	r = min(r, w, h/2)
	if r <= 0 {
		return fmt.Sprintf("M%s,%s H%s V%s H%s Z", num(x), num(y), num(x+w), num(y+h), num(x))
	}
	return fmt.Sprintf("M%s,%s H%s Q%s,%s %s,%s V%s Q%s,%s %s,%s H%s Z",
		num(x), num(y), num(x+w-r), num(x+w), num(y), num(x+w), num(y+r),
		num(y+h-r), num(x+w), num(y+h), num(x+w-r), num(y+h), num(x))
}
