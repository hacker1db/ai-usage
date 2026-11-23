package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"time"
)

type Str string

func (s *Str) UnmarshalJSON(b []byte) error {
	var v string
	if json.Unmarshal(b, &v) == nil {
		*s = Str(v)
	}
	return nil
}

type Int int64

func (n *Int) UnmarshalJSON(b []byte) error {
	var v json.Number
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	if i, err := v.Int64(); err == nil {
		*n = Int(i)
	} else if f, err := v.Float64(); err == nil {
		*n = Int(int64(f))
	}
	return nil
}

type Float float64

func (f *Float) UnmarshalJSON(b []byte) error {
	var v float64
	if json.Unmarshal(b, &v) == nil {
		*f = Float(v)
	}
	return nil
}

type Bool bool

func (x *Bool) UnmarshalJSON(b []byte) error {
	var v bool
	if json.Unmarshal(b, &v) == nil {
		*x = Bool(v)
	}
	return nil
}

type Stamp struct{ time.Time }

func (s *Stamp) UnmarshalJSON(b []byte) error {
	var str string
	if json.Unmarshal(b, &str) == nil {
		s.Time = ParseTime(str)
		return nil
	}
	var f float64
	if json.Unmarshal(b, &f) == nil {
		sec, frac := int64(f), f-float64(int64(f))
		s.Time = time.Unix(sec, int64(frac*1e9)).UTC()
	}
	return nil
}

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
}

var naiveLayouts = []string{
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04",
}

func ParseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t
		}
	}
	for _, l := range naiveLayouts {
		if t, err := time.ParseInLocation(l, s, time.UTC); err == nil {
			return t
		}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Unix(int64(f), 0).UTC()
	}
	return time.Time{}
}

func EachJSONLine(path string, fn func(line []byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 1 && line[0] == '{' {
			fn(line)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
