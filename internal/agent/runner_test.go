package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/sync"
)

// fakeClock drives the Runner's single timer by hand.
type fakeClock struct {
	mu      gosync.Mutex
	now     time.Time
	gen     int
	current *fakeTimer // the most recent timer, while armed
}

type fakeTimer struct {
	fc *fakeClock
	at time.Time
	c  chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{fc: f, at: f.now.Add(d), c: make(chan time.Time, 1)}
	f.gen++
	if d <= 0 {
		t.c <- f.now
		f.current = nil
	} else {
		f.current = t
	}
	return t
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() bool {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	if t.fc.current == t {
		t.fc.current = nil
		return true
	}
	return false
}

// waitArmed blocks until the runner has handled everything up to now and is
// waiting on a timer created after generation after.
func (f *fakeClock) waitArmed(t *testing.T, after int) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		if f.gen > after && f.current != nil {
			g := f.gen
			f.mu.Unlock()
			return g
		}
		f.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("runner never armed its next timer")
		}
		time.Sleep(time.Millisecond)
	}
}

// next reports when the armed timer fires.
func (f *fakeClock) next() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current.at
}

// fire moves the clock to at (if later) and fires the armed timer.
func (f *fakeClock) fire(at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if at.After(f.now) {
		f.now = at
	}
	t := f.current
	f.current = nil
	t.c <- f.now
}

type syncResult struct {
	report *sync.SyncReport
	err    error
}

type harness struct {
	t     *testing.T
	clock *fakeClock
	r     *Runner
	paths Paths
	t0    time.Time
	gen   int
	log   *bytes.Buffer

	mu       gosync.Mutex
	cfg      *config.Config
	cfgErr   error
	syncs    []time.Time
	results  []syncResult
	local    string
	remote   string
	appStart time.Time
	appUp    bool
	notes    []string
}

func newHarness(t *testing.T, tweak func(h *harness, o *Options)) *harness {
	t.Helper()
	h := &harness{
		t:      t,
		clock:  newFakeClock(),
		paths:  PathsFor(t.TempDir()),
		log:    &bytes.Buffer{},
		cfg:    &config.Config{SyncIntervalSeconds: 300},
		local:  "L0",
		remote: "R0",
	}
	h.t0 = h.clock.Now()
	var logMu gosync.Mutex
	logger := NewLogger(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return h.log.Write(p)
	}))
	logger.now = h.clock.Now
	o := Options{
		Paths:   h.paths,
		Version: "1.2.3",
		Device:  "mac-a",
		Clock:   h.clock,
		Log:     logger,
		LoadConfig: func() (*config.Config, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.cfg, h.cfgErr
		},
		Sync: func(ctx context.Context, cfg *config.Config) (*sync.SyncReport, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.syncs = append(h.syncs, h.clock.Now())
			if len(h.results) == 0 {
				return &sync.SyncReport{}, nil
			}
			res := h.results[0]
			h.results = h.results[1:]
			return res.report, res.err
		},
		LocalProbe: func(*config.Config) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.local, nil
		},
		RemoteProbe: func(context.Context, *config.Config) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.remote, nil
		},
		CountSessions: func(*config.Config) int { return 9 },
		AppStart: func(context.Context) (time.Time, bool) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.appStart, h.appUp
		},
		Notify: func(title, body string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.notes = append(h.notes, body)
		},
	}
	if tweak != nil {
		tweak(h, &o)
	}
	h.r = NewRunner(o)
	return h
}

type writerFunc func(p []byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

// start runs the loop and waits for the startup sync to finish.
func (h *harness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = h.r.Run(ctx)
		close(done)
	}()
	h.t.Cleanup(func() {
		cancel()
		<-done
	})
	h.gen = h.clock.waitArmed(h.t, 0)
}

// advance runs the loop until the clock reaches t0+d, firing every timer
// that falls due on the way exactly on time (no lateness, so no "wake").
func (h *harness) advance(d time.Duration) {
	target := h.t0.Add(d)
	for {
		next := h.clock.next()
		if next.After(target) {
			h.clock.mu.Lock()
			if target.After(h.clock.now) {
				h.clock.now = target
			}
			h.clock.mu.Unlock()
			return
		}
		h.clock.fire(next)
		h.gen = h.clock.waitArmed(h.t, h.gen)
	}
}

// jump moves the clock forward by d in one go and fires the pending timer
// late, the way it looks after the Mac sleeps.
func (h *harness) jump(d time.Duration) {
	h.clock.fire(h.clock.Now().Add(d))
	h.gen = h.clock.waitArmed(h.t, h.gen)
}

// settle waits for the loop to handle a channel event (SyncNow, a nudge).
func (h *harness) settle() {
	h.gen = h.clock.waitArmed(h.t, h.gen)
}

func (h *harness) set(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn()
}

// syncOffsets returns when each sync ran, relative to t0.
func (h *harness) syncOffsets() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]time.Duration, len(h.syncs))
	for i, s := range h.syncs {
		out[i] = s.Sub(h.t0)
	}
	return out
}

func (h *harness) expectSyncs(want ...time.Duration) {
	h.t.Helper()
	got := h.syncOffsets()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		h.t.Fatalf("syncs at %v, want %v\nlog:\n%s", got, want, h.log.String())
	}
}

func (h *harness) status() *Status {
	h.t.Helper()
	st, err := ReadStatus(h.paths.Status)
	if err != nil {
		h.t.Fatalf("read status: %v", err)
	}
	return st
}

const s = time.Second

func TestRunnerSyncsOnStartAndWritesStatus(t *testing.T) {
	h := newHarness(t, func(h *harness, _ *Options) {
		h.results = []syncResult{{report: &sync.SyncReport{
			Uploaded:   []string{"a", "b"},
			Restored:   []string{"c"},
			Downloaded: []string{"d"},
			Merged:     []string{"e"},
			Deferred:   []string{"f"},
			Conflicts:  []string{"g"},
		}}}
	})
	h.start()
	h.expectSyncs(0)

	st := h.status()
	if !st.LastSyncAt.Equal(h.t0) || st.LastError != "" {
		t.Fatalf("status = %+v", st)
	}
	if st.Uploaded != 3 || st.Downloaded != 1 || st.Merged != 1 || st.Deferred != 1 || st.Conflicts != 1 {
		t.Fatalf("counts = %+v", st)
	}
	if st.SessionsTotal != 9 || st.AgentVersion != "1.2.3" || st.Device != "mac-a" || st.Paused {
		t.Fatalf("status = %+v", st)
	}
	if !strings.Contains(h.log.String(), "sync ok (start)") {
		t.Fatalf("log missing sync line:\n%s", h.log.String())
	}
}

func TestRunnerDebouncesLocalChanges(t *testing.T) {
	h := newHarness(t, nil)
	h.start()

	h.advance(5 * s)
	h.set(func() { h.local = "L1" }) // a session starts writing
	h.advance(20 * s)                // poll sees it; waits for quiet
	h.expectSyncs(0)

	h.advance(25 * s)
	h.set(func() { h.local = "L2" }) // still writing
	h.advance(30 * s)                // quiet check fails: changed again
	h.expectSyncs(0)

	h.advance(39 * s)
	h.expectSyncs(0)
	h.advance(40 * s) // ten quiet seconds
	h.expectSyncs(0, 40*s)

	// The new state is the baseline: nothing more happens.
	h.advance(100 * s)
	h.expectSyncs(0, 40*s)
}

func TestRunnerDebounceIsCapped(t *testing.T) {
	h := newHarness(t, nil)
	h.start()

	// A session writes every few seconds without a quiet gap.
	for i := 1; i <= 20; i++ {
		h.advance(time.Duration(i*8) * s)
		h.set(func() { h.local = fmt.Sprintf("L%d", i) })
	}
	got := h.syncOffsets()
	if len(got) < 2 {
		t.Fatalf("a continuous stream of writes never synced: %v", got)
	}
	// First change seen at the 20s poll; the cap is two minutes after that.
	if got[1] > 20*s+DefaultMaxDebounce+DefaultQuiet {
		t.Fatalf("debounce overran its cap: second sync at %v", got[1])
	}
}

func TestRunnerSyncsOnRemoteChange(t *testing.T) {
	h := newHarness(t, nil)
	h.start()

	h.advance(30 * s)
	h.set(func() { h.remote = "R1" })
	h.advance(59 * s)
	h.expectSyncs(0)
	h.advance(60 * s)
	h.expectSyncs(0, 60*s)
	h.advance(200 * s)
	h.expectSyncs(0, 60*s)
}

func TestRunnerFullSyncInterval(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.advance(299 * s)
	h.expectSyncs(0)
	h.advance(300 * s)
	h.expectSyncs(0, 300*s)
	h.advance(600 * s)
	h.expectSyncs(0, 300*s, 600*s)
}

func TestRunnerSyncsOnWake(t *testing.T) {
	h := newHarness(t, func(h *harness, _ *Options) {
		h.cfg.SyncIntervalSeconds = 3600 // rule out the interval
	})
	h.start()
	h.advance(20 * s)
	h.jump(2 * time.Minute) // the 40s timer fires two minutes late
	h.expectSyncs(0, 140*s)
	if !strings.Contains(h.log.String(), "(wake)") {
		t.Fatalf("expected a wake sync:\n%s", h.log.String())
	}

	// A tick that is late but within 2x is not a wake.
	h.jump(DefaultLocalPoll + 10*s)
	h.expectSyncs(0, 140*s)
}

func TestRunnerSyncNow(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.advance(7 * s)
	h.r.SyncNow()
	h.settle()
	h.expectSyncs(0, 7*s)
}

func TestRunnerPauseAndResume(t *testing.T) {
	h := newHarness(t, nil)
	h.start()

	// Another process (the CLI) pauses: noticed at the next poll.
	if err := SetPaused(h.paths, true); err != nil {
		t.Fatal(err)
	}
	h.advance(20 * s)
	if !h.status().Paused || !h.r.Snapshot().Paused {
		t.Fatal("pause flag not reflected in status")
	}

	// Nothing syncs while paused: not local or remote changes, not the
	// interval.
	h.set(func() { h.local, h.remote = "L1", "R1" })
	h.advance(700 * s)
	h.expectSyncs(0)

	// An explicit Sync Now still runs.
	h.r.SyncNow()
	h.settle()
	h.expectSyncs(0, 700*s)

	h.advance(710 * s)
	if err := h.r.Resume(); err != nil {
		t.Fatal(err)
	}
	h.settle()
	h.expectSyncs(0, 700*s, 710*s)
	if h.status().Paused {
		t.Fatal("status still paused after resume")
	}

	// Pausing through the runner takes effect at once.
	if err := h.r.Pause(); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if !IsPaused(h.paths) || !h.status().Paused {
		t.Fatal("Pause did not set the flag")
	}
}

func TestRunnerStartsPaused(t *testing.T) {
	h := newHarness(t, nil)
	if err := SetPaused(h.paths, true); err != nil {
		t.Fatal(err)
	}
	h.start()
	h.advance(400 * s)
	h.expectSyncs()
	if !h.status().Paused {
		t.Fatal("status not paused")
	}
}

func TestRunnerRetriesWhenLocked(t *testing.T) {
	h := newHarness(t, func(h *harness, _ *Options) {
		h.results = []syncResult{{err: sync.ErrLocked}}
	})
	h.start()
	if !h.status().LastSyncAt.IsZero() {
		t.Fatal("a skipped sync must not count as a sync")
	}
	h.advance(14 * s)
	h.expectSyncs(0)
	h.advance(15 * s)
	h.expectSyncs(0, 15*s)
	if st := h.status(); !st.LastSyncAt.Equal(h.t0.Add(15*s)) || st.LastError != "" {
		t.Fatalf("status after retry = %+v", st)
	}
}

func TestRunnerBacksOffAndNotifiesOnRepeatedErrors(t *testing.T) {
	boom := errors.New("network is unreachable")
	h := newHarness(t, func(h *harness, _ *Options) {
		for i := 0; i < 10; i++ {
			h.results = append(h.results, syncResult{err: boom})
		}
	})
	h.start()
	if st := h.status(); st.LastError != boom.Error() {
		t.Fatalf("lastError = %q", st.LastError)
	}
	h.advance(30*s + 60*s + 120*s)
	h.expectSyncs(0, 30*s, 90*s, 210*s)

	h.mu.Lock()
	notes := append([]string(nil), h.notes...)
	h.mu.Unlock()
	if len(notes) != 1 || !strings.Contains(notes[0], "network is unreachable") {
		t.Fatalf("want exactly one error notification within the hour, got %q", notes)
	}

	// Recovery clears the error.
	h.set(func() { h.results = nil })
	h.advance(210*s + 240*s)
	if st := h.status(); st.LastError != "" {
		t.Fatalf("error not cleared after a good sync: %q", st.LastError)
	}
}

func TestRunnerReportsPerFileErrors(t *testing.T) {
	h := newHarness(t, func(h *harness, _ *Options) {
		h.results = []syncResult{{report: &sync.SyncReport{Errors: []error{errors.New("a: denied"), errors.New("b: denied")}}}}
	})
	h.start()
	if st := h.status(); !strings.Contains(st.LastError, "2 files failed") {
		t.Fatalf("lastError = %q", st.LastError)
	}
	h.advance(30 * s) // retried
	h.expectSyncs(0, 30*s)
}

func TestRunnerRetriesSoonAfterDeferredFiles(t *testing.T) {
	h := newHarness(t, func(h *harness, _ *Options) {
		h.results = []syncResult{{report: &sync.SyncReport{Deferred: []string{"projects/x/s.jsonl"}}}}
	})
	h.start()
	h.advance(44 * s)
	h.expectSyncs(0)
	h.advance(45 * s)
	h.expectSyncs(0, 45*s)
	// A clean sync afterwards doesn't keep retrying (offsets are from start).
	h.advance(165 * s)
	h.expectSyncs(0, 45*s)
}

func TestRunnerTracksDesktopSessionsUntilClaudeRestarts(t *testing.T) {
	h := newHarness(t, func(h *harness, _ *Options) {
		h.appUp = true
		h.appStart = h.t0.Add(-time.Hour)
		h.results = []syncResult{{report: &sync.SyncReport{NewDesktopSessions: []string{"x", "y"}}}}
	})
	h.start()
	if st := h.status(); st.PendingDesktopRestart != 2 {
		t.Fatalf("pending = %d, want 2", st.PendingDesktopRestart)
	}
	snap := h.r.Snapshot()
	if !snap.AppRunning || RestartLine(snap) != "Restart Claude to show 2 new sessions" {
		t.Fatalf("restart line = %q (running %v)", RestartLine(snap), snap.AppRunning)
	}
	h.mu.Lock()
	notes := append([]string(nil), h.notes...)
	h.mu.Unlock()
	if len(notes) != 1 || !strings.Contains(notes[0], "2 sessions arrived from your other Macs") || !strings.Contains(notes[0], "restart Claude") {
		t.Fatalf("notifications = %q", notes)
	}

	// More arrive later; the app still hasn't restarted.
	h.set(func() {
		h.remote = "R1"
		h.results = []syncResult{{report: &sync.SyncReport{NewDesktopSessions: []string{"z"}}}}
	})
	h.advance(60 * s)
	if st := h.status(); st.PendingDesktopRestart != 3 {
		t.Fatalf("pending = %d, want 3", st.PendingDesktopRestart)
	}

	// Claude restarted between the two batches: only the second is pending.
	h.set(func() { h.appStart = h.t0.Add(30 * s) })
	h.advance(80 * s)
	if st := h.status(); st.PendingDesktopRestart != 1 {
		t.Fatalf("pending = %d, want 1", st.PendingDesktopRestart)
	}

	// Restarted again after everything arrived: nothing pending.
	h.set(func() { h.appStart = h.t0.Add(70 * s) })
	h.advance(100 * s)
	if st := h.status(); st.PendingDesktopRestart != 0 || len(st.PendingDesktop) != 0 {
		t.Fatalf("pending = %d (%v), want 0", st.PendingDesktopRestart, st.PendingDesktop)
	}
}

func TestRunnerCarriesPendingAcrossRestarts(t *testing.T) {
	h := newHarness(t, nil)
	prev := &Status{PendingDesktop: []PendingBatch{{At: h.t0.Add(-time.Minute), Count: 4}}}
	if err := WriteStatus(h.paths.Status, prev); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(Options{Paths: h.paths})
	if got := r.Snapshot().PendingDesktopRestart; got != 4 {
		t.Fatalf("pending after restart = %d, want 4", got)
	}
}

func TestRunnerWaitsForConfig(t *testing.T) {
	h := newHarness(t, func(h *harness, _ *Options) {
		h.cfgErr = ErrNotConfigured
	})
	h.start()
	h.expectSyncs()
	st := h.status()
	if st.LastError != NotConfigured {
		t.Fatalf("lastError = %q", st.LastError)
	}
	if line := StatusLine(h.r.Snapshot(), h.t0); !strings.HasPrefix(line, "Not set up") {
		t.Fatalf("status line = %q", line)
	}

	if !st.LastSyncAt.IsZero() {
		t.Fatal("no sync was attempted, so there is no last sync time")
	}

	h.advance(35 * s) // the 30s retry still finds no config
	h.set(func() { h.cfgErr = nil })
	h.advance(50 * s) // the next local poll notices it, well before the 90s retry
	h.expectSyncs(50 * s)
	if h.status().LastError != "" {
		t.Fatal("error not cleared once configured")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.notes) != 0 {
		t.Fatalf("no notification expected for a missing config, got %q", h.notes)
	}
}
