package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Commander runs a program and returns its standard output. Tests replace it
// so they never touch real processes or apps.
type Commander func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecCommand runs name with the C locale, so tools like ps print dates in a
// fixed format whatever the user's language.
func ExecCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.Output()
}

// DesktopApp observes and restarts the Claude desktop app, which reads its
// Code-tab sidebar only at launch.
type DesktopApp struct {
	Run         Commander
	Name        string        // process and application name, "Claude"
	QuitTimeout time.Duration // how long Restart waits for the app to exit
	PollEvery   time.Duration
}

// NewDesktopApp returns a DesktopApp for the real Claude app.
func NewDesktopApp() *DesktopApp {
	return &DesktopApp{Run: ExecCommand, Name: "Claude", QuitTimeout: 20 * time.Second, PollEvery: 500 * time.Millisecond}
}

// pids lists the app's main processes (exact name match, so helper
// processes and the lowercase `claude` CLI don't count).
func (a *DesktopApp) pids(ctx context.Context) []int {
	out, err := a.Run(ctx, "/usr/bin/pgrep", "-x", a.Name)
	if err != nil {
		return nil // pgrep exits 1 when nothing matches
	}
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// StartTime reports when the running app was launched. When several
// processes match, the earliest launch wins: the restart prompt then errs
// toward showing, never toward hiding sessions the app hasn't loaded.
func (a *DesktopApp) StartTime(ctx context.Context) (time.Time, bool) {
	var earliest time.Time
	for _, pid := range a.pids(ctx) {
		out, err := a.Run(ctx, "/bin/ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
		if err != nil {
			continue
		}
		t, err := parseLstart(string(out))
		if err != nil {
			continue
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest, !earliest.IsZero()
}

// parseLstart parses ps's lstart column in the C locale, e.g.
// "Sat Oct  4 09:12:03 2026", as local time.
func parseLstart(s string) (time.Time, error) {
	return time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(s), " "), time.Local)
}

// Restart quits the app (as if the user chose Quit, so it can save state),
// waits for it to exit, then launches it again. If it doesn't exit in time,
// for example because it is asking the user something, Restart gives up
// without force-quitting.
func (a *DesktopApp) Restart(ctx context.Context) error {
	if len(a.pids(ctx)) > 0 {
		if _, err := a.Run(ctx, "/usr/bin/osascript", "-e", fmt.Sprintf("tell application %q to quit", a.Name)); err != nil {
			return fmt.Errorf("failed to ask %s to quit: %w", a.Name, err)
		}
		deadline := time.Now().Add(a.QuitTimeout)
		for len(a.pids(ctx)) > 0 {
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not quit within %s", a.Name, a.QuitTimeout)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(a.PollEvery):
			}
		}
	}
	if _, err := a.Run(ctx, "/usr/bin/open", "-a", a.Name); err != nil {
		return fmt.Errorf("failed to open %s: %w", a.Name, err)
	}
	return nil
}

// Notify shows a macOS notification. Title and body travel as script
// arguments, so no quoting can break out of the AppleScript.
func Notify(ctx context.Context, run Commander, title, body string) error {
	_, err := run(ctx, "/usr/bin/osascript",
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		title, body)
	return err
}

// CountSessions counts transcripts on this device: projects/<dir>/<id>.jsonl
// under claudeDir (subagent transcripts nested deeper are part of their
// parent session).
func CountSessions(claudeDir string) int {
	projects := filepath.Join(claudeDir, "projects")
	dirs, err := os.ReadDir(projects)
	if err != nil {
		return 0
	}
	n := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(projects, d.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".jsonl") {
				n++
			}
		}
	}
	return n
}
