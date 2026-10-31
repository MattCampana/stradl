package profiles

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Runner runs a program and returns its standard output (agent.ExecCommand,
// or a fake in tests).
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

const appName = "Claude"

// DataDir returns the data folder of a Claude process started with args:
// its --user-data-dir, else mainDir.
func DataDir(args []string, mainDir string) string {
	for i, a := range args {
		switch {
		case strings.HasPrefix(a, "--user-data-dir="):
			return filepath.Clean(strings.TrimPrefix(a, "--user-data-dir="))
		case a == "--user-data-dir" && i+1 < len(args):
			return filepath.Clean(args[i+1])
		}
	}
	return filepath.Clean(mainDir)
}

// LaunchArgs returns the open(1) arguments that start a new instance of the
// app on p's folder.
func LaunchArgs(p Profile) []string {
	args := []string{"-n", "-a", appName}
	if !p.Main {
		args = append(args, "--args", "--user-data-dir="+p.Dir)
	}
	return args
}

// Open brings p's Claude window forward, starting the app on p's folder when
// it isn't running. Its records link is checked first.
func (e Env) Open(ctx context.Context, run Runner, p Profile) error {
	if p.Running && p.PID > 0 {
		// Bring that instance forward; open -a would pick any of them.
		if _, err := run(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-e",
			fmt.Sprintf("ObjC.import('AppKit'); $.NSRunningApplication.runningApplicationWithProcessIdentifier(%d).activateWithOptions(3)", p.PID)); err == nil {
			return nil
		}
	}
	if !p.Main {
		if err := e.Link(p.Dir); err != nil {
			return fmt.Errorf("profile %s: %w", p.ID, err)
		}
	}
	if _, err := run(ctx, "/usr/bin/open", LaunchArgs(p)...); err != nil {
		return fmt.Errorf("failed to open Claude: %w", err)
	}
	return nil
}

// Quit asks the instance with pid to quit, as if the user chose Quit, and
// waits up to timeout for it to exit. alive reports whether a process runs
// (nil: signal 0). If the app doesn't answer the request, it gets SIGTERM,
// which it also handles as a normal quit; it is never killed.
func Quit(ctx context.Context, run Runner, pid int, timeout time.Duration, alive func(int) bool) error {
	if alive == nil {
		alive = func(pid int) bool { return syscall.Kill(pid, 0) == nil }
	}
	out, err := run(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-e",
		"ObjC.import('AppKit'); $.NSRunningApplication.runningApplicationWithProcessIdentifier("+strconv.Itoa(pid)+").terminate")
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(timeout)
	for alive(pid) {
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not quit within %s", appName, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil
}
