package dashboard

import (
	"sort"
	"time"

	"github.com/tawanorg/claude-sync/internal/sync"
)

// activityDays is how many calendar days the overview covers.
const activityDays = 7

// dayKeys returns the last activityDays local dates, newest first, with
// their labels.
func dayKeys(now time.Time, loc *time.Location) []Day {
	local := now.In(loc)
	days := make([]Day, 0, activityDays)
	for i := 0; i < activityDays; i++ {
		d := time.Date(local.Year(), local.Month(), local.Day()-i, 12, 0, 0, 0, loc)
		label := d.Format("Mon Jan 2")
		switch i {
		case 0:
			label = "Today"
		case 1:
			label = "Yesterday"
		}
		days = append(days, Day{Date: d.Format("2006-01-02"), Label: label, Sessions: []DaySession{}})
	}
	return days
}

// buildDays lists, for each of the last seven days, the sessions in which
// something was asked or a file was changed that day, latest activity first.
func buildDays(sessions []Session, summaries map[string]*sessionSummary, devices *deviceActivity, now time.Time, loc *time.Location) []Day {
	days := dayKeys(now, loc)
	for i := range days {
		date := days[i].Date
		for _, s := range sessions {
			sum := summaries[s.ID]
			if sum == nil {
				continue
			}
			d, ok := sum.Days[date]
			if !ok || (d.Turns == 0 && len(d.Files) == 0) {
				continue
			}
			days[i].Sessions = append(days[i].Sessions, DaySession{
				ID:           s.ID,
				Title:        s.Title,
				Project:      s.Project,
				Turns:        d.Turns,
				Prompts:      nonNil(d.Prompts),
				FilesTouched: nonNil(d.Files),
				Devices:      devices.onDay(s.ID, date),
				FirstAt:      d.First,
				LastAt:       d.Last,
			})
		}
		sort.SliceStable(days[i].Sessions, func(a, b int) bool {
			sa, sb := days[i].Sessions[a], days[i].Sessions[b]
			if !sa.LastAt.Equal(sb.LastAt) {
				return sa.LastAt.After(sb.LastAt)
			}
			return sa.ID < sb.ID
		})
	}
	return days
}

// deviceActivity answers which Macs changed a session recently, from the
// devices' heartbeats.
type deviceActivity struct {
	loc    *time.Location
	recent map[string]map[string]bool            // session → device names (last 7 days)
	byDay  map[string]map[string]map[string]bool // session → date → device names
	order  map[string]int                        // device name → display order
}

func newDeviceActivity(loc *time.Location) *deviceActivity {
	return &deviceActivity{
		loc:    loc,
		recent: make(map[string]map[string]bool),
		byDay:  make(map[string]map[string]map[string]bool),
		order:  make(map[string]int),
	}
}

// add records that device name changed the sessions in changes. Entries
// older than the recent window are ignored.
func (a *deviceActivity) add(name string, changes []sync.RecentChange, now time.Time) {
	if _, ok := a.order[name]; !ok {
		a.order[name] = len(a.order)
	}
	floor := now.Add(-sync.RecentWindow)
	for _, c := range changes {
		if c.SessionID == "" || c.At.Before(floor) {
			continue
		}
		if a.recent[c.SessionID] == nil {
			a.recent[c.SessionID] = make(map[string]bool)
		}
		a.recent[c.SessionID][name] = true
		date := c.At.In(a.loc).Format("2006-01-02")
		if a.byDay[c.SessionID] == nil {
			a.byDay[c.SessionID] = make(map[string]map[string]bool)
		}
		if a.byDay[c.SessionID][date] == nil {
			a.byDay[c.SessionID][date] = make(map[string]bool)
		}
		a.byDay[c.SessionID][date][name] = true
	}
}

// forSession lists the Macs that changed a session in the last 7 days.
func (a *deviceActivity) forSession(id string) []string {
	return a.names(a.recent[id])
}

// onDay lists the Macs that changed a session on date.
func (a *deviceActivity) onDay(id, date string) []string {
	return a.names(a.byDay[id][date])
}

func (a *deviceActivity) names(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return a.order[out[i]] < a.order[out[j]] })
	return out
}

// recentIDs lists the distinct sessions in changes within the recent
// window, most recently changed first.
func recentIDs(changes []sync.RecentChange, now time.Time) []string {
	latest := make(map[string]time.Time)
	floor := now.Add(-sync.RecentWindow)
	for _, c := range changes {
		if c.SessionID == "" || c.At.Before(floor) {
			continue
		}
		if c.At.After(latest[c.SessionID]) {
			latest[c.SessionID] = c.At
		}
	}
	out := make([]string, 0, len(latest))
	for id := range latest {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		if !latest[out[i]].Equal(latest[out[j]]) {
			return latest[out[i]].After(latest[out[j]])
		}
		return out[i] < out[j]
	})
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
