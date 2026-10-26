package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/sync"
)

// Schedule defaults. Local polling is a metadata-only walk; remote polling is
// one List call; a full sync runs every Config.EffectiveSyncInterval().
const (
	DefaultLocalPoll   = 20 * time.Second
	DefaultQuiet       = 10 * time.Second
	DefaultRemotePoll  = 60 * time.Second
	DefaultLockedRetry = 15 * time.Second
	// DefaultDeferredRetry re-runs a sync soon after one that held files
	// back (a session open in Claude, or a file written mid-sync), so they
	// land shortly after the session closes instead of at the next interval.
	DefaultDeferredRetry = 45 * time.Second
	// DefaultMaxDebounce caps how long a stream of local writes (a session
	// that keeps talking) can postpone a sync while waiting for quiet.
	DefaultMaxDebounce = 2 * time.Minute

	syncTimeout    = 15 * time.Minute
	errorNotifyGap = time.Hour // per kind of error
	// errorNotifyRuns is how many syncs in a row must fail before a
	// notification, except for errors only the user can fix (a wrong
	// passphrase, an expired token), which notify at once.
	errorNotifyRuns = 2
)

// NotConfigured is the status error shown until `stradl init` has run.
const NotConfigured = "not set up: run stradl init"

// ErrNotConfigured is returned by the config loader when there is no config.
var ErrNotConfigured = errors.New(NotConfigured)

// Options wires a Runner. Only Paths is required; the rest default to the
// real implementations.
type Options struct {
	Paths   Paths
	Version string
	Device  string

	Clock      Clock
	LoadConfig func() (*config.Config, error)
	// Sync runs one reconcile. The default is sync.RunSync(ctx, cfg, true,
	// false), which fails fast with sync.ErrLocked while another sync runs.
	Sync        func(ctx context.Context, cfg *config.Config) (*sync.SyncReport, error)
	LocalProbe  func(cfg *config.Config) (string, error)
	RemoteProbe func(ctx context.Context, cfg *config.Config) (string, error)
	// CountSessions returns the number of transcripts on this device.
	CountSessions func(cfg *config.Config) int
	// AppStart reports when the Claude desktop app was launched, and whether
	// it is running. Nil means unknown: pending sessions are never cleared.
	AppStart func(ctx context.Context) (time.Time, bool)
	// ApplySidebar applies sidebar layout changes waiting for the Claude app
	// to quit (it does nothing while the app runs) and reports whether any
	// are still waiting. Nil disables it.
	ApplySidebar func(ctx context.Context) (stillPending bool)
	// Notify shows a user notification; nil disables notifications.
	Notify func(title, body string)
	// OnChange is called from the runner goroutine whenever Snapshot may
	// have changed. It must not block.
	OnChange func()
	Log      *Logger

	LocalPoll, Quiet, RemotePoll, LockedRetry, DeferredRetry, MaxDebounce time.Duration
}

// Snapshot is the Runner's current state for display.
type Snapshot struct {
	Status
	Syncing    bool
	AppRunning bool // Claude desktop app running at the last check
	// SyncDone of SyncTotal files are handled in the sync running now
	// (SyncTotal is 0 while it is still comparing).
	SyncDone, SyncTotal int
}

// Runner is the background sync loop. It syncs at start, then whenever local
// files settle after a change, the remote changes, the Mac wakes from sleep,
// SyncNow is called, or the full-sync interval passes. Pausing (a flag file
// any process can create) stops automatic syncs.
type Runner struct {
	o       Options
	trigger chan struct{}
	nudge   chan struct{}

	// writeMu serializes status changes together with their write to
	// status.json, so concurrent updates (the loop and the menu bar's
	// RefreshDesktop) land in the file in the order they were made.
	writeMu gosync.Mutex

	mu         gosync.Mutex
	status     Status
	syncing    bool
	appRunning bool
	syncDone   int // progress of the sync running now
	syncTotal  int

	// Loop state, owned by the Run goroutine.
	paused      bool
	cfgErr      bool
	localFP     string
	remoteFP    string
	pendingFP   string
	changeSince time.Time
	quietAt     time.Time
	retryAt     time.Time
	nextLocal   time.Time
	nextRemote  time.Time
	nextFull    time.Time
	lastLoop    time.Time
	failures    int
	lastNotify  map[ErrorKind]time.Time
	lastWarned  map[string]time.Time // per warning text
}

// NewRunner returns a Runner with defaults filled in. It carries over the
// previous status file, including desktop sessions still waiting for a
// Claude restart.
func NewRunner(o Options) *Runner {
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	if o.LoadConfig == nil {
		o.LoadConfig = ConfigLoader(config.ConfigFilePath(), config.Load)
	}
	if o.Sync == nil {
		o.Sync = func(ctx context.Context, cfg *config.Config) (*sync.SyncReport, error) {
			return sync.RunSync(ctx, cfg, true, false)
		}
	}
	if o.LocalProbe == nil {
		o.LocalProbe = sync.LocalFingerprint
	}
	if o.RemoteProbe == nil {
		o.RemoteProbe = (&probeCache{}).remote
	}
	if o.CountSessions == nil {
		o.CountSessions = func(cfg *config.Config) int { return CountSessions(claudeDirOf(cfg)) }
	}
	if o.Log == nil {
		o.Log = NewLogger(nil)
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.LocalPoll, DefaultLocalPoll)
	def(&o.Quiet, DefaultQuiet)
	def(&o.RemotePoll, DefaultRemotePoll)
	def(&o.LockedRetry, DefaultLockedRetry)
	def(&o.DeferredRetry, DefaultDeferredRetry)
	def(&o.MaxDebounce, DefaultMaxDebounce)

	r := &Runner{o: o, trigger: make(chan struct{}, 1), nudge: make(chan struct{}, 1), lastNotify: map[ErrorKind]time.Time{}, lastWarned: map[string]time.Time{}}
	if prev, err := ReadStatus(o.Paths.Status); err == nil {
		r.status = *prev // keep the last result on display until the first sync
		r.status.recount()
	}
	r.status.AgentVersion = o.Version
	r.status.Device = o.Device
	r.status.PID = os.Getpid()
	return r
}

// SyncNow asks the loop to sync as soon as it is free, even while paused.
func (r *Runner) SyncNow() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Pause stops automatic syncing until Resume, here and in any other agent
// process, by creating the pause flag.
func (r *Runner) Pause() error {
	err := SetPaused(r.o.Paths, true)
	r.poke()
	return err
}

// Resume removes the pause flag; the loop syncs right away.
func (r *Runner) Resume() error {
	err := SetPaused(r.o.Paths, false)
	r.poke()
	return err
}

func (r *Runner) poke() {
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

// Snapshot returns the current state.
func (r *Runner) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.status
	st.PendingDesktop = append([]PendingBatch(nil), r.status.PendingDesktop...)
	return Snapshot{Status: st, Syncing: r.syncing, AppRunning: r.appRunning, SyncDone: r.syncDone, SyncTotal: r.syncTotal}
}

// SetPaused creates or removes the pause flag for p.
func SetPaused(p Paths, paused bool) error {
	if !paused {
		if err := os.Remove(p.Pause); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.Pause), 0700); err != nil {
		return err
	}
	return os.WriteFile(p.Pause, []byte(time.Now().Format(time.RFC3339)+"\n"), 0600)
}

// IsPaused reports whether the pause flag exists.
func IsPaused(p Paths) bool {
	_, err := os.Stat(p.Pause)
	return err == nil
}

// Run loops until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	now := r.o.Clock.Now()
	r.lastLoop = now
	r.paused = IsPaused(r.o.Paths)
	r.update(func(st *Status) { st.Paused = r.paused })
	if r.paused {
		r.o.Log.Printf("agent %s started (pid %d), paused", r.o.Version, os.Getpid())
		r.reschedule(now, nil)
	} else {
		r.o.Log.Printf("agent %s started (pid %d)", r.o.Version, os.Getpid())
		// At login the Claude app usually hasn't started yet: apply sidebar
		// changes left waiting before syncing.
		r.applySidebar(ctx)
		r.runSync(ctx, "start")
	}

	for {
		now = r.o.Clock.Now()
		wait := r.nextDeadline().Sub(now)
		if wait < 0 {
			wait = 0
		}
		timer := r.o.Clock.NewTimer(wait)
		manual := false
		select {
		case <-ctx.Done():
			timer.Stop()
			r.o.Log.Printf("agent stopped")
			return nil
		case <-r.trigger:
			timer.Stop()
			manual = true
		case <-r.nudge:
			timer.Stop()
		case <-timer.C():
		}
		r.step(ctx, r.o.Clock.Now(), manual)
	}
}

// nextDeadline is the earliest pending event. While paused only the local
// poll matters: it is when the pause flag is checked again.
func (r *Runner) nextDeadline() time.Time {
	next := r.nextLocal
	if r.paused {
		return next
	}
	for _, t := range []time.Time{r.nextRemote, r.nextFull, r.quietAt, r.retryAt} {
		if !t.IsZero() && t.Before(next) {
			next = t
		}
	}
	return next
}

func (r *Runner) step(ctx context.Context, now time.Time, manual bool) {
	// Every wait is at most one local poll long, so a much longer gap means
	// the timer couldn't fire: the Mac was asleep.
	asleep := wallSince(now, r.lastLoop) > 2*r.o.LocalPoll
	r.lastLoop = now

	if paused := IsPaused(r.o.Paths); paused != r.paused {
		r.paused = paused
		r.update(func(st *Status) { st.Paused = paused })
		if paused {
			r.o.Log.Printf("paused")
		} else {
			r.o.Log.Printf("resumed")
			r.runSync(ctx, "resume")
			return
		}
	}

	switch {
	case manual:
		r.runSync(ctx, "manual")
		return
	case r.paused:
		r.nextLocal = now.Add(r.o.LocalPoll)
		return
	case asleep:
		r.runSync(ctx, "wake")
		return
	case due(now, r.retryAt):
		r.runSync(ctx, "retry")
		return
	case due(now, r.nextFull):
		r.runSync(ctx, "interval")
		return
	}

	if due(now, r.quietAt) {
		r.quietAt = time.Time{}
		cfg, err := r.o.LoadConfig()
		if err == nil {
			fp, err := r.o.LocalProbe(cfg)
			if err == nil && (fp == r.pendingFP || now.Sub(r.changeSince) >= r.o.MaxDebounce) {
				r.runSync(ctx, "local")
				return
			}
			if err == nil {
				r.pendingFP = fp
			}
			r.quietAt = now.Add(r.o.Quiet) // still changing: wait for quiet again
		}
	}

	if due(now, r.nextLocal) {
		r.nextLocal = now.Add(r.o.LocalPoll)
		cfg, err := r.o.LoadConfig()
		if err == nil && r.cfgErr {
			r.runSync(ctx, "configured")
			return
		}
		if err == nil && r.quietAt.IsZero() {
			if fp, err := r.o.LocalProbe(cfg); err == nil && fp != r.localFP {
				r.pendingFP, r.changeSince = fp, now
				r.quietAt = now.Add(r.o.Quiet)
			}
		}
		r.refreshDesktop(ctx)
	}

	if due(now, r.nextRemote) {
		r.nextRemote = now.Add(r.o.RemotePoll)
		if cfg, err := r.o.LoadConfig(); err == nil {
			fp, err := r.o.RemoteProbe(ctx, cfg)
			if err == nil && fp != r.remoteFP {
				r.runSync(ctx, "remote")
				return
			}
		}
	}
}

func due(now, t time.Time) bool { return !t.IsZero() && !now.Before(t) }

// runSync performs one sync and records the outcome.
func (r *Runner) runSync(ctx context.Context, reason string) {
	start := r.o.Clock.Now()
	cfg, err := r.o.LoadConfig()
	if err != nil {
		if !r.cfgErr || r.failures == 0 {
			r.o.Log.Printf("cannot sync (%s): %v", reason, err)
		}
		r.cfgErr = true
		r.finish(ctx, start, start, nil, nil, err)
		return
	}
	r.cfgErr = false

	// Take both fingerprints before syncing. A change made while the sync
	// runs, including the sync's own writes, then shows up at the next poll:
	// at worst one extra sync that finds nothing to do, never a missed change.
	if fp, err := r.o.LocalProbe(cfg); err == nil {
		r.localFP = fp
	}
	if fp, err := r.o.RemoteProbe(ctx, cfg); err == nil {
		r.remoteFP = fp
	} else {
		r.remoteFP = "" // unknown: the next successful poll triggers a sync
	}

	r.setSyncing(true)
	start = r.o.Clock.Now()
	sctx, cancel := context.WithTimeout(ctx, syncTimeout)
	sctx = sync.WithProgress(sctx, func(done, total int) {
		r.mu.Lock()
		r.syncDone, r.syncTotal = done, total
		r.mu.Unlock()
	})
	report, err := r.o.Sync(sctx, cfg)
	cancel()
	end := r.o.Clock.Now()
	r.setSyncing(false)

	if ctx.Err() != nil {
		return // shutting down
	}
	if errors.Is(err, sync.ErrLocked) {
		// Another sync (a manual `stradl sync`) is covering the same files.
		r.o.Log.Printf("sync skipped (%s): another sync is running; retrying in %s", reason, r.o.LockedRetry)
		r.reschedule(end, cfg)
		r.retryAt = end.Add(r.o.LockedRetry)
		return
	}
	r.logSync(reason, end.Sub(start), report, err)
	r.finish(ctx, start, end, cfg, report, err)
}

// finish records a sync outcome in the status file and schedules what
// comes next.
func (r *Runner) finish(ctx context.Context, start, end time.Time, cfg *config.Config, report *sync.SyncReport, err error) {
	errMsg := ""
	var kind ErrorKind
	switch {
	case err != nil:
		errMsg = err.Error()
		kind = ClassifyError(err)
	case report != nil && len(report.Errors) > 0:
		errMsg = report.Errors[0].Error()
		if n := len(report.Errors); n > 1 {
			errMsg = fmt.Sprintf("%d files failed to sync; first: %s", n, errMsg)
		}
		kind = classifyFileErrors(report.Errors)
	}

	newDesktop := 0
	sessions := 0
	if cfg != nil {
		sessions = r.o.CountSessions(cfg) // a directory walk: not under the lock
	}
	r.update(func(st *Status) {
		st.LastError, st.LastErrorKind = errMsg, kind
		// Only a clean sync counts as the last sync; a failed one is an
		// attempt, so "Last sync" never claims "just now" for a failure.
		// A config that fails to load is a failed attempt too, but waiting
		// for `stradl init` is not.
		if errMsg != NotConfigured {
			st.LastAttemptAt = end
		}
		if cfg == nil {
			return // no sync ran (no usable config)
		}
		if errMsg == "" {
			st.LastSyncAt = end
			st.LastDurationMs = end.Sub(start).Milliseconds()
		}
		if report != nil { // a sync that failed outright keeps the last counts
			st.Uploaded = len(report.Uploaded) + len(report.Restored)
			st.Downloaded = len(report.Downloaded)
			st.Merged = len(report.Merged)
			st.Deferred = len(report.Deferred)
			st.Conflicts = len(report.Conflicts)
			st.Deleted = len(report.Deleted)
			st.Trashed = len(report.Trashed)
			st.Warnings = report.Warnings
			newDesktop = len(report.NewDesktopSessions)
			st.addPending(end, newDesktop)
			if report.SidebarPending {
				st.SidebarPending = true
			}
		}
		st.SessionsTotal = sessions
	})
	r.refreshDesktop(ctx)

	r.reschedule(end, cfg)
	if errMsg != "" {
		r.failures++
		r.retryAt = end.Add(r.backoff(cfg))
	} else {
		r.failures = 0
		if report != nil && len(report.Deferred) > 0 {
			r.retryAt = end.Add(r.o.DeferredRetry)
		}
	}
	r.notify(end, errMsg, kind, newDesktop)
	if report != nil {
		r.notifyWarnings(end, report.Warnings)
	}
}

// reschedule restarts every timer from t after a sync attempt.
func (r *Runner) reschedule(t time.Time, cfg *config.Config) {
	interval := (&config.Config{}).EffectiveSyncInterval()
	if cfg != nil {
		interval = cfg.EffectiveSyncInterval()
	}
	r.nextLocal = t.Add(r.o.LocalPoll)
	r.nextRemote = t.Add(r.o.RemotePoll)
	r.nextFull = t.Add(interval)
	r.quietAt = time.Time{}
	r.retryAt = time.Time{}
	r.lastLoop = t
}

// backoff is the retry delay after r.failures consecutive failures: 30s,
// doubling, capped at the full-sync interval.
func (r *Runner) backoff(cfg *config.Config) time.Duration {
	limit := (&config.Config{}).EffectiveSyncInterval()
	if cfg != nil {
		limit = cfg.EffectiveSyncInterval()
	}
	d := 30 * time.Second
	for i := 1; i < r.failures && d < limit; i++ {
		d *= 2
	}
	if d > limit {
		d = limit
	}
	return d
}

func (r *Runner) notify(now time.Time, errMsg string, kind ErrorKind, newDesktop int) {
	if r.o.Notify == nil {
		return
	}
	if newDesktop > 0 {
		r.mu.Lock()
		running := r.appRunning
		r.mu.Unlock()
		what := fmt.Sprintf("%d sessions arrived", newDesktop)
		if newDesktop == 1 {
			what = "1 session arrived"
		}
		if running {
			r.o.Notify("Stradl", what+" from your other Macs — restart Claude to see them")
		} else {
			r.o.Notify("Stradl", what+" from your other Macs — they'll appear when you open Claude")
		}
	}
	if errMsg == "" || errMsg == NotConfigured || (r.failures < errorNotifyRuns && !kind.urgent()) {
		return
	}
	if last, ok := r.lastNotify[kind]; ok && wallSince(now, last) < errorNotifyGap {
		return
	}
	r.lastNotify[kind] = now
	switch kind {
	case ErrorKeyMismatch:
		r.o.Notify("Stradl", kind.Summary()+". To resume syncing, run 'stradl init --passphrase' in Terminal with the passphrase they use.")
	case ErrorAuth:
		r.o.Notify("Stradl", kind.Summary()+". To resume syncing, run 'stradl init' in Terminal with new storage credentials.")
	default:
		r.o.Notify("Stradl", "Sync keeps failing: "+truncate(errMsg, 120)+" (see the log)")
	}
}

// notifyWarnings tells the user about problems that held part of a sync back
// (deletions the mass-delete guard stopped, a stale git lock), each at most
// once per errorNotifyGap.
func (r *Runner) notifyWarnings(now time.Time, warnings []string) {
	if r.o.Notify == nil {
		return
	}
	for _, w := range warnings {
		if last, ok := r.lastWarned[w]; ok && wallSince(now, last) < errorNotifyGap {
			continue
		}
		r.lastWarned[w] = now
		r.o.Notify("Stradl", truncate(w, 240))
	}
}

// RefreshDesktop re-checks whether the Claude app has restarted since
// pending desktop sessions arrived. Safe to call from any goroutine.
func (r *Runner) RefreshDesktop(ctx context.Context) { r.refreshDesktop(ctx) }

func (r *Runner) refreshDesktop(ctx context.Context) {
	if r.o.AppStart == nil {
		return
	}
	r.mu.Lock()
	pending := r.status.PendingDesktopRestart
	sidebar := r.status.SidebarPending
	r.mu.Unlock()
	if pending == 0 && !sidebar {
		return
	}
	started, running := r.o.AppStart(ctx)
	r.mu.Lock()
	r.appRunning = running
	r.mu.Unlock()
	if running && pending > 0 {
		r.update(func(st *Status) { st.clearPendingBefore(started) })
	}
	if !running && sidebar {
		r.applySidebar(ctx)
	}
}

// applySidebar applies waiting sidebar changes while the Claude app is
// closed, whether the user quit it or Restart Claude did.
func (r *Runner) applySidebar(ctx context.Context) {
	if r.o.ApplySidebar == nil {
		return
	}
	left := r.o.ApplySidebar(ctx)
	r.update(func(st *Status) { st.SidebarPending = left })
}

// update changes the status, persists it, and tells the UI. Safe to call
// from any goroutine: writeMu covers both the change and the write, so the
// file always ends up holding the latest status, and the status is encoded
// under mu, so no slice it shares is read while another update rewrites it.
func (r *Runner) update(fn func(st *Status)) {
	r.writeMu.Lock()
	r.mu.Lock()
	fn(&r.status)
	data, err := encodeStatus(&r.status)
	r.mu.Unlock()
	if err == nil {
		err = writeFileAtomic(r.o.Paths.Status, data)
	}
	r.writeMu.Unlock()
	if err != nil {
		r.o.Log.Printf("failed to write status: %v", err)
	}
	r.changed()
}

func (r *Runner) setSyncing(v bool) {
	r.mu.Lock()
	r.syncing = v
	r.syncDone, r.syncTotal = 0, 0
	r.mu.Unlock()
	r.changed()
}

func (r *Runner) changed() {
	if r.o.OnChange != nil {
		r.o.OnChange()
	}
}

func (r *Runner) logSync(reason string, took time.Duration, rep *sync.SyncReport, err error) {
	if err != nil {
		r.o.Log.Printf("sync failed (%s) after %s: %v", reason, took.Round(time.Millisecond), err)
		return
	}
	r.o.Log.Printf("sync ok (%s) %s: up=%d down=%d merged=%d restored=%d deleted=%d trashed=%d deferred=%d conflicts=%d skipped=%d new-desktop=%d remote-control=%d errors=%d",
		reason, took.Round(time.Millisecond), len(rep.Uploaded), len(rep.Downloaded), len(rep.Merged), len(rep.Restored),
		len(rep.Deleted), len(rep.Trashed), len(rep.Deferred), len(rep.Conflicts), len(rep.Skipped), len(rep.NewDesktopSessions),
		len(rep.RemoteControlled), len(rep.Errors))
	for _, e := range rep.Errors {
		r.o.Log.Printf("  error: %v", e)
	}
	for _, w := range rep.Warnings {
		r.o.Log.Printf("  warning: %s", w)
	}
	for _, t := range rep.Trashed {
		r.o.Log.Printf("  trashed: %s (deleted on another Mac; kept in ~/.stradl/trash for 30 days)", t)
	}
	for _, c := range rep.Conflicts {
		r.o.Log.Printf("  conflict: %s (other version kept beside it or in ~/.stradl/conflicts)", c)
	}
}

// ConfigLoader returns a loader that re-reads the config only when the file
// changes, so the loop can consult it on every poll and still pick up edits
// (credentials, interval) without a restart.
func ConfigLoader(path string, load func() (*config.Config, error)) func() (*config.Config, error) {
	var (
		mu     gosync.Mutex
		cached *config.Config
		mod    time.Time
		size   int64
	)
	return func() (*config.Config, error) {
		mu.Lock()
		defer mu.Unlock()
		fi, err := os.Stat(path)
		if os.IsNotExist(err) {
			cached = nil
			return nil, ErrNotConfigured
		}
		if err != nil {
			return nil, err
		}
		if cached != nil && fi.ModTime().Equal(mod) && fi.Size() == size {
			return cached, nil
		}
		cfg, err := load()
		if err != nil {
			return nil, err
		}
		cached, mod, size = cfg, fi.ModTime(), fi.Size()
		return cfg, nil
	}
}

// probeCache keeps one storage client for remote probes per loaded config.
type probeCache struct {
	mu    gosync.Mutex
	cfg   *config.Config
	probe *sync.Probe
}

func (p *probeCache) remote(ctx context.Context, cfg *config.Config) (string, error) {
	p.mu.Lock()
	if p.cfg != cfg || p.probe == nil {
		probe, err := sync.NewProbe(cfg)
		if err != nil {
			p.mu.Unlock()
			return "", err
		}
		p.cfg, p.probe = cfg, probe
	}
	probe := p.probe
	p.mu.Unlock()
	return probe.Remote(ctx)
}

func claudeDirOf(cfg *config.Config) string {
	if cfg.ClaudeDirOverride != "" {
		return cfg.ClaudeDirOverride
	}
	return config.ClaudeDir()
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
