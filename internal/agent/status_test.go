package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/storage"
	"github.com/tawanorg/claude-sync/internal/sync"
)

func TestStatusRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "status.json")
	if _, err := ReadStatus(path); !IsNotExist(err) {
		t.Fatalf("missing file: err = %v", err)
	}
	at := time.Date(2026, 10, 25, 9, 30, 0, 0, time.UTC)
	in := &Status{
		LastSyncAt: at, LastDurationMs: 1234, LastAttemptAt: at.Add(time.Minute), LastError: "boom", LastErrorKind: ErrorOffline,
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
	for _, k := range []string{"lastSyncAt", "lastDurationMs", "lastAttemptAt", "lastError", "lastErrorKind", "uploaded", "downloaded", "merged",
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
		{Snapshot{Syncing: true, SyncPhase: "compare", SyncDone: 50, SyncTotal: 3200}, "Checking files… 50 of 3200"},
		{Snapshot{Syncing: true, SyncPhase: "transfer", SyncDone: 3, SyncTotal: 9}, "Syncing… 3 of 9 files"},
		{Snapshot{Status: Status{Paused: true, LastError: "x"}}, "Paused"},
		{Snapshot{Status: Status{LastError: NotConfigured}}, "Not set up: run stradl init in Terminal"},
		{Snapshot{Status: Status{LastError: "a: denied", LastErrorKind: ErrorOther}}, "Sync error (see log)"},
		{Snapshot{Status: Status{LastError: "x", LastErrorKind: ErrorKeyMismatch}}, "Passphrase doesn't match your other Macs"},
		{Snapshot{Status: Status{LastError: "x", LastErrorKind: ErrorAuth}}, "Storage token expired or invalid"},
		{Snapshot{Status: Status{LastError: "x", LastErrorKind: ErrorNoContainer}}, "Storage container not found"},
		{Snapshot{Status: Status{LastError: "x", LastErrorKind: ErrorOffline}}, "Offline: will retry"},
		// A status file from an older agent has no kind: sorted by message.
		{Snapshot{Status: Status{LastError: "dial tcp: connection refused"}}, "Offline: will retry"},
		// A failure after a good sync shows the failure, not "Synced just now".
		{Snapshot{Status: Status{LastSyncAt: now.Add(-time.Hour), LastAttemptAt: now, LastError: "x", LastErrorKind: ErrorOffline}}, "Offline: will retry"},
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

func TestAttemptLine(t *testing.T) {
	now := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		st   Status
		want string
	}{
		{Status{}, ""},
		{Status{LastSyncAt: now, LastAttemptAt: now}, ""},
		{Status{LastError: NotConfigured}, ""}, // no attempt without a config
		{Status{LastAttemptAt: now, LastError: NotConfigured}, ""},
		{Status{LastSyncAt: now.Add(-2 * time.Hour), LastAttemptAt: now.Add(-30 * time.Second), LastError: "x"},
			"Last attempt: just now (failed) · last sync: 2 hr ago"},
		{Status{LastAttemptAt: now.Add(-5 * time.Minute), LastError: "x"},
			"Last attempt: 5 min ago (failed) · last sync: never"},
	}
	for _, c := range cases {
		if got := AttemptLine(Snapshot{Status: c.st}, now); got != c.want {
			t.Errorf("AttemptLine(%+v) = %q, want %q", c.st, got, c.want)
		}
	}
}

// netErr is a net.Error that isn't a timeout.
type netErr struct{}

func (netErr) Error() string   { return "read: broken pipe" }
func (netErr) Timeout() bool   { return false }
func (netErr) Temporary() bool { return false }

func TestClassifyError(t *testing.T) {
	azureAuth := errors.New("Azure rejected the credentials; the SAS token may have expired (extend its stored access policy in the portal, or rerun init with a new token): GET https://acct.blob.core.windows.net/c\nRESPONSE 403\nERROR CODE: AuthenticationFailed")
	cases := []struct {
		name string
		err  error
		want ErrorKind
	}{
		{"nil", nil, ""},
		{"wrong passphrase", sync.ErrKeyMismatch, ErrorKeyMismatch},
		{"wrapped wrong passphrase", fmt.Errorf("sync: %w", sync.ErrKeyMismatch), ErrorKeyMismatch},
		{"expired SAS", azureAuth, ErrorAuth},
		{"expired SAS through a transport error", &url.Error{Op: "Get", URL: "https://x", Err: azureAuth}, ErrorAuth},
		{"auth code only", errors.New("RESPONSE 403: AuthenticationFailed"), ErrorAuth},
		{"S3 expired token", errors.New("operation error S3: ListObjectsV2, api error ExpiredToken: token expired"), ErrorAuth},
		{"container missing on list", fmt.Errorf("failed to list remote objects: %w", fmt.Errorf("%w: ContainerNotFound", storage.ErrNotFound)), ErrorNoContainer},
		{"not found on list", fmt.Errorf("failed to list remote objects: %w", storage.ErrNotFound), ErrorNoContainer},
		{"container code", errors.New("ERROR CODE: ContainerNotFound"), ErrorNoContainer},
		{"S3 bucket missing", errors.New("api error NoSuchBucket: The specified bucket does not exist"), ErrorNoContainer},
		{"DNS", &net.DNSError{Err: "no such host", Name: "acct.blob.core.windows.net"}, ErrorOffline},
		{"url error", &url.Error{Op: "Get", URL: "https://x", Err: errors.New("EOF")}, ErrorOffline},
		{"net.Error", fmt.Errorf("list: %w", netErr{}), ErrorOffline},
		{"refused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), ErrorOffline},
		{"iCloud-only file", fmt.Errorf("x.md: %w", &fs.PathError{Op: "read", Path: "/Users/m/Desktop/x.md", Err: syscall.EDEADLK}), ErrorICloud},
		{"iCloud-only by text", errors.New("1008 files failed to sync; first: read /Users/m/Desktop/x.md: resource deadlock avoided"), ErrorICloud},
		{"macOS refused", &fs.PathError{Op: "open", Path: "/Users/m/Desktop", Err: syscall.EPERM}, ErrorPermission},
		{"disk full is not offline", &fs.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC}, ErrorOther},
		{"unreachable", fmt.Errorf("dial: %w", syscall.ENETUNREACH), ErrorOffline},
		{"timeout", context.DeadlineExceeded, ErrorOffline},
		{"refused by text", errors.New("dial tcp 127.0.0.1:10000: connect: connection refused"), ErrorOffline},
		{"no such host by text", errors.New("dial tcp: lookup acct.blob.core.windows.net: no such host"), ErrorOffline},
		{"i/o timeout by text", errors.New("read tcp: i/o timeout"), ErrorOffline},
		{"other", errors.New("failed to decrypt: bad header"), ErrorOther},
		{"permission", errors.New("the SAS token lacks a required permission"), ErrorOther},
	}
	for _, c := range cases {
		if got := ClassifyError(c.err); got != c.want {
			t.Errorf("%s: ClassifyError(%v) = %q, want %q", c.name, c.err, got, c.want)
		}
	}

	// Per-file errors: a missing object is that file's problem, and the most
	// telling error wins.
	files := []struct {
		errs []error
		want ErrorKind
	}{
		{[]error{fmt.Errorf("a: %w", storage.ErrNotFound)}, ErrorOther},
		{[]error{errors.New("a: denied"), fmt.Errorf("b: %w", syscall.ECONNRESET)}, ErrorOffline},
		{[]error{fmt.Errorf("b: %w", syscall.ECONNRESET), azureAuth, errors.New("c")}, ErrorAuth},
		{[]error{errors.New("MCP servers: boom")}, ErrorOther},
	}
	for _, c := range files {
		if got := classifyFileErrors(c.errs); got != c.want {
			t.Errorf("classifyFileErrors(%v) = %q, want %q", c.errs, got, c.want)
		}
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
	if got := RestartLine(Snapshot{AppRunning: true, Status: Status{SidebarPending: true}}); got != "Restart Claude to apply sidebar changes" {
		t.Errorf("sidebar only: %q", got)
	}
	if got := RestartLine(Snapshot{AppRunning: true, Status: Status{PendingDesktopRestart: 2, SidebarPending: true}}); got != "Restart Claude to show 2 new sessions and sidebar changes" {
		t.Errorf("both: %q", got)
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
	if f.calls[0] != "pgrep -a -x Claude" {
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

	// BeforeLaunch runs after the app is gone and before it reopens.
	order := &fakeCommands{pgrep: []string{"812", ""}}
	app = &DesktopApp{Run: order.run, Name: "Claude", QuitTimeout: time.Second, PollEvery: time.Millisecond}
	app.BeforeLaunch = func(context.Context) { order.calls = append(order.calls, "before-launch") }
	if err := app.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(order.calls, "\n")
	if !strings.Contains(got, "pgrep -a -x Claude\nbefore-launch\nopen -a Claude") {
		t.Fatalf("BeforeLaunch out of order:\n%s", got)
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
	if strings.Join(off.calls, "\n") != "pgrep -a -x Claude\nopen -a Claude" {
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

func TestDesktopAppRestartReopensEachProfile(t *testing.T) {
	f := &fakeCommands{pgrep: []string{""}}
	quit := map[int]bool{}
	app := &DesktopApp{Run: f.run, Name: "Claude", QuitTimeout: time.Second, PollEvery: time.Millisecond,
		MainDir:   "/main",
		Instances: func() map[int]string { return map[int]string{10: "/main", 20: "/profiles/judge"} },
		Alive:     func(pid int) bool { return !quit[pid] },
	}
	run := f.run
	app.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		for _, pid := range []int{10, 20} {
			if strings.Contains(joined, fmt.Sprintf("(%d).terminate", pid)) {
				quit[pid] = true
				return []byte("true\n"), nil
			}
		}
		return run(ctx, name, args...)
	}
	app.BeforeLaunch = func(context.Context) { f.calls = append(f.calls, "before-launch") }
	if err := app.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls, "\n")
	want := "before-launch\nopen -n -a Claude\nopen -n -a Claude --args --user-data-dir=/profiles/judge"
	if !quit[10] || !quit[20] || got != want {
		t.Fatalf("quit=%v calls:\n%s\nwant:\n%s", quit, got, want)
	}
}

func TestLaunchersAreStradlsOwn(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "Claude (Mine).app", "Contents")
	if err := os.MkdirAll(other, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "Info.plist"), []byte("<plist/>"), 0644); err != nil {
		t.Fatal(err)
	}
	ls := []Launcher{{Name: "Claude (Mine)", ID: "x"}, {Name: "Claude (it's/odd)", ID: "y", Args: []string{"open", "it's"}}}
	made, err := WriteLaunchers(dir, "/bin/stradl x", "", ls)
	if err != nil {
		t.Fatal(err)
	}
	if len(made) != 1 || filepath.Base(made[0]) != "Claude (it's-odd).app" {
		t.Fatalf("made = %v (an app Stradl didn't make must be left alone)", made)
	}
	script, _ := os.ReadFile(filepath.Join(made[0], "Contents", "MacOS", "launcher"))
	if !strings.Contains(string(script), `exec '/bin/stradl x' 'open' 'it'\''s'`) {
		t.Fatalf("script:\n%s", script)
	}
	if fi, err := os.Stat(filepath.Join(made[0], "Contents", "MacOS", "launcher")); err != nil || fi.Mode()&0111 == 0 {
		t.Fatal("the launcher script must be executable")
	}
	// A launcher no longer wanted goes; the other app stays.
	if _, err := WriteLaunchers(dir, "/bin/stradl", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(made[0]); !os.IsNotExist(err) {
		t.Fatal("an old launcher stayed")
	}
	if n, err := RemoveLaunchers(dir); err != nil || n != 0 {
		t.Fatalf("RemoveLaunchers = %d %v", n, err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("removed an app Stradl didn't make")
	}
}
