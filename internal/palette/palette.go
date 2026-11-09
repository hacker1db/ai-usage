// Package palette is the one color source for the web and terminal renderers: the dataviz
// reference palette's categorical slots (validated adjacent-pair CVD safe in both modes), the blue
// sequential ramp, the reserved status colors, and chart ink. Text never wears a series color.
package palette

// Mode is a light or dark surface.
type Mode int

const (
	Light Mode = iota
	Dark
)

// Series are categorical slots 1-8 in fixed order; index 0 is "Other" (neutral gray).
var Series = map[Mode][9]string{
	Light: {"#8f8e86", "#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300", "#6250d6", "#e34948"},
	Dark:  {"#8f8e86", "#3987e5", "#d95926", "#199e70", "#c98500", "#d55181", "#008300", "#9085e9", "#e66767"},
}

// Sequential is the blue ramp, step 100 (near zero) to 700, for magnitude (heatmaps).
var Sequential = []string{"#cde2fb", "#b7d3f6", "#9ec5f4", "#86b6ef", "#6da7ec", "#5598e7",
	"#3987e5", "#2a78d6", "#256abf", "#1c5cab", "#184f95", "#104281", "#0d366b"}

// Status colors are reserved for state and always ship with an icon and label.
var Status = map[string]string{"good": "#0ca30c", "warning": "#fab219", "serious": "#ec835a", "critical": "#d03b3b"}

// Ink holds the chart chrome for one mode.
type Ink struct {
	Surface, Page, Primary, Secondary, Muted, Grid, Axis string
}

// Inks per mode.
var Inks = map[Mode]Ink{
	Light: {"#fcfcfb", "#f9f9f7", "#0b0b0b", "#52514e", "#898781", "#e1e0d9", "#c3c2b7"},
	Dark:  {"#1a1a19", "#0d0d0d", "#f0efec", "#c3c2b7", "#898781", "#2c2c2a", "#383835"},
}

// Mix colors the token-composition parts in cost order (cache read → fresh input → output),
// as three ordinal steps of the sequential ramp.
var Mix = map[Mode][3]string{
	Light: {"#9ec5f4", "#3987e5", "#184f95"},
	Dark:  {"#184f95", "#3987e5", "#9ec5f4"},
}

// RGB parses #rrggbb.
func RGB(hex string) (r, g, b uint8) {
	if len(hex) != 7 || hex[0] != '#' {
		return 128, 128, 128
	}
	v := func(s string) uint8 {
		var n uint8
		for _, c := range s {
			n <<= 4
			switch {
			case c >= '0' && c <= '9':
				n |= uint8(c - '0')
			case c >= 'a' && c <= 'f':
				n |= uint8(c-'a') + 10
			case c >= 'A' && c <= 'F':
				n |= uint8(c-'A') + 10
			}
		}
		return n
	}
	return v(hex[1:3]), v(hex[3:5]), v(hex[5:7])
}

// SeqStep picks the sequential step for v in [0, top]: 0 for v <= 0, else 1..len-1.
func SeqStep(v, top float64) int {
	if v <= 0 || top <= 0 {
		return 0
	}
	n := len(Sequential) - 1
	i := int(v/top*float64(n-1)+0.5) + 1
	return min(max(i, 1), n)
}

// SeqColor returns the sequential color for a step from SeqStep. Near-zero steps recede toward the
// surface, so dark mode walks the ramp from its dark end.
func SeqColor(m Mode, step int) string {
	n := len(Sequential) - 1
	step = min(max(step, 1), n)
	if m == Dark {
		return Sequential[n+1-step]
	}
	return Sequential[step]
}
