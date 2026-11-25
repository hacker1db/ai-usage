package palette

type Mode int

const (
	Light Mode = iota
	Dark
)

var Series = map[Mode][9]string{
	Light: {"#8f8e86", "#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300", "#6250d6", "#e34948"},
	Dark:  {"#8f8e86", "#3987e5", "#d95926", "#199e70", "#c98500", "#d55181", "#008300", "#9085e9", "#e66767"},
}

var Sequential = []string{"#fff4cf", "#fde7a4", "#fdd283", "#fcb86a", "#f99b5c", "#f17c57", "#e35f5c",
	"#cc4866", "#ae366f", "#8c2a74", "#6b2273", "#4b1b6a", "#2f145a"}

var Status = map[string]string{"good": "#0ca30c", "warning": "#fab219", "serious": "#ec835a", "critical": "#d03b3b"}

type Ink struct {
	Surface, Page, Primary, Secondary, Muted, Grid, Axis string
}

var Inks = map[Mode]Ink{
	Light: {"#fcfcfb", "#f9f9f7", "#0b0b0b", "#52514e", "#898781", "#e1e0d9", "#c3c2b7"},
	Dark:  {"#1a1a19", "#0d0d0d", "#f0efec", "#c3c2b7", "#898781", "#2c2c2a", "#383835"},
}

var Mix = map[Mode][3]string{
	Light: {"#fdd283", "#d9484f", "#6b2273"},
	Dark:  {"#8c2a74", "#d9484f", "#fdd283"},
}

var Accent = map[Mode][4]string{
	Light: {"#2a78d6", "#6250d6", "#e34f8a", "#eb6834"},
	Dark:  {"#5aa2ff", "#9b8cff", "#ff6fae", "#ff8a4c"},
}

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

func SeqStep(v, top float64) int {
	if v <= 0 || top <= 0 {
		return 0
	}
	n := len(Sequential) - 1
	i := int(v/top*float64(n-1)+0.5) + 1
	return min(max(i, 1), n)
}

func SeqColor(m Mode, step int) string {
	n := len(Sequential) - 1
	step = min(max(step, 1), n)
	if m == Dark {
		return Sequential[n+1-step]
	}
	return Sequential[step]
}
