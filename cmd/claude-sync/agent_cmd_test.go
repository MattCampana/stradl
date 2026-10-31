package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/agent"
	"github.com/tawanorg/claude-sync/internal/sync"
)

func TestPrintAgentStatus(t *testing.T) {
	now := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	p := agent.PathsFor("/Users/example")
	st := &agent.Status{
		LastSyncAt: now.Add(-2 * time.Minute), LastDurationMs: 1300,
		Uploaded: 1, Downloaded: 2, SessionsTotal: 9, PendingDesktopRestart: 3,
		AgentVersion: "1.2.3", LastError: "a: denied",
	}
	var b bytes.Buffer
	printAgentStatus(&b, p, &agent.JobState{Installed: true, Loaded: true, PID: 4242}, st, false, true, now)
	out := b.String()
	for _, want := range []string{"running", "pid 4242", "2 min ago", "took 1.3s", "1 uploaded", "2 downloaded",
		"9 sessions on this Mac", "3 new sessions", "a: denied", "1.2.3", p.Log} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}

	b.Reset()
	printAgentStatus(&b, p, &agent.JobState{}, &agent.Status{}, true, false, now)
	out = b.String()
	for _, want := range []string{"not installed", "paused", "never"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}

	// Without launchd (Linux) there is no service line.
	b.Reset()
	printAgentStatus(&b, p, nil, &agent.Status{}, false, false, now)
	if strings.Contains(b.String(), "Service") {
		t.Errorf("service line shown without launchd:\n%s", b.String())
	}
}

// A failed sync shows as a failed attempt; "Last sync" stays on the last
// one that worked.
func TestPrintAgentStatusFailedAttempt(t *testing.T) {
	now := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	p := agent.PathsFor("/Users/example")
	st := &agent.Status{
		LastSyncAt: now.Add(-3 * time.Hour), LastDurationMs: 900, LastAttemptAt: now.Add(-10 * time.Second),
		LastError:     "failed to list remote objects: dial tcp: lookup acct.blob.core.windows.net: no such host",
		LastErrorKind: agent.ErrorOffline, SessionsTotal: 4,
	}
	var b bytes.Buffer
	printAgentStatus(&b, p, nil, st, false, true, now)
	out := b.String()
	for _, want := range []string{"3 hr ago (took 900ms)", "Last attempt", "just now", "(failed: Offline: will retry)",
		"no such host", "4 sessions on this Mac"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Last sync") && !strings.Contains(line, "3 hr ago") {
			t.Errorf("a failed sync shown as the last sync: %q", line)
		}
	}

	// Never synced: the attempt still shows, the result row doesn't.
	b.Reset()
	st = &agent.Status{LastAttemptAt: now.Add(-2 * time.Minute), LastError: sync.ErrKeyMismatch.Error(), LastErrorKind: agent.ErrorKeyMismatch}
	printAgentStatus(&b, p, nil, st, false, true, now)
	out = b.String()
	for _, want := range []string{"never", "2 min ago", "(failed: Passphrase doesn't match your other Macs)", "stradl init --passphrase"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "uploaded") {
		t.Errorf("result row shown without a sync:\n%s", out)
	}
}

// lockedBuffer is a bytes.Buffer safe to write from one goroutine while the
// test reads it from another.
type lockedBuffer struct {
	mu gosync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestAcquireAgentLockWaitsForTheFirstAgent: a second `agent run` must not
// exit (launchd would then never restart it) but wait and take over.
func TestAcquireAgentLockWaitsForTheFirstAgent(t *testing.T) {
	p := agent.PathsFor(t.TempDir())
	defer func(d time.Duration) { agentLockPoll = d }(agentLockPoll)
	agentLockPoll = 5 * time.Millisecond

	first, err := sync.AcquireLock(p.Lock, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	var stderr lockedBuffer
	got := make(chan error, 1)
	go func() {
		release, err := acquireAgentLock(context.Background(), p, &stderr, false)
		if err == nil {
			release()
		}
		got <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stderr.String(), "waiting to take over") {
		if time.Now().After(deadline) {
			t.Fatalf("second agent never said it was waiting; stderr: %q", stderr.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
	select {
	case err := <-got:
		t.Fatalf("second agent returned (%v) while the first still runs", err)
	case <-time.After(50 * time.Millisecond):
	}
	if msg := stderr.String(); strings.Count(msg, "\n") != 1 || !strings.Contains(msg, fmt.Sprintf("(pid %d)", os.Getpid())) {
		t.Fatalf("want one log line naming the running agent, got %q", msg)
	}

	first()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second agent never took over")
	}
}

func TestAcquireAgentLockStopsWhileWaiting(t *testing.T) {
	p := agent.PathsFor(t.TempDir())
	defer func(d time.Duration) { agentLockPoll = d }(agentLockPoll)
	agentLockPoll = 5 * time.Millisecond

	first, err := sync.AcquireLock(p.Lock, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var stderr bytes.Buffer
	if _, err := acquireAgentLock(ctx, p, &stderr, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if !strings.HasPrefix(stderr.String(), "Another Stradl agent is running") || !strings.Contains(stderr.String(), "Ctrl-C") {
		t.Fatalf("terminal message = %q", stderr.String())
	}

	// Free lock: taken at once, silently.
	free := agent.PathsFor(t.TempDir())
	stderr.Reset()
	release, err := acquireAgentLock(context.Background(), free, &stderr, true)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if stderr.Len() != 0 {
		t.Fatalf("unexpected output %q", stderr.String())
	}
}

func TestLooksTemporary(t *testing.T) {
	tmp, _ := filepath.EvalSymlinks(os.TempDir())
	cases := map[string]bool{
		filepath.Join(tmp, "go-build123", "b001", "exe", "claude-sync"): true,
		"/private/tmp/stradl":                  true,
		"/Users/a/Library/Caches/go-build/x/y": true,
		"/usr/local/bin/stradl":                false,
		"/Users/a/.local/bin/stradl":           false,
	}
	for path, want := range cases {
		if got := looksTemporary(path); got != want {
			t.Errorf("looksTemporary(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestAgentPauseCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := agent.PathsFor(home)

	quiet = true
	defer func() { quiet = false }()
	if err := agentPauseCmd(true).RunE(nil, nil); err != nil {
		t.Fatal(err)
	}
	if !agent.IsPaused(p) {
		t.Fatal("pause did not create the flag")
	}
	if err := agentPauseCmd(false).RunE(nil, nil); err != nil {
		t.Fatal(err)
	}
	if agent.IsPaused(p) {
		t.Fatal("resume did not remove the flag")
	}
}

// The menu bar reads the config through Options.LoadConfig (NewRunner only
// fills its own copy), so the agent's options must carry one: without it
// v2.5.0 and v2.5.1 crashed at launch.
func TestAgentOptionsCarryAConfigLoader(t *testing.T) {
	o := agentOptions(agent.Paths{}, nil)
	if o.LoadConfig == nil {
		t.Fatal("the agent's options have no LoadConfig; the menu bar would crash")
	}
}
