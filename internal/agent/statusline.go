package agent

import (
	"fmt"
	"time"
)

// StatusLine is the one-line summary shown at the top of the menu, e.g.
// "Synced 2 min ago · 9 sessions".
func StatusLine(s Snapshot, now time.Time) string {
	switch {
	case s.Syncing:
		return "Syncing…"
	case s.Paused:
		return "Paused"
	case s.LastError == NotConfigured:
		return "Not set up: run stradl init in Terminal"
	case s.LastError != "":
		return "Error: " + truncate(s.LastError, 48) + " (see log)"
	case s.LastSyncAt.IsZero():
		return "Starting…"
	}
	return fmt.Sprintf("Synced %s · %s", Ago(now.Sub(s.LastSyncAt)), Plural(s.SessionsTotal, "session"))
}

// RestartLine labels the menu item that restarts the Claude app, or "" when
// there is nothing waiting for a restart.
func RestartLine(s Snapshot) string {
	n := s.PendingDesktopRestart
	if n <= 0 {
		return ""
	}
	verb := "Restart"
	if !s.AppRunning {
		verb = "Open"
	}
	if n == 1 {
		return verb + " Claude to show 1 new session"
	}
	return fmt.Sprintf("%s Claude to show %d new sessions", verb, n)
}

// Ago renders a duration in the past for humans.
func Ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return Plural(int(d/time.Minute), "min") + " ago"
	case d < 24*time.Hour:
		return Plural(int(d/time.Hour), "hr") + " ago"
	default:
		return Plural(int(d/(24*time.Hour)), "day") + " ago"
	}
}

// Plural renders "1 session" / "3 sessions". "min" and "hr" stay as is.
func Plural(n int, unit string) string {
	if n == 1 || unit == "min" || unit == "hr" {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
