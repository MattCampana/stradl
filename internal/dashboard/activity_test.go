package dashboard

import (
	"reflect"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/sync"
)

func TestDayKeys(t *testing.T) {
	loc := newYork(t)
	now := time.Date(2026, 10, 25, 15, 0, 0, 0, loc)
	var got [][2]string
	for _, d := range dayKeys(now, loc) {
		got = append(got, [2]string{d.Date, d.Label})
		if d.Sessions == nil {
			t.Errorf("%s: Sessions is nil, want an empty list", d.Date)
		}
	}
	want := [][2]string{
		{"2026-10-25", "Today"}, {"2026-10-24", "Yesterday"}, {"2026-10-23", "Fri Oct 23"},
		{"2026-10-22", "Thu Oct 22"}, {"2026-10-21", "Wed Oct 21"}, {"2026-10-20", "Tue Oct 20"},
		{"2026-10-19", "Mon Oct 19"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("days = %v", got)
	}

	// Just after midnight following the end of daylight saving time (a
	// 25-hour day), yesterday is still Nov 1.
	after := time.Date(2026, 11, 2, 0, 30, 0, 0, loc)
	days := dayKeys(after, loc)
	if days[0].Date != "2026-11-02" || days[1].Date != "2026-11-01" || days[2].Date != "2026-10-31" {
		t.Errorf("around DST: %v %v %v", days[0].Date, days[1].Date, days[2].Date)
	}
}

func TestBuildDays(t *testing.T) {
	loc := newYork(t)
	now := time.Date(2026, 10, 25, 15, 0, 0, 0, loc)
	desk := parseFixture(t, "desktop-session.jsonl", loc, false).summary(testHome)
	desk.ID = desktopID
	cli := parseFixture(t, "cli-session.jsonl", loc, false).summary(testHome)
	cli.ID = cliID
	sessions := []Session{{ID: desktopID, Title: "Login page", Project: "app"}, {ID: cliID, Title: "API", Project: "api"}}
	summaries := map[string]*sessionSummary{desktopID: desk, cliID: cli}

	act := newDeviceActivity(loc)
	act.add("Studio", []sync.RecentChange{{SessionID: desktopID, At: ts("2026-10-24T03:35:00Z")}}, now) // Oct 23 in New York
	act.add("Laptop", []sync.RecentChange{
		{SessionID: desktopID, At: ts("2026-10-25T13:05:00Z")},
		{SessionID: cliID, At: ts("2026-10-10T12:00:00Z")}, // outside the window
	}, now)

	days := buildDays(sessions, summaries, act, now, loc)
	byDate := map[string][]DaySession{}
	for _, d := range days {
		byDate[d.Date] = d.Sessions
	}
	if got := byDate["2026-10-25"]; len(got) != 1 || got[0].ID != desktopID || !reflect.DeepEqual(got[0].Devices, []string{"Laptop"}) {
		t.Errorf("today = %+v", got)
	}
	if got := byDate["2026-10-23"]; len(got) != 1 || !reflect.DeepEqual(got[0].Devices, []string{"Studio"}) || got[0].Turns != 1 {
		t.Errorf("Oct 23 = %+v", got)
	}
	if got := byDate["2026-10-24"]; len(got) != 1 || len(got[0].Devices) != 0 || got[0].Devices == nil {
		t.Errorf("Oct 24 = %+v (no Mac recorded a change that day)", got)
	}
	if got := byDate["2026-10-19"]; len(got) != 1 || got[0].ID != cliID || !reflect.DeepEqual(got[0].FilesTouched, []string{"~/scratch/server.go"}) {
		t.Errorf("Oct 19 = %+v", got)
	}
	if got := byDate["2026-10-22"]; len(got) != 0 {
		t.Errorf("a quiet day lists %+v", got)
	}
	if got := act.forSession(desktopID); !reflect.DeepEqual(got, []string{"Studio", "Laptop"}) {
		t.Errorf("devices for the session = %v, want in device order", got)
	}
	if got := act.forSession(cliID); len(got) != 0 {
		t.Errorf("a change older than a week still counts: %v", got)
	}
}

func TestRecentIDs(t *testing.T) {
	now := ts("2026-10-25T12:00:00Z")
	got := recentIDs([]sync.RecentChange{
		{SessionID: "a", At: now.Add(-3 * time.Hour)},
		{SessionID: "b", At: now.Add(-time.Hour)},
		{SessionID: "a", At: now.Add(-30 * time.Minute)},
		{SessionID: "c", At: now.Add(-8 * 24 * time.Hour)},
		{SessionID: "", At: now},
	}, now)
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("recentIDs = %v", got)
	}
}
