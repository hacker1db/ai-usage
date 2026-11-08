package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"ai-usage/internal/usage"
)

// SQLitePath is the sqlite3 CLI used to read the Copilot session store. macOS ships it, which keeps
// this module free of a database driver; the store is opened -readonly.
var SQLitePath = "/usr/bin/sqlite3"

// queryJSON runs one read-only statement and decodes its rows. sqlite3 prints nothing for an empty
// result, which decodes as no rows.
func queryJSON[T any](ctx context.Context, db, sql string) ([]T, error) {
	bin := SQLitePath
	if _, err := os.Stat(bin); err != nil {
		if p, lerr := exec.LookPath("sqlite3"); lerr == nil {
			bin = p
		}
	}
	cmd := exec.CommandContext(ctx, bin, "-readonly", "-json", db, sql)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	if len(bytes.TrimSpace(stdout.Bytes())) == 0 {
		return nil, nil
	}
	var rows []T
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		return nil, fmt.Errorf("decode sqlite3 output: %w", err)
	}
	return rows, nil
}

type copilotEvent struct {
	SessionID        usage.Str   `json:"session_id"`
	Model            usage.Str   `json:"model"`
	InputTokens      usage.Int   `json:"input_tokens"`
	OutputTokens     usage.Int   `json:"output_tokens"`
	CacheReadTokens  usage.Int   `json:"cache_read_tokens"`
	CacheWriteTokens usage.Int   `json:"cache_write_tokens"`
	ReasoningTokens  usage.Int   `json:"reasoning_tokens"`
	TotalNanoAIU     usage.Float `json:"total_nano_aiu"`
	CreatedAt        usage.Stamp `json:"created_at"`
}

type copilotSession struct {
	ID         usage.Str `json:"id"`
	Cwd        usage.Str `json:"cwd"`
	Repository usage.Str `json:"repository"`
	Branch     usage.Str `json:"branch"`
	Summary    usage.Str `json:"summary"`
}

// CollectCopilot reads the GitHub Copilot CLI session store's assistant_usage_events, plus session
// titles, prompts, files, and refs when those tables exist.
func CollectCopilot(agg *usage.Aggregator, dbPath string) Status {
	dbPath = usage.ExpandHome(dbPath)
	st := Status{Source: usage.ToolCopilot, Path: usage.Tilde(dbPath)}
	if _, err := os.Stat(dbPath); err != nil {
		st.Status = "not found"
		return st
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The cutoff is formatted by us, never user input, so inlining it is safe.
	cutoff := agg.Cutoff.UTC().Format("2006-01-02T15:04:05")
	inWindow := fmt.Sprintf("SELECT DISTINCT session_id FROM assistant_usage_events WHERE created_at >= '%s'", cutoff)
	events, err := queryJSON[copilotEvent](ctx, dbPath, fmt.Sprintf(
		"SELECT session_id, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,"+
			" reasoning_tokens, total_nano_aiu, created_at FROM assistant_usage_events WHERE created_at >= '%s'", cutoff))
	if err != nil {
		st.Status = "error: " + err.Error()
		return st
	}
	sessions, err := queryJSON[copilotSession](ctx, dbPath,
		"SELECT id, cwd, repository, branch, summary FROM sessions WHERE id IN ("+inWindow+")")
	if err != nil {
		st.Status = "error: " + err.Error()
		return st
	}

	// Detail tables are newer than the usage table; a store without them still reports usage.
	extra := map[string]*usage.Meta{}
	metaOf := func(sid string) *usage.Meta {
		if extra[sid] == nil {
			extra[sid] = &usage.Meta{}
		}
		return extra[sid]
	}
	if turns, err := queryJSON[struct {
		SessionID   usage.Str `json:"session_id"`
		UserMessage usage.Str `json:"user_message"`
	}](ctx, dbPath, "SELECT session_id, user_message FROM turns WHERE session_id IN ("+inWindow+") ORDER BY session_id, turn_index"); err == nil {
		for _, t := range turns {
			m := metaOf(string(t.SessionID))
			if text := usage.CleanTitle(string(t.UserMessage), 240); text != "" && len(m.Prompts) < usage.MaxPrompts {
				m.Prompts = append(m.Prompts, text)
			}
		}
	}
	if files, err := queryJSON[struct {
		SessionID usage.Str `json:"session_id"`
		FilePath  usage.Str `json:"file_path"`
		ToolName  usage.Str `json:"tool_name"`
	}](ctx, dbPath, "SELECT session_id, file_path, tool_name FROM session_files WHERE session_id IN ("+inWindow+")"); err == nil {
		for _, f := range files {
			m := metaOf(string(f.SessionID))
			if f.FilePath != "" {
				m.Files.Add(usage.Tilde(string(f.FilePath)), 1)
			}
			m.Tools.Add(or(string(f.ToolName), "?"), 1)
		}
	}
	if refs, err := queryJSON[struct {
		SessionID usage.Str `json:"session_id"`
		RefType   usage.Str `json:"ref_type"`
		RefValue  usage.Str `json:"ref_value"`
	}](ctx, dbPath, "SELECT session_id, ref_type, ref_value FROM session_refs WHERE session_id IN ("+inWindow+")"); err == nil {
		for _, r := range refs {
			m := metaOf(string(r.SessionID))
			if ref := usage.CleanTitle(string(r.RefType)+": "+string(r.RefValue), 140); ref != "" && len(m.Refs) < usage.MaxRefs {
				m.Refs = append(m.Refs, ref)
			}
		}
	}

	for _, e := range events {
		if agg.Add(usage.ToolCopilot, "github-copilot", usage.Record{
			Model: string(e.Model), Time: e.CreatedAt.Time, Session: string(e.SessionID),
			Input: int64(e.InputTokens), Cached: int64(e.CacheReadTokens), CacheWrite: int64(e.CacheWriteTokens),
			Output: int64(e.OutputTokens), Reasoning: int64(e.ReasoningTokens), CopilotAIU: float64(e.TotalNanoAIU) / 1e9,
		}) {
			st.Requests++
		}
	}
	for _, s := range sessions {
		k := usage.SessionKey{Tool: usage.ToolCopilot, Profile: "github-copilot", ID: string(s.ID)}
		if !agg.HasSession(k) {
			continue
		}
		m := usage.Meta{}
		if x := extra[string(s.ID)]; x != nil {
			m = *x
		}
		m.Title = usage.CleanTitle(string(s.Summary), 100)
		if m.Title != "" {
			m.TitleSrc = "summary"
		}
		m.Cwd, m.Repo, m.Branch = string(s.Cwd), string(s.Repository), string(s.Branch)
		agg.Note(k, m)
	}
	st.Status = "ok"
	return st
}
