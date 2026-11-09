// Package snapshot builds the dashboard's data: hourly rows (merged with the history store),
// sessions, source status, budgets, and the optional ai-work-report overlay. The web server and
// the TUI render the same Snapshot, and /api/usage serves it as JSON.
package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"ai-usage/internal/budget"
	"ai-usage/internal/history"
	"ai-usage/internal/ownership"
	"ai-usage/internal/report"
	"ai-usage/internal/sources"
	"ai-usage/internal/usage"
	"ai-usage/internal/workreport"
)

// Meta describes one collection.
type Meta struct {
	Generated       time.Time `json:"generated"`
	Since           time.Time `json:"since"`
	TZ              string    `json:"tz"`
	Days            int       `json:"days"`
	Interval        int       `json:"interval"`
	CollectMS       int64     `json:"collect_ms"`
	Config          string    `json:"config"`
	ConfigExists    bool      `json:"config_exists"`
	ConfigError     string    `json:"config_error"`
	CacheHits       int       `json:"cache_hits"`
	CacheMisses     int       `json:"cache_misses"`
	History         string    `json:"history"`
	HistoryRows     int       `json:"history_rows"`
	HistoryOnlyRows int       `json:"history_only_rows"`
	HistorySince    string    `json:"history_since"`
	HistoryWarning  string    `json:"history_warning"`
	WorkReportDir   string    `json:"work_report_dir"`
	Ownership       Ownership `json:"ownership"`
}

// Ownership describes the rules that labeled the sessions.
type Ownership struct {
	Source  string   `json:"source"`  // config path, or how the automatic rules were chosen
	Auto    bool     `json:"auto"`    // no config file
	Labels  []string `json:"labels"`  // display order
	Default string   `json:"default"` // label when no rule matches
	Warning string   `json:"warning"`
	// Profiles are the named profile filters from projects.yaml's profiles: key.
	Profiles []ownership.ProfileSet `json:"profiles,omitempty"`
}

// Snapshot is everything a dashboard view needs.
type Snapshot struct {
	Meta       Meta                     `json:"meta"`
	Sources    []sources.Status         `json:"sources"`
	Budgets    map[string]budget.Budget `json:"budgets"`
	Columns    []string                 `json:"columns"`
	Rows       []history.HourRow        `json:"rows"`
	Sessions   []usage.SessionRow       `json:"sessions"`
	WorkReport *workreport.Report       `json:"work_report"`
}

// Options configure a Collector.
type Options struct {
	Days          int           // history to load (2-400)
	Interval      time.Duration // between collections
	Sources       sources.Options
	ConfigPath    string // budgets (dashboard.json)
	HistoryPath   string // hourly store ("" = memory only)
	ReadOnly      bool   // never write the history store
	WorkReportDir string
	ProjectsPath  string // ownership rules (projects.yaml)
	Logger        *slog.Logger
}

// Collector rebuilds the snapshot on a timer or on request.
type Collector struct {
	opts    Options
	caches  *sources.Caches
	history *history.Store
	wake    chan struct{}

	mu    sync.RWMutex
	snap  *Snapshot
	body  []byte
	etag  string
	err   error
	ready chan struct{}
	once  sync.Once
	owner *ownership.Classifier
	types *ownership.Types // GitHub user-or-org answers, cached next to the history store
}

// New returns a collector; call Run to start collecting.
func New(o Options) *Collector {
	o.Days = min(max(o.Days, 2), 400)
	if o.Interval < 5*time.Second {
		o.Interval = 5 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	typesPath := ""
	if o.HistoryPath != "" && !o.ReadOnly {
		typesPath = filepath.Join(filepath.Dir(o.HistoryPath), "owners.json")
	}
	return &Collector{opts: o, caches: sources.NewCaches(), history: history.Open(o.HistoryPath, o.ReadOnly),
		wake: make(chan struct{}, 1), ready: make(chan struct{}), types: ownership.NewTypes(typesPath)}
}

// Run collects immediately, then on every interval or Wake, until ctx ends.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.opts.Interval)
	defer t.Stop()
	for {
		c.Refresh()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.wake:
		}
	}
}

// Wake asks Run to collect now.
func (c *Collector) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Ready is closed after the first collection finishes (successfully or not).
func (c *Collector) Ready() <-chan struct{} { return c.ready }

// Refresh collects once; on failure the last good snapshot keeps being served.
func (c *Collector) Refresh() {
	defer c.once.Do(func() { close(c.ready) })
	snap, err := c.build(time.Now())
	if err != nil {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		c.opts.Logger.Error("collect failed", "error", err)
		return
	}
	body, err := json.Marshal(snap)
	if err != nil {
		c.opts.Logger.Error("encode snapshot", "error", err)
		return
	}
	sum := sha256.Sum256(body)
	c.mu.Lock()
	c.snap, c.body, c.etag, c.err = snap, body, `"`+hex.EncodeToString(sum[:8])+`"`, nil
	c.mu.Unlock()
	m := snap.Meta
	c.opts.Logger.Info("collected", "rows", len(snap.Rows), "ms", m.CollectMS, "reread", m.CacheMisses,
		"cached", m.CacheHits, "history_only", m.HistoryOnlyRows)
}

// Current returns the latest snapshot, its JSON, and its ETag; snap is nil before the first success.
func (c *Collector) Current() (snap *Snapshot, body []byte, etag string, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snap, c.body, c.etag, c.err
}

// Session returns one session with its detail from the history store, labeled by the rules from
// the latest collection.
func (c *Collector) Session(key string) (usage.SessionRow, bool) {
	x, ok := c.history.Session(key)
	c.mu.RLock()
	cls := c.owner
	c.mu.RUnlock()
	if ok && cls != nil {
		cls.Apply(&x)
	}
	return x, ok
}

func (c *Collector) build(now time.Time) (*Snapshot, error) {
	started := time.Now()
	o := c.opts
	cutoff := report.Cutoff(now, o.Days)
	agg := usage.NewAggregator(cutoff, usage.Hourly)
	status, stats := sources.CollectAll(agg, o.Sources, c.caches)

	live := agg.Rows()
	rows := make([]history.HourRow, 0, len(live))
	for _, r := range live {
		rows = append(rows, history.FromUsage(r))
	}
	held, err := c.history.Merge(rows, agg.SessionRows(true))
	if err != nil {
		return nil, fmt.Errorf("save history: %w", err)
	}
	firstHour := cutoff.Format("2006-01-02T15") + ":00"
	kept := 0
	for k := range held {
		if k.Bucket >= firstHour {
			kept++
		}
	}
	budgets, budgetWarn := budget.Load(o.ConfigPath)
	_, cfgErr := os.Stat(o.ConfigPath)
	histRows, histSince := c.history.Stats()
	// Rules are reread every collection, so editing projects.yaml applies on the next refresh.
	cls := ownership.Load(o.ProjectsPath, c.types)
	sessions := c.history.SessionsSince(cutoff)
	for i := range sessions {
		cls.Apply(&sessions[i])
	}
	if err := c.types.Save(); err != nil {
		o.Logger.Warn("save owner types", "error", err)
	}
	c.mu.Lock()
	c.owner = cls
	c.mu.Unlock()
	return &Snapshot{
		Meta: Meta{
			Generated: now, Since: cutoff, TZ: now.Format("MST"), Days: o.Days, Interval: int(o.Interval / time.Second),
			CollectMS: time.Since(started).Milliseconds(), Config: tildeOr(o.ConfigPath), ConfigExists: o.ConfigPath != "" && cfgErr == nil,
			ConfigError: budgetWarn, CacheHits: stats.Hits, CacheMisses: stats.Misses,
			History: tildeOr(o.HistoryPath), HistoryRows: histRows, HistoryOnlyRows: kept, HistorySince: histSince,
			HistoryWarning: c.history.Warning, WorkReportDir: tildeOr(o.WorkReportDir),
			Ownership: Ownership{Source: cls.Source, Auto: cls.Auto, Labels: cls.Labels, Default: cls.Default,
				Warning: cls.Warning, Profiles: cls.Profiles},
		},
		Sources:    status,
		Budgets:    budgets,
		Columns:    history.Columns,
		Rows:       c.history.Since(firstHour),
		Sessions:   sessions,
		WorkReport: workreport.Load(o.WorkReportDir),
	}, nil
}

func tildeOr(p string) string {
	if p == "" {
		return ""
	}
	return usage.Tilde(p)
}

// Decode parses a /api/usage body (used by the TUI when a dashboard is already running).
func Decode(body []byte) (*Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, err
	}
	if len(s.Columns) != len(history.Columns) {
		return nil, fmt.Errorf("snapshot columns %v, want %v", s.Columns, history.Columns)
	}
	return &s, nil
}
