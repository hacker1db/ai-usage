package history

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"ai-usage/internal/usage"
)

var Columns = []string{"hour", "tool", "profile", "model", "requests", "input", "cached", "cache_write",
	"output", "reasoning", "total", "sessions", "copilot_aiu", "cache_write_1h"}

var legacyColumns = Columns[:13]

type HourRow struct {
	Hour    string
	Tool    string
	Profile string
	Model   string
	usage.Metrics
	Sessions   int
	CopilotAIU float64
	T          time.Time
}

func (r HourRow) Key() usage.Key {
	return usage.Key{Bucket: r.Hour, Tool: r.Tool, Profile: r.Profile, Model: r.Model}
}

func FromUsage(r usage.Row) HourRow {
	h := HourRow{Hour: r.Bucket, Tool: r.Tool, Profile: r.Profile, Model: r.Model, Metrics: r.Metrics,
		Sessions: r.Sessions, CopilotAIU: math.Round(r.CopilotAIU*1e4) / 1e4}
	h.T = parseHour(h.Hour)
	return h
}

func parseHour(s string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02T15:04", s, time.Local)
	return t
}

func (r HourRow) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{r.Hour, r.Tool, r.Profile, r.Model, r.Requests, r.Input, r.Cached, r.CacheWrite,
		r.Output, r.Reasoning, r.Total, r.Sessions, r.CopilotAIU, r.CacheWrite1h})
}

func (r *HourRow) UnmarshalJSON(b []byte) error {
	var a []json.RawMessage
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	if len(a) != len(Columns) && len(a) != len(legacyColumns) {
		return fmt.Errorf("history row has %d columns, want %d", len(a), len(Columns))
	}
	strs := []*string{&r.Hour, &r.Tool, &r.Profile, &r.Model}
	for i, p := range strs {
		if err := json.Unmarshal(a[i], p); err != nil {
			return fmt.Errorf("history row column %s: %w", Columns[i], err)
		}
	}
	ints := []*int64{&r.Requests, &r.Input, &r.Cached, &r.CacheWrite, &r.Output, &r.Reasoning, &r.Total}
	for i, p := range ints {
		var n usage.Int
		_ = n.UnmarshalJSON(a[4+i])
		*p = int64(n)
	}
	var s usage.Int
	_ = s.UnmarshalJSON(a[11])
	r.Sessions = int(s)
	var f usage.Float
	_ = f.UnmarshalJSON(a[12])
	r.CopilotAIU = float64(f)
	r.CacheWrite1h = 0
	if len(a) > 13 {
		var n usage.Int
		_ = n.UnmarshalJSON(a[13])
		r.CacheWrite1h = int64(n)
	}
	r.T = parseHour(r.Hour)
	return nil
}

type file struct {
	Columns  []string           `json:"columns"`
	Rows     []HourRow          `json:"rows"`
	Sessions []usage.SessionRow `json:"sessions"`
}

type Store struct {
	path     string
	readOnly bool
	mu       sync.RWMutex
	rows     map[usage.Key]HourRow
	sessions map[string]usage.SessionRow
	Warning  string
}

func Open(path string, readOnly bool) *Store {
	s := &Store{path: path, readOnly: readOnly, rows: map[usage.Key]HourRow{}, sessions: map[string]usage.SessionRow{}}
	if path == "" {
		return s
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s
	}
	var f file
	if err == nil {
		err = json.Unmarshal(raw, &f)
	}
	switch {
	case err != nil:
		s.Warning = fmt.Sprintf("could not read %s: %v; starting fresh", usage.Tilde(path), err)
	case !slices.Equal(f.Columns, Columns) && !slices.Equal(f.Columns, legacyColumns):
		s.Warning = fmt.Sprintf("column layout changed in %s; starting fresh", usage.Tilde(path))
	default:
		for _, r := range f.Rows {
			s.rows[r.Key()] = r
		}
		for _, x := range f.Sessions {
			s.sessions[x.Key().String()] = x
		}
	}
	return s
}

func (s *Store) Path() string { return s.path }

func (s *Store) Merge(live []HourRow, liveSessions []usage.SessionRow) (map[usage.Key]struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, x := range liveSessions {
		k := x.Key().String()
		old, ok := s.sessions[k]
		if !ok || (x.Requests >= old.Requests && !reflect.DeepEqual(x, old)) {
			s.sessions[k] = x
			changed = true
		}
	}
	held, seen := map[usage.Key]struct{}{}, map[usage.Key]struct{}{}
	for _, r := range live {
		k := r.Key()
		seen[k] = struct{}{}
		old, ok := s.rows[k]
		if ok && r.Requests < old.Requests {
			held[k] = struct{}{}
			continue
		}
		if !ok || !sameRow(old, r) {
			s.rows[k] = r
			changed = true
		}
	}
	for k := range s.rows {
		if _, ok := seen[k]; !ok {
			held[k] = struct{}{}
		}
	}
	if changed {
		return held, s.save()
	}
	return held, nil
}

func sameRow(a, b HourRow) bool {
	return a.Metrics == b.Metrics && a.Sessions == b.Sessions && a.CopilotAIU == b.CopilotAIU
}

func (s *Store) save() error {
	if s.path == "" || s.readOnly {
		return nil
	}
	f := file{Columns: Columns, Rows: make([]HourRow, 0, len(s.rows)), Sessions: make([]usage.SessionRow, 0, len(s.sessions))}
	for _, r := range s.rows {
		f.Rows = append(f.Rows, r)
	}
	sort.Slice(f.Rows, func(i, j int) bool { return f.Rows[i].Key().Less(f.Rows[j].Key()) })
	for _, x := range s.sessions {
		f.Sessions = append(f.Sessions, x)
	}
	sort.Slice(f.Sessions, func(i, j int) bool { return f.Sessions[i].Last < f.Sessions[j].Last })
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) Since(firstHour string) []HourRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]HourRow, 0, len(s.rows))
	for _, r := range s.rows {
		if r.Hour >= firstHour {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key().Less(out[j].Key()) })
	return out
}

func (s *Store) Stats() (rows int, earliest string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for k := range s.rows {
		if earliest == "" || k.Bucket < earliest {
			earliest = k.Bucket
		}
	}
	return len(s.rows), earliest
}

func (s *Store) SessionsSince(cutoff time.Time) []usage.SessionRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]usage.SessionRow, 0, len(s.sessions))
	for _, x := range s.sessions {
		if !x.LastTime().Before(cutoff) {
			x.Detail = nil
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastTime().After(out[j].LastTime()) })
	return out
}

func (s *Store) Session(key string) (usage.SessionRow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	x, ok := s.sessions[key]
	return x, ok
}
