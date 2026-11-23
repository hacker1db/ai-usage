package sources

import (
	"path/filepath"
	"testing"
	"time"

	"ai-usage/internal/m365"
	"ai-usage/internal/usage"
)

func TestCollectM365(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m365.json")
	today := time.Now().Format("2006-01-02")
	old := time.Now().AddDate(0, 0, -60).Format("2006-01-02")
	s, _ := m365.Load(path)
	s.Merge("me@x", []m365.Day{
		{Date: today, Actions: 4, Word: true, Outlook: true},
		{Date: old, Actions: 7},
	})
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	agg := usage.NewAggregator(time.Now().AddDate(0, 0, -7), usage.Daily)
	st := CollectM365(agg, path)
	if st.Status != "ok" || st.Requests != 4 {
		t.Fatalf("status %+v", st)
	}
	var reqs, total int64
	for _, r := range agg.Rows() {
		if r.Tool != usage.ToolM365 || r.Profile != M365Profile {
			t.Fatalf("row %+v", r)
		}
		reqs += r.Requests
		total += r.Total
	}
	if reqs != 4 || total != 0 {
		t.Fatalf("requests %d total %d", reqs, total)
	}
	if st := CollectM365(usage.NewAggregator(time.Now(), usage.Daily), filepath.Join(t.TempDir(), "none")); st.Status != "not found" {
		t.Fatalf("missing store: %+v", st)
	}
}
