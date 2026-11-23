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

type costs struct {
	run    billing.Run
	path   string
	cache  string
	detect func() []string
	auto   func(endpoints []string) []billing.Account
	gap    time.Duration
	jitter func(time.Duration) time.Duration

	mu      sync.Mutex
	order   []string
	status  map[string]billing.Status
	warning string
}

const (
	backoffBase = 5 * time.Minute
	backoffMax  = 2 * time.Hour
	claimTTL    = 3 * time.Minute
	queryGap    = 15 * time.Second
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
		jitter: func(d time.Duration) time.Duration { return d + time.Duration(rand.Int64N(int64(d)/5+1)) }} //nolint:gosec // G404: poll jitter, not a security value.
}

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
				st, ok = d, true
			}
			if !ok {
				st = billing.Status{ID: p.acct.ID(), Label: label(p.acct, nil), Provider: p.acct.Provider, Mode: p.acct.Mode, Month: p.month}
			}
			if st.Error == "" && st.Report != nil {

				if fresh := st.Checked.Add(p.refresh); st.Next.IsZero() || fresh.Before(st.Next) {
					st.Next = fresh
				}
			}
			k.status[p.key] = st
			if now.Before(st.Next) || time.Since(claims[p.key]) < claimTTL {
				continue
			}
			claims[p.key] = time.Now()
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
			delete(k.status, key)
		}
	}
	changed := !slices.Equal(order, k.order) || warn != k.warning
	k.order, k.warning = order, warn
	k.mu.Unlock()
	if changed {
		wake()
	}

	for n, j := range due {
		if n > 0 {
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

type cacheFile struct {
	Status map[string]billing.Status `json:"status"`
	Claims map[string]time.Time      `json:"claims"`
}

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
		defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
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

func (c *Collector) SyncCosts(ctx context.Context) { c.costs.sync(ctx, time.Now(), func() {}) }
