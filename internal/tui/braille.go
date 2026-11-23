package tui

type Braille struct {
	W, H  int
	bits  []uint8
	color []string
}

func NewBraille(w, h int) *Braille {
	return &Braille{W: w, H: h, bits: make([]uint8, w*h), color: make([]string, w*h)}
}

var brailleBit = [4][2]uint8{{0x01, 0x08}, {0x02, 0x10}, {0x04, 0x20}, {0x40, 0x80}}

func (b *Braille) Set(x, y int, color string) {
	if x < 0 || y < 0 || x >= b.W*2 || y >= b.H*4 {
		return
	}
	i := (y/4)*b.W + x/2
	b.bits[i] |= brailleBit[y%4][x%2]
	b.color[i] = color
}

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

func (b *Braille) Cell(cx, cy int) (rune, string) {
	i := cy*b.W + cx
	if b.bits[i] == 0 {
		return ' ', ""
	}
	return 0x2800 + rune(b.bits[i]), b.color[i]
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
