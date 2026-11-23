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

	"ai-usage/internal/billing"
	"ai-usage/internal/budget"
	"ai-usage/internal/history"
	"ai-usage/internal/ownership"
	"ai-usage/internal/report"
	"ai-usage/internal/sources"
	"ai-usage/internal/usage"
	"ai-usage/internal/workreport"
)

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
	BillingWarning  string    `json:"billing_warning,omitempty"`
	Ownership       Ownership `json:"ownership"`
}

type Ownership struct {
	Source  string   `json:"source"`
	Auto    bool     `json:"auto"`
	Labels  []string `json:"labels"`
	Default string   `json:"default"`
	Warning string   `json:"warning"`

	Profiles []ownership.ProfileSet `json:"profiles,omitempty"`
}

type Snapshot struct {
	Meta       Meta                     `json:"meta"`
	Sources    []sources.Status         `json:"sources"`
	Budgets    map[string]budget.Budget `json:"budgets"`
	Columns    []string                 `json:"columns"`
	Rows       []history.HourRow        `json:"rows"`
	Sessions   []usage.SessionRow       `json:"sessions"`
	WorkReport *workreport.Report       `json:"work_report"`

	Costs []billing.Status `json:"costs,omitempty"`
}

type Options struct {
	Days          int
	Interval      time.Duration
	Sources       sources.Options
	ConfigPath    string
	HistoryPath   string
	ReadOnly      bool
	WorkReportDir string
	ProjectsPath  string
	Logger        *slog.Logger
	BillingRun    billing.Run

	BillingCache string
}

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
	types *ownership.Types
	costs *costs
}

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
	c := &Collector{opts: o, caches: sources.NewCaches(), history: history.Open(o.HistoryPath, o.ReadOnly),
		wake: make(chan struct{}, 1), ready: make(chan struct{}), types: ownership.NewTypes(typesPath),
		costs: newCosts(o.BillingRun, o.ConfigPath, func() []string { return sources.FoundryEndpoints(o.Sources) },
			func(endpoints []string) []billing.Account {
				return billing.AutoAccounts(endpoints, sources.CopilotUsed(o.Sources), billing.OnPath)
			})}
	c.costs.cache = o.BillingCache
	return c
}

func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.opts.Interval)
	defer t.Stop()
	go c.costs.loop(ctx, c.Wake)
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

func (c *Collector) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Collector) Ready() <-chan struct{} { return c.ready }

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

func (c *Collector) Current() (snap *Snapshot, body []byte, etag string, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snap, c.body, c.etag, c.err
}

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
	costs, costWarn := c.costs.current()
	_, cfgErr := os.Stat(o.ConfigPath)
	histRows, histSince := c.history.Stats()

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
			BillingWarning: costWarn,
			Ownership: Ownership{Source: cls.Source, Auto: cls.Auto, Labels: cls.Labels, Default: cls.Default,
				Warning: cls.Warning, Profiles: cls.Profiles},
		},
		Sources:    status,
		Budgets:    budgets,
		Columns:    history.Columns,
		Rows:       c.history.Since(firstHour),
		Sessions:   sessions,
		WorkReport: workreport.Load(o.WorkReportDir),
		Costs:      costs,
	}, nil
}

func tildeOr(p string) string {
	if p == "" {
		return ""
	}
	return usage.Tilde(p)
}

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
