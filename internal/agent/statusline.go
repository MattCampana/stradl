package agent

import (
	"fmt"
	"time"
)

// StatusLine is the one-line summary shown at the top of the menu, e.g.
// "Synced 2 min ago · 9 sessions", or after a failure a short line saying
// what is wrong, e.g. "Offline: will retry".
func StatusLine(s Snapshot, now time.Time) string {
	switch {
	case s.Syncing && s.SyncTotal > 0:
		return fmt.Sprintf("Syncing… %d of %s", s.SyncDone, Plural(s.SyncTotal, "file"))
	case s.Syncing:
		return "Syncing…"
	case s.Paused:
		return "Paused"
	case s.LastError == NotConfigured:
		return "Not set up: run stradl init in Terminal"
	case s.LastError != "":
		return s.ErrorSummary()
	case s.LastSyncAt.IsZero():
		return "Starting…"
	}
	return fmt.Sprintf("Synced %s · %s", Ago(now.Sub(s.LastSyncAt)), Plural(s.SessionsTotal, "session"))
}

// AttemptLine is the menu line under StatusLine after a failed sync, e.g.
// "Last attempt: just now (failed) · last sync: 2 hr ago", or "" when the
// last attempt succeeded.
func AttemptLine(s Snapshot, now time.Time) string {
	if !s.AttemptFailed() {
		return ""
	}
	last := "never"
	if !s.LastSyncAt.IsZero() {
		last = Ago(now.Sub(s.LastSyncAt))
	}
	return fmt.Sprintf("Last attempt: %s (failed) · last sync: %s", Ago(now.Sub(s.LastAttemptAt)), last)
}

// RestartLine labels the menu item that restarts the Claude app, or "" when
// there is nothing waiting for a restart.
func RestartLine(s Snapshot) string {
	n := s.PendingDesktopRestart
	if n <= 0 && !s.SidebarPending {
		return ""
	}
	verb := "Restart"
	if !s.AppRunning {
		verb = "Open"
	}
	switch {
	case n <= 0:
		return verb + " Claude to apply sidebar changes"
	case n == 1 && s.SidebarPending:
		return verb + " Claude to show 1 new session and sidebar changes"
	case n == 1:
		return verb + " Claude to show 1 new session"
	case s.SidebarPending:
		return fmt.Sprintf("%s Claude to show %d new sessions and sidebar changes", verb, n)
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
