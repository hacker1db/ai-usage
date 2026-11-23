package view

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

func Compact(v float64) string {
	a := math.Abs(v)
	if a < 1000 {
		return Exact(v)
	}
	units := []struct {
		div float64
		s   string
	}{{1e12, "T"}, {1e9, "B"}, {1e6, "M"}, {1e3, "K"}}
	for i, u := range units {
		if a >= u.div {
			x := math.Round(v/u.div*10) / 10
			if math.Abs(x) >= 1000 && i > 0 {
				u = units[i-1] //nolint:gosec // G602: guarded by i > 0.
				x = math.Round(v/u.div*10) / 10
			}
			s := strconv.FormatFloat(x, 'f', 1, 64)
			return strings.TrimSuffix(s, ".0") + u.s
		}
	}
	return Exact(v)
}

func Exact(v float64) string {
	n := int64(math.Round(v))
	s := strconv.FormatInt(n, 10)
	neg := n < 0
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func Duration(minutes float64) string {
	if minutes >= 60 {
		return fmt.Sprintf("%.1fh", minutes/60)
	}
	return fmt.Sprintf("%dm", int(math.Round(minutes)))
}

func Pct(share float64) string {
	switch {
	case math.IsNaN(share) || math.IsInf(share, 0):
		return "—"
	case share > 0 && share < 0.01:
		return "<1%"
	}
	return fmt.Sprintf("%.0f%%", share*100)
}

func DayLabel(t time.Time) string  { return t.Format("Mon Jan 2") }
func ShortDay(t time.Time) string  { return t.Format("Jan 2") }
func HourLabel(t time.Time) string { return t.Format("3pm") }
func HourFull(t time.Time) string  { return t.Format("Mon Jan 2, 3pm") }
func WhenLabel(t time.Time) string { return t.Format("Jan 2, 3:04pm") }

func NiceMax(v float64) float64 {
	if v <= 0 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10} {
		if m*p >= v {
			return m * p
		}
	}
	return 10 * p
}

func Ticks(top float64, n int) []float64 {
	out := make([]float64, n+1)
	for i := range out {
		out[i] = top / float64(n) * float64(i)
	}
	return out
}
