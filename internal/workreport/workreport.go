package workreport

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ai-usage/internal/usage"
)

const ReportName = "ai-work-report.html"

type Theme struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	D    string `json:"d,omitempty"`
}

type Classified struct {
	Theme        string      `json:"theme"`
	ManualLikely usage.Float `json:"manual_likely"`
	Basis        string      `json:"basis"`
}

type Classification struct {
	Themes   []Theme               `json:"themes"`
	Sessions map[string]Classified `json:"sessions"`
}

type Narrative struct {
	TitleHTML  string `json:"title_html"`
	ThesisHTML string `json:"thesis_html"`
}

type Report struct {
	Dir             string          `json:"dir"`
	Classified      *Classification `json:"classified,omitempty"`
	ClassifiedMtime string          `json:"classified_mtime,omitempty"`
	Narrative       *Narrative      `json:"narrative,omitempty"`
	ReportMtime     string          `json:"report_mtime,omitempty"`
	Error           string          `json:"error,omitempty"`
}

func Load(dir string) *Report {
	if st, err := os.Stat(dir); dir == "" || err != nil || !st.IsDir() {
		return nil
	}
	r := &Report{Dir: usage.Tilde(dir)}
	var problems []string
	read := func(name string, into any) (string, bool) {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return "", false
		}
		if err == nil {
			err = json.Unmarshal(raw, into)
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			return "", false
		}
		return mtime(path), true
	}
	var c Classification
	if m, ok := read("classified.json", &c); ok {
		if c.Themes == nil || c.Sessions == nil {
			problems = append(problems, "classified.json: expected {themes: [...], sessions: {...}}")
		} else {
			r.Classified, r.ClassifiedMtime = &c, m
		}
	}
	var n Narrative
	if _, ok := read("narrative.json", &n); ok {
		r.Narrative = &n
	}
	if _, err := os.Stat(filepath.Join(dir, ReportName)); err == nil {
		r.ReportMtime = mtime(filepath.Join(dir, ReportName))
	}
	r.Error = strings.Join(problems, "; ")
	if r.Classified == nil && r.ReportMtime == "" && r.Error == "" {
		return nil
	}
	return r
}

func mtime(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return st.ModTime().Format(time.RFC3339)
}

func (c *Classification) Lookup(id string) (Classified, bool) {
	if c == nil {
		return Classified{}, false
	}
	if x, ok := c.Sessions[id]; ok {
		return x, true
	}
	for k, x := range c.Sessions {
		if len(k) >= 6 && strings.HasPrefix(id, k) {
			return x, true
		}
	}
	return Classified{}, false
}

func (c *Classification) ThemeName(id string) string {
	for _, t := range c.Themes {
		if t.ID == id {
			return t.Name
		}
	}
	return id
}
