package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// Home is the user's home directory, used to shorten paths to ~/...
var Home = func() string {
	h, _ := os.UserHomeDir()
	return h
}()

// Secret patterns from the ai-work-report skill's claude_digest.py. Titles, prompts, and commands
// pass through Redact before they reach a report, the history store, or a screen.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|passwd|pwd|client[_-]?secret)\s*[:=]\s*\S+`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/-]{16,}=*`),
	regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,})\b`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
}

var (
	injectedPrefixes = []string{"<", "# AGENTS.md", "# CLAUDE.md", "Caveat:"}
	shortTag         = regexp.MustCompile(`<[^>]{1,80}>`)
)

// Redact replaces anything that looks like a credential with [REDACTED].
func Redact(s string) string {
	for _, p := range secretPatterns {
		s = p.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}

// CleanTitle returns one redacted line of at most limit runes, or "" for empty or tool-injected
// text (system reminders, AGENTS.md dumps, caveats).
func CleanTitle(s string, limit int) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, "<INSTRUCTIONS>") {
		return ""
	}
	for _, p := range injectedPrefixes {
		if strings.HasPrefix(s, p) {
			return ""
		}
	}
	line := strings.Join(strings.Fields(shortTag.ReplaceAllString(s, " ")), " ")
	if line == "" {
		return ""
	}
	line = Redact(line)
	if utf8.RuneCountInString(line) <= limit {
		return line
	}
	r := []rune(line)
	return strings.TrimRight(string(r[:limit-1]), " \t") + "…"
}

// Tilde shortens a path under the home directory to ~/...
func Tilde(p string) string {
	if Home != "" && strings.HasPrefix(p, Home) {
		return "~" + p[len(Home):]
	}
	return p
}

// ExpandHome turns a leading ~/ into the home directory.
func ExpandHome(p string) string {
	if p == "~" {
		return Home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(Home, p[2:])
	}
	return p
}

var (
	projectMu    sync.Mutex
	projectCache = map[string]string{}
)

// ProjectOf names the project for a session: the repository name when known, else the git root
// of cwd (bare-clone aware: <repo>/<worktree> with <repo>/.bare reports <repo>), else the path.
func ProjectOf(cwd, repo string) string {
	if repo != "" {
		name := strings.TrimSuffix(strings.TrimRight(repo, "/"), ".git")
		name = strings.ReplaceAll(name, ":", "/")
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		if name != "" {
			return name
		}
	}
	if cwd == "" {
		return ""
	}
	projectMu.Lock()
	defer projectMu.Unlock()
	if name, ok := projectCache[cwd]; ok {
		return name
	}
	name := ""
	for p := cwd; p != "" && p != "/" && p != Home && len(p) > 1; p = filepath.Dir(p) {
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			parent := filepath.Dir(p)
			if st, err := os.Stat(filepath.Join(parent, ".bare")); err == nil && st.IsDir() {
				name = filepath.Base(parent)
			} else {
				name = filepath.Base(p)
			}
			break
		}
	}
	if name == "" {
		name = Tilde(cwd)
	}
	projectCache[cwd] = name
	return name
}

// contentBlock is one element of a message content array (Claude or OpenAI Responses).
type contentBlock struct {
	Type  Str             `json:"type"`
	Text  Str             `json:"text"`
	Name  Str             `json:"name"`
	Input json.RawMessage `json:"input"`
}

// FirstText returns the first text of a message content value: a plain string, or the first
// text/input_text block of an array.
func FirstText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	for _, b := range blocks(raw) {
		if (b.Type == "text" || b.Type == "input_text") && b.Text != "" {
			return string(b.Text)
		}
	}
	return ""
}

// blocks decodes a content array one element at a time, skipping elements that don't decode.
func blocks(raw json.RawMessage) []contentBlock {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]contentBlock, 0, len(items))
	for _, it := range items {
		var b contentBlock
		if json.Unmarshal(it, &b) == nil {
			out = append(out, b)
		}
	}
	return out
}
