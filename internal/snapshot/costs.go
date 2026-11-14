package snapshot

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"ai-usage/internal/billing"
)

// costs keeps the latest provider billing reports and queries providers well inside their rate
// limits:
//
//   - each account and month is refreshed on its provider's cadence (billing.Account.Refresh):
//     Azure every 4 hours (its data refreshes about that often and its query quota is shared by
//     the whole tenant), Copilot hourly, and a closed month daily;
//   - queries run one at a time, spaced apart;
//   - a rate-limited or failed query backs off exponentially with jitter, honoring the provider's
//     retry-after when it gives one, and the last good report keeps being shown;
//   - with a cache path, results and back-off are shared on disk under a lock, so restarts, the
//     TUI, and a second dashboard reuse fresh results instead of querying again.
type costs struct {
	run    billing.Run
	path   string                                     // dashboard.json
	cache  string                                     // shared state file ("" = memory only)
	detect func() []string                            // Foundry endpoints from the local tools' configs
	auto   func(endpoints []string) []billing.Account // accounts when dashboard.json has no billing list
	gap    time.Duration                              // between consecutive provider queries
	jitter func(time.Duration) time.Duration          // randomizes back-off (replaced in tests)

	mu      sync.Mutex
	order   []string // account/month keys in display order
	status  map[string]billing.Status
	warning string
}

const (
	backoffBase = 5 * time.Minute  // first wait after a failure (GitHub asks for at least 1 minute)
	backoffMax  = 2 * time.Hour    // longest wait between attempts while failing
	claimTTL    = 3 * time.Minute  // another process's in-flight query blocks ours this long
	queryGap    = 15 * time.Second // between queries: Azure allows 12 per 10 s per tenant, shared
)

func newCosts(run billing.Run, path string, detect func() []string, auto func([]string) []billing.Account) *costs {
	if run == nil {
		run = billing.Command
	}
	if detect == nil {
		detect = func() []string { return nil }
	}
	if auto == nil {
		auto = func([]string) []billing.Account { return nil }
	}
	return &costs{run: run, path: path, detect: detect, auto: auto, gap: queryGap, status: map[string]billing.Status{},
		jitter: func(d time.Duration) time.Duration { return d + time.Duration(rand.Int64N(int64(d)/5+1)) }}
}

// loop rereads the billing config every minute and queries whatever is due, until ctx ends.
func (k *costs) loop(ctx context.Context, wake func()) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		k.sync(ctx, time.Now(), wake)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// backoff is the wait after the n-th consecutive failure: the provider's retry-after when given,
// else 5, 10, 20 … minutes up to 2 hours, with jitter so processes don't retry in lockstep.
func (k *costs) backoff(n int, after time.Duration) time.Duration {
	if after > 0 {
		return min(after, backoffMax) + time.Second
	}
	d := backoffBase << min(max(n-1, 0), 5)
	return k.jitter(min(d, backoffMax))
}

type job struct {
	key   string
	acct  billing.Account
	month string
}

// sync queries every account/month that is due.
func (k *costs) sync(ctx context.Context, now time.Time, wake func()) {
	cfg, warn := billing.LoadConfig(k.path)
	if !cfg.Explicit {
		cfg.Accounts = k.auto(k.detect())
	}
	for i, a := range cfg.Accounts {
		if a.Provider == "azure" && a.Subscription == "" && len(a.Resources) == 0 && len(a.Endpoints) == 0 {
			cfg.Accounts[i].Endpoints = k.detect()
		}
	}
	months := billing.Months(now)
	type planned struct {
		job
		refresh time.Duration
	}
	var order []string
	var plan []planned
	for _, a := range cfg.Accounts {
		for _, m := range months {
			key := a.ID() + "@" + m
			order = append(order, key)
			plan = append(plan, planned{job{key, a, m}, a.Refresh(m, months[0], cfg.Refresh)})
		}
	}

	var due []job
	k.shared(func(disk map[string]billing.Status, claims map[string]time.Time) {
		k.mu.Lock()
		defer k.mu.Unlock()
		for _, p := range plan {
			st, ok := k.status[p.key]
			if d, dok := disk[p.key]; dok && (!ok || d.Checked.After(st.Checked)) {
				st, ok = d, true // another process (or an earlier run) has newer results
			}
			if !ok {
				st = billing.Status{ID: p.acct.ID(), Label: label(p.acct, nil), Provider: p.acct.Provider, Mode: p.acct.Mode, Month: p.month}
			}
			if st.Error == "" && st.Report != nil {
				// cadence can change (config edit, month closing): never wait longer than it says
				if fresh := st.Checked.Add(p.refresh); st.Next.IsZero() || fresh.Before(st.Next) {
					st.Next = fresh
				}
			}
			k.status[p.key] = st
			if now.Before(st.Next) || time.Since(claims[p.key]) < claimTTL {
				continue
			}
			claims[p.key] = time.Now() // tell other processes this query is in flight (wall clock)
			due = append(due, p.job)
		}
	})

	k.mu.Lock()
	keep := map[string]bool{}
	for _, key := range order {
		keep[key] = true
	}
	for key := range k.status {
		if !keep[key] {
			delete(k.status, key) // account removed from config, or month rolled off
		}
	}
	changed := !slices.Equal(order, k.order) || warn != k.warning
	k.order, k.warning = order, warn
	k.mu.Unlock()
	if changed {
		wake()
	}

	for n, j := range due {
		if n > 0 { // one query at a time, spaced out
			select {
			case <-ctx.Done():
				return
			case <-time.After(k.gap):
			}
		}
		qctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		r, err := j.acct.Fetch(qctx, k.run, j.month)
		cancel()
		if ctx.Err() != nil {
			return
		}
		done := time.Now()
		k.mu.Lock()
		st := k.status[j.key]
		st.Checked = done
		if err != nil {
			// keep the last good report; a failure is never shown as zero
			st.Failures++
			st.Error, st.Fix, st.Retrying = err.Error(), billing.FixOf(err), billing.Transient(err)
			st.Next = done.Add(k.backoff(st.Failures, billing.RetryAfterOf(err)))
		} else {
			st.Failures, st.Error, st.Fix, st.Retrying, st.Report = 0, "", "", false, &r
			st.Next = done.Add(j.acct.Refresh(j.month, months[0], cfg.Refresh))
		}
		st.Label = label(j.acct, st.Report)
		k.status[j.key] = st
		k.mu.Unlock()
		k.shared(func(disk map[string]billing.Status, claims map[string]time.Time) {
			disk[j.key] = st
			delete(claims, j.key)
		})
		wake()
	}
}

// cacheFile is the on-disk shared state.
type cacheFile struct {
	Status map[string]billing.Status `json:"status"`
	Claims map[string]time.Time      `json:"claims"`
}

// shared runs f on the on-disk state under an exclusive lock and saves what f leaves. Without a
// cache path it runs f on throwaway maps, so the in-memory logic is the same.
func (k *costs) shared(f func(status map[string]billing.Status, claims map[string]time.Time)) {
	c := cacheFile{Status: map[string]billing.Status{}, Claims: map[string]time.Time{}}
	if k.cache == "" {
		f(c.Status, c.Claims)
		return
	}
	if err := os.MkdirAll(filepath.Dir(k.cache), 0o700); err != nil {
		f(c.Status, c.Claims)
		return
	}
	lock, err := os.OpenFile(k.cache+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err == nil {
		defer lock.Close()
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
		defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	}
	if raw, err := os.ReadFile(k.cache); err == nil {
		_ = json.Unmarshal(raw, &c)
		if c.Status == nil {
			c.Status = map[string]billing.Status{}
		}
		if c.Claims == nil {
			c.Claims = map[string]time.Time{}
		}
	}
	f(c.Status, c.Claims)
	for key, t := range c.Claims {
		if time.Since(t) > claimTTL {
			delete(c.Claims, key)
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return
	}
	tmp := k.cache + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, k.cache)
	}
}

// label names an account, filling in the resolved gh login or subscription when not configured.
func label(a billing.Account, r *billing.Report) string {
	switch {
	case a.Label != "":
		return a.Label
	case a.Provider == "azure" && a.Subscription == "" && len(a.Resources) == 0 && len(a.Endpoints) > 0:
		return "Azure Foundry · " + strings.Join(a.Endpoints, ", ")
	case r == nil:
		return a.Name()
	case a.Provider == "copilot" && a.GHAccount == "":
		_, login, _ := strings.Cut(r.Account, "/")
		return "Copilot · " + login
	}
	return a.Name()
}

// current returns the statuses in display order and the config warning.
func (k *costs) current() ([]billing.Status, string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]billing.Status, 0, len(k.order))
	for _, key := range k.order {
		if st, ok := k.status[key]; ok {
			out = append(out, st)
		}
	}
	return out, k.warning
}

// SyncCosts queries billing once, synchronously, for single-shot runs (tui -once) that never call Run.
func (c *Collector) SyncCosts(ctx context.Context) { c.costs.sync(ctx, time.Now(), func() {}) }
