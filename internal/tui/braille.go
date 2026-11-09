package tui

// Braille is a dot canvas: each cell holds a 2x4 grid of dots (U+2800 block), so a W x H cell
// canvas plots 2W x 4H points. A cell takes the color of the last dot drawn in it.
type Braille struct {
	W, H  int
	bits  []uint8
	color []string
}

// NewBraille returns a w x h cell canvas.
func NewBraille(w, h int) *Braille {
	return &Braille{W: w, H: h, bits: make([]uint8, w*h), color: make([]string, w*h)}
}

// dot bit for (dx, dy) inside a cell: columns 0-1, rows 0-3 (Unicode braille numbering).
var brailleBit = [4][2]uint8{{0x01, 0x08}, {0x02, 0x10}, {0x04, 0x20}, {0x40, 0x80}}

// Set plots dot (x, y) with 0,0 at the top left; points outside the canvas are ignored.
func (b *Braille) Set(x, y int, color string) {
	if x < 0 || y < 0 || x >= b.W*2 || y >= b.H*4 {
		return
	}
	i := (y/4)*b.W + x/2
	b.bits[i] |= brailleBit[y%4][x%2]
	b.color[i] = color
}

// Line draws a segment between dots; dotted keeps every other point.
func (b *Braille) Line(x0, y0, x1, y1 int, color string, dotted bool) {
	dx, dy := abs(x1-x0), -abs(y1-y0)
	sx, sy := sign(x1-x0), sign(y1-y0)
	err, n := dx+dy, 0
	for {
		if !dotted || n%2 == 0 {
			b.Set(x0, y0, color)
		}
		n++
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

// Cell returns the braille rune and color of cell (cx, cy); an empty cell is a space.
func (b *Braille) Cell(cx, cy int) (rune, string) {
	i := cy*b.W + cx
	if b.bits[i] == 0 {
		return ' ', ""
	}
	return rune(0x2800 + int(b.bits[i])), b.color[i]
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func sign(v int) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}
