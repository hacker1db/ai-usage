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
	Invalidate()
}

type AzCLI struct {
	AzPath      string
	Scope       string
	Tenant      string
	RefreshSkew time.Duration
	ExecTimeout time.Duration
	Run         Runner
	Now         func() time.Time
	mu          sync.Mutex
	token       string
	expiresAt   time.Time
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

// Token returns a cached token until it is within RefreshSkew of expiry.
func (a *AzCLI) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.token != "" && a.Now().Add(a.RefreshSkew).Before(a.expiresAt) {
		return a.token, nil
	}

	tok, exp, err := a.acquire(ctx)
	if err != nil {
		return "", err
	}
	a.token, a.expiresAt = tok, exp
	return tok, nil
}

// Invalidate drops the cached token so the next Token call re-runs az.
func (a *AzCLI) Invalidate() {
	a.mu.Lock()
	defer a.mu.Unlock()
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

func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}
