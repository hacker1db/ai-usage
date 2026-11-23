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

var Home = func() string {
	h, _ := os.UserHomeDir()
	return h
}()

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

func Redact(s string) string {
	for _, p := range secretPatterns {
		s = p.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}

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

func Tilde(p string) string {
	if Home != "" && strings.HasPrefix(p, Home) {
		return "~" + p[len(Home):]
	}
	return p
}

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

type contentBlock struct {
	Type  Str             `json:"type"`
	Text  Str             `json:"text"`
	Name  Str             `json:"name"`
	Input json.RawMessage `json:"input"`
}

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
