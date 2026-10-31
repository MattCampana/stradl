package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/profiles"
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
	// SessionsDir holds Claude Code's per-process session records
	// (~/.claude/sessions), which ConfirmRestart reads to find work in
	// progress. Empty skips the check.
	SessionsDir string
	// Alive reports whether a process is running; nil means signal 0.
	Alive func(pid int) bool
	// BeforeLaunch runs after the app has quit and before Restart opens it
	// again: the one moment its settings file can change without the app
	// overwriting it. Stradl applies pending sidebar layout changes here.
	BeforeLaunch func(ctx context.Context)
	// Instances lists the app's running instances, process ID to data
	// folder (see package profiles); MainDir is the app's own folder. With
	// it, Restart reopens each instance on its own profile. Nil treats the
	// app as one instance.
	Instances func() map[int]string
	MainDir   string
}

// NewDesktopApp returns a DesktopApp for the real Claude app.
func NewDesktopApp() *DesktopApp {
	main := (&config.Config{}).ClaudeAppDir()
	return &DesktopApp{
		Run: ExecCommand, Name: "Claude", QuitTimeout: 20 * time.Second, PollEvery: 500 * time.Millisecond,
		SessionsDir: filepath.Join(config.ClaudeDir(), "sessions"),
		Instances:   func() map[int]string { return profiles.Instances(main) },
		MainDir:     main,
	}
}

// pids lists the app's main processes (exact name match, so helper
// processes and the lowercase `claude` CLI don't count).
func (a *DesktopApp) pids(ctx context.Context) []int {
	out, err := a.Run(ctx, "/usr/bin/pgrep", "-a", "-x", a.Name)
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
	if a.Instances != nil {
		if inst := a.Instances(); len(inst) > 1 || (len(inst) == 1 && !a.onlyMain(inst)) {
			return a.restartProfiles(ctx, inst)
		}
	}
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
	// Claude has quit: reopen it even if ctx ran out meanwhile, so a restart
	// never leaves it closed.
	launch, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if a.BeforeLaunch != nil {
		a.BeforeLaunch(launch)
	}
	if _, err := a.Run(launch, "/usr/bin/open", "-a", a.Name); err != nil {
		return fmt.Errorf("failed to open %s: %w", a.Name, err)
	}
	return nil
}

func (a *DesktopApp) onlyMain(inst map[int]string) bool {
	for _, dir := range inst {
		if filepath.Clean(dir) != filepath.Clean(a.MainDir) {
			return false
		}
	}
	return true
}

// restartProfiles restarts several instances (or one on a profile of its
// own): each is asked to quit, and once all have, each opens again on its
// own folder. One that doesn't quit in time is left running and the rest
// reopen.
func (a *DesktopApp) restartProfiles(ctx context.Context, inst map[int]string) error {
	pids := make([]int, 0, len(inst))
	for pid := range inst {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	var failed error
	var reopen []profiles.Profile
	for _, pid := range pids {
		dir := filepath.Clean(inst[pid])
		if err := profiles.Quit(ctx, profiles.Runner(a.Run), pid, a.QuitTimeout, a.Alive); err != nil {
			failed = err
			continue
		}
		main := dir == filepath.Clean(a.MainDir)
		reopen = append(reopen, profiles.Profile{Dir: dir, Main: main})
	}
	launch, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if a.BeforeLaunch != nil && len(reopen) > 0 {
		a.BeforeLaunch(launch)
	}
	for _, p := range reopen {
		if _, err := a.Run(launch, "/usr/bin/open", profiles.LaunchArgs(p)...); err != nil {
			failed = fmt.Errorf("failed to open %s: %w", a.Name, err)
		}
	}
	return failed
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
