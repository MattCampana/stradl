package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
)

func TestStatusRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "status.json")
	if _, err := ReadStatus(path); !IsNotExist(err) {
		t.Fatalf("missing file: err = %v", err)
	}
	at := time.Date(2026, 10, 25, 9, 30, 0, 0, time.UTC)
	in := &Status{
		LastSyncAt: at, LastDurationMs: 1234, LastError: "boom",
		Uploaded: 1, Downloaded: 2, Merged: 3, Deferred: 4, Conflicts: 5,
		SessionsTotal: 9, Paused: true, AgentVersion: "1.0.0", Device: "mac-a",
	}
	in.addPending(at, 3)
	if err := WriteStatus(path, in); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("status file mode: %v %v", fi, err)
	}
	out, err := ReadStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(*out) != fmt.Sprint(*in) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", out, in)
	}

	// The documented field names are what other tools read.
	raw, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"lastSyncAt", "lastDurationMs", "lastError", "uploaded", "downloaded", "merged",
		"deferred", "conflicts", "sessionsTotal", "pendingDesktopRestart", "paused", "agentVersion", "device"} {
		if _, ok := m[k]; !ok {
			t.Errorf("status.json lacks %q", k)
		}
	}
	if m["pendingDesktopRestart"].(float64) != 3 {
		t.Errorf("pendingDesktopRestart = %v", m["pendingDesktopRestart"])
	}

	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}

	if err := os.WriteFile(path, []byte("{nope"), 0600); err != nil {
		t.Fatal(err)
	}
	if st, err := ReadStatus(path); err == nil || st == nil {
		t.Fatal("corrupt file: want an error and a zero Status")
	}
}

func TestClearPendingBefore(t *testing.T) {
	at := time.Date(2026, 10, 25, 9, 0, 0, 500e6, time.Local)
	var st Status
	st.addPending(at, 2)
	st.addPending(at.Add(time.Minute), 1)

	// ps reports whole seconds: a launch in the same second as a batch
	// might predate it, so the batch stays.
	if st.clearPendingBefore(at.Truncate(time.Second)) || st.PendingDesktopRestart != 3 {
		t.Fatalf("same-second launch cleared a batch: %+v", st)
	}
	if !st.clearPendingBefore(at.Add(time.Second)) || st.PendingDesktopRestart != 1 {
		t.Fatalf("after first batch: %+v", st)
	}
	if !st.clearPendingBefore(at.Add(2*time.Minute)) || st.PendingDesktopRestart != 0 || st.PendingDesktop != nil {
		t.Fatalf("after all: %+v", st)
	}
}

func TestStatusLine(t *testing.T) {
	now := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		s    Snapshot
		want string
	}{
		{Snapshot{}, "Starting…"},
		{Snapshot{Syncing: true, Status: Status{Paused: true}}, "Syncing…"},
		{Snapshot{Status: Status{Paused: true, LastError: "x"}}, "Paused"},
		{Snapshot{Status: Status{LastError: NotConfigured}}, "Not set up: run stradl init in Terminal"},
		{Snapshot{Status: Status{LastError: "dial tcp: connection refused"}}, "Error: dial tcp: connection refused (see log)"},
		{Snapshot{Status: Status{LastSyncAt: now.Add(-2 * time.Minute), SessionsTotal: 9}}, "Synced 2 min ago · 9 sessions"},
		{Snapshot{Status: Status{LastSyncAt: now.Add(-10 * time.Second), SessionsTotal: 1}}, "Synced just now · 1 session"},
		{Snapshot{Status: Status{LastSyncAt: now.Add(-3 * time.Hour)}}, "Synced 3 hr ago · 0 sessions"},
		{Snapshot{Status: Status{LastSyncAt: now.Add(-49 * time.Hour)}}, "Synced 2 days ago · 0 sessions"},
	}
	for _, c := range cases {
		if got := StatusLine(c.s, now); got != c.want {
			t.Errorf("StatusLine(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
	long := Snapshot{Status: Status{LastError: strings.Repeat("x", 200)}}
	if got := StatusLine(long, now); len([]rune(got)) > 70 {
		t.Errorf("long error not truncated: %q", got)
	}
}

func TestRestartLine(t *testing.T) {
	if got := RestartLine(Snapshot{}); got != "" {
		t.Errorf("nothing pending: %q", got)
	}
	if got := RestartLine(Snapshot{AppRunning: true, Status: Status{PendingDesktopRestart: 1}}); got != "Restart Claude to show 1 new session" {
		t.Errorf("got %q", got)
	}
	if got := RestartLine(Snapshot{Status: Status{PendingDesktopRestart: 4}}); got != "Open Claude to show 4 new sessions" {
		t.Errorf("got %q", got)
	}
}

func TestConfigLoader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	loads := 0
	load := ConfigLoader(path, func() (*config.Config, error) {
		loads++
		return &config.Config{SyncIntervalSeconds: loads * 100}, nil
	})
	if _, err := load(); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("missing config: err = %v", err)
	}
	if err := os.WriteFile(path, []byte("a: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := load()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := load()
	if a != b || loads != 1 {
		t.Fatalf("unchanged file reloaded (loads=%d)", loads)
	}
	later := time.Now().Add(time.Minute)
	if err := os.WriteFile(path, []byte("a: 22\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(path, later, later)
	c, _ := load()
	if c == a || loads != 2 {
		t.Fatalf("edited file not reloaded (loads=%d)", loads)
	}
}

func TestCountSessions(t *testing.T) {
	claude := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(claude, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("projects/-Users-a-app/s1.jsonl")
	write("projects/-Users-a-app/s2.jsonl")
	write("projects/-Users-a-app/s2/subagents/agent-1.jsonl") // part of s2
	write("projects/-Users-a-app/s1.jsonl.conflict.20260101-000000")
	write("projects/-Users-a-web/s3.jsonl")
	write("history.jsonl")
	if n := CountSessions(claude); n != 3 {
		t.Fatalf("CountSessions = %d, want 3", n)
	}
	if n := CountSessions(filepath.Join(claude, "missing")); n != 0 {
		t.Fatalf("missing dir = %d", n)
	}
}

func TestLogFileRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	lf, err := OpenLogFile(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lf.Close() }()
	log := NewLogger(lf)
	for i := 0; i < 10; i++ {
		log.Printf("line %d with some padding", i)
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	if len(cur) > 100 || len(old) > 100 {
		t.Fatalf("files exceed the limit: %d, %d", len(cur), len(old))
	}
	if !strings.Contains(string(cur), "line 9") {
		t.Fatalf("latest line missing from current log:\n%s", cur)
	}
	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Fatal("more than one old log kept")
	}
}

// fakeCommands answers DesktopApp's commands from a script.
type fakeCommands struct {
	calls []string
	pgrep []string // successive pgrep outputs; the last repeats
	ps    map[string]string
}

func (f *fakeCommands) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, filepath.Base(name)+" "+strings.Join(args, " "))
	switch filepath.Base(name) {
	case "pgrep":
		out := f.pgrep[0]
		if len(f.pgrep) > 1 {
			f.pgrep = f.pgrep[1:]
		}
		if out == "" {
			return nil, errors.New("exit status 1")
		}
		return []byte(out), nil
	case "ps":
		pid := args[len(args)-1]
		if out, ok := f.ps[pid]; ok {
			return []byte(out), nil
		}
		return nil, errors.New("no such process")
	}
	return nil, nil
}

func TestDesktopAppStartTime(t *testing.T) {
	f := &fakeCommands{
		pgrep: []string{"812\n977\n"},
		ps:    map[string]string{"812": "Sat Oct 25 09:12:03 2026\n", "977": "Sat Oct  4 08:00:00 2026\n"},
	}
	app := &DesktopApp{Run: f.run, Name: "Claude"}
	got, ok := app.StartTime(context.Background())
	want := time.Date(2026, 10, 4, 8, 0, 0, 0, time.Local)
	if !ok || !got.Equal(want) {
		t.Fatalf("StartTime = %v %v, want %v", got, ok, want)
	}
	if f.calls[0] != "pgrep -x Claude" {
		t.Fatalf("calls = %q", f.calls)
	}

	none := &DesktopApp{Run: (&fakeCommands{pgrep: []string{""}}).run, Name: "Claude"}
	if _, ok := none.StartTime(context.Background()); ok {
		t.Fatal("not running reported as running")
	}
}

func TestDesktopAppRestart(t *testing.T) {
	f := &fakeCommands{pgrep: []string{"812", "812", "812", ""}}
	app := &DesktopApp{Run: f.run, Name: "Claude", QuitTimeout: time.Second, PollEvery: time.Millisecond}
	if err := app.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, `osascript -e tell application "Claude" to quit`) || !strings.HasSuffix(joined, "open -a Claude") {
		t.Fatalf("calls:\n%s", joined)
	}

	// An app that won't quit is left alone, not reopened or killed.
	stuck := &fakeCommands{pgrep: []string{"812"}}
	app = &DesktopApp{Run: stuck.run, Name: "Claude", QuitTimeout: 20 * time.Millisecond, PollEvery: time.Millisecond}
	if err := app.Restart(context.Background()); err == nil {
		t.Fatal("expected a timeout")
	}
	if strings.Contains(strings.Join(stuck.calls, "\n"), "open -a") {
		t.Fatal("reopened an app that never quit")
	}

	// Not running: just open it.
	off := &fakeCommands{pgrep: []string{""}}
	app = &DesktopApp{Run: off.run, Name: "Claude", QuitTimeout: time.Second, PollEvery: time.Millisecond}
	if err := app.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(off.calls, "\n") != "pgrep -x Claude\nopen -a Claude" {
		t.Fatalf("calls: %q", off.calls)
	}
}

func TestNotifyPassesTextAsArguments(t *testing.T) {
	f := &fakeCommands{}
	body := `3 sessions "arrived" \ end tell`
	if err := Notify(context.Background(), f.run, "Stradl", body); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(f.calls[0], " end run Stradl "+body) {
		t.Fatalf("call = %q", f.calls[0])
	}
}
