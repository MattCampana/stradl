package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSessions creates ~/.claude/sessions-style records in a temp dir.
func writeSessions(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// alivePIDs fakes the process table.
func alivePIDs(pids ...int) func(int) bool {
	return func(pid int) bool {
		for _, p := range pids {
			if p == pid {
				return true
			}
		}
		return false
	}
}

// dialogCommands answers the restart question like osascript would, and
// everything else through fakeCommands.
type dialogCommands struct {
	fakeCommands
	answer string // stdout of display dialog
	err    error
	asked  []string // prompts shown
}

func (d *dialogCommands) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if filepath.Base(name) == "osascript" && strings.Contains(strings.Join(args, " "), "display dialog") {
		d.calls = append(d.calls, "dialog")
		d.asked = append(d.asked, args[len(args)-1])
		return []byte(d.answer), d.err
	}
	return d.fakeCommands.run(ctx, name, args...)
}

func TestLiveSessions(t *testing.T) {
	dir := writeSessions(t, map[string]string{
		"101.json":     `{"pid":101,"sessionId":"a","status":"busy","entrypoint":"claude-desktop"}`,
		"102.json":     `{"pid":102,"sessionId":"b","status":"busy","entrypoint":"claude-desktop"}`, // exited
		"103.json":     `{"pid":103,"sessionId":"c","status":"idle","entrypoint":"cli"}`,
		"bad.json":     `{nope`,
		"104.json.tmp": `{"pid":104,"sessionId":"d","status":"busy"}`,
		"105.key":      `secret`,
	})
	live := LiveSessions(dir, alivePIDs(101, 103, 104))
	want := []CodeSession{
		{PID: 101, SessionID: "a", Status: "busy", Entrypoint: "claude-desktop"},
		{PID: 103, SessionID: "c", Status: "idle", Entrypoint: "cli"},
	}
	if fmt.Sprint(live) != fmt.Sprint(want) {
		t.Fatalf("live = %+v, want %+v", live, want)
	}
	if got := LiveSessions(filepath.Join(dir, "missing"), nil); got != nil {
		t.Fatalf("missing dir = %+v", got)
	}
}

func TestBusySessions(t *testing.T) {
	dir := writeSessions(t, map[string]string{
		"1.json": `{"pid":1,"sessionId":"desk-busy","status":"busy","entrypoint":"claude-desktop"}`,
		"2.json": `{"pid":2,"sessionId":"desk-idle","status":"idle","entrypoint":"claude-desktop"}`,
		"3.json": `{"pid":3,"sessionId":"terminal","status":"busy","entrypoint":"cli"}`, // survives the app quitting
		"4.json": `{"pid":4,"sessionId":"unknown","status":"busy"}`,                     // unknown: assume the app's
		"5.json": `{"pid":5,"sessionId":"gone","status":"busy","entrypoint":"claude-desktop"}`,
	})
	app := &DesktopApp{SessionsDir: dir, Alive: alivePIDs(1, 2, 3, 4)}
	if n := app.BusySessions(); n != 2 {
		t.Fatalf("BusySessions = %d, want 2", n)
	}
	if n := (&DesktopApp{}).BusySessions(); n != 0 {
		t.Fatalf("no sessions dir: %d", n)
	}
}

func TestConfirmRestart(t *testing.T) {
	busy := map[string]string{
		"1.json": `{"pid":1,"sessionId":"a","status":"busy","entrypoint":"claude-desktop"}`,
		"2.json": `{"pid":2,"sessionId":"b","status":"busy","entrypoint":"claude-desktop"}`,
	}
	newApp := func(sessions map[string]string, alive []int, d *dialogCommands) *DesktopApp {
		return &DesktopApp{Run: d.run, Name: "Claude", QuitTimeout: time.Second, PollEvery: time.Millisecond,
			SessionsDir: writeSessions(t, sessions), Alive: alivePIDs(alive...)}
	}
	ctx := context.Background()

	// Nothing working: no question.
	d := &dialogCommands{}
	ok, err := newApp(map[string]string{"1.json": `{"pid":1,"status":"idle","entrypoint":"claude-desktop"}`}, []int{1}, d).ConfirmRestart(ctx)
	if !ok || err != nil || len(d.asked) != 0 {
		t.Fatalf("idle: ok=%v err=%v asked=%q", ok, err, d.asked)
	}
	// Busy records whose processes are gone don't count.
	d = &dialogCommands{}
	if ok, _ := newApp(busy, nil, d).ConfirmRestart(ctx); !ok || len(d.asked) != 0 {
		t.Fatalf("dead sessions: ok=%v asked=%q", ok, d.asked)
	}

	// Busy: asks, and the answer decides.
	d = &dialogCommands{answer: "button returned:Restart, gave up:false\n"}
	ok, err = newApp(busy, []int{1, 2}, d).ConfirmRestart(ctx)
	if !ok || err != nil {
		t.Fatalf("confirmed: ok=%v err=%v", ok, err)
	}
	want := "2 Claude sessions are still working. Restart Claude anyway? Work in progress may stop."
	if len(d.asked) != 1 || d.asked[0] != want {
		t.Fatalf("prompt = %q, want %q", d.asked, want)
	}

	d = &dialogCommands{err: errors.New("execution error: User canceled. (-128)")}
	ok, err = newApp(busy, []int{1}, d).ConfirmRestart(ctx)
	if ok || err != nil {
		t.Fatalf("cancelled: ok=%v err=%v", ok, err)
	}
	if d.asked[0] != "1 Claude session is still working. Restart Claude anyway? Work in progress may stop." {
		t.Fatalf("prompt = %q", d.asked[0])
	}

	d = &dialogCommands{answer: "button returned:, gave up:true\n"}
	if ok, err := newApp(busy, []int{1}, d).ConfirmRestart(ctx); ok || err != nil {
		t.Fatalf("unanswered dialog: ok=%v err=%v", ok, err)
	}

	d = &dialogCommands{err: errors.New("exit status 1: not allowed to display")}
	if ok, err := newApp(busy, []int{1}, d).ConfirmRestart(ctx); ok || err == nil {
		t.Fatalf("osascript failure: ok=%v err=%v", ok, err)
	}
}

func TestRestartIfConfirmed(t *testing.T) {
	busy := map[string]string{"1.json": `{"pid":1,"sessionId":"a","status":"busy","entrypoint":"claude-desktop"}`}
	ctx := context.Background()

	// Cancel: Claude is left alone, not quit or reopened.
	d := &dialogCommands{fakeCommands: fakeCommands{pgrep: []string{"812"}}, err: errors.New("User canceled. (-128)")}
	app := &DesktopApp{Run: d.run, Name: "Claude", QuitTimeout: time.Second, PollEvery: time.Millisecond,
		SessionsDir: writeSessions(t, busy), Alive: alivePIDs(1)}
	restarted, err := app.RestartIfConfirmed(ctx)
	if restarted || err != nil {
		t.Fatalf("cancel: restarted=%v err=%v", restarted, err)
	}
	if strings.Join(d.calls, "\n") != "dialog" {
		t.Fatalf("cancel ran %q", d.calls)
	}

	// Restart chosen: quit, wait, open.
	d = &dialogCommands{fakeCommands: fakeCommands{pgrep: []string{"812", ""}}, answer: "button returned:Restart, gave up:false"}
	app.Run = d.run
	restarted, err = app.RestartIfConfirmed(ctx)
	if !restarted || err != nil {
		t.Fatalf("confirm: restarted=%v err=%v", restarted, err)
	}
	joined := strings.Join(d.calls, "\n")
	if !strings.HasPrefix(joined, "dialog\n") || !strings.Contains(joined, `tell application "Claude" to quit`) || !strings.HasSuffix(joined, "open -a Claude") {
		t.Fatalf("calls:\n%s", joined)
	}

	// Nothing working: restarts without asking.
	d = &dialogCommands{fakeCommands: fakeCommands{pgrep: []string{""}}}
	app = &DesktopApp{Run: d.run, Name: "Claude", QuitTimeout: time.Second, PollEvery: time.Millisecond,
		SessionsDir: writeSessions(t, busy), Alive: alivePIDs()}
	if restarted, err := app.RestartIfConfirmed(ctx); !restarted || err != nil || len(d.asked) != 0 {
		t.Fatalf("idle: restarted=%v err=%v asked=%q", restarted, err, d.asked)
	}
}
