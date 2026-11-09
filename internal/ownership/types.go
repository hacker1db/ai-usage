package ownership

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Account types from the GitHub users API.
const (
	TypeUser = "User"
	TypeOrg  = "Organization"
)

// Types looks up whether a GitHub owner is a user or an organization with `gh api users/<owner>`,
// so it uses gh's existing login and needs no token handling here. Answers are cached in memory
// and, when path is set, in a small JSON file, since an account's type practically never changes.
// Failed lookups (gh missing, offline, unknown owner) are retried after RetryAfter.
type Types struct {
	path   string
	lookup func(ctx context.Context, owner string) (string, error)

	mu     sync.Mutex
	known  map[string]typeEntry // lowercased owner
	failed map[string]time.Time
	dirty  bool
}

type typeEntry struct {
	Type    string    `json:"type"`
	Checked time.Time `json:"checked"`
}

// Cache lifetimes.
const (
	TypeTTL    = 90 * 24 * time.Hour
	RetryAfter = time.Hour
)

// NewTypes returns a lookup cache stored at path ("" = memory only).
func NewTypes(path string) *Types {
	t := &Types{path: path, lookup: ghLookup, known: map[string]typeEntry{}, failed: map[string]time.Time{}}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &t.known)
		}
	}
	return t
}

// Of returns TypeUser, TypeOrg, or "" when unknown. Only github.com owners are looked up.
func (t *Types) Of(owner string) string {
	if t == nil || owner == "" {
		return ""
	}
	k := strings.ToLower(owner)
	t.mu.Lock()
	if e, ok := t.known[k]; ok && time.Since(e.Checked) < TypeTTL {
		t.mu.Unlock()
		return e.Type
	}
	if at, ok := t.failed[k]; ok && time.Since(at) < RetryAfter {
		t.mu.Unlock()
		return ""
	}
	t.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	typ, err := t.lookup(ctx, owner)
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil || (typ != TypeUser && typ != TypeOrg) {
		t.failed[k] = time.Now()
		return ""
	}
	t.known[k] = typeEntry{typ, time.Now()}
	t.dirty = true
	return typ
}

// Save writes new answers to the cache file.
func (t *Types) Save() error {
	if t == nil || t.path == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.dirty {
		return nil
	}
	b, err := json.MarshalIndent(t.known, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
		return err
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, t.path); err != nil {
		return err
	}
	t.dirty = false
	return nil
}

func ghLookup(ctx context.Context, owner string) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", "api", "users/"+owner, "--jq", ".type").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// IsGitHub reports whether a remote host is github.com: the host itself, ssh.github.com, or an
// SSH config alias such as "github-work" (a dotless name containing "github").
func IsGitHub(host string) bool {
	h := strings.ToLower(host)
	switch {
	case h == "" || h == "github.com" || h == "ssh.github.com" || h == "www.github.com":
		return true
	case !strings.Contains(h, ".") && strings.Contains(h, "github"):
		return true
	}
	return false
}
