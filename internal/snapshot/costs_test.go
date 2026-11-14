package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ai-usage/internal/billing"
)

// fakeGH answers gh's identity probe and counts billing queries; failWith makes them fail.
type fakeGH struct {
	calls    atomic.Int32
	failWith atomic.Value // string: CLI stderr to fail with, "" = succeed
}

func (f *fakeGH) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if strings.Contains(strings.Join(args, " "), "--include /user") {
		return []byte("HTTP/2.0 200 OK\nX-Oauth-Scopes: repo, user\n\n{\"login\":\"me\"}"), nil
	}
	f.calls.Add(1)
	if s, _ := f.failWith.Load().(string); s != "" {
		return nil, &billing.CLIError{Name: "gh", Stderr: s, Err: errors.New("exit 1")}
	}
	return []byte(`{"usageItems":[{"product":"Copilot","model":"m","netAmount":2.5}]}`), nil
}

func copilotConfig(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "dashboard.json")
	if err := os.WriteFile(p, []byte(`{"billing":[{"provider":"copilot"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestCosts(f *fakeGH, cfg, cache string) *costs {
	k := newCosts(f.run, cfg, nil, nil)
	k.gap, k.cache = 0, cache
	k.jitter = func(d time.Duration) time.Duration { return d }
	return k
}

func TestCostsCadence(t *testing.T) {
	f := &fakeGH{}
	k := newTestCosts(f, copilotConfig(t), "")
	now := time.Now()
	wakes := 0
	k.sync(context.Background(), now, func() { wakes++ })
	got, _ := k.current()
	if f.calls.Load() != 2 || len(got) != 2 || got[0].Report == nil || got[0].Label != "Copilot · me" || wakes == 0 {
		t.Fatalf("first sync: calls=%d %+v", f.calls.Load(), got)
	}
	k.sync(context.Background(), now.Add(time.Minute), func() {})
	if f.calls.Load() != 2 {
		t.Fatalf("requeried before refresh: %d", f.calls.Load())
	}
	// After an hour Copilot's month in progress refreshes; the closed month waits a day.
	k.sync(context.Background(), now.Add(billing.CopilotRefresh+time.Minute), func() {})
	if f.calls.Load() != 3 {
		t.Fatalf("after an hour: calls=%d, want only the current month requeried", f.calls.Load())
	}
	k.sync(context.Background(), now.Add(billing.ClosedRefresh+time.Minute), func() {})
	if f.calls.Load() != 5 {
		t.Fatalf("after a day: calls=%d", f.calls.Load())
	}
	// Removing the account drops its rows.
	if err := os.WriteFile(k.path, []byte(`{"billing":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	k.sync(context.Background(), now, func() {})
	if got, _ = k.current(); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestCostsBackOffKeepsLastGoodReport(t *testing.T) {
	f := &fakeGH{}
	k := newTestCosts(f, copilotConfig(t), "")
	now := time.Now()
	k.sync(context.Background(), now, func() {})
	f.failWith.Store("gh: API rate limit exceeded for user (HTTP 429)")
	at := now.Add(billing.ClosedRefresh + time.Minute) // both months due
	k.sync(context.Background(), at, func() {})
	got, _ := k.current()
	st := got[0]
	if f.calls.Load() != 4 || st.Report == nil || !st.Retrying || st.Failures != 1 {
		t.Fatalf("rate limit: calls=%d %+v", f.calls.Load(), st)
	}
	if wait := st.Next.Sub(st.Checked); wait != 5*time.Minute {
		t.Fatalf("first back-off = %v, want 5m", wait)
	}
	// Nothing is retried inside the back-off window, however often sync runs.
	for i := 0; i < 4; i++ {
		k.sync(context.Background(), time.Now().Add(time.Minute), func() {})
	}
	if f.calls.Load() != 4 {
		t.Fatalf("retried during back-off: %d", f.calls.Load())
	}
	// The next failure doubles the wait; a success clears it.
	k.sync(context.Background(), time.Now().Add(6*time.Minute), func() {})
	got, _ = k.current()
	if got[0].Failures != 2 || got[0].Next.Sub(got[0].Checked) != 10*time.Minute {
		t.Fatalf("second back-off %+v", got[0])
	}
	f.failWith.Store("")
	k.sync(context.Background(), time.Now().Add(11*time.Minute), func() {})
	got, _ = k.current()
	if got[0].Failures != 0 || got[0].Error != "" || got[0].Next.Sub(got[0].Checked) != billing.CopilotRefresh {
		t.Fatalf("after recovery %+v", got[0])
	}
}

func TestCostsHonorsRetryAfter(t *testing.T) {
	f := &fakeGH{}
	f.failWith.Store("gh: You have exceeded a secondary rate limit (HTTP 403)\nRetry-After: 120")
	k := newTestCosts(f, copilotConfig(t), "")
	k.sync(context.Background(), time.Now(), func() {})
	got, _ := k.current()
	if wait := got[0].Next.Sub(got[0].Checked); wait != 121*time.Second || !got[0].Retrying {
		t.Fatalf("retry-after wait = %v %+v", wait, got[0])
	}
}

// Two processes (say the dashboard and the TUI) share results on disk: the second one doesn't
// query providers while the first one's results are fresh.
func TestCostsSharedAcrossProcesses(t *testing.T) {
	f := &fakeGH{}
	cfg := copilotConfig(t)
	cache := filepath.Join(t.TempDir(), "billing.json")
	now := time.Now()
	newTestCosts(f, cfg, cache).sync(context.Background(), now, func() {})
	second := newTestCosts(f, cfg, cache)
	second.sync(context.Background(), now.Add(time.Minute), func() {})
	got, _ := second.current()
	if f.calls.Load() != 2 || len(got) != 2 || got[0].Report == nil {
		t.Fatalf("second process requeried: calls=%d %+v", f.calls.Load(), got)
	}
	// An in-flight claim by another process also holds queries back.
	third := newTestCosts(f, cfg, cache)
	third.shared(func(_ map[string]billing.Status, claims map[string]time.Time) {
		for _, st := range got {
			claims[st.ID+"@"+st.Month] = time.Now()
		}
	})
	third.sync(context.Background(), now.Add(billing.ClosedRefresh+time.Minute), func() {})
	if f.calls.Load() != 2 {
		t.Fatalf("queried while another process held the claim: %d", f.calls.Load())
	}
}

func TestAzureRefreshIsFourHours(t *testing.T) {
	a := billing.Account{Provider: "azure"}
	if a.Refresh("2026-11", "2026-11", 0) != 4*time.Hour || a.Refresh("2026-10", "2026-11", 0) != 24*time.Hour {
		t.Fatal("azure cadence")
	}
	if a.Refresh("2026-11", "2026-11", time.Hour) != time.Hour {
		t.Fatal("billing_refresh_minutes should override the month in progress")
	}
}
