package sources

import (
	"os"

	"ai-usage/internal/usage"
)

// Options locate each tool's logs.
type Options struct {
	Claude    []ClaudeDir // nil means DefaultClaudeDirs()
	CodexHome string
	CopilotDB string
	ProxyLog  string
}

// DefaultOptions returns the standard macOS locations (CODEX_HOME is honored).
func DefaultOptions() Options {
	codex := os.Getenv("CODEX_HOME")
	if codex == "" {
		codex = "~/.codex"
	}
	return Options{
		CodexHome: codex,
		CopilotDB: "~/.copilot/session-store.db",
		ProxyLog:  "~/Library/Logs/foundry-entra-proxy/proxy.log",
	}
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
	status = append(status, CollectCodex(agg, o.CodexHome, c.Codex))
	status = append(status, CollectCopilot(agg, o.CopilotDB))
	status = append(status, CollectProxy(agg, o.ProxyLog))
	c.Claude.Prune()
	c.Codex.Prune()
	a, b := c.Claude.Stats(), c.Codex.Stats()
	return status, Stats{Hits: a.Hits + b.Hits, Misses: a.Misses + b.Misses}
}
