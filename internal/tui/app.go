package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"ai-usage/internal/snapshot"
	"ai-usage/internal/view"
)

// Options configure the TUI.
type Options struct {
	URL     string           // running dashboard, e.g. http://127.0.0.1:8790
	Local   bool             // skip the dashboard and collect in-process
	Collect snapshot.Options // used for local collection (history should be read-only)
	Theme   Theme
	Width   int // -once width; 0 = detect
	Out     io.Writer
	Filters view.Filters // initial selections
}

// source supplies snapshots.
type source interface {
	Start(ctx context.Context)
	Current() (*snapshot.Snapshot, Status)
	Refresh()
}

// serverSource polls a running dashboard's /api/usage.
type serverSource struct {
	base   string
	label  string
	client *http.Client

	mu     sync.Mutex
	etag   string
	snap   *snapshot.Snapshot
	err    error
	lastOK time.Time
	poke   chan struct{}
}

func newServerSource(base string) *serverSource {
	u, _ := url.Parse(base)
	label := "live via dashboard"
	if u != nil && u.Port() != "" {
		label += " :" + u.Port()
	}
	return &serverSource{base: strings.TrimRight(base, "/"), label: label,
		client: &http.Client{Timeout: 10 * time.Second}, poke: make(chan struct{}, 1)}
}

// fetch returns a network error (as opposed to an HTTP or decode problem) when the server is down.
func (s *serverSource) fetch(ctx context.Context) (netErr error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/api/usage", nil)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.etag != "" {
		req.Header.Set("If-None-Match", s.etag)
	}
	s.mu.Unlock()
	resp, err := s.client.Do(req)
	if err != nil {
		s.setErr(err)
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err != nil:
		s.err = err
	case resp.StatusCode == http.StatusNotModified:
		s.err, s.lastOK = nil, time.Now()
	case resp.StatusCode != http.StatusOK:
		s.err = fmt.Errorf("dashboard answered %s", resp.Status)
	default:
		snap, derr := snapshot.Decode(body)
		if derr != nil {
			s.err = fmt.Errorf("dashboard is still collecting or sent an unexpected snapshot (%v)", derr)
			break
		}
		s.snap, s.etag, s.err, s.lastOK = snap, resp.Header.Get("ETag"), nil, time.Now()
	}
	return nil
}

func (s *serverSource) setErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

func (s *serverSource) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-s.poke:
			}
			_ = s.fetch(ctx)
		}
	}()
}

func (s *serverSource) Current() (*snapshot.Snapshot, Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Source: s.label, Stale: time.Since(s.lastOK) > 30*time.Second}
	if s.err != nil {
		st.Notice = s.err.Error()
	}
	return s.snap, st
}

// Refresh asks the dashboard to collect now, then re-fetches a few times while it does.
func (s *serverSource) Refresh() {
	go func() {
		req, err := http.NewRequest(http.MethodPost, s.base+"/api/refresh", nil)
		if err != nil {
			return
		}
		req.Header.Set("X-Requested-With", "ai-usage")
		if resp, err := s.client.Do(req); err == nil {
			resp.Body.Close()
		}
		for range 6 {
			time.Sleep(time.Second)
			select {
			case s.poke <- struct{}{}:
			default:
			}
		}
	}()
}

// localSource collects in-process.
type localSource struct{ c *snapshot.Collector }

func (l *localSource) Start(ctx context.Context) { go l.c.Run(ctx) }

func (l *localSource) Current() (*snapshot.Snapshot, Status) {
	snap, _, _, err := l.c.Current()
	st := Status{Source: "local collect"}
	if err != nil {
		st.Notice = err.Error()
	}
	if snap == nil {
		st.Notice = "collecting…"
	}
	return snap, st
}

func (l *localSource) Refresh() { l.c.Wake() }

// pickSource uses the dashboard when it answers, else collects locally.
func pickSource(ctx context.Context, o Options) (source, string) {
	if !o.Local && o.URL != "" {
		s := newServerSource(o.URL)
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := s.fetch(pctx)
		cancel()
		var ne net.Error
		var oe *net.OpError
		if err == nil {
			return s, ""
		}
		if !errors.As(err, &oe) && !errors.As(err, &ne) {
			return s, ""
		}
	}
	return &localSource{snapshot.New(o.Collect)}, ""
}

// Once prints one frame (header and body) to o.Out.
func Once(ctx context.Context, o Options) error {
	src, _ := pickSource(ctx, o)
	if l, ok := src.(*localSource); ok {
		l.c.Refresh()
	}
	snap, st := src.Current()
	if snap == nil {
		return fmt.Errorf("no data: %s", st.Notice)
	}
	w := o.Width
	if w <= 0 {
		w = 120
	}
	d := view.Build(snap, o.Filters, time.Now())
	f := Draw(&d, o.Theme, w, st)
	_, err := io.WriteString(o.Out, Render(append(f.Header, f.Body...), o.Theme.Escapes, "\n")+"\n")
	return err
}

// Run starts the live dashboard on the terminal until the user quits.
func Run(ctx context.Context, o Options) (err error) {
	in, out := int(os.Stdin.Fd()), os.Stdout
	if !IsTerminal(in) {
		return errors.New("tui needs a terminal on stdin (use -once to print one frame)")
	}
	raw, err := makeRaw(in)
	if err != nil {
		return err
	}
	_, _ = io.WriteString(out, "\x1b[?1049h\x1b[?25l")
	restore := func() {
		_, _ = io.WriteString(out, "\x1b[0m\x1b[?25h\x1b[?1049l")
		raw.restore()
	}
	defer func() {
		if p := recover(); p != nil {
			restore()
			panic(p)
		}
		restore()
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	src, _ := pickSource(ctx, o)
	src.Start(ctx)

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	keys := make(chan string, 16)
	go readKeys(os.Stdin, keys)

	ui := &screen{filters: o.Filters.Normalize(), theme: o.Theme}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		ui.draw(out, src)
		select {
		case <-ctx.Done():
			return nil
		case s := <-sigs:
			if s != syscall.SIGWINCH {
				return nil
			}
		case <-tick.C:
		case k, ok := <-keys:
			if !ok || ui.key(k, src) {
				return nil
			}
		}
	}
}

// screen is the live view state.
type screen struct {
	filters view.Filters
	theme   Theme
	scroll  int
	help    bool
	bodyH   int
	bodyLen int
}

// key handles one key; it returns true to quit.
func (s *screen) key(k string, src source) bool {
	f := &s.filters
	page := max(s.bodyH-2, 1)
	switch k {
	case "q", "ctrl-c":
		return true
	case "?":
		s.help = !s.help
	case "esc":
		s.help = false
	case "tab":
		f.Tab = view.Cycle(view.TabOptions(), f.Tab, 1)
	case "shift-tab":
		f.Tab = view.Cycle(view.TabOptions(), f.Tab, -1)
	case "1", "2", "3", "4", "5":
		f.Tab = view.Tabs[int(k[0]-'1')].ID
	case "m":
		f.Metric = view.Cycle(view.MetricOptions(), f.Metric, 1)
	case "M":
		f.Metric = view.Cycle(view.MetricOptions(), f.Metric, -1)
	case "r":
		f.Range = view.Cycle(view.RangeOptions(), f.Range, 1)
	case "R":
		f.Range = view.Cycle(view.RangeOptions(), f.Range, -1)
	case "b":
		f.Burn = view.Cycle(view.BurnOptions(), f.Burn, 1)
	case "s":
		f.Sort = view.Cycle(view.SessionSorts, f.Sort, 1)
	case "o":
		opts := []view.Option{{ID: "", Label: "All"}}
		if snap, _ := src.Current(); snap != nil {
			for _, l := range snap.Meta.Ownership.Labels {
				opts = append(opts, view.Option{ID: l, Label: l})
			}
		}
		f.Own = view.Cycle(opts, f.Own, 1)
	case "f":
		src.Refresh()
	case "j", "down":
		s.scroll++
	case "k", "up":
		s.scroll--
	case " ", "pgdn", "ctrl-f", "ctrl-d":
		s.scroll += page
	case "pgup", "ctrl-b", "ctrl-u":
		s.scroll -= page
	case "g", "home":
		s.scroll = 0
	case "G", "end":
		s.scroll = s.bodyLen
	}
	return false
}

var helpLines = []string{
	"Keys",
	"",
	"  tab / shift-tab, 1-5   switch tab (All, Claude, Codex, Copilot, Foundry)",
	"  m / M                  next / previous metric",
	"  r / R                  next / previous range (24h, 7d, 30d)",
	"  b                      burn-rate window (1h, 3h, 6h)",
	"  s                      session sort (recent, tokens, active time)",
	"  o                      ownership filter for projects and sessions (all, personal, work, ...)",
	"  f                      refresh now",
	"  j k ↑ ↓                scroll a line",
	"  space PgDn / PgUp      scroll a page",
	"  g / G                  top / bottom",
	"  ?  esc                 toggle / close this help",
	"  q  ctrl-c              quit",
}

func (s *screen) draw(out io.Writer, src source) {
	cols, rows, ok := Size(int(os.Stdout.Fd()))
	if !ok {
		cols, rows = 120, 40
	}
	th := s.theme
	var lines []Line
	snap, st := src.Current()
	switch {
	case s.help:
		doc := &Doc{W: cols}
		for i, l := range helpLines {
			if i == 0 {
				doc.Text(l, th.Title())
			} else {
				doc.Text(l, th.Secondary())
			}
		}
		lines = doc.Lines
	case snap == nil:
		doc := &Doc{W: cols}
		doc.Text("ai-usage · Token usage", th.Title())
		doc.Text(st.Source+" · "+st.Notice, th.Muted())
		lines = doc.Lines
	default:
		d := view.Build(snap, s.filters, time.Now())
		f := Draw(&d, th, cols, st)
		s.bodyH = max(rows-len(f.Header)-1, 1)
		s.bodyLen = len(f.Body)
		s.scroll = min(max(s.scroll, 0), max(len(f.Body)-s.bodyH, 0))
		end := min(s.scroll+s.bodyH, len(f.Body))
		lines = append(append([]Line{}, f.Header...), f.Body[s.scroll:end]...)
		for len(lines) < rows-1 {
			lines = append(lines, fit(nil, cols))
		}
		pos := fmt.Sprintf(" %d-%d of %d ", s.scroll+1, end, len(f.Body))
		hint := "? help · tab switch · m metric · r range · b burn · s sort · o owner · f refresh · j/k scroll · q quit"
		foot := new(B).T(Truncate(hint, cols-Width(pos)-1), th.Muted())
		foot.To(cols-Width(pos)).T(pos, th.Secondary())
		lines = append(lines, foot.Line(cols))
	}
	if len(lines) > rows {
		lines = lines[:rows]
	}
	var b strings.Builder
	b.WriteString("\x1b[H")
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(Render([]Line{l}, th.Escapes, ""))
		b.WriteString("\x1b[K")
	}
	b.WriteString("\x1b[J")
	_, _ = io.WriteString(out, b.String())
}

// readKeys decodes stdin bytes into key names until stdin closes.
func readKeys(r io.Reader, keys chan<- string) {
	defer close(keys)
	buf := make([]byte, 64)
	for {
		n, err := r.Read(buf)
		if err != nil {
			return
		}
		for _, k := range ParseKeys(buf[:n]) {
			keys <- k
		}
	}
}

// ParseKeys splits a read into key names: printable characters as themselves, plus "tab",
// "shift-tab", "up", "down", "pgup", "pgdn", "home", "end", "esc", and "ctrl-<letter>".
func ParseKeys(b []byte) []string {
	var out []string
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == 0x1b:
			if i+2 < len(b) && (b[i+1] == '[' || b[i+1] == 'O') {
				seq, j := csi(b[i+2:])
				out = append(out, seq)
				i += 1 + j
				continue
			}
			out = append(out, "esc")
		case c == '\t':
			out = append(out, "tab")
		case c == '\r' || c == '\n':
			out = append(out, "enter")
		case c < 0x20:
			out = append(out, "ctrl-"+string(rune('a'+c-1)))
		case c < 0x80:
			out = append(out, string(rune(c)))
		}
	}
	return out
}

// csi decodes the rest of an escape sequence and returns its name and length consumed.
func csi(b []byte) (string, int) {
	names := map[string]string{"A": "up", "B": "down", "C": "right", "D": "left", "H": "home", "F": "end",
		"Z": "shift-tab", "5~": "pgup", "6~": "pgdn", "1~": "home", "4~": "end", "7~": "home", "8~": "end"}
	for j := range b {
		if c := b[j]; c >= 0x40 && c <= 0x7e {
			seq := string(b[:j+1])
			if n, ok := names[seq]; ok {
				return n, j + 1
			}
			return "unknown", j + 1
		}
	}
	return "unknown", len(b)
}
