package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/agent"
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
