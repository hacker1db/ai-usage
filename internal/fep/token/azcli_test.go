package token

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

type fakeAz struct {
	calls  int
	args   [][]string
	stdout string
	stderr string
	err    error
}

func (f *fakeAz) run(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
	f.calls++
	f.args = append(f.args, args)
	return []byte(f.stdout), []byte(f.stderr), f.err
}

func newTestSource(f *fakeAz, now *time.Time) *AzCLI {
	a := NewAzCLI("az", "https://ai.azure.com/.default", "")
	a.Run = f.run
	a.Now = func() time.Time { return *now }
	return a
}

func TestTokenParsesExpiry(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.Local)
	tests := []struct {
		name    string
		stdout  string
		wantExp time.Time
		wantErr bool
	}{
		{
			name:    "expires_on epoch",
			stdout:  `{"accessToken":"tok","expires_on":` + itoa(now.Add(time.Hour).Unix()) + `}`,
			wantExp: now.Add(time.Hour),
		},
		{
			name:    "expiresOn local timestamp",
			stdout:  `{"accessToken":"tok","expiresOn":"` + now.Add(time.Hour).Format("2006-01-02 15:04:05.000000") + `"}`,
			wantExp: now.Add(time.Hour),
		},
		{name: "missing expiry", stdout: `{"accessToken":"tok"}`, wantErr: true},
		{name: "missing token", stdout: `{"expires_on":1}`, wantErr: true},
		{name: "not json", stdout: `nope`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := now
			src := newTestSource(&fakeAz{stdout: tt.stdout}, &n)
			tok, err := src.Token(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tok != "tok" || !src.expiresAt.Equal(tt.wantExp) {
				t.Fatalf("got %q exp %v, want tok exp %v", tok, src.expiresAt, tt.wantExp)
			}
		})
	}
}

func TestTokenCachingAndRefresh(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := &fakeAz{stdout: `{"accessToken":"tok","expires_on":` + itoa(now.Add(time.Hour).Unix()) + `}`}
	src := newTestSource(f, &now)
	ctx := context.Background()

	steps := []struct {
		advance   time.Duration
		invalid   bool
		wantCalls int
	}{
		{wantCalls: 1},
		{advance: 30 * time.Minute, wantCalls: 1},
		{advance: 26 * time.Minute, wantCalls: 2},
		{invalid: true, wantCalls: 3},
	}
	for i, s := range steps {
		now = now.Add(s.advance)
		if s.invalid {
			src.Invalidate("tok")
		}
		if _, err := src.Token(ctx); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if f.calls != s.wantCalls {
			t.Fatalf("step %d: az calls = %d, want %d", i, f.calls, s.wantCalls)
		}
	}
}

func TestTokenErrorClassification(t *testing.T) {
	tests := []struct {
		name      string
		stderr    string
		wantLogin bool
	}{
		{name: "not logged in", stderr: "ERROR: Please run 'az login' to setup account.", wantLogin: true},
		{name: "aadsts expired", stderr: "AADSTS700082: The refresh token has expired", wantLogin: true},
		{name: "other failure", stderr: "ERROR: network unreachable", wantLogin: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			src := newTestSource(&fakeAz{stderr: tt.stderr, err: &exec.ExitError{}}, &now)
			_, err := src.Token(context.Background())
			if err == nil {
				t.Fatal("expected error")
			}
			if got := errors.Is(err, ErrLoginRequired); got != tt.wantLogin {
				t.Fatalf("ErrLoginRequired = %v, want %v (%v)", got, tt.wantLogin, err)
			}
		})
	}
}

func TestTokenPassesTenant(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := &fakeAz{stdout: `{"accessToken":"tok","expires_on":` + itoa(now.Add(time.Hour).Unix()) + `}`}
	src := newTestSource(f, &now)
	src.Tenant = "contoso"
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.args[0], "--tenant") || !slices.Contains(f.args[0], "contoso") {
		t.Fatalf("args missing tenant: %v", f.args[0])
	}
}

func TestInvalidateOnlyDropsRejectedToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	exp := itoa(now.Add(time.Hour).Unix())
	f := &fakeAz{stdout: `{"accessToken":"tok-1","expires_on":` + exp + `}`}
	src := newTestSource(f, &now)
	ctx := context.Background()

	if tok, _ := src.Token(ctx); tok != "tok-1" {
		t.Fatalf("token = %q", tok)
	}
	f.stdout = `{"accessToken":"tok-2","expires_on":` + exp + `}`
	src.Invalidate("tok-1")
	if tok, _ := src.Token(ctx); tok != "tok-2" || f.calls != 2 {
		t.Fatalf("after invalidating tok-1: token = %q, az calls = %d", tok, f.calls)
	}
	// A late 401 for the old token must not discard its replacement.
	src.Invalidate("tok-1")
	if tok, _ := src.Token(ctx); tok != "tok-2" || f.calls != 2 {
		t.Fatalf("stale invalidate: token = %q, az calls = %d", tok, f.calls)
	}
}

// gatedAz blocks every az call until release is closed and counts calls.
type gatedAz struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
	stdout  string
	stderr  string
	err     error
}

func newGatedAz(stdout string) *gatedAz {
	return &gatedAz{stdout: stdout, started: make(chan struct{}, 16), release: make(chan struct{})}
}

func (g *gatedAz) run(ctx context.Context, _ string, _ ...string) ([]byte, []byte, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	g.started <- struct{}{}
	select {
	case <-g.release:
		return []byte(g.stdout), []byte(g.stderr), g.err
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func (g *gatedAz) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func TestTokenSharesOneFlight(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	g := newGatedAz(`{"accessToken":"tok","expires_on":` + itoa(now.Add(time.Hour).Unix()) + `}`)
	src := newTestSource(&fakeAz{}, &now)
	src.Run = g.run

	const n = 10
	results := make(chan string, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			tok, err := src.Token(context.Background())
			if err != nil {
				t.Error(err)
			}
			results <- tok
		})
	}
	<-g.started
	// Give the other callers time to join the flight before releasing it.
	time.Sleep(50 * time.Millisecond)
	close(g.release)
	wg.Wait()
	close(results)
	for tok := range results {
		if tok != "tok" {
			t.Fatalf("token = %q", tok)
		}
	}
	if c := g.count(); c != 1 {
		t.Fatalf("az calls = %d, want 1", c)
	}
}

func TestTokenSharesFlightError(t *testing.T) {
	now := time.Now()
	g := newGatedAz("")
	g.stderr, g.err = "ERROR: Please run 'az login' to setup account.", &exec.ExitError{}
	src := newTestSource(&fakeAz{}, &now)
	src.Run = g.run

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := src.Token(context.Background())
			errs <- err
		}()
	}
	<-g.started
	time.Sleep(50 * time.Millisecond)
	close(g.release)
	for range 2 {
		if err := <-errs; !errors.Is(err, ErrLoginRequired) {
			t.Fatalf("err = %v, want ErrLoginRequired", err)
		}
	}
	if c := g.count(); c != 1 {
		t.Fatalf("az calls = %d, want 1", c)
	}
}

func TestTokenWaitHonorsCallerContext(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	g := newGatedAz(`{"accessToken":"tok","expires_on":` + itoa(now.Add(time.Hour).Unix()) + `}`)
	src := newTestSource(&fakeAz{}, &now)
	src.Run = g.run

	// The first caller starts az and then gives up; that must not abort the shared acquisition.
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := src.Token(leaderCtx)
		leaderErr <- err
	}()
	<-g.started

	// A waiter with a short deadline returns on its own deadline while az is still running.
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelWait()
	start := time.Now()
	if _, err := src.Token(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter err = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("waiter blocked for %v despite its deadline", d)
	}

	cancelLeader()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err = %v, want Canceled", err)
	}

	// A later caller joins the still-running flight and gets its result.
	got := make(chan string, 1)
	go func() {
		tok, _ := src.Token(context.Background())
		got <- tok
	}()
	time.Sleep(50 * time.Millisecond)
	close(g.release)
	if tok := <-got; tok != "tok" {
		t.Fatalf("token = %q", tok)
	}
	if c := g.count(); c != 1 {
		t.Fatalf("az calls = %d, want 1 (leader cancel must not restart az)", c)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
