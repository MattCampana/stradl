package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	gosync "sync"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/sync"
)

// LocalOptions configure the real Provider.
type LocalOptions struct {
	// LoadConfig returns the current Stradl config (re-read on each request so
	// a re-init is picked up).
	LoadConfig func() (*config.Config, error)
	// StatusPath is the agent's status.json.
	StatusPath string
	// AgentRunning reports whether the background agent is running.
	AgentRunning func() bool
	// SyncNow triggers an immediate sync; nil when no agent is attached.
	SyncNow func() error
	// Now returns the current time (tests).
	Now func() time.Time
	// Syncing reports whether the agent is syncing right now (status.json
	// doesn't say); nil when unknown.
	Syncing func() bool
}

const (
	overviewTTL = 10 * time.Second // how long an Overview is reused
	remoteTTL   = 30 * time.Second // how long a storage check is reused, unless a sync finished since
	remoteWait  = 20 * time.Second // the longest a request waits on storage
	remoteMax   = 2 * time.Minute  // the longest a storage check runs in the background
	syncTimeout = 15 * time.Minute // a sync started from a standalone dashboard
)

// NewLocalProvider returns the Provider backed by this Mac's data. It is
// long-lived: parsed transcripts and storage checks are cached between
// requests.
func NewLocalProvider(opts LocalOptions) Provider { return newLocalProvider(opts) }

type localProvider struct {
	opts LocalOptions
	loc  *time.Location

	// newSyncer builds the syncer used for the dry-run preview and the
	// heartbeats (tests swap in in-memory storage).
	newSyncer func(cfg *config.Config) (*sync.Syncer, error)
	// runSync runs one sync when no agent is attached.
	runSync func(ctx context.Context, cfg *config.Config) error

	overviewTTL, remoteTTL, remoteWait, remoteMax time.Duration

	scanner *scanner
	desktop *desktopIndex

	buildMu  gosync.Mutex // one Overview build at a time
	cacheMu  gosync.Mutex
	cached   *Overview
	cachedAt time.Time
	paths    map[string]string // session ID → transcript, from the cached Overview's scan

	remoteMu    gosync.Mutex
	remote      *remoteResult
	remoteStale bool
	remoteDone  chan struct{}
	lastBeats   []sync.DeviceHeartbeat

	syncMu  gosync.Mutex
	syncing bool
}

func newLocalProvider(opts LocalOptions) *localProvider {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.LoadConfig == nil {
		opts.LoadConfig = config.Load
	}
	if opts.StatusPath == "" {
		opts.StatusPath = config.StatusFilePath()
	}
	return &localProvider{
		opts:      opts,
		loc:       time.Local,
		newSyncer: func(cfg *config.Config) (*sync.Syncer, error) { return sync.NewSyncer(cfg, true) },
		runSync: func(ctx context.Context, cfg *config.Config) error {
			_, err := sync.RunSync(ctx, cfg, true, false)
			return err
		},
		overviewTTL: overviewTTL,
		remoteTTL:   remoteTTL,
		remoteWait:  remoteWait,
		remoteMax:   remoteMax,
		scanner:     newScanner(),
		desktop:     newDesktopIndex(),
	}
}

// Overview returns the dashboard's main page data, reusing one built in the
// last few seconds. Problems that leave part of it unknown (storage
// unreachable, an unreadable transcript) become warnings, not errors.
func (p *localProvider) Overview(ctx context.Context) (*Overview, error) {
	p.buildMu.Lock()
	defer p.buildMu.Unlock()
	now := p.opts.Now()
	p.cacheMu.Lock()
	cached, at := p.cached, p.cachedAt
	p.cacheMu.Unlock()
	if cached != nil && !now.Before(at) && now.Sub(at) < p.overviewTTL {
		return cached, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ov, paths := p.build(ctx, now)
	p.cacheMu.Lock()
	p.cached, p.cachedAt, p.paths = ov, now, paths
	p.cacheMu.Unlock()
	return ov, nil
}

// Session returns one session with its timeline.
func (p *localProvider) Session(ctx context.Context, id string) (*SessionDetail, error) {
	ov, err := p.Overview(ctx)
	if err != nil {
		return nil, err
	}
	s, file := p.lookup(ov, id)
	if s == nil {
		// Maybe a session that started after the cached overview: rescan
		// (cheap, cached per file) before rebuilding anything.
		cfg, _ := p.config()
		if p.scanner.scan(claudeDirOf(cfg), p.loc).ByID[id] == nil {
			return nil, ErrNotFound
		}
		p.invalidate(false)
		if ov, err = p.Overview(ctx); err != nil {
			return nil, err
		}
		if s, file = p.lookup(ov, id); s == nil {
			return nil, ErrNotFound
		}
	}
	timeline, err := sessionTimeline(file, p.loc, p.scanner.home)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read session %s: %w", id, err)
	}
	return &SessionDetail{Session: *s, Timeline: timeline}, nil
}

func (p *localProvider) lookup(ov *Overview, id string) (*Session, string) {
	p.cacheMu.Lock()
	file := p.paths[id]
	p.cacheMu.Unlock()
	if file == "" {
		return nil, ""
	}
	for i := range ov.Sessions {
		if ov.Sessions[i].ID == id {
			return &ov.Sessions[i], file
		}
	}
	return nil, ""
}

// SyncNow asks the agent for a sync, or runs one in this process when no
// agent is attached (`stradl dashboard` on its own). It returns at once.
func (p *localProvider) SyncNow() error {
	if p.opts.SyncNow != nil {
		err := p.opts.SyncNow()
		if err == nil {
			p.invalidate(true)
		}
		return err
	}
	cfg, err := p.opts.LoadConfig()
	if err != nil || cfg == nil {
		return errors.New("stradl isn't set up on this Mac: run stradl init")
	}
	p.syncMu.Lock()
	if p.syncing {
		p.syncMu.Unlock()
		return nil // the running sync covers it
	}
	p.syncing = true
	p.syncMu.Unlock()
	p.invalidate(false)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
		defer cancel()
		_ = p.runSync(ctx, cfg) // the next overview shows the outcome
		p.syncMu.Lock()
		p.syncing = false
		p.syncMu.Unlock()
		p.invalidate(true)
	}()
	return nil
}

func (p *localProvider) standaloneSyncing() bool {
	p.syncMu.Lock()
	defer p.syncMu.Unlock()
	return p.syncing
}

// invalidate drops the cached overview, and the storage check too when a
// sync may have changed what is pending.
func (p *localProvider) invalidate(remote bool) {
	p.cacheMu.Lock()
	p.cached = nil
	p.cacheMu.Unlock()
	if remote {
		p.remoteMu.Lock()
		p.remoteStale = true
		p.remoteMu.Unlock()
	}
}

func (p *localProvider) config() (*config.Config, error) {
	cfg, err := p.opts.LoadConfig()
	if err == nil && cfg == nil {
		err = errors.New("no config")
	}
	if err != nil {
		return &config.Config{}, err
	}
	return cfg, nil
}

func claudeDirOf(cfg *config.Config) string {
	if cfg != nil && cfg.ClaudeDirOverride != "" {
		return cfg.ClaudeDirOverride
	}
	return config.ClaudeDir()
}

// desktopDirOf is where this Mac's Claude sidebar records are, whether or
// not they sync.
func desktopDirOf(cfg *config.Config) string {
	if dir := cfg.EffectiveDesktopDir(); dir != "" {
		return dir
	}
	return config.DesktopSessionsDir()
}

// build assembles a fresh Overview and the transcript path of each session.
func (p *localProvider) build(ctx context.Context, now time.Time) (*Overview, map[string]string) {
	ov := &Overview{
		GeneratedAt: now,
		Devices:     []Device{},
		Sessions:    []Session{},
		Pending:     Pending{Files: []PendingFile{}},
	}
	warn := func(format string, args ...any) { ov.Warnings = append(ov.Warnings, fmt.Sprintf(format, args...)) }

	cfg, cfgErr := p.config()
	configured := cfgErr == nil
	if !configured {
		warn("Stradl isn't set up on this Mac yet: run stradl init. Showing local sessions only.")
	}
	claudeDir := claudeDirOf(cfg)
	st := readAgentStatus(p.opts.StatusPath)
	ov.Agent = p.agentInfo(st)
	if n := st.ICloudPending; n > 0 {
		files := "files are"
		if n == 1 {
			files = "file is"
		}
		warn("%d %s only in iCloud on this Mac (Optimize Mac Storage). Stradl is downloading them in the background and syncs each one as it arrives.", n, files)
	}

	thisID, thisName, err := sync.ThisDevice(cfg)
	if err != nil {
		warn("Couldn't read this Mac's device ID: %v", err)
	}
	ov.ThisDevice = thisName

	scan := p.scanner.scan(claudeDir, p.loc)
	if scan.Unreadable > 0 {
		warn("%d transcript(s) couldn't be read.", scan.Unreadable)
	}
	records := p.desktop.load(desktopDirOf(cfg))
	live := sync.LiveSessionIDs(claudeDir)

	// The sync side: local scope and state always, the dry-run preview and
	// heartbeats when storage answers in time.
	var (
		scope     *sync.ScopeSummary
		preview   *sync.SyncReport
		previewAt time.Time
		beats     []sync.DeviceHeartbeat
	)
	if configured {
		if scope, err = sync.ScopeScanFor(cfg); err != nil {
			warn("Couldn't scan the sync folders: %v", err)
		}
		r, timedOut := p.remoteState(ctx, cfg, st.LastAttemptAt, now)
		switch {
		case r == nil && timedOut:
			warn("Storage didn't answer within %s: pending changes and the other Macs are unknown for now.", p.remoteWait.Round(time.Second))
		case r != nil:
			if timedOut {
				warn("Storage is slow to answer: pending changes are as of %s.", r.fetchedAt.In(p.loc).Format("15:04:05"))
			}
			switch {
			case r.previewErr != nil && errors.Is(r.previewErr, context.DeadlineExceeded):
				warn("Storage was slow to answer (a big sync may be running), so what's waiting to download is unknown for now.")
			case r.previewErr != nil:
				warn("Couldn't reach storage, so pending downloads are unknown: %s", shortError(r.previewErr))
			default:
				preview, previewAt = r.preview, r.fetchedAt
			}
			if r.beatsErr != nil && r.previewErr == nil {
				warn("Couldn't read the other Macs' heartbeats: %s", shortError(r.beatsErr))
			}
		}
		beats = p.knownBeats()
	}
	view := newSyncView(scope, preview, previewAt)

	devices, activity := buildDevices(beats, thisID, thisName, st, sync.ReadRecent(cfg, now), len(scan.Sessions), now, p.loc)
	ov.Devices = devices

	paths := make(map[string]string, len(scan.Sessions))
	projects := make(map[string]bool)
	for _, sum := range scan.Sessions {
		rec, inSidebar := records[sum.ID]
		s := Session{
			ID:             sum.ID,
			Title:          sessionTitle(sum, rec),
			Project:        projectName(sum.ProjectPath),
			ProjectPath:    sum.ProjectPath,
			Branch:         sum.Branch,
			Surface:        sum.Surface,
			StartedAt:      sum.StartedAt,
			LastActivityAt: sum.LastAt,
			UserTurns:      sum.UserTurns,
			AssistantTurns: sum.Assistant,
			FirstPrompt:    sum.FirstPrompt,
			LastPrompt:     sum.LastPrompt,
			FilesTouched:   nonNil(sum.Files),
			SizeBytes:      sum.SizeBytes,
			SyncState:      view.sessionState(sum.NPath, live[sum.ID], scan.Conflicts[sum.ID]),
			Live:           live[sum.ID],
			Archived:       inSidebar && rec.IsArchived,
			InSidebar:      inSidebar,
			Devices:        activity.forSession(sum.ID),
		}
		ov.Sessions = append(ov.Sessions, s)
		paths[s.ID] = sum.Path
		if s.ProjectPath != "" {
			projects[s.ProjectPath] = true
		}
		ov.Totals.Messages += s.UserTurns + s.AssistantTurns
	}
	sort.SliceStable(ov.Sessions, func(i, j int) bool {
		a, b := ov.Sessions[i], ov.Sessions[j]
		if !a.LastActivityAt.Equal(b.LastActivityAt) {
			return a.LastActivityAt.After(b.LastActivityAt)
		}
		return a.ID < b.ID
	})

	ov.Days = buildDays(ov.Sessions, scan.ByID, activity, now, p.loc)
	ov.Totals.Sessions = len(ov.Sessions)
	ov.Totals.Projects = len(projects)
	if scope != nil {
		ov.Totals.Files = len(scope.Files)
		ov.Totals.Bytes = scope.Bytes
		ov.Totals.SyncedFiles = view.syncedFiles()
	}
	ov.Pending = view.pendingSummary()
	return ov, paths
}

// sessionTitle: the session's custom title, else its sidebar title, else its
// first prompt.
func sessionTitle(s *sessionSummary, rec desktopRecord) string {
	switch {
	case s.CustomTitle != "":
		return s.CustomTitle
	case rec.Title != "":
		return rec.Title
	case s.FirstPrompt != "":
		return truncate(s.FirstPrompt, maxTitleChars)
	}
	return "Untitled session"
}

func projectName(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Base(filepath.Clean(dir))
}

func shortError(err error) string {
	return truncate(collapseSpace(err.Error()), 200)
}

// --- storage checks ------------------------------------------------------------------

// remoteResult is one check of remote storage: the engine's dry run and the
// devices' heartbeats.
type remoteResult struct {
	fetchedAt  time.Time
	stamp      time.Time // the agent's last attempt when the check started
	preview    *sync.SyncReport
	previewErr error
	beats      []sync.DeviceHeartbeat
	beatsErr   error
}

// remoteState returns a storage check no older than remoteTTL (and newer
// than the agent's last sync attempt, stamp), starting one when needed. It
// waits at most remoteWait: the check keeps running in the background and
// a later request picks it up. timedOut reports that the result, if any, is
// an older one.
func (p *localProvider) remoteState(ctx context.Context, cfg *config.Config, stamp, now time.Time) (r *remoteResult, timedOut bool) {
	p.remoteMu.Lock()
	if r := p.remote; r != nil && !p.remoteStale && r.stamp.Equal(stamp) && !now.Before(r.fetchedAt) && now.Sub(r.fetchedAt) < p.remoteTTL {
		p.remoteMu.Unlock()
		return r, false
	}
	if p.remoteDone == nil {
		done := make(chan struct{})
		p.remoteDone = done
		p.remoteStale = false
		go func() {
			res := p.fetchRemote(cfg, stamp)
			p.remoteMu.Lock()
			p.remote = res
			if res.beatsErr == nil {
				p.lastBeats = res.beats
			}
			p.remoteDone = nil
			p.remoteMu.Unlock()
			close(done)
		}()
	}
	done := p.remoteDone
	p.remoteMu.Unlock()

	timer := time.NewTimer(p.remoteWait)
	defer timer.Stop()
	select {
	case <-done:
		timedOut = false
	case <-timer.C:
		timedOut = true
	case <-ctx.Done():
		timedOut = true
	}
	p.remoteMu.Lock()
	defer p.remoteMu.Unlock()
	return p.remote, timedOut
}

// knownBeats returns the latest heartbeats read successfully.
func (p *localProvider) knownBeats() []sync.DeviceHeartbeat {
	p.remoteMu.Lock()
	defer p.remoteMu.Unlock()
	return p.lastBeats
}

func (p *localProvider) fetchRemote(cfg *config.Config, stamp time.Time) *remoteResult {
	r := &remoteResult{fetchedAt: p.opts.Now(), stamp: stamp}
	ctx, cancel := context.WithTimeout(context.Background(), p.remoteMax)
	defer cancel()
	s, err := p.newSyncer(cfg)
	if err != nil {
		r.previewErr, r.beatsErr = err, err
		return r
	}
	var wg gosync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.preview, r.previewErr = s.Preview(ctx)
		if r.previewErr == nil && r.preview == nil {
			r.previewErr = errors.New("no preview")
		}
	}()
	go func() {
		defer wg.Done()
		r.beats, r.beatsErr = s.Heartbeats(ctx)
	}()
	wg.Wait()
	return r
}
