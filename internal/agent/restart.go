package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// CodeSession is the record Claude Code keeps for each running process in
// ~/.claude/sessions/<pid>.json.
type CodeSession struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	Status     string `json:"status"`     // "busy" while a turn is running
	Entrypoint string `json:"entrypoint"` // "claude-desktop" for the app's Code tab, "cli" in a terminal
}

// standaloneEntrypoints start Claude Code outside the desktop app, in a
// terminal or an editor. Those sessions keep running when the app quits, so
// they never hold up a restart. Unknown entry points count as the app's: a
// needless question beats losing work.
var standaloneEntrypoints = map[string]bool{"cli": true, "claude-vscode": true}

// LiveSessions returns the session records in dir whose process is still
// running. alive nil means signal 0. Unreadable records are skipped.
func LiveSessions(dir string, alive func(pid int) bool) []CodeSession {
	if alive == nil {
		alive = processAlive
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var live []CodeSession
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var s CodeSession
		if json.Unmarshal(data, &s) != nil || s.PID <= 0 || !alive(s.PID) {
			continue
		}
		live = append(live, s)
	}
	return live
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// BusySessions counts the running sessions that quitting the app would stop
// in the middle of a turn.
func (a *DesktopApp) BusySessions() int {
	if a.SessionsDir == "" {
		return 0
	}
	n := 0
	for _, s := range LiveSessions(a.SessionsDir, a.Alive) {
		if s.Status == "busy" && !standaloneEntrypoints[s.Entrypoint] {
			n++
		}
	}
	return n
}

// ConfirmRestart reports whether the app may restart now. When sessions are
// in the middle of a turn it asks first, with Cancel as the default button;
// a dialog nobody answers gives up after two minutes, which counts as
// Cancel.
func (a *DesktopApp) ConfirmRestart(ctx context.Context) (bool, error) {
	n := a.BusySessions()
	if n == 0 {
		return true, nil
	}
	// The prompt travels as a script argument, so no quoting can break out
	// of the AppleScript.
	out, err := a.Run(ctx, "/usr/bin/osascript",
		"-e", "on run argv",
		"-e", "activate",
		"-e", `display dialog (item 1 of argv) with title "Stradl" buttons {"Cancel", "Restart"} default button "Cancel" cancel button "Cancel" with icon caution giving up after 120`,
		"-e", "end run",
		restartPrompt(n))
	switch {
	case ctx.Err() != nil:
		return false, ctx.Err()
	case err != nil && userCancelled(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("failed to ask before restarting %s: %w", a.Name, err)
	}
	return strings.Contains(string(out), "button returned:Restart"), nil
}

// RestartIfConfirmed restarts the app unless ConfirmRestart says no. It
// reports whether the app was restarted.
func (a *DesktopApp) RestartIfConfirmed(ctx context.Context) (bool, error) {
	ok, err := a.ConfirmRestart(ctx)
	if err != nil || !ok {
		return false, err
	}
	if err := a.Restart(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func restartPrompt(busy int) string {
	what := fmt.Sprintf("%d Claude sessions are", busy)
	if busy == 1 {
		what = "1 Claude session is"
	}
	return what + " still working. Restart Claude anyway? Work in progress may stop."
}

// userCancelled reports whether osascript failed because the user chose the
// dialog's cancel button (AppleScript error -128).
func userCancelled(err error) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "(-128)") {
		return true
	}
	return strings.Contains(err.Error(), "(-128)")
}
