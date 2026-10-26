package dashboard

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	gosync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/crypto"
	"github.com/tawanorg/claude-sync/internal/storage"
	"github.com/tawanorg/claude-sync/internal/sync"
)

// memStore is an in-memory bucket with ETags and compare-and-swap, like Azure.
type memStore struct {
	mu    gosync.Mutex
	objs  map[string]memObj
	seq   int
	lists atomic.Int32
}

type memObj struct {
	data []byte
	etag string
	mod  time.Time
}

func newMemStore() *memStore { return &memStore{objs: map[string]memObj{}} }

func (m *memStore) put(key string, data []byte) string {
	m.seq++
	etag := fmt.Sprintf(`"0x%04d"`, m.seq)
	m.objs[key] = memObj{data: append([]byte(nil), data...), etag: etag, mod: time.Now()}
	return etag
}

func (m *memStore) Upload(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.put(key, data)
	return nil
}

func (m *memStore) UploadIf(_ context.Context, key string, data []byte, ifMatch string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, exists := m.objs[key]
	if (ifMatch == "" && exists) || (ifMatch != "" && (!exists || cur.etag != ifMatch)) {
		return "", fmt.Errorf("%s: %w", key, storage.ErrPreconditionFailed)
	}
	return m.put(key, data), nil
}

func (m *memStore) Download(ctx context.Context, key string) ([]byte, error) {
	d, _, err := m.DownloadWithETag(ctx, key)
	return d, err
}

func (m *memStore) DownloadWithETag(_ context.Context, key string) ([]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return nil, "", fmt.Errorf("%s: %w", key, storage.ErrNotFound)
	}
	return append([]byte(nil), o.data...), o.etag, nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}

func (m *memStore) DeleteBatch(ctx context.Context, keys []string) error {
	for _, k := range keys {
		_ = m.Delete(ctx, k)
	}
	return nil
}

func (m *memStore) List(_ context.Context, prefix string) ([]storage.ObjectInfo, error) {
	m.lists.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []storage.ObjectInfo
	for k, o := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, storage.ObjectInfo{Key: k, Size: int64(len(o.data)), LastModified: o.mod, ETag: o.etag})
		}
	}
	return out, nil
}

func (m *memStore) Head(_ context.Context, key string) (*storage.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &storage.ObjectInfo{Key: key, Size: int64(len(o.data)), LastModified: o.mod, ETag: o.etag}, nil
}

func (m *memStore) BucketExists(context.Context) (bool, error) { return true, nil }

// downStore fails every listing, like storage that can't be reached.
type downStore struct{ *memStore }

func (downStore) List(context.Context, string) ([]storage.ObjectInfo, error) {
	return nil, errors.New("dial tcp: lookup example.blob.core.windows.net: no such host")
}

// slowStore holds every listing until release is closed.
type slowStore struct {
	*memStore
	release chan struct{}
}

func (s slowStore) List(ctx context.Context, prefix string) ([]storage.ObjectInfo, error) {
	<-s.release
	return s.memStore.List(ctx, prefix)
}

// --- harness ---------------------------------------------------------------------

const (
	acct = "11111111-2222-4333-8444-555555555555"
	org  = "66666666-7777-4888-9999-000000000000"

	s1 = "00000000-0000-4000-8000-000000000001" // synced
	s2 = "00000000-0000-4000-8000-000000000002" // changed here since it synced
	s3 = "00000000-0000-4000-8000-000000000003" // changed here and on the other Mac
	s4 = "00000000-0000-4000-8000-000000000004" // changed on the other Mac
	s5 = "00000000-0000-4000-8000-000000000005" // open in Claude here, changed on the other Mac
	s6 = "00000000-0000-4000-8000-000000000006" // has a conflict copy
	s7 = "00000000-0000-4000-8000-000000000007" // excluded from sync
	s8 = "00000000-0000-4000-8000-000000000008" // exists only on the other Mac
)

// mac is one simulated Mac.
type mac struct {
	name, claude, state, desktop string
	cfg                          *config.Config
}

type harness struct {
	t     *testing.T
	home  string
	key   string
	store *memStore
	now   time.Time
	loc   *time.Location
	alice *mac
	bob   *mac
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home) // path mapping and the default folders stay inside the test
	h := &harness{t: t, home: home, key: filepath.Join(home, "age-key.txt"), store: newMemStore(), loc: newYork(t)}
	if err := crypto.GenerateKey(h.key); err != nil {
		t.Fatal(err)
	}
	// The most recent noon in the test zone: offsets of a few hours never
	// cross midnight, so the day grouping the test checks is stable at any
	// hour, and the clock never runs ahead of the files' real mtimes.
	real := time.Now().In(h.loc)
	y, m, d := real.Date()
	h.now = time.Date(y, m, d, 12, 0, 0, 0, h.loc)
	if h.now.After(real) {
		h.now = h.now.AddDate(0, 0, -1)
	}
	h.alice = h.newMac("Alice's Mac", "alice")
	h.bob = h.newMac("Bob's Mac", "bob")
	return h
}

func (h *harness) newMac(name, dir string) *mac {
	root := filepath.Join(h.home, dir)
	m := &mac{
		name:    name,
		claude:  filepath.Join(root, ".claude"),
		state:   filepath.Join(root, ".stradl"),
		desktop: filepath.Join(root, "claude-code-sessions"),
	}
	for _, d := range []string{m.claude, m.state, filepath.Join(m.desktop, acct, org)} {
		if err := os.MkdirAll(d, 0700); err != nil {
			h.t.Fatal(err)
		}
	}
	writeFile(h.t, filepath.Join(m.state, "device-name"), name)
	m.cfg = &config.Config{
		Scope:              config.ScopeSessions,
		EncryptionKey:      h.key,
		Exclude:            []string{"projects/-Users-test-code-secret"},
		ClaudeDirOverride:  m.claude,
		StateDirOverride:   m.state,
		DesktopDirOverride: m.desktop,
	}
	return m
}

func (h *harness) syncerFor(cfg *config.Config, store storage.Storage) (*sync.Syncer, error) {
	state, err := sync.LoadStateFromDir(cfg.StateDirOverride)
	if err != nil {
		return nil, err
	}
	enc, err := crypto.NewEncryptor(cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}
	return sync.NewSyncerWith(cfg, store, enc, state, cfg.ClaudeDirOverride, true), nil
}

// sync runs one full sync on m, heartbeat included, as RunSync does.
func (h *harness) sync(m *mac) {
	h.t.Helper()
	s, err := h.syncerFor(m.cfg, h.store)
	if err != nil {
		h.t.Fatal(err)
	}
	rep, err := s.Sync(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	if len(rep.Errors) > 0 {
		h.t.Fatalf("%s: sync errors: %v", m.name, rep.Errors)
	}
	s.AfterSync(context.Background(), rep)
}

func transcriptPath(m *mac, project, id string) string {
	return filepath.Join(m.claude, "projects", "-Users-test-code-"+project, id+".jsonl")
}

// session writes (or extends) a transcript with one prompt and one edit at
// at, and dates the file then: a transcript written in the last few seconds
// counts as open in Claude.
func (h *harness) session(m *mac, project, id string, at time.Time, prompt string) {
	h.t.Helper()
	cwd := "/Users/test/code/" + project
	rows := userRow(id, cwd, at, prompt) + editRow(id, cwd, at.Add(time.Second), cwd+"/"+strings.ReplaceAll(prompt, " ", "_")+".go")
	p := transcriptPath(m, project, id)
	if _, err := os.Stat(p); err == nil {
		appendFile(h.t, p, rows)
	} else {
		writeFile(h.t, p, rows)
	}
	mt := at.Add(2 * time.Second)
	if err := os.Chtimes(p, mt, mt); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) provider(m *mac, store storage.Storage) *localProvider {
	p := newLocalProvider(LocalOptions{
		LoadConfig:   func() (*config.Config, error) { c := *m.cfg; return &c, nil },
		StatusPath:   filepath.Join(m.state, "status.json"),
		AgentRunning: func() bool { return true },
		Now:          func() time.Time { return h.now },
	})
	p.loc = h.loc
	p.newSyncer = func(cfg *config.Config) (*sync.Syncer, error) { return h.syncerFor(cfg, store) }
	return p
}

func (h *harness) markLive(m *mac, id string) {
	rec := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"sessionId":"` + id + `","startedAt":` + strconv.FormatInt(time.Now().UnixMilli(), 10) + `}`
	writeFile(h.t, filepath.Join(m.claude, "sessions", "1.json"), rec)
}

func (h *harness) date(t time.Time) string { return t.In(h.loc).Format("2006-01-02") }

func findSession(ov *Overview, id string) *Session {
	for i := range ov.Sessions {
		if ov.Sessions[i].ID == id {
			return &ov.Sessions[i]
		}
	}
	return nil
}

func findDaySession(ov *Overview, date, id string) *DaySession {
	for _, d := range ov.Days {
		if d.Date != date {
			continue
		}
		for i := range d.Sessions {
			if d.Sessions[i].ID == id {
				return &d.Sessions[i]
			}
		}
	}
	return nil
}

// scenario builds the two-Mac history every sync state comes from, and
// returns when each Mac last changed s3.
func (h *harness) scenario() (aliceAt, bobAt time.Time) {
	a, b := h.alice, h.bob
	aliceAt = h.now.Add(-50 * time.Hour)
	bobAt = h.now.Add(-30 * time.Minute)
	h.session(a, "app", s1, h.now.Add(-2*time.Hour), "first session")
	h.session(a, "app", s2, h.now.Add(-26*time.Hour), "second session")
	h.session(a, "app", s3, aliceAt, "third session")
	h.session(a, "api", s4, h.now.Add(-74*time.Hour), "fourth session")
	h.session(a, "api", s5, h.now.Add(-3*time.Hour), "fifth session")
	h.session(a, "api", s6, h.now.Add(-4*time.Hour), "sixth session")
	h.session(a, "secret", s7, h.now.Add(-9*24*time.Hour), "seventh session")
	writeFile(h.t, filepath.Join(a.desktop, acct, org, "local_one.json"), `{"cliSessionId":"`+s1+`","title":"Sidebar title","isArchived":false,"cwd":"/Users/test/code/app","lastActivityAt":1}`)
	writeFile(h.t, filepath.Join(a.desktop, acct, org, "local_two.json"), `{"cliSessionId":"`+s2+`","title":"","isArchived":true,"cwd":"/Users/test/code/app","lastActivityAt":1}`)
	h.sync(a)

	h.sync(b)
	h.session(b, "app", s3, bobAt, "bob continues")
	h.session(b, "api", s4, h.now.Add(-time.Hour), "bob on four")
	h.session(b, "api", s5, h.now.Add(-time.Hour), "bob on five")
	h.session(b, "api", s8, h.now.Add(-time.Hour), "bob alone")
	h.sync(b)

	h.session(a, "app", s2, h.now.Add(-20*time.Minute), "more on two")
	h.session(a, "app", s3, h.now.Add(-10*time.Minute), "alice continues")
	writeFile(h.t, transcriptPath(a, "api", s6)+".conflict.20261024-101010.other-mac", "{}\n")
	h.markLive(a, s5)

	writeFile(h.t, filepath.Join(a.state, "status.json"), fmt.Sprintf(
		`{"lastSyncAt":%q,"lastAttemptAt":%q,"lastError":"","agentVersion":"2.1.0","pendingDesktopRestart":2,"sessionsTotal":7,"paused":false}`,
		h.now.Add(-5*time.Minute).Format(time.RFC3339), h.now.Add(-5*time.Minute).Format(time.RFC3339)))
	return aliceAt, bobAt
}

// --- tests -----------------------------------------------------------------------

func TestOverviewSyncStates(t *testing.T) {
	h := newHarness(t)
	aliceAt, bobAt := h.scenario()
	p := h.provider(h.alice, h.store)

	ov, err := p.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Warnings) != 0 {
		t.Errorf("warnings: %v", ov.Warnings)
	}
	if ov.ThisDevice != "Alice's Mac" {
		t.Errorf("ThisDevice = %q", ov.ThisDevice)
	}

	wantStates := map[string]string{
		s1: "synced", s2: "pending-upload", s3: "merging", s4: "pending-download",
		s5: "held", s6: "conflict", s7: "not-synced",
	}
	if len(ov.Sessions) != len(wantStates) {
		t.Errorf("%d sessions, want %d (the other Mac's own session is not on this one)", len(ov.Sessions), len(wantStates))
	}
	for id, want := range wantStates {
		s := findSession(ov, id)
		if s == nil {
			t.Errorf("session %s missing", id)
			continue
		}
		if s.SyncState != want {
			t.Errorf("%s: SyncState = %q, want %q", id, s.SyncState, want)
		}
	}
	if s := findSession(ov, s5); !s.Live {
		t.Error("s5 is open in Claude: Live")
	}
	for i := 1; i < len(ov.Sessions); i++ {
		if ov.Sessions[i].LastActivityAt.After(ov.Sessions[i-1].LastActivityAt) {
			t.Errorf("sessions not most recent first at %d", i)
		}
	}

	one := findSession(ov, s1)
	if one.Title != "Sidebar title" || !one.InSidebar || one.Archived || one.Project != "app" || one.Surface != "cli" {
		t.Errorf("s1 = %+v", one)
	}
	if two := findSession(ov, s2); !two.Archived || two.Title != "second session" {
		t.Errorf("s2: archived=%v title=%q (an empty sidebar title falls back to the first prompt)", two.Archived, two.Title)
	}
	if !reflect.DeepEqual(one.Devices, []string{"Alice's Mac"}) {
		t.Errorf("s1 devices = %v", one.Devices)
	}
	if three := findSession(ov, s3); !reflect.DeepEqual(three.Devices, []string{"Alice's Mac", "Bob's Mac"}) {
		t.Errorf("s3 devices = %v", three.Devices)
	}
	if four := findSession(ov, s4); !reflect.DeepEqual(four.Devices, []string{"Alice's Mac", "Bob's Mac"}) {
		t.Errorf("s4 devices = %v", four.Devices)
	}

	// Pending: s2 up, s4 and s8 down, s3 merges, s5 waits, one conflict copy.
	pd := ov.Pending
	if pd.Upload != 1 || pd.Download != 2 || pd.Merge != 1 || pd.Deferred != 1 || pd.Conflicts != 1 {
		t.Errorf("pending = %+v", pd)
	}
	actions := map[string]string{}
	for _, f := range pd.Files {
		actions[f.Path] = f.Action
	}
	if actions["projects/-Users-test-code-api/"+s8+".jsonl"] != "download" || actions["projects/-Users-test-code-app/"+s3+".jsonl"] != "merge" {
		t.Errorf("pending files = %v", pd.Files)
	}

	// Totals: six transcripts and two sidebar records in scope (s7 is
	// excluded); s1, s6 and both records match their remote copies.
	tot := ov.Totals
	if tot.Sessions != 7 || tot.Projects != 3 || tot.Files != 8 || tot.SyncedFiles != 4 || tot.Bytes == 0 {
		t.Errorf("totals = %+v", tot)
	}
	if tot.Messages != 2*7+2*2 { // one prompt and one edit per session write; s2 and s3 got a second
		t.Errorf("messages = %d", tot.Messages)
	}

	// Devices: this Mac first, both from heartbeats.
	if len(ov.Devices) != 2 || !ov.Devices[0].This || ov.Devices[0].Name != "Alice's Mac" || ov.Devices[1].Name != "Bob's Mac" {
		t.Fatalf("devices = %+v", ov.Devices)
	}
	bob := ov.Devices[1]
	if !bob.Healthy || bob.Sessions != 7 || bob.Version != sync.AppVersion {
		t.Errorf("bob = %+v", bob)
	}
	if want := []string{s3, s4, s5, s8}; !sameSet(bob.RecentSessionIDs, want) {
		t.Errorf("bob recent = %v, want %v", bob.RecentSessionIDs, want)
	}
	alice := ov.Devices[0]
	if !alice.Healthy || alice.Sessions != 7 || len(alice.RecentSessionIDs) != 6 {
		t.Errorf("alice = %+v", alice)
	}

	// Agent.
	ag := ov.Agent
	if !ag.Running || ag.Version != "2.1.0" || ag.PendingDesktopRestart != 2 || ag.LastSyncAt == nil || ag.LastError != "" {
		t.Errorf("agent = %+v", ag)
	}

	// Days: the last seven, newest first, each session on the days it was
	// worked on, with the Mac that changed it that day.
	if len(ov.Days) != 7 || ov.Days[0].Label != "Today" || ov.Days[1].Label != "Yesterday" || ov.Days[0].Date != h.date(h.now) {
		t.Fatalf("days = %+v", ov.Days)
	}
	if ds := findDaySession(ov, h.date(aliceAt), s3); ds == nil || !contains(ds.Devices, "Alice's Mac") || !contains(ds.Prompts, "third session") {
		t.Errorf("s3 on %s = %+v", h.date(aliceAt), ds)
	}
	// Bob's rows reach this Mac with the merge; his heartbeat already says
	// he worked on it.
	if ds := findDaySession(ov, h.date(bobAt), s3); ds == nil || !contains(ds.Devices, "Bob's Mac") {
		t.Errorf("s3 on %s = %+v", h.date(bobAt), ds)
	}
	for _, d := range ov.Days {
		for _, ds := range d.Sessions {
			if ds.ID == s7 {
				t.Errorf("a session last worked on nine days ago shows on %s", d.Date)
			}
			if ds.Turns == 0 && len(ds.FilesTouched) == 0 {
				t.Errorf("%s on %s: listed with no prompts or edits", ds.ID, d.Date)
			}
		}
	}
}

func TestOverviewWhenStorageIsUnreachable(t *testing.T) {
	h := newHarness(t)
	h.scenario()
	p := h.provider(h.alice, downStore{h.store})

	ov, err := p.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Warnings) != 1 || !strings.Contains(ov.Warnings[0], "pending downloads are unknown") {
		t.Errorf("warnings = %v", ov.Warnings)
	}
	// Without a preview the sync state still tells what changed here.
	want := map[string]string{s1: "synced", s2: "pending-upload", s3: "pending-upload", s4: "synced", s5: "held", s6: "conflict", s7: "not-synced"}
	for id, w := range want {
		if got := findSession(ov, id).SyncState; got != w {
			t.Errorf("%s: %q, want %q", id, got, w)
		}
	}
	if ov.Pending.Upload != 2 || ov.Pending.Download != 0 || len(ov.Pending.Files) != 2 {
		t.Errorf("pending = %+v", ov.Pending)
	}
	if len(ov.Devices) != 1 || !ov.Devices[0].This || ov.Devices[0].Name != "Alice's Mac" {
		t.Errorf("devices = %+v (this Mac is always listed)", ov.Devices)
	}
	if ov.Totals.SyncedFiles != 6 { // s1, s4, s5, s6 and both records are unchanged since they synced
		t.Errorf("synced files = %d", ov.Totals.SyncedFiles)
	}
}

func TestOverviewDoesNotWaitOnSlowStorage(t *testing.T) {
	h := newHarness(t)
	h.scenario()
	slow := slowStore{memStore: h.store, release: make(chan struct{})}
	p := h.provider(h.alice, slow)
	p.remoteWait = 50 * time.Millisecond

	start := time.Now()
	ov, err := p.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("overview took %s", time.Since(start))
	}
	if len(ov.Warnings) == 0 || !strings.Contains(ov.Warnings[0], "didn't answer") {
		t.Errorf("warnings = %v", ov.Warnings)
	}
	if len(ov.Sessions) != 7 {
		t.Errorf("local sessions are shown meanwhile: %d", len(ov.Sessions))
	}

	// The check finishes in the background; the next overview uses it.
	close(slow.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.remoteMu.Lock()
		done := p.remote != nil
		p.remoteMu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.now = h.now.Add(overviewTTL + time.Second)
	ov, _ = p.Overview(context.Background())
	if len(ov.Warnings) != 0 || findSession(ov, s4).SyncState != "pending-download" {
		t.Errorf("after the check finished: warnings %v, s4 %q", ov.Warnings, findSession(ov, s4).SyncState)
	}
}

func TestOverviewCaching(t *testing.T) {
	h := newHarness(t)
	h.session(h.alice, "app", s1, h.now.Add(-time.Hour), "first")
	h.sync(h.alice)
	p := h.provider(h.alice, h.store)
	ctx := context.Background()

	first, _ := p.Overview(ctx)
	lists := h.store.lists.Load()
	if again, _ := p.Overview(ctx); again != first {
		t.Error("an overview is reused within its TTL")
	}

	// A new overview reuses the storage check until it expires...
	h.now = h.now.Add(overviewTTL + time.Second)
	if next, _ := p.Overview(ctx); next == first {
		t.Error("the overview is rebuilt after its TTL")
	}
	if got := h.store.lists.Load(); got != lists {
		t.Errorf("storage listed %d more times within the check's TTL", got-lists)
	}
	// ...or a sync finishes.
	writeFile(t, filepath.Join(h.alice.state, "status.json"), fmt.Sprintf(`{"lastAttemptAt":%q}`, h.now.Format(time.RFC3339)))
	h.now = h.now.Add(overviewTTL + time.Second)
	_, _ = p.Overview(ctx)
	if got := h.store.lists.Load(); got == lists {
		t.Error("storage is checked again after a sync")
	}

	// A transcript that changes shows at the next overview, as pending upload
	// although the reused storage check predates the change.
	h.now = h.now.Add(overviewTTL + time.Second)
	h.session(h.alice, "app", s1, h.now, "follow-up")
	ov, _ := p.Overview(ctx)
	if got := findSession(ov, s1); got.UserTurns != 2 || got.LastPrompt != "follow-up" || got.SyncState != "pending-upload" {
		t.Errorf("after a change: %+v", got)
	}
	if ov.Pending.Upload != 1 || ov.Totals.SyncedFiles != 0 {
		t.Errorf("after a change: pending %+v, synced files %d", ov.Pending, ov.Totals.SyncedFiles)
	}
}

func TestThisMacListedBeforeFirstHeartbeat(t *testing.T) {
	h := newHarness(t)
	h.session(h.alice, "app", s1, h.now.Add(-time.Hour), "first")
	h.session(h.bob, "app", s2, h.now.Add(-time.Hour), "bob's")
	h.sync(h.bob) // only Bob has published a heartbeat
	writeFile(t, filepath.Join(h.alice.state, "status.json"), fmt.Sprintf(
		`{"lastSyncAt":%q,"lastAttemptAt":%q,"agentVersion":"2.1.0"}`, h.now.Add(-time.Minute).Format(time.RFC3339), h.now.Add(-time.Minute).Format(time.RFC3339)))

	ov, err := h.provider(h.alice, h.store).Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Devices) != 2 {
		t.Fatalf("devices = %+v", ov.Devices)
	}
	me := ov.Devices[0]
	id, _ := sync.DeviceID(h.alice.cfg)
	if !me.This || me.ID != id || me.Name != "Alice's Mac" || me.Version != "2.1.0" || !me.Healthy || me.Sessions != 1 {
		t.Errorf("this Mac = %+v", me)
	}
	if ov.Devices[1].This || ov.Devices[1].Name != "Bob's Mac" {
		t.Errorf("other = %+v", ov.Devices[1])
	}
	if findSession(ov, s1).SyncState != "pending-upload" {
		t.Errorf("never-synced session: %q", findSession(ov, s1).SyncState)
	}
}

func TestOverviewWithoutConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, filepath.Join(home, ".claude", "projects", "-w", s1+".jsonl"), userRow(s1, "/w", time.Now().Add(-time.Hour), "hello"))
	p := newLocalProvider(LocalOptions{
		LoadConfig: func() (*config.Config, error) { return nil, errors.New("no config") },
		StatusPath: filepath.Join(home, ".stradl", "status.json"),
	})
	ov, err := p.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Sessions) != 1 || ov.Sessions[0].SyncState != "not-synced" {
		t.Errorf("sessions = %+v", ov.Sessions)
	}
	if len(ov.Warnings) != 1 || !strings.Contains(ov.Warnings[0], "stradl init") {
		t.Errorf("warnings = %v", ov.Warnings)
	}
	if len(ov.Devices) != 1 || !ov.Devices[0].This || len(ov.Days) != 7 {
		t.Errorf("devices = %+v, days = %d", ov.Devices, len(ov.Days))
	}
}

func TestSessionDetail(t *testing.T) {
	h := newHarness(t)
	h.session(h.alice, "app", s1, h.now.Add(-2*time.Hour), "first")
	h.session(h.alice, "app", s1, h.now.Add(-time.Hour), "second")
	p := h.provider(h.alice, h.store)
	ctx := context.Background()

	d, err := p.Session(ctx, s1)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != s1 || d.UserTurns != 2 {
		t.Errorf("session = %+v", d.Session)
	}
	var kinds []string
	for _, e := range d.Timeline {
		kinds = append(kinds, e.Kind+":"+e.Text)
	}
	if want := []string{"prompt:first", "edit:first.go", "prompt:second", "edit:second.go"}; !reflect.DeepEqual(kinds, want) {
		t.Errorf("timeline = %v, want %v", kinds, want)
	}

	for _, id := range []string{"nope", "../../etc/passwd", ""} {
		if _, err := p.Session(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Session(%q) = %v, want ErrNotFound", id, err)
		}
	}

	// A session that started after the cached overview is found.
	h.session(h.alice, "app", s2, h.now.Add(-time.Minute), "brand new")
	if d, err := p.Session(ctx, s2); err != nil || d.FirstPrompt != "brand new" {
		t.Errorf("new session: %v, %v", d, err)
	}
}

func TestSyncNow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// With an agent attached, the request goes to it.
	calls := 0
	p := h.provider(h.alice, h.store)
	p.opts.SyncNow = func() error { calls++; return nil }
	first, _ := p.Overview(ctx)
	if err := p.SyncNow(); err != nil || calls != 1 {
		t.Fatalf("SyncNow = %v, calls = %d", err, calls)
	}
	if next, _ := p.Overview(ctx); next == first {
		t.Error("SyncNow drops the cached overview")
	}

	// Standalone, the dashboard runs the sync itself, one at a time.
	p = h.provider(h.alice, h.store)
	started, release := make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	p.runSync = func(context.Context, *config.Config) error {
		runs.Add(1)
		close(started)
		<-release
		return nil
	}
	if err := p.SyncNow(); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := p.SyncNow(); err != nil {
		t.Fatal(err)
	}
	if ov, _ := p.Overview(ctx); !ov.Agent.Syncing {
		t.Error("a running standalone sync shows as syncing")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for p.standaloneSyncing() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if runs.Load() != 1 {
		t.Errorf("runs = %d, want 1", runs.Load())
	}

	// Not set up: nothing to sync with.
	p = newLocalProvider(LocalOptions{LoadConfig: func() (*config.Config, error) { return nil, errors.New("none") }})
	if err := p.SyncNow(); err == nil {
		t.Error("SyncNow without a config should fail")
	}
}

func TestNoAgentImport(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		ast, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range ast.Imports {
			if strings.Contains(imp.Path.Value, "internal/agent") {
				t.Errorf("%s imports %s: the agent imports the dashboard", f, imp.Path.Value)
			}
		}
	}
}

func sameSet(a, b []string) bool {
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return reflect.DeepEqual(x, y)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
