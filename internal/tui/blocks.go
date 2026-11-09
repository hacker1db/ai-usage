package tui

import "math"

// Eighth-block glyphs: vertical bars fill from the bottom, horizontal bars from the left.
var (
	lowerBlocks = []rune{' ', '▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}
	leftBlocks  = []rune{' ', '▏', '▎', '▍', '▌', '▋', '▊', '▉', '█'}
	sparkBlocks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}
)

// Segment is one colored run of a stacked bar, in eighths of a cell from the baseline.
type Segment struct {
	Color      string
	Start, End int // [Start, End) in eighths
}

// Stack converts values into contiguous segments scaled so top maps to cells*8 eighths. Each
// non-zero value keeps at least one eighth so small parts stay visible.
func Stack(values []float64, colors []string, top float64, cells int) []Segment {
	units := float64(cells * 8)
	var out []Segment
	run, pos := 0.0, 0
	for i, v := range values {
		if v <= 0 || top <= 0 {
			continue
		}
		run += v
		end := int(math.Round(run / top * units))
		end = min(max(end, pos+1), cells*8)
		if end <= pos {
			continue
		}
		out = append(out, Segment{colors[i], pos, end})
		pos = end
	}
	return out
}

// StackCell picks the glyph fill (0-8 eighths), foreground, and background for cell index i
// (counted from the baseline) of a stacked bar. The segment at the cell's base fills n eighths in
// fg; whatever occupies the rest of the cell shows as bg, so a boundary inside a cell stays exact.
func StackCell(segs []Segment, i int) (n int, fg, bg string) {
	lo, hi := i*8, i*8+8
	for k, s := range segs {
		if s.End <= lo || s.Start >= hi {
			continue
		}
		if s.Start > lo {
			// cell base is empty (can't happen for contiguous stacks, but be safe)
			return 0, "", ""
		}
		n = min(s.End, hi) - lo
		fg = s.Color
		if n < 8 && k+1 < len(segs) && segs[k+1].Start < hi {
			bg = segs[k+1].Color
		}
		return n, fg, bg
	}
	return 0, "", ""
}

// Spark draws values as one row of block glyphs, resampling to width by summing.
func Spark(values []float64, width int) []rune {
	vals := resample(values, width)
	mx := 0.0
	for _, v := range vals {
		mx = math.Max(mx, v)
	}
	out := make([]rune, len(vals))
	for i, v := range vals {
		if mx <= 0 || v <= 0 {
			out[i] = ' '
			continue
		}
		out[i] = sparkBlocks[min(int(v/mx*float64(len(sparkBlocks)-1)+0.5), len(sparkBlocks)-1)]
	}
	return out
}

// resample sums consecutive values so the result has at most n entries.
func resample(values []float64, n int) []float64 {
	if n <= 0 || len(values) <= n {
		return values
	}
	k := int(math.Ceil(float64(len(values)) / float64(n)))
	out := make([]float64, 0, n)
	for i := 0; i < len(values); i += k {
		s := 0.0
		for j := i; j < min(i+k, len(values)); j++ {
			s += values[j]
		}
		out = append(out, s)
	}
	return out
}
