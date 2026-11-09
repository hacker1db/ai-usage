// Package tui renders view.Dashboard models as a terminal dashboard: a document of styled cell
// lines that renders to ANSI text, shared by the live screen and `ai-usage tui -once`.
package tui

import (
	"strconv"
	"strings"

	"ai-usage/internal/palette"
)

// Style is a cell's appearance. Colors are #rrggbb hex, "" for the terminal default.
type Style struct {
	FG, BG                   string
	Bold, Inverse, Underline bool
}

// Cell is one terminal column. A wide rune is followed by a Cell with R == 0 that renders nothing.
type Cell struct {
	R rune
	S Style
}

// Line is one row of cells.
type Line []Cell

// Escapes selects how much terminal styling to emit.
type Escapes int

const (
	NoEscapes Escapes = iota // plain text (piped output)
	StyleOnly                // bold/underline/inverse but no color (NO_COLOR)
	FullColor                // 24-bit color
)

// Theme selects the palette mode and the styling level.
type Theme struct {
	Mode    palette.Mode
	Ink     palette.Ink
	Escapes Escapes
}

// NewTheme returns a theme for mode.
func NewTheme(mode palette.Mode, esc Escapes) Theme {
	return Theme{Mode: mode, Ink: palette.Inks[mode], Escapes: esc}
}

// Series returns a categorical slot's color (0 = Other).
func (t Theme) Series(slot int) string {
	s := palette.Series[t.Mode]
	if slot < 0 || slot >= len(s) {
		slot = 0
	}
	return s[slot]
}

// Text styles built from ink tokens. Text never wears a series color.
func (t Theme) Primary() Style   { return Style{FG: t.Ink.Primary} }
func (t Theme) Secondary() Style { return Style{FG: t.Ink.Secondary} }
func (t Theme) Muted() Style     { return Style{FG: t.Ink.Muted} }
func (t Theme) Title() Style     { return Style{FG: t.Ink.Primary, Bold: true} }

// Accent is decorative chrome color i (wrapping); it never encodes data.
func (t Theme) Accent(i int) Style {
	a := palette.Accent[t.Mode]
	return Style{FG: a[((i%len(a))+len(a))%len(a)], Bold: true}
}
func (t Theme) Status(level string) Style {
	return Style{FG: palette.Status[level]}
}

// RuneWidth is the display width of r: 0 for combining marks, 2 for wide East Asian and emoji.
func RuneWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20 || r == 0x7f:
		return 0
	case r >= 0x0300 && r <= 0x036f, r >= 0x200b && r <= 0x200f, r >= 0xfe00 && r <= 0xfe0f, r == 0x20e3:
		return 0
	case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf, r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f, r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6, r >= 0x1f300 && r <= 0x1faff, r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}

// Width is the display width of s.
func Width(s string) int {
	n := 0
	for _, r := range s {
		n += RuneWidth(r)
	}
	return n
}

// Truncate shortens s to at most w columns, ending with … when cut.
func Truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if Width(s) <= w {
		return s
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := RuneWidth(r)
		if used+rw > w-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return strings.TrimRight(b.String(), " ") + "…"
}

// PadRight pads s with spaces to w columns (truncating when longer).
func PadRight(s string, w int) string {
	s = Truncate(s, w)
	return s + strings.Repeat(" ", max(w-Width(s), 0))
}

// PadLeft right-aligns s in w columns.
func PadLeft(s string, w int) string {
	s = Truncate(s, w)
	return strings.Repeat(" ", max(w-Width(s), 0)) + s
}

// B builds a Line.
type B struct{ l Line }

// T appends text in a style.
func (b *B) T(s string, st Style) *B {
	for _, r := range s {
		w := RuneWidth(r)
		if w == 0 {
			continue
		}
		b.l = append(b.l, Cell{r, st})
		if w == 2 {
			b.l = append(b.l, Cell{0, st})
		}
	}
	return b
}

// C appends one cell.
func (b *B) C(r rune, st Style) *B {
	b.l = append(b.l, Cell{r, st})
	return b
}

// Sp appends n spaces.
func (b *B) Sp(n int) *B {
	for range max(n, 0) {
		b.l = append(b.l, Cell{' ', Style{}})
	}
	return b
}

// To pads with spaces up to column col.
func (b *B) To(col int) *B { return b.Sp(col - len(b.l)) }

// Len is the current width.
func (b *B) Len() int { return len(b.l) }

// Line returns the line fitted to width w.
func (b *B) Line(w int) Line { return fit(b.l, w) }

// fit truncates or pads a line to exactly w cells without splitting a wide rune.
func fit(l Line, w int) Line {
	out := make(Line, w)
	n := copy(out, l)
	if n == w && len(l) > w && l[w].R == 0 {
		out[w-1] = Cell{' ', Style{}} // the wide rune's second half fell off the edge
	}
	for i := n; i < w; i++ {
		out[i] = Cell{' ', Style{}}
	}
	return out
}

// Doc is a sequence of lines of a fixed width.
type Doc struct {
	W     int
	Lines []Line
	sec   int // sections drawn so far, to rotate their accent colors
}

// Add appends a builder's line.
func (d *Doc) Add(b *B) { d.Lines = append(d.Lines, b.Line(d.W)) }

// AddLine appends a prepared line.
func (d *Doc) AddLine(l Line) { d.Lines = append(d.Lines, fit(l, d.W)) }

// Blank appends an empty line.
func (d *Doc) Blank() { d.Lines = append(d.Lines, fit(nil, d.W)) }

// Text appends a single-style line.
func (d *Doc) Text(s string, st Style) { d.Add(new(B).T(s, st)) }

// Render writes lines as ANSI text separated by sep at the given styling level.
func Render(lines []Line, esc Escapes, sep string) string {
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteString(sep)
		}
		renderLine(&b, l, esc)
	}
	return b.String()
}

func renderLine(b *strings.Builder, l Line, esc Escapes) {
	var cur Style
	open := false
	for _, c := range l {
		if c.R == 0 {
			continue
		}
		s := c.S
		switch esc {
		case NoEscapes:
			s = Style{}
		case StyleOnly:
			s.FG, s.BG = "", ""
		}
		if s != cur {
			b.WriteString(sgr(s))
			cur, open = s, true
		}
		b.WriteRune(c.R)
	}
	if open && cur != (Style{}) {
		b.WriteString("\x1b[0m")
	}
}

func sgr(s Style) string {
	codes := []string{"0"}
	if s.Bold {
		codes = append(codes, "1")
	}
	if s.Underline {
		codes = append(codes, "4")
	}
	if s.Inverse {
		codes = append(codes, "7")
	}
	if s.FG != "" {
		r, g, bl := palette.RGB(s.FG)
		codes = append(codes, "38;2;"+strconv.Itoa(int(r))+";"+strconv.Itoa(int(g))+";"+strconv.Itoa(int(bl)))
	}
	if s.BG != "" {
		r, g, bl := palette.RGB(s.BG)
		codes = append(codes, "48;2;"+strconv.Itoa(int(r))+";"+strconv.Itoa(int(g))+";"+strconv.Itoa(int(bl)))
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

// Plain returns a line's text without styling (for tests).
func Plain(l Line) string {
	var b strings.Builder
	for _, c := range l {
		if c.R != 0 {
			b.WriteRune(c.R)
		}
	}
	return b.String()
}
