package sources

import (
	"os"
	"path/filepath"
	"sort"

	"ai-usage/internal/usage"
)

type Options struct {
	Claude     []ClaudeDir
	CodexHome  string
	CodexExtra []string
	CopilotDB  string

	AntigravityHome string
	ProxyLog        string
	GatewayLog      string
}

func DefaultOptions() Options {
	codex := os.Getenv("CODEX_HOME")
	if codex == "" {
		codex = "~/.codex"
	}
	return Options{
		CodexHome:       codex,
		CodexExtra:      DefaultCodexExtras(codex),
		CopilotDB:       "~/.copilot/session-store.db",
		AntigravityHome: "~/.gemini/antigravity-cli",
		ProxyLog:        "~/Library/Logs/foundry-entra-proxy/proxy.log",
		GatewayLog:      "~/Library/Logs/ai-gateway/gateway.log",
	}
}

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

func (o Options) CodexHomes() []string {
	if o.CodexHome == "" {
		return dedupeHomes(o.CodexExtra)
	}
	return dedupeHomes(append([]string{o.CodexHome}, o.CodexExtra...))
}

func dedupeHomes(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		resolved := usage.ExpandHome(p)
		if r, err := filepath.EvalSymlinks(resolved); err == nil {
			resolved = r
		}
		if !seen[resolved] {
			seen[resolved] = true
			out = append(out, p)
		}
	}
	return out
}

type Caches struct {
	Claude *FileCache[ClaudeFile]
	Codex  *FileCache[CodexFile]
}

func NewCaches() *Caches {
	return &Caches{Claude: NewFileCache[ClaudeFile](), Codex: NewFileCache[CodexFile]()}
}

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
	if o.AntigravityHome != "" {
		status = append(status, CollectAntigravity(agg, o.AntigravityHome))
	}
	status = append(status, CollectProxy(agg, o.ProxyLog))
	status = append(status, CollectGateway(agg, o.GatewayLog))
	c.Claude.Prune()
	c.Codex.Prune()
	a, b := c.Claude.Stats(), c.Codex.Stats()
	return status, Stats{Hits: a.Hits + b.Hits, Misses: a.Misses + b.Misses}
}

func CopilotUsed(o Options) bool {
	if o.CopilotDB == "" {
		return false
	}
	_, err := os.Stat(usage.ExpandHome(o.CopilotDB))
	return err == nil
}
