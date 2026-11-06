package token

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strconv"
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
			src.Invalidate()
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

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
