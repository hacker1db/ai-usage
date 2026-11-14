// Package history keeps hourly usage rows and session summaries on disk so usage survives tools
// deleting old transcripts (Claude Code's cleanupPeriodDays, deleted Codex rollouts).
//
// The file layout matches the earlier Python dashboard's data/hourly.json, so an existing store
// carries over unchanged.
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

// Columns is the row layout of the store and of the /api/usage snapshot.
var Columns = []string{"hour", "tool", "profile", "model", "requests", "input", "cached", "cache_write",
	"output", "reasoning", "total", "sessions", "copilot_aiu", "cache_write_1h"}

// legacyColumns is the layout before cache_write_1h; stores in it load with that column at 0 and
// are rewritten in the current layout on the next save.
var legacyColumns = Columns[:13]

// HourRow is one (local hour, tool, profile, model) row, stored as a JSON array in Columns order.
type HourRow struct {
	Hour    string // local 2006-01-02T15:00
	Tool    string
	Profile string
	Model   string
	usage.Metrics
	Sessions   int
	CopilotAIU float64
	T          time.Time // Hour parsed in local time; not stored
}

// Key is the row's identity.
func (r HourRow) Key() usage.Key {
	return usage.Key{Bucket: r.Hour, Tool: r.Tool, Profile: r.Profile, Model: r.Model}
}

// FromUsage converts an hourly aggregator row.
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

// Store is the on-disk history. It is safe for concurrent use.
type Store struct {
	path     string
	readOnly bool
	mu       sync.RWMutex
	rows     map[usage.Key]HourRow
	sessions map[string]usage.SessionRow
	Warning  string // set when an existing file couldn't be used
}

// Open loads the store at path ("" keeps history in memory only). A read-only store never writes,
// which lets a second process (the TUI) use history without racing the dashboard's writes.
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

// Path is the store's file ("" for in-memory).
func (s *Store) Path() string { return s.path }

// Merge folds a live scan into the store and returns the keys whose stored row outlived (part of)
// its transcripts. A past hour only grows: a live row with fewer requests than the stored one means
// transcripts were deleted, so the stored row is kept. Stored rows the scan no longer sees at all
// are kept as they are. Sessions follow the same rule on their request counts.
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

// Since returns rows at or after firstHour (local 2006-01-02T15:00), sorted by key.
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

// Stats returns the number of stored rows and the earliest stored hour.
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

// SessionsSince returns sessions active at or after cutoff, newest first, without detail.
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

// Session returns one stored session with its detail.
func (s *Store) Session(key string) (usage.SessionRow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	x, ok := s.sessions[key]
	return x, ok
}
