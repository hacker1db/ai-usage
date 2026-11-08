package sources

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"ai-usage/internal/usage"
)

// ClaudeDir is a Claude Code config dir (CLAUDE_CONFIG_DIR) and its profile label.
type ClaudeDir struct {
	Path  string // may start with ~/
	Label string
}

// ClaudeProfiles are the current per-profile config dirs.
var ClaudeProfiles = []ClaudeDir{
	{"~/.claude-personal", "personal"},
	{"~/.claude-subscription", "work-subscription"},
	{"~/.claude-foundry", "work-foundry"},
}

// DefaultClaudeDirs is the current profiles, then retired ~/.claude copies ("legacy"), then a live
// ~/.claude ("no-profile": sessions launched without CLAUDE_CONFIG_DIR). Dedupe keeps the first hit.
func DefaultClaudeDirs() []ClaudeDir {
	dirs := append([]ClaudeDir(nil), ClaudeProfiles...)
	retired, _ := filepath.Glob(filepath.Join(usage.Home, ".claude.retired-*"))
	sort.Strings(retired)
	for _, p := range retired {
		if isDir(filepath.Join(p, "projects")) {
			dirs = append(dirs, ClaudeDir{usage.Tilde(p), "legacy"})
		}
	}
	if isDir(filepath.Join(usage.Home, ".claude", "projects")) {
		dirs = append(dirs, ClaudeDir{"~/.claude", "no-profile"})
	}
	return dirs
}

// editTools are tool names whose file_path input counts as a changed file.
var editTools = map[string]bool{
	"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true,
	"create": true, "edit": true, "apply_patch": true, "str_replace_editor": true,
}

type claudeUsage struct {
	InputTokens              usage.Int `json:"input_tokens"`
	OutputTokens             usage.Int `json:"output_tokens"`
	CacheReadInputTokens     usage.Int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens usage.Int `json:"cache_creation_input_tokens"`
}

type claudeMessage struct {
	ID      usage.Str       `json:"id"`
	Model   usage.Str       `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   json.RawMessage `json:"usage"`
}

type claudeEntry struct {
	Type        usage.Str      `json:"type"`
	SessionID   usage.Str      `json:"sessionId"`
	Timestamp   usage.Stamp    `json:"timestamp"`
	AITitle     usage.Str      `json:"aiTitle"`
	Summary     usage.Str      `json:"summary"`
	CustomTitle usage.Str      `json:"customTitle"`
	Cwd         usage.Str      `json:"cwd"`
	GitBranch   usage.Str      `json:"gitBranch"`
	IsMeta      usage.Bool     `json:"isMeta"`
	IsSidechain usage.Bool     `json:"isSidechain"`
	RequestID   usage.Str      `json:"requestId"`
	Message     *claudeMessage `json:"message"`
}

type toolInput struct {
	FilePath     usage.Str `json:"file_path"`
	NotebookPath usage.Str `json:"notebook_path"`
	Description  usage.Str `json:"description"`
	Command      usage.Str `json:"command"`
}

type claudeRecord struct {
	dedupe string // message id + request id; "" when neither is present
	rec    usage.Record
}

// ClaudeFile is one parsed transcript: usage records plus per-session metadata.
type ClaudeFile struct {
	records []claudeRecord
	meta    map[string]*usage.Meta
}

// ParseClaudeFile reads one Claude Code transcript.
func ParseClaudeFile(path string) ClaudeFile {
	out := ClaudeFile{meta: map[string]*usage.Meta{}}
	fallback := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	metaOf := func(sid string) *usage.Meta {
		m := out.meta[sid]
		if m == nil {
			m = &usage.Meta{Slots: map[int64]struct{}{}}
			out.meta[sid] = m
		}
		return m
	}
	_ = usage.EachJSONLine(path, func(line []byte) {
		var e claudeEntry
		if json.Unmarshal(line, &e) != nil {
			return
		}
		sid := string(e.SessionID)
		if sid == "" {
			sid = fallback
		}
		switch e.Type {
		case "ai-title", "summary", "custom-title":
			raw := e.AITitle
			if raw == "" {
				raw = e.Summary
			}
			if raw == "" {
				raw = e.CustomTitle
			}
			if t := usage.CleanTitle(string(raw), 100); t != "" {
				m := metaOf(sid)
				m.Title = t
				m.TitleSrc = string(e.Type)
				if e.Type == "ai-title" {
					m.TitleSrc = "ai"
				}
			}
			return
		case "user", "assistant":
		default:
			return
		}
		m := metaOf(sid)
		if !e.Timestamp.IsZero() {
			m.Slots[usage.Slot(e.Timestamp.Time)] = struct{}{}
		}
		if m.Cwd == "" {
			m.Cwd = string(e.Cwd)
		}
		if m.Branch == "" && e.GitBranch != "" && e.GitBranch != "HEAD" {
			m.Branch = string(e.GitBranch)
		}
		msg := e.Message
		if msg == nil {
			msg = &claudeMessage{}
		}
		if e.Type == "user" {
			if e.IsMeta || e.IsSidechain {
				return
			}
			if text := usage.CleanTitle(usage.FirstText(msg.Content), 240); text != "" {
				if m.Prompt == "" {
					m.Prompt = usage.CleanTitle(text, 100)
				}
				if len(m.Prompts) < usage.MaxPrompts {
					m.Prompts = append(m.Prompts, text)
				}
			}
			return
		}
		// assistant: tool calls, then usage
		if msg.Content != nil {
			var items []json.RawMessage
			if json.Unmarshal(msg.Content, &items) == nil {
				for _, it := range items {
					var b struct {
						Type  usage.Str       `json:"type"`
						Name  usage.Str       `json:"name"`
						Input json.RawMessage `json:"input"`
					}
					if json.Unmarshal(it, &b) != nil || b.Type != "tool_use" {
						continue
					}
					name := string(b.Name)
					if name == "" {
						name = "?"
					}
					var in toolInput
					_ = json.Unmarshal(b.Input, &in)
					m.Tools.Add(name, 1)
					p := in.FilePath
					if p == "" {
						p = in.NotebookPath
					}
					if editTools[name] && p != "" {
						m.Files.Add(usage.Tilde(string(p)), 1)
					}
					if name == "Bash" && len(m.Commands) < usage.MaxCommands {
						c := in.Description
						if c == "" {
							c = in.Command
						}
						if cmd := usage.CleanTitle(string(c), 140); cmd != "" {
							m.Commands = append(m.Commands, cmd)
						}
					}
				}
			}
		}
		// A missing, null, or empty usage object is skipped; an all-zero one still counts as a request.
		var u claudeUsage
		if raw := strings.TrimSpace(string(msg.Usage)); raw == "" || raw == "null" || raw == "{}" ||
			raw[0] != '{' || json.Unmarshal(msg.Usage, &u) != nil || msg.Model == "<synthetic>" {
			return
		}
		dedupe := ""
		if msg.ID != "" || e.RequestID != "" {
			dedupe = string(msg.ID) + "\x00" + string(e.RequestID)
		}
		cr, cw := int64(u.CacheReadInputTokens), int64(u.CacheCreationInputTokens)
		out.records = append(out.records, claudeRecord{dedupe, usage.Record{
			Model: string(msg.Model), Time: e.Timestamp.Time, Session: sid,
			Input: int64(u.InputTokens) + cr + cw, Cached: cr, CacheWrite: cw, Output: int64(u.OutputTokens),
		}})
	})
	return out
}

// CollectClaude adds every profile's transcripts. Streaming writes one line per content block with
// the same usage, and a message can exist in a retired copy of a profile dir, so records are
// deduplicated on (message id, request id) across all dirs.
func CollectClaude(agg *usage.Aggregator, dirs []ClaudeDir, cache *FileCache[ClaudeFile]) []Status {
	if cache == nil {
		cache = NewFileCache[ClaudeFile]()
	}
	seen := map[string]struct{}{}
	var status []Status
	for _, d := range dirs {
		root := usage.ExpandHome(d.Path)
		if real, err := filepath.EvalSymlinks(root); err == nil {
			root = real
		}
		name := usage.ToolClaude + ":" + d.Label
		projects := filepath.Join(root, "projects")
		if !isDir(projects) {
			status = append(status, Status{name, usage.Tilde(root), "not found", 0})
			continue
		}
		count := 0
		for _, f := range parseAll(cache, jsonlFiles(projects), agg.Cutoff, ParseClaudeFile) {
			for _, r := range f.val.records {
				if r.dedupe != "" {
					if _, dup := seen[r.dedupe]; dup {
						continue
					}
					seen[r.dedupe] = struct{}{}
				}
				if agg.Add(usage.ToolClaude, d.Label, r.rec) {
					count++
				}
			}
			for sid, m := range f.val.meta {
				k := usage.SessionKey{Tool: usage.ToolClaude, Profile: d.Label, ID: sid}
				if agg.HasSession(k) {
					agg.Note(k, *m)
				}
			}
		}
		status = append(status, Status{name, usage.Tilde(root), "ok", count})
	}
	return status
}
