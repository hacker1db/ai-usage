package sources

import (
	"os"
	"path/filepath"
	"sort"

	"ai-usage/internal/usage"
)

// Options locate each tool's logs.
type Options struct {
	Claude     []ClaudeDir // nil means DefaultClaudeDirs()
	CodexHome  string
	CodexExtra []string // more Codex homes (separate CODEX_HOME profiles); DefaultOptions finds them
	CopilotDB  string
	ProxyLog   string
}

// DefaultOptions returns the standard macOS locations (CODEX_HOME is honored).
func DefaultOptions() Options {
	codex := os.Getenv("CODEX_HOME")
	if codex == "" {
		codex = "~/.codex"
	}
	return Options{
		CodexHome:  codex,
		CodexExtra: DefaultCodexExtras(codex),
		CopilotDB:  "~/.copilot/session-store.db",
		ProxyLog:   "~/Library/Logs/foundry-entra-proxy/proxy.log",
	}
}

// DefaultCodexExtras returns the other Codex homes (~/.codex-* and ~/.codex_*, as used for a
// separate CODEX_HOME per provider) that hold sessions, excluding primary.
func DefaultCodexExtras(primary string) []string {
	var out []string
	for _, pat := range []string{".codex-*", ".codex_*"} {
		m, _ := filepath.Glob(filepath.Join(usage.Home, pat))
		for _, p := range m {
			if isDir(filepath.Join(p, "sessions")) || isDir(filepath.Join(p, "archived_sessions")) {
				out = append(out, usage.Tilde(p))
			}
		}
	}
	sort.Strings(out)
	return dedupeHomes(append([]string{primary}, out...))[1:]
}

// CodexHomes returns the primary Codex home and the extra ones, without duplicates.
func (o Options) CodexHomes() []string {
	if o.CodexHome == "" {
		return dedupeHomes(o.CodexExtra)
	}
	return dedupeHomes(append([]string{o.CodexHome}, o.CodexExtra...))
}

// dedupeHomes drops empty paths and later paths that resolve to an earlier one.
func dedupeHomes(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		real := usage.ExpandHome(p)
		if r, err := filepath.EvalSymlinks(real); err == nil {
			real = r
		}
		if !seen[real] {
			seen[real] = true
			out = append(out, p)
		}
	}
	return out
}

// Caches are reused across collections by long-running commands.
type Caches struct {
	Claude *FileCache[ClaudeFile]
	Codex  *FileCache[CodexFile]
}

// NewCaches returns empty caches.
func NewCaches() *Caches {
	return &Caches{Claude: NewFileCache[ClaudeFile](), Codex: NewFileCache[CodexFile]()}
}

// CollectAll reads every source into agg and returns their statuses and cache stats.
func CollectAll(agg *usage.Aggregator, o Options, c *Caches) ([]Status, Stats) {
	if c == nil {
		c = NewCaches()
	}
	c.Claude.Start()
	c.Codex.Start()
	dirs := o.Claude
	if dirs == nil {
		dirs = DefaultClaudeDirs()
	}
	status := CollectClaude(agg, dirs, c.Claude)
	for _, home := range o.CodexHomes() {
		status = append(status, CollectCodex(agg, home, c.Codex))
	}
	status = append(status, CollectCopilot(agg, o.CopilotDB))
	status = append(status, CollectProxy(agg, o.ProxyLog))
	c.Claude.Prune()
	c.Codex.Prune()
	a, b := c.Claude.Stats(), c.Codex.Stats()
	return status, Stats{Hits: a.Hits + b.Hits, Misses: a.Misses + b.Misses}
}

// CopilotUsed reports whether Copilot CLI has left a session store on this machine.
func CopilotUsed(o Options) bool {
	if o.CopilotDB == "" {
		return false
	}
	_, err := os.Stat(usage.ExpandHome(o.CopilotDB))
	return err == nil
}
