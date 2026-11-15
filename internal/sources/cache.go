// Package sources reads each AI tool's local logs (read-only) into a usage.Aggregator.
package sources

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"ai-usage/internal/usage"
)

// Status is one source's collection result.
type Status struct {
	Source   string `json:"source"`
	Path     string `json:"path"`
	Status   string `json:"status"` // "ok", "not found", or "error: ..."
	Requests int    `json:"requests"`
	// Errors lists files or directories that could not be read while Status is "ok". The source
	// still reports what it read, but its totals may be short.
	Errors []string `json:"errors,omitempty"`
}

// maxStatusErrors caps Status.Errors; the last entry then says how many more there were.
const maxStatusErrors = 5

// capErrors trims errs to maxStatusErrors entries.
func capErrors(errs []string) []string {
	if len(errs) <= maxStatusErrors {
		return errs
	}
	out := append([]string(nil), errs[:maxStatusErrors-1]...)
	return append(out, fmt.Sprintf("and %d more", len(errs)-(maxStatusErrors-1)))
}

// Stats counts parse-cache hits and misses for one collection.
type Stats struct{ Hits, Misses int }

type fileSig struct {
	size  int64
	mtime time.Time
}

type cacheEntry[T any] struct {
	sig fileSig
	val T
}

// FileCache keeps parsed results per file while its size and mtime are unchanged, so a refresh
// re-reads only new or growing transcripts. A one-shot report uses a throwaway cache.
type FileCache[T any] struct {
	mu      sync.Mutex
	entries map[string]cacheEntry[T]
	live    map[string]struct{}
	stats   Stats
}

// NewFileCache returns an empty cache.
func NewFileCache[T any]() *FileCache[T] {
	return &FileCache[T]{entries: map[string]cacheEntry[T]{}, live: map[string]struct{}{}}
}

// Start resets per-collection stats and the set of files seen.
func (c *FileCache[T]) Start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats, c.live = Stats{}, map[string]struct{}{}
}

// Prune forgets files that weren't seen since Start (deleted or out of the window).
func (c *FileCache[T]) Prune() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if _, ok := c.live[k]; !ok {
			delete(c.entries, k)
		}
	}
}

// Stats returns hits and misses since Start.
func (c *FileCache[T]) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

func (c *FileCache[T]) get(path string, info fs.FileInfo, parse func(string) T) T {
	sig := fileSig{info.Size(), info.ModTime()}
	c.mu.Lock()
	c.live[path] = struct{}{}
	if e, ok := c.entries[path]; ok && e.sig == sig {
		c.stats.Hits++
		c.mu.Unlock()
		return e.val
	}
	c.stats.Misses++
	c.mu.Unlock()
	v := parse(path)
	c.mu.Lock()
	c.entries[path] = cacheEntry[T]{sig, v}
	c.mu.Unlock()
	return v
}

type parsedFile[T any] struct {
	path string
	val  T
}

// parseAll parses files modified at or after cutoff in parallel and returns results in path order,
// so dedupe (first file wins) stays deterministic. Files that cannot be stat'ed are returned as
// errors instead of being skipped silently.
func parseAll[T any](c *FileCache[T], paths []string, cutoff time.Time, parse func(string) T) ([]parsedFile[T], []string) {
	type job struct {
		i    int
		path string
		info fs.FileInfo
	}
	var jobs []job
	var errs []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			errs = append(errs, usage.Tilde(p)+": "+err.Error())
			continue
		}
		if info.ModTime().Before(cutoff) {
			continue
		}
		jobs = append(jobs, job{len(jobs), p, info})
	}
	out := make([]parsedFile[T], len(jobs))
	ch := make(chan job)
	var wg sync.WaitGroup
	for range max(runtime.NumCPU()-1, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				out[j.i] = parsedFile[T]{j.path, c.get(j.path, j.info, parse)}
			}
		}()
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	return out, errs
}

// jsonlFiles lists *.jsonl files under root, skipping dot-directories and dot-files like a shell
// glob would. Symlinked directories are not followed. Unreadable entries below root are returned
// as errors; a missing root is not an error.
func jsonlFiles(root string) (out, errs []string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p != root || !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, usage.Tilde(p)+": "+err.Error())
			}
			return nil
		}
		name := d.Name()
		if p != root && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(name, ".jsonl") {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out, errs
}

// statSource tells a missing log location ("not found") from one that exists but can't be read
// ("error: ..."), so a permission problem never passes for a tool that simply isn't installed.
// It returns "" when p exists (and is a directory, if wantDir).
func statSource(p string, wantDir bool) string {
	st, err := os.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not found"
	case err != nil:
		return "error: " + err.Error()
	case wantDir && !st.IsDir():
		return "error: " + usage.Tilde(p) + " is not a directory"
	}
	return ""
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
