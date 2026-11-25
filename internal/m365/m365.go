// Package m365 syncs one user's Microsoft 365 Copilot daily usage from the Microsoft Graph
// Copilot usage report into a local store that the collector reads offline.
//
// Graph exposes no per-prompt token counts. The beta report
// getMicrosoft365CopilotUsageUserDetailByDate gives, per user and day, a Copilot action count,
// chat query counts, and per-app activity flags, for the latest refresh date and the seven days
// before it. The data lags 24-72 hours. Calling it needs Reports.Read.All (admin consent) and, for
// delegated tokens, a reporting role such as Reports Reader.
package m365

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GraphBase is the Graph endpoint root. Tests point it at a fake server.
const GraphBase = "https://graph.microsoft.com"

// Scope is the token scope for Graph.
const Scope = "https://graph.microsoft.com/.default"

// Day is one user's Copilot usage on one date, as the report returns it.
type Day struct {
	RefreshDate  string `json:"reportRefreshDate"`
	Date         string `json:"reportSelectedDate"`
	UPN          string `json:"userPrincipalName"`
	Actions      int64  `json:"allAppsCopilotActionCount"`
	ChatQueries  int64  `json:"m365ChatQueriesSubmitted"`
	WebPrompts   int64  `json:"businessChatWebPromptsCount"`
	IsActive     bool   `json:"isActive"`
	Teams        bool   `json:"isActiveInTeams"`
	Word         bool   `json:"isActiveInWord"`
	PowerPoint   bool   `json:"isActiveInPowerPoint"`
	Excel        bool   `json:"isActiveInExcel"`
	Outlook      bool   `json:"isActiveInOutlook"`
	OneNote      bool   `json:"isActiveInOneNote"`
	Loop         bool   `json:"isActiveInLoop"`
	M365Chat     bool   `json:"isActiveInM365Chat"`
	BizChatWeb   bool   `json:"isActiveInBusinessChatWeb"`
	Edge         bool   `json:"isActiveInEdge"`
	CopilotApp   bool   `json:"isActiveInM365CopilotChatCopilotApp"`
	DisplayName  string `json:"displayName,omitempty"`
	FetchedAtUTC string `json:"fetchedAt,omitempty"`
}

// Apps lists the apps the day was active in.
func (d Day) Apps() []string {
	var out []string
	for _, a := range []struct {
		on   bool
		name string
	}{{d.Teams, "Teams"}, {d.Outlook, "Outlook"}, {d.Word, "Word"}, {d.Excel, "Excel"},
		{d.PowerPoint, "PowerPoint"}, {d.OneNote, "OneNote"}, {d.Loop, "Loop"}, {d.M365Chat, "M365 Chat"},
		{d.BizChatWeb, "Chat (web)"}, {d.Edge, "Edge"}, {d.CopilotApp, "Copilot app"}} {
		if a.on {
			out = append(out, a.name)
		}
	}
	return out
}

// TokenSource returns a bearer token for Graph.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// Client calls the Graph report.
type Client struct {
	Base   string // GraphBase when empty
	HTTP   *http.Client
	Tokens TokenSource
	// Sleep waits between throttled retries; tests replace it.
	Sleep func(time.Duration)
}

// ErrNoRows means the report had no row for the user, which usually means the tenant conceals
// user names in reports (Microsoft 365 admin center > Settings > Org settings > Reports).
var ErrNoRows = errors.New("no report rows for this user; if the tenant conceals user names in reports, ask an admin to show them, or pass the concealed id as -upn")

// FetchDay returns every row the report has for date (YYYY-MM-DD), following @odata.nextLink.
func (c *Client) FetchDay(ctx context.Context, date string) ([]Day, error) {
	base := c.Base
	if base == "" {
		base = GraphBase
	}
	next := base + "/beta/copilot/reports/getMicrosoft365CopilotUsageUserDetailByDate(date=" + date + ")"
	var out []Day
	for pages := 0; next != ""; pages++ {
		if pages > 500 {
			return nil, errors.New("too many report pages")
		}
		// Only follow links back to the same host, so a token never leaves Graph.
		if !strings.HasPrefix(next, base+"/") {
			return nil, fmt.Errorf("refusing nextLink outside %s", base)
		}
		var page struct {
			Value    []Day  `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		if err := c.get(ctx, next, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Value...)
		next = page.NextLink
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, url string, v any) error {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	sleep := c.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.Tokens.Token(ctx)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return json.Unmarshal(body, v)
		case (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < 4:
			wait := time.Duration(attempt+1) * 5 * time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 && s <= 300 {
				wait = time.Duration(s) * time.Second
			}
			sleep(wait)
			continue
		}
		return graphError(resp.StatusCode, body)
	}
}

// GraphError is a non-success Graph response.
type GraphError struct {
	Status int
	Msg    string
}

func (e *GraphError) Error() string { return fmt.Sprintf("graph %d: %s", e.Status, e.Msg) }

func graphError(status int, body []byte) error {
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &e) == nil && e.Error.Code != "" {
		msg = e.Error.Code + ": " + e.Error.Message
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	hint := ""
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		hint = " (needs Reports.Read.All with admin consent, plus a reporting role such as Reports Reader for a signed-in user)"
	}
	return &GraphError{Status: status, Msg: msg + hint}
}

// Sync fetches the report's newest window and returns upn's rows. The window is the latest
// refresh date and the seven days before it; the refresh date is learned from the first call.
func (c *Client) Sync(ctx context.Context, upn string, now time.Time) ([]Day, error) {
	// Probe dates back from today until one is inside the window and has rows; a date outside the
	// window is rejected with 400, and any other error (auth, throttling) is returned as is.
	refresh := ""
	for back := 2; back <= 10 && refresh == ""; back++ {
		rows, err := c.FetchDay(ctx, now.UTC().AddDate(0, 0, -back).Format("2006-01-02"))
		var ge *GraphError
		if errors.As(err, &ge) && ge.Status == http.StatusBadRequest {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, d := range rows {
			if d.RefreshDate > refresh {
				refresh = d.RefreshDate
			}
		}
	}
	end, err := time.Parse("2006-01-02", refresh)
	if err != nil {
		return nil, ErrNoRows
	}
	fetched := now.UTC().Format(time.RFC3339)
	var out []Day
	seen := map[string]bool{}
	for i := 7; i >= 0; i-- {
		date := end.AddDate(0, 0, -i).Format("2006-01-02")
		rows, err := c.FetchDay(ctx, date)
		if err != nil {
			return out, fmt.Errorf("%s: %w", date, err)
		}
		for _, d := range rows {
			if !strings.EqualFold(d.UPN, upn) || seen[d.Date] {
				continue
			}
			if d.Date == "" {
				d.Date = date
			}
			seen[d.Date] = true
			d.FetchedAtUTC = fetched
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil, ErrNoRows
	}
	return out, nil
}

// Store is the local file of synced days, keyed by date. Newer fetches of a date replace older
// ones, and dates that fall out of Graph's window are kept, so history grows with each sync.
type Store struct {
	UPN  string         `json:"upn"`
	Days map[string]Day `json:"days"`
}

// Load reads a store; a missing file is an empty store.
func Load(path string) (Store, error) {
	s := Store{Days: map[string]Day{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	if s.Days == nil {
		s.Days = map[string]Day{}
	}
	return s, nil
}

// Merge adds days for upn. A store for another user is replaced rather than mixed.
func (s *Store) Merge(upn string, days []Day) {
	if !strings.EqualFold(s.UPN, upn) {
		s.UPN, s.Days = upn, map[string]Day{}
	}
	for _, d := range days {
		s.Days[d.Date] = d
	}
}

// Sorted returns the days in date order.
func (s Store) Sorted() []Day {
	out := make([]Day, 0, len(s.Days))
	for _, d := range s.Days {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

// Save writes the store atomically with owner-only permissions.
func (s Store) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// CreateTemp makes a new owner-only file with a random name, so concurrent syncs never share a
	// temporary file and a pre-existing one can't lend its permissions or a symlink.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
