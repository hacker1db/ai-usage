package sources

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ai-usage/internal/usage"
)

// AntigravityProfile is the profile label for Google Antigravity rows.
const AntigravityProfile = "google"

// antigravityConvDir holds one SQLite database per conversation under the Antigravity home.
const antigravityConvDir = "conversations"

// CollectAntigravity reads Google Antigravity CLI conversations (~/.gemini/antigravity-cli). Each
// conversation is a SQLite database whose steps carry protobuf metadata; a model-response step
// holds a usage message with the model enum and token counts. Model names come from the same
// database's gen_metadata rows, titles and workspaces from conversation_summaries.db, and prompts
// from history.jsonl. Everything is opened read-only through the sqlite3 CLI.
func CollectAntigravity(agg *usage.Aggregator, home string) Status {
	home = usage.ExpandHome(home)
	st := Status{Source: usage.ToolAntigravity, Path: usage.Tilde(home)}
	convDir := filepath.Join(home, antigravityConvDir)
	if missing := statSource(convDir, true); missing != "" {
		st.Status = missing
		return st
	}
	dbs, err := filepath.Glob(filepath.Join(convDir, "*.db"))
	if err != nil {
		st.Status = "error: " + err.Error()
		return st
	}
	sort.Strings(dbs)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var errs []string
	seen := map[string]bool{}
	for _, db := range dbs {
		if newest(db).Before(agg.Cutoff) {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(db), ".db")
		recs, err := readAntigravityConversation(ctx, db)
		if err != nil {
			errs = append(errs, usage.Tilde(db)+": "+err.Error())
			continue
		}
		for _, r := range recs {
			r.Session = id
			if agg.Add(usage.ToolAntigravity, AntigravityProfile, r) {
				st.Requests++
				seen[id] = true
			}
		}
	}
	if len(seen) > 0 {
		noteAntigravitySessions(ctx, agg, home, seen)
	}
	st.Status = "ok"
	st.Errors = capErrors(errs)
	return st
}

// newest returns the later modification time of a database and its write-ahead log, since a
// running Antigravity appends to the -wal file without touching the database file.
func newest(db string) time.Time {
	var t time.Time
	for _, p := range []string{db, db + "-wal"} {
		if info, err := os.Stat(p); err == nil && info.ModTime().After(t) {
			t = info.ModTime()
		}
	}
	return t
}

// queryAntigravity runs one read-only query against a conversation database. It first opens the
// database with mode=ro, which sees uncommitted WAL frames while Antigravity is running. A
// read-only connection cannot create a missing -shm file, so when that open fails it retries with
// immutable=1, which reads the main file as is; that is complete once the WAL is checkpointed.
func queryAntigravity[T any](ctx context.Context, db, sql string) ([]T, error) {
	rows, err := queryJSON[T](ctx, sqliteURI(db, "mode=ro"), sql)
	if err == nil {
		return rows, nil
	}
	if fi, werr := os.Stat(db + "-wal"); werr == nil && fi.Size() > 0 {
		return nil, err // a WAL with content: immutable would silently drop recent steps
	}
	return queryJSON[T](ctx, sqliteURI(db, "immutable=1"), sql)
}

// sqliteURI returns a file: URI for path with the given query, escaping characters that SQLite
// would otherwise read as URI syntax.
func sqliteURI(path, query string) string {
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query}).String()
}

// readAntigravityConversation returns one record per model response in a conversation database.
// Session is left for the caller to fill.
func readAntigravityConversation(ctx context.Context, db string) ([]usage.Record, error) {
	type hexRow struct {
		Data usage.Str `json:"d"`
	}
	steps, err := queryAntigravity[hexRow](ctx, db, "SELECT hex(metadata) AS d FROM steps WHERE metadata IS NOT NULL ORDER BY idx")
	if err != nil {
		return nil, err
	}
	// gen_metadata is optional: without it models are named by their enum.
	names := map[uint64]string{}
	if gens, err := queryAntigravity[hexRow](ctx, db, "SELECT hex(data) AS d FROM gen_metadata ORDER BY idx"); err == nil {
		for _, g := range gens {
			if b, err := hex.DecodeString(string(g.Data)); err == nil {
				if enum, name := antigravityModelName(b); enum != 0 && name != "" {
					names[enum] = name
				}
			}
		}
	}
	var out []usage.Record
	for _, s := range steps {
		b, err := hex.DecodeString(string(s.Data))
		if err != nil {
			continue
		}
		r, enum, ok := antigravityStepUsage(b)
		if !ok {
			continue
		}
		r.Model = names[enum]
		if r.Model == "" {
			r.Model = fmt.Sprintf("antigravity-model-%d", enum)
		}
		out = append(out, r)
	}
	return out, nil
}

// Antigravity step metadata (protobuf) fields this reader uses.
const (
	agStepCreated = 1 // google.protobuf.Timestamp
	agStepUsage   = 9 // model usage stats

	agUsageModel      = 1 // model enum
	agUsageInput      = 2 // fresh input tokens (excludes cache reads and writes)
	agUsageOutput     = 3 // output tokens, thinking included
	agUsageCacheWrite = 4
	agUsageCacheRead  = 5
	agUsageThinking   = 9

	agGenInfo      = 1  // generation info message
	agGenInfoModel = 3  // model enum
	agGenInfoName  = 19 // model id, e.g. "gemini-4-neon"
)

// antigravityStepUsage decodes a step's metadata into a record. ok is false for steps without
// usage or with no tokens (a failed request).
func antigravityStepUsage(b []byte) (r usage.Record, enum uint64, ok bool) {
	var created, stats []byte
	err := pbEach(b, func(f int, v uint64, sub []byte) {
		switch f {
		case agStepCreated:
			created = sub
		case agStepUsage:
			stats = sub
		}
	})
	if err != nil || stats == nil || created == nil {
		return r, 0, false
	}
	var sec, nanos uint64
	_ = pbEach(created, func(f int, v uint64, _ []byte) {
		switch f {
		case 1:
			sec = v
		case 2:
			nanos = v
		}
	})
	var in, out, cw, cr, think uint64
	if pbEach(stats, func(f int, v uint64, _ []byte) {
		switch f {
		case agUsageModel:
			enum = v
		case agUsageInput:
			in = v
		case agUsageOutput:
			out = v
		case agUsageCacheWrite:
			cw = v
		case agUsageCacheRead:
			cr = v
		case agUsageThinking:
			think = v
		}
	}) != nil || sec == 0 || in+out+cw+cr == 0 {
		return r, 0, false
	}
	r = usage.Record{
		Time:  time.Unix(int64(sec), int64(nanos)).UTC(),                     //nolint:gosec // G115: protobuf timestamps fit int64
		Input: int64(in + cr + cw), Cached: int64(cr), CacheWrite: int64(cw), //nolint:gosec // G115: token counts
		Output: int64(out), Reasoning: int64(think), //nolint:gosec // G115: token counts
	}
	return r, enum, true
}

// antigravityModelName returns the model enum and id from one gen_metadata row.
func antigravityModelName(b []byte) (enum uint64, name string) {
	_ = pbEach(b, func(f int, _ uint64, sub []byte) {
		if f != agGenInfo || sub == nil {
			return
		}
		_ = pbEach(sub, func(f int, v uint64, s []byte) {
			switch f {
			case agGenInfoModel:
				enum = v
			case agGenInfoName:
				name = string(s)
			}
		})
	})
	return enum, name
}

var errPB = errors.New("malformed protobuf")

// pbEach walks one protobuf message's top-level fields. fn gets the varint value for wire type 0
// and the payload for wire type 2 (nil otherwise); fixed-width fields are skipped.
func pbEach(b []byte, fn func(field int, v uint64, sub []byte)) error {
	for len(b) > 0 {
		key, n := pbVarint(b)
		if n == 0 {
			return errPB
		}
		b = b[n:]
		field, wire := int(key>>3), key&7 //nolint:gosec // G115: field numbers fit int
		if field == 0 {
			return errPB
		}
		switch wire {
		case 0:
			v, n := pbVarint(b)
			if n == 0 {
				return errPB
			}
			b = b[n:]
			fn(field, v, nil)
		case 1:
			if len(b) < 8 {
				return errPB
			}
			b = b[8:]
		case 2:
			l, n := pbVarint(b)
			if n == 0 || l > uint64(len(b)-n) { //nolint:gosec // G115: len is non-negative
				return errPB
			}
			sub := b[n : n+int(l)] //nolint:gosec // G115: bounded by len(b) above
			b = b[n+int(l):]       //nolint:gosec // G115: bounded by len(b) above
			fn(field, 0, sub)
		case 5:
			if len(b) < 4 {
				return errPB
			}
			b = b[4:]
		default:
			return errPB
		}
	}
	return nil
}

// pbVarint decodes a base-128 varint, returning its length (0 when truncated or too long).
func pbVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

// noteAntigravitySessions adds titles, workspaces, and prompts for the sessions that had usage.
func noteAntigravitySessions(ctx context.Context, agg *usage.Aggregator, home string, ids map[string]bool) {
	meta := map[string]*usage.Meta{}
	metaOf := func(id string) *usage.Meta {
		if meta[id] == nil {
			meta[id] = &usage.Meta{}
		}
		return meta[id]
	}
	summaries := filepath.Join(home, "conversation_summaries.db")
	if _, err := os.Stat(summaries); err == nil {
		rows, err := queryAntigravity[struct {
			ID         usage.Str `json:"conversation_id"`
			Title      usage.Str `json:"title"`
			Workspaces usage.Str `json:"workspace_uris"`
		}](ctx, summaries, "SELECT conversation_id, title, workspace_uris FROM conversation_summaries")
		if err == nil {
			for _, r := range rows {
				id := string(r.ID)
				if !ids[id] {
					continue
				}
				m := metaOf(id)
				if t := usage.CleanTitle(string(r.Title), 100); t != "" {
					m.Title, m.TitleSrc = t, "summary"
				}
				var uris []string
				if json.Unmarshal([]byte(r.Workspaces), &uris) == nil && len(uris) > 0 {
					if u, err := url.Parse(uris[0]); err == nil && u.Scheme == "file" {
						m.Cwd = u.Path
					}
				}
			}
		}
	}
	if f, err := os.Open(filepath.Join(home, "history.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var h struct {
				Display        string `json:"display"`
				Type           string `json:"type"`
				ConversationID string `json:"conversationId"`
				Workspace      string `json:"workspace"`
			}
			if json.Unmarshal(sc.Bytes(), &h) != nil || !ids[h.ConversationID] || h.Type == "slash_command" {
				continue
			}
			m := metaOf(h.ConversationID)
			text := usage.CleanTitle(h.Display, 240)
			if text != "" && m.Prompt == "" {
				m.Prompt = usage.CleanTitle(h.Display, 100)
			}
			if text != "" && len(m.Prompts) < usage.MaxPrompts {
				m.Prompts = append(m.Prompts, text)
			}
			if m.Cwd == "" {
				m.Cwd = h.Workspace
			}
		}
		_ = f.Close()
	}
	for id, m := range meta {
		k := usage.SessionKey{Tool: usage.ToolAntigravity, Profile: AntigravityProfile, ID: id}
		if !agg.HasSession(k) {
			continue
		}
		agg.Note(k, *m)
	}
}
