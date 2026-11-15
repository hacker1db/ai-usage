// Package token acquires Entra access tokens by shelling out to the Azure CLI.
package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrLoginRequired means the Azure CLI has no usable session; the fix is `az login`.
var ErrLoginRequired = errors.New("azure cli login required")

// Runner executes a command and returns its stdout and stderr separately.
type Runner func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

type Source interface {
	Token(ctx context.Context) (string, error)
	// Invalidate drops the cached token if it is still the one the upstream rejected.
	Invalidate(rejected string)
}

type AzCLI struct {
	AzPath      string
	Scope       string
	Tenant      string
	RefreshSkew time.Duration
	ExecTimeout time.Duration
	Run         Runner
	Now         func() time.Time

	mu        sync.Mutex // guards the fields below; never held while az runs
	token     string
	expiresAt time.Time
	inflight  *flight
}

// flight is one shared az invocation. Its result fields are written before done is closed.
type flight struct {
	done      chan struct{}
	token     string
	expiresAt time.Time
	err       error
}

func NewAzCLI(azPath, scope, tenant string) *AzCLI {
	return &AzCLI{
		AzPath:      azPath,
		Scope:       scope,
		Tenant:      tenant,
		RefreshSkew: 5 * time.Minute,
		ExecTimeout: 30 * time.Second,
		Run:         ExecRunner,
		Now:         time.Now,
	}
}

// Token returns a cached token until it is within RefreshSkew of expiry. Concurrent callers that
// need a new token share one az invocation, which runs detached from any caller's context and is
// bounded only by ExecTimeout; each caller stops waiting when its own ctx is done.
func (a *AzCLI) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	if a.token != "" && a.Now().Add(a.RefreshSkew).Before(a.expiresAt) {
		tok := a.token
		a.mu.Unlock()
		return tok, nil
	}
	f := a.inflight
	if f == nil {
		f = &flight{done: make(chan struct{})}
		a.inflight = f
		go a.fly(context.WithoutCancel(ctx), f)
	}
	a.mu.Unlock()

	select {
	case <-f.done:
		return f.token, f.err
	case <-ctx.Done():
		return "", fmt.Errorf("waiting for az account get-access-token: %w", ctx.Err())
	}
}

func (a *AzCLI) fly(ctx context.Context, f *flight) {
	f.token, f.expiresAt, f.err = a.acquire(ctx)
	a.mu.Lock()
	if f.err == nil {
		a.token, a.expiresAt = f.token, f.expiresAt
	}
	a.inflight = nil
	a.mu.Unlock()
	close(f.done)
}

// Invalidate drops the cached token when it is the rejected one, so the next Token call re-runs
// az. A token that has already been replaced is left alone.
func (a *AzCLI) Invalidate(rejected string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != rejected {
		return
	}
	a.token = ""
	a.expiresAt = time.Time{}
}

type azToken struct {
	AccessToken string `json:"accessToken"`
	ExpiresOn   string `json:"expiresOn"`
	ExpiresUnix *int64 `json:"expires_on"`
}

func (a *AzCLI) acquire(ctx context.Context) (string, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, a.ExecTimeout)
	defer cancel()

	args := []string{"account", "get-access-token", "--scope", a.Scope, "--output", "json"}
	if a.Tenant != "" {
		args = append(args, "--tenant", a.Tenant)
	}

	stdout, stderr, err := a.Run(ctx, a.AzPath, args...)
	if err != nil {
		if ctx.Err() != nil {
			return "", time.Time{}, fmt.Errorf("az account get-access-token: %w", ctx.Err())
		}
		msg := summarize(stderr)
		if needsLogin(msg) {
			return "", time.Time{}, fmt.Errorf("%w: %s", ErrLoginRequired, msg)
		}
		return "", time.Time{}, fmt.Errorf("az account get-access-token failed: %w: %s", err, msg)
	}

	var parsed azToken
	if err := json.Unmarshal(stdout, &parsed); err != nil {
		return "", time.Time{}, errors.New("az account get-access-token returned unparseable JSON")
	}
	if parsed.AccessToken == "" {
		return "", time.Time{}, errors.New("az account get-access-token returned no accessToken")
	}

	exp, err := parsed.expiry()
	if err != nil {
		return "", time.Time{}, err
	}
	return parsed.AccessToken, exp, nil
}

func (t azToken) expiry() (time.Time, error) {
	if t.ExpiresUnix != nil {
		return time.Unix(*t.ExpiresUnix, 0), nil
	}
	if t.ExpiresOn != "" {
		for _, layout := range []string{"2006-01-02 15:04:05.000000", "2006-01-02 15:04:05"} {
			if exp, err := time.ParseInLocation(layout, t.ExpiresOn, time.Local); err == nil {
				return exp, nil
			}
		}
	}
	return time.Time{}, errors.New("az account get-access-token returned no parseable expiry")
}

func needsLogin(stderr string) bool {
	lower := strings.ToLower(stderr)
	for _, marker := range []string{"az login", "aadsts", "interactive authentication is needed", "refresh token has expired"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func summarize(stderr []byte) string {
	msg := strings.Join(strings.Fields(string(stderr)), " ")
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	if msg == "" {
		msg = "no stderr output"
	}
	return msg
}

// execWaitDelay bounds how long ExecRunner waits for output pipes to close after the context is
// done, in case a descendant outside the killed process group still holds them.
const execWaitDelay = 3 * time.Second

// ExecRunner runs the command in its own process group and, when ctx is done, kills the whole
// group. The Homebrew az is a bash wrapper around python; killing only bash would leave python
// holding stdout and stderr, and Run would block until python exited on its own.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	killProcessGroupOnCancel(cmd)
	cmd.WaitDelay = execWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}
