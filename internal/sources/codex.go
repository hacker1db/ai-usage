package sources

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"ai-usage/internal/usage"
)

// CodexProviders maps a Codex model_provider id to a profile label.
var CodexProviders = map[string]string{"openai": "personal-chatgpt", "foundry": "work-foundry"}

var patchFile = regexp.MustCompile(`(?m)^\*\*\* (?:Update|Add|Delete) File: (.+)$`)

type codexEvent struct {
	Type      usage.Str       `json:"type"`
	Timestamp usage.Stamp     `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

type codexUsage struct {
	InputTokens           usage.Int `json:"input_tokens"`
	CachedInputTokens     usage.Int `json:"cached_input_tokens"`
	CacheWriteInputTokens usage.Int `json:"cache_write_input_tokens"`
	OutputTokens          usage.Int `json:"output_tokens"`
	ReasoningOutputTokens usage.Int `json:"reasoning_output_tokens"`
	TotalTokens           usage.Int `json:"total_tokens"`
}

func (u codexUsage) sub(prev codexUsage) codexUsage {
	return codexUsage{u.InputTokens - prev.InputTokens, u.CachedInputTokens - prev.CachedInputTokens,
		u.CacheWriteInputTokens - prev.CacheWriteInputTokens, u.OutputTokens - prev.OutputTokens,
		u.ReasoningOutputTokens - prev.ReasoningOutputTokens, u.TotalTokens - prev.TotalTokens}
}

type codexPayload struct {
	Type            usage.Str       `json:"type"`
	ID              usage.Str       `json:"id"`
	Model           usage.Str       `json:"model"`
	ModelProvider   usage.Str       `json:"model_provider"`
	ModelProviderID usage.Str       `json:"model_provider_id"`
	Cwd             usage.Str       `json:"cwd"`
	Role            usage.Str       `json:"role"`
	Content         json.RawMessage `json:"content"`
	Name            usage.Str       `json:"name"`
	Input           usage.Str       `json:"input"`
	Arguments       usage.Str       `json:"arguments"`
	ResponseID      usage.Str       `json:"response_id"`
	Usage           *codexUsage     `json:"usage"`
	Git             *struct {
		RepositoryURL usage.Str `json:"repository_url"`
		Branch        usage.Str `json:"branch"`
	} `json:"git"`
	Action *struct {
		Command json.RawMessage `json:"command"`
	} `json:"action"`
	ThreadSettings *struct {
		Model           usage.Str `json:"model"`
		ModelProviderID usage.Str `json:"model_provider_id"`
	} `json:"thread_settings"`
	Info *struct {
		TotalTokenUsage *codexUsage `json:"total_token_usage"`
	} `json:"info"`
}

type codexRecord struct {
	responseID string
	provider   string
	rec        usage.Record
}

// CodexFile is one parsed rollout.
type CodexFile struct {
	records []codexRecord
	session string
	meta    usage.Meta
}

// commandText reads a shell command given as a string or an argv array.
func commandText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var argv []json.RawMessage
	if json.Unmarshal(raw, &argv) == nil {
		parts := make([]string, 0, len(argv))
		for _, a := range argv {
			var p string
			if json.Unmarshal(a, &p) != nil {
				p = strings.Trim(string(a), `"`)
			}
			parts = append(parts, p)
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// ParseCodexFile reads one Codex rollout. Newer rollouts carry token_usage_record events (one per
// response); older ones only have cumulative token_count events, which are diffed so duplicates
// don't double count.
func ParseCodexFile(path string) CodexFile {
	var events []codexEvent
	_ = usage.EachJSONLine(path, func(line []byte) {
		var e codexEvent
		if json.Unmarshal(line, &e) == nil {
			events = append(events, e)
		}
	})
	out := CodexFile{session: strings.TrimSuffix(filepath.Base(path), ".jsonl"), meta: usage.Meta{Slots: map[int64]struct{}{}}}
	hasRecords := false
	for _, e := range events {
		if e.Type == "token_usage_record" {
			hasRecords = true
			break
		}
	}
	var provider, model string
	var prevTotal *codexUsage
	m := &out.meta
	for _, e := range events {
		var p codexPayload
		_ = json.Unmarshal(e.Payload, &p)
		if !e.Timestamp.IsZero() && (e.Type == "response_item" || e.Type == "event_msg") {
			m.Slots[usage.Slot(e.Timestamp.Time)] = struct{}{}
		}
		switch {
		case e.Type == "session_meta":
			provider = or(string(p.ModelProvider), provider)
			out.session = or(string(p.ID), out.session)
			model = or(string(p.Model), model)
			if m.Cwd == "" {
				m.Cwd = string(p.Cwd)
			}
			if p.Git != nil {
				if m.Repo == "" {
					m.Repo = string(p.Git.RepositoryURL)
				}
				if m.Branch == "" {
					m.Branch = string(p.Git.Branch)
				}
			}
		case e.Type == "response_item" && p.Role == "user":
			if text := usage.CleanTitle(usage.FirstText(p.Content), 240); text != "" {
				if m.Prompt == "" {
					m.Prompt = usage.CleanTitle(text, 100)
				}
				if len(m.Prompts) < usage.MaxPrompts {
					m.Prompts = append(m.Prompts, text)
				}
			}
		case e.Type == "response_item" && (p.Type == "function_call" || p.Type == "custom_tool_call" || p.Type == "local_shell_call"):
			name := or(string(p.Name), string(p.Type))
			m.Tools.Add(name, 1)
			raw := string(p.Input)
			if raw == "" {
				raw = string(p.Arguments)
			}
			var args struct {
				Cmd     json.RawMessage `json:"cmd"`
				Command json.RawMessage `json:"command"`
			}
			if strings.HasPrefix(raw, "{") {
				_ = json.Unmarshal([]byte(raw), &args)
			}
			for _, f := range patchFile.FindAllStringSubmatch(raw, -1) {
				m.Files.Add(usage.Tilde(strings.TrimSpace(f[1])), 1)
			}
			cmd := commandText(args.Cmd)
			if cmd == "" {
				cmd = commandText(args.Command)
			}
			if cmd == "" && p.Action != nil {
				cmd = commandText(p.Action.Command)
			}
			if cmd != "" && len(m.Commands) < usage.MaxCommands {
				if c := usage.CleanTitle(cmd, 140); c != "" {
					m.Commands = append(m.Commands, c)
				}
			}
		case e.Type == "turn_context":
			model = or(string(p.Model), model)
			provider = or(or(string(p.ModelProviderID), string(p.ModelProvider)), provider)
		case p.Type == "thread_settings_applied" && p.ThreadSettings != nil:
			model = or(string(p.ThreadSettings.Model), model)
			provider = or(string(p.ThreadSettings.ModelProviderID), provider)
		}

		var u *codexUsage
		rid := ""
		switch {
		case e.Type == "token_usage_record":
			rid = string(p.ResponseID)
			u = p.Usage
			if u == nil {
				u = &codexUsage{}
			}
		case !hasRecords && p.Type == "token_count":
			if p.Info == nil || p.Info.TotalTokenUsage == nil || *p.Info.TotalTokenUsage == (codexUsage{}) {
				continue
			}
			tot := *p.Info.TotalTokenUsage
			d := tot
			if prevTotal != nil && tot.TotalTokens >= prevTotal.TotalTokens {
				d = tot.sub(*prevTotal)
			}
			prevTotal = &tot
			if d == (codexUsage{}) {
				continue
			}
			u = &d
		}
		if u == nil {
			continue
		}
		out.records = append(out.records, codexRecord{rid, provider, usage.Record{
			Model: model, Time: e.Timestamp.Time, Session: out.session,
			Input: int64(u.InputTokens), Cached: int64(u.CachedInputTokens), CacheWrite: int64(u.CacheWriteInputTokens),
			Output: int64(u.OutputTokens), Reasoning: int64(u.ReasoningOutputTokens),
		}})
	}
	return out
}

func or(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

// codexTitles reads thread names from session_index.jsonl (the latest entry per id wins).
func codexTitles(root string) map[string]string {
	titles := map[string]string{}
	_ = usage.EachJSONLine(filepath.Join(root, "session_index.jsonl"), func(line []byte) {
		var e struct {
			ID         usage.Str `json:"id"`
			ThreadName usage.Str `json:"thread_name"`
		}
		if json.Unmarshal(line, &e) == nil && e.ID != "" {
			titles[string(e.ID)] = string(e.ThreadName)
		}
	})
	return titles
}

// CollectCodex adds Codex rollouts from sessions/ and archived_sessions/ under root.
func CollectCodex(agg *usage.Aggregator, root string, cache *FileCache[CodexFile]) Status {
	if cache == nil {
		cache = NewFileCache[CodexFile]()
	}
	root = usage.ExpandHome(root)
	var files []string
	for _, d := range []string{"sessions", "archived_sessions"} {
		files = append(files, jsonlFiles(filepath.Join(root, d))...)
	}
	if len(files) == 0 {
		return Status{usage.ToolCodex, usage.Tilde(root), "not found", 0}
	}
	titles := map[string]string{}
	if _, err := os.Stat(filepath.Join(root, "session_index.jsonl")); err == nil {
		titles = codexTitles(root)
	}
	count, seen := 0, map[string]struct{}{}
	for _, f := range parseAll(cache, files, agg.Cutoff, ParseCodexFile) {
		type ps struct{ profile, session string }
		profiles := map[ps]struct{}{}
		var order []ps
		for _, r := range f.val.records {
			if r.responseID != "" {
				if _, dup := seen[r.responseID]; dup {
					continue
				}
				seen[r.responseID] = struct{}{}
			}
			prov := or(r.provider, "openai")
			prof, ok := CodexProviders[prov]
			if !ok {
				prof = prov
			}
			key := ps{prof, r.rec.Session}
			if _, ok := profiles[key]; !ok {
				profiles[key] = struct{}{}
				order = append(order, key)
			}
			if agg.Add(usage.ToolCodex, prof, r.rec) {
				count++
			}
		}
		for _, k := range order {
			title := usage.CleanTitle(or(titles[k.session], titles[f.val.session]), 100)
			meta := f.val.meta
			meta.Title, meta.TitleSrc = title, ""
			if title != "" {
				meta.TitleSrc = "index"
			}
			agg.Note(usage.SessionKey{Tool: usage.ToolCodex, Profile: k.profile, ID: k.session}, meta)
		}
	}
	return Status{usage.ToolCodex, usage.Tilde(root), "ok", count}
}
