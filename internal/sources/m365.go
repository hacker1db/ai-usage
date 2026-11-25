package sources

import (
	"strings"
	"time"

	"ai-usage/internal/m365"
	"ai-usage/internal/usage"
)

const M365Profile = "work-m365"

func CollectM365(agg *usage.Aggregator, storePath string) Status {
	storePath = usage.ExpandHome(storePath)
	st := Status{Source: usage.ToolM365, Path: usage.Tilde(storePath)}
	if missing := statSource(storePath, false); missing != "" {
		st.Status = missing
		return st
	}
	s, err := m365.Load(storePath)
	if err != nil {
		st.Status = "error: " + err.Error()
		return st
	}
	for _, d := range s.Sorted() {
		day, err := time.ParseInLocation("2006-01-02", d.Date, time.Local)
		if err != nil {
			continue
		}
		at := day.Add(12 * time.Hour)
		sid := "day-" + d.Date
		added := false
		for range d.Actions {
			if agg.Add(usage.ToolM365, M365Profile, usage.Record{Model: "m365-copilot", Time: at, Session: sid}) {
				st.Requests++
				added = true
			}
		}
		if !added {
			continue
		}
		m := usage.Meta{TitleSrc: "report"}
		apps := d.Apps()
		m.Title = "Microsoft 365 Copilot"
		if len(apps) > 0 {
			m.Title += ": " + strings.Join(apps, ", ")
		}
		for _, a := range apps {
			m.Tools.Add(a, 1)
		}
		agg.Note(usage.SessionKey{Tool: usage.ToolM365, Profile: M365Profile, ID: sid}, m)
	}
	st.Status = "ok"
	return st
}
