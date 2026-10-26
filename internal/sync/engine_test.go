package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/crypto"
	"github.com/tawanorg/claude-sync/internal/storage"
)

// casStore is an in-memory bucket with ETags and compare-and-swap, like Azure.
type casStore struct {
	mu   gosync.Mutex
	objs map[string]casObj
	seq  int
	// clockSkew shifts the store's LastModified, to prove sync never trusts it.
	clockSkew time.Duration
	// beforeUploadIf runs (unlocked) before a conditional write, to inject races.
	beforeUploadIf func(key string)
	// beforeDeleteIf runs (unlocked) before a conditional delete, likewise.
	beforeDeleteIf func(key string)
}

var _ storage.Conditional = (*casStore)(nil)

type casObj struct {
	data []byte
	etag string
	mod  time.Time
}

func newCASStore() *casStore { return &casStore{objs: map[string]casObj{}} }

func (c *casStore) put(key string, data []byte) string {
	c.seq++
	etag := fmt.Sprintf(`"0x%04d"`, c.seq)
	c.objs[key] = casObj{data: append([]byte(nil), data...), etag: etag, mod: time.Now().Add(c.clockSkew)}
	return etag
}

func (c *casStore) Upload(_ context.Context, key string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.put(key, data)
	return nil
}

func (c *casStore) UploadIf(_ context.Context, key string, data []byte, ifMatch string) (string, error) {
	if hook := c.beforeUploadIf; hook != nil {
		hook(key)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, exists := c.objs[key]
	if (ifMatch == "" && exists) || (ifMatch != "" && (!exists || cur.etag != ifMatch)) {
		return "", fmt.Errorf("%s: %w", key, storage.ErrPreconditionFailed)
	}
	return c.put(key, data), nil
}

func (c *casStore) Download(ctx context.Context, key string) ([]byte, error) {
	d, _, err := c.DownloadWithETag(ctx, key)
	return d, err
}

func (c *casStore) DownloadWithETag(_ context.Context, key string) ([]byte, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, ok := c.objs[key]
	if !ok {
		return nil, "", fmt.Errorf("%s: %w", key, storage.ErrNotFound)
	}
	return append([]byte(nil), o.data...), o.etag, nil
}

func (c *casStore) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.objs, key)
	return nil
}

func (c *casStore) DeleteIf(_ context.Context, key, ifMatch string) error {
	if hook := c.beforeDeleteIf; hook != nil {
		hook(key)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, exists := c.objs[key]
	if !exists {
		return nil
	}
	if normETag(cur.etag) != normETag(ifMatch) {
		return fmt.Errorf("%s: %w", key, storage.ErrPreconditionFailed)
	}
	delete(c.objs, key)
	return nil
}

func (c *casStore) DeleteBatch(ctx context.Context, keys []string) error {
	for _, k := range keys {
		_ = c.Delete(ctx, k)
	}
	return nil
}

func (c *casStore) List(_ context.Context, prefix string) ([]storage.ObjectInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []storage.ObjectInfo
	for k, o := range c.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, storage.ObjectInfo{Key: k, Size: int64(len(o.data)), LastModified: o.mod, ETag: o.etag})
		}
	}
	return out, nil
}

func (c *casStore) Head(_ context.Context, key string) (*storage.ObjectInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o, ok := c.objs[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &storage.ObjectInfo{Key: key, Size: int64(len(o.data)), LastModified: o.mod, ETag: o.etag}, nil
}

func (c *casStore) BucketExists(context.Context) (bool, error) { return true, nil }

func (c *casStore) keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ks []string
	for k := range c.objs {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// device is one simulated Mac with its own home directory and username.
type device struct {
	t       *testing.T
	name    string
	home    string
	claude  string
	desktop string
	s       *Syncer
}

const testAcct = "11111111-2222-4333-8444-555555555555"
const testOrg = "66666666-7777-4888-9999-000000000000"

func newDevice(t *testing.T, root, name string, store storage.Storage) *device {
	t.Helper()
	home := filepath.Join(root, "Users", name)
	claudeDir := filepath.Join(home, ".claude")
	stateDir := filepath.Join(home, ".claude-sync")
	desktopDir := filepath.Join(home, "Library", "Application Support", "Claude", "claude-code-sessions")
	for _, d := range []string{claudeDir, stateDir, filepath.Join(desktopDir, testAcct, testOrg)} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	state, err := LoadStateFromDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	mapper, err := NewPathMapper(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Scope: config.ScopeSessions, ClaudeDirOverride: claudeDir, StateDirOverride: stateDir, DesktopDirOverride: desktopDir}
	s := &Syncer{storage: store, encryptor: testEncryptor(t, stateDir), state: state, claudeDir: claudeDir, quiet: true, cfg: cfg, paths: mapper, home: home}
	return &device{t: t, name: name, home: home, claude: claudeDir, desktop: desktopDir, s: s}
}

func (d *device) projectDir(project string) string {
	return filepath.Join(d.claude, "projects", EncodeClaudePath(filepath.Join(d.home, "code", project)))
}

// line renders a transcript entry whose cwd is this device's project folder.
func (d *device) line(project, uuid, parent string, at time.Time, text string) string {
	cwd := filepath.Join(d.home, "code", project)
	return fmt.Sprintf(`{"type":"user","uuid":%q,"parentUuid":%q,"cwd":%q,"timestamp":%q,"message":{"content":%q}}`+"\n",
		uuid, parent, cwd, at.UTC().Format(time.RFC3339Nano), text)
}

func (d *device) writeSession(project, id, content string) {
	d.t.Helper()
	p := filepath.Join(d.projectDir(project), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		d.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		d.t.Fatal(err)
	}
}

func (d *device) appendSession(project, id, content string) {
	d.t.Helper()
	f, err := os.OpenFile(filepath.Join(d.projectDir(project), id+".jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		d.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(content); err != nil {
		d.t.Fatal(err)
	}
}

func (d *device) readSession(project, id string) string {
	d.t.Helper()
	b, err := os.ReadFile(filepath.Join(d.projectDir(project), id+".jsonl"))
	if err != nil {
		d.t.Fatalf("%s: read %s: %v", d.name, id, err)
	}
	return string(b)
}

func (d *device) sessions(project string) []string {
	entries, _ := os.ReadDir(d.projectDir(project))
	var ids []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			ids = append(ids, strings.TrimSuffix(e.Name(), ".jsonl"))
		}
	}
	sort.Strings(ids)
	return ids
}

func (d *device) sync() *SyncReport {
	d.t.Helper()
	r, err := d.s.Sync(context.Background())
	if err != nil {
		d.t.Fatalf("%s: sync: %v", d.name, err)
	}
	if len(r.Errors) > 0 {
		d.t.Fatalf("%s: sync errors: %v", d.name, r.Errors)
	}
	return r
}

func TestSyncThreeDeviceUnion(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	b := newDevice(t, root, "matthew", store)
	c := newDevice(t, root, "breakpoint", store)
	now := time.Now()

	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("a%d", i)
		a.writeSession("proj", id, a.line("proj", id+"-1", "", now, "from alice"))
	}
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("b%d", i)
		b.writeSession("proj", id, b.line("proj", id+"-1", "", now, "from matthew"))
	}
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("c%d", i)
		c.writeSession("proj", id, c.line("proj", id+"-1", "", now, "from breakpoint"))
	}

	// Any order converges; two rounds lets everyone see everyone.
	for round := 0; round < 2; round++ {
		a.sync()
		c.sync()
		b.sync()
	}

	want := []string{"a0", "a1", "a2", "b0", "b1", "c0", "c1", "c2", "c3"}
	for _, d := range []*device{a, b, c} {
		got := d.sessions("proj")
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s has %v, want all 9 %v", d.name, got, want)
		}
		// Paths inside every transcript point at this device's own home.
		for _, id := range got {
			content := d.readSession("proj", id)
			if !strings.Contains(content, filepath.Join(d.home, "code", "proj")) || strings.Contains(content, "${HOME}") {
				t.Fatalf("%s/%s not resolved to %s: %s", d.name, id, d.home, content)
			}
		}
	}

	// A third round moves nothing.
	for _, d := range []*device{a, b, c} {
		if r := d.sync(); r.Changed() {
			t.Fatalf("%s: idle sync moved data: %+v", d.name, r)
		}
	}
}

func TestSyncContinueOnAnotherDevice(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	b := newDevice(t, root, "bob", store)
	t0 := time.Now()

	a.writeSession("proj", "s1", a.line("proj", "u1", "", t0, "hello"))
	a.sync()
	b.sync()

	b.appendSession("proj", "s1", b.line("proj", "u2", "u1", t0.Add(time.Minute), "continued on bob"))
	if r := b.sync(); len(r.Uploaded) != 1 {
		t.Fatalf("bob should upload the continued session, got %+v", r)
	}
	if r := a.sync(); len(r.Downloaded) != 1 {
		t.Fatalf("alice should download the continued session, got %+v", r)
	}
	got := a.readSession("proj", "s1")
	if !strings.Contains(got, "continued on bob") || strings.Count(got, "\n") != 2 {
		t.Fatalf("alice transcript = %q", got)
	}
}

func TestSyncConcurrentAppendsMerge(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	b := newDevice(t, root, "bob", store)
	t0 := time.Now()

	a.writeSession("proj", "s1", a.line("proj", "u1", "", t0, "start"))
	a.sync()
	b.sync()

	// Both continue the same session before syncing.
	a.appendSession("proj", "s1", a.line("proj", "ua", "u1", t0.Add(1*time.Minute), "alice turn"))
	b.appendSession("proj", "s1", b.line("proj", "ub", "u1", t0.Add(2*time.Minute), "bob turn"))

	a.sync()
	rb := b.sync()
	if len(rb.Merged) != 1 || len(rb.Conflicts) != 0 {
		t.Fatalf("bob should merge without conflict copies: %+v", rb)
	}
	a.sync()

	for _, d := range []*device{a, b} {
		got := d.readSession("proj", "s1")
		if !strings.Contains(got, "alice turn") || !strings.Contains(got, "bob turn") || strings.Count(got, "\n") != 3 {
			t.Fatalf("%s lost a turn: %q", d.name, got)
		}
		// Newest entry ends the file.
		if !strings.HasSuffix(strings.TrimSpace(got), `"bob turn"}}`) {
			t.Fatalf("%s: newest turn should be last: %q", d.name, got)
		}
		matches, _ := filepath.Glob(filepath.Join(d.projectDir("proj"), "*.conflict.*"))
		if len(matches) != 0 {
			t.Fatalf("%s: unexpected conflict copies %v", d.name, matches)
		}
	}
}

func TestSyncCompareAndSwapRace(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	b := newDevice(t, root, "bob", store)
	t0 := time.Now()

	a.writeSession("proj", "s1", a.line("proj", "u1", "", t0, "start"))
	a.sync()
	b.sync()
	b.appendSession("proj", "s1", b.line("proj", "ub", "u1", t0.Add(time.Minute), "bob raced in"))
	a.appendSession("proj", "s1", a.line("proj", "ua", "u1", t0.Add(2*time.Minute), "alice turn"))

	// Bob's sync lands between Alice's listing and her upload.
	var once gosync.Once
	store.beforeUploadIf = func(key string) {
		if strings.HasSuffix(key, "s1.jsonl.age") {
			once.Do(func() {
				store.beforeUploadIf = nil
				b.sync()
			})
		}
	}
	ra := a.sync()
	if len(ra.Merged) != 1 {
		t.Fatalf("alice should lose the race and merge: %+v", ra)
	}
	b.sync()
	for _, d := range []*device{a, b} {
		got := d.readSession("proj", "s1")
		if !strings.Contains(got, "bob raced in") || !strings.Contains(got, "alice turn") {
			t.Fatalf("%s lost data in the race: %q", d.name, got)
		}
	}
}

func TestSyncLocalDeleteStaysLocal(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	b := newDevice(t, root, "bob", store)
	t0 := time.Now()

	a.writeSession("proj", "s1", a.line("proj", "u1", "", t0, "keep me"))
	a.sync()
	b.sync()

	if err := os.Remove(filepath.Join(b.projectDir("proj"), "s1.jsonl")); err != nil {
		t.Fatal(err)
	}
	b.sync()
	b.sync()
	if got := b.sessions("proj"); len(got) != 0 {
		t.Fatalf("deleted session came back on bob: %v", got)
	}
	if len(store.keys()) == 0 {
		t.Fatal("bob's local delete removed the remote copy")
	}
	a.sync()
	if got := a.sessions("proj"); len(got) != 1 {
		t.Fatalf("alice lost the session: %v", got)
	}

	// New activity elsewhere brings it back.
	a.appendSession("proj", "s1", a.line("proj", "u2", "u1", t0.Add(time.Minute), "new activity"))
	a.sync()
	b.sync()
	if got := b.readSession("proj", "s1"); !strings.Contains(got, "new activity") {
		t.Fatalf("bob should get the session back after new activity: %q", got)
	}
}

func TestSyncRestoresVanishedRemote(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	a.writeSession("proj", "s1", a.line("proj", "u1", "", time.Now(), "x"))
	a.sync()
	for _, k := range store.keys() {
		_ = store.Delete(context.Background(), k)
	}
	r := a.sync()
	if len(r.Restored) != 1 {
		t.Fatalf("expected the vanished file to be restored: %+v", r)
	}
}

func TestSyncIgnoresClockSkew(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	store.clockSkew = -3 * time.Hour // the bucket's clock is far behind every device
	a := newDevice(t, root, "alice", store)
	b := newDevice(t, root, "bob", store)
	t0 := time.Now()

	a.writeSession("proj", "s1", a.line("proj", "u1", "", t0, "one"))
	a.sync()
	b.sync()
	a.appendSession("proj", "s1", a.line("proj", "u2", "u1", t0.Add(time.Minute), "two"))
	a.sync()
	if r := b.sync(); len(r.Downloaded) != 1 {
		t.Fatalf("a skewed bucket clock hid an update: %+v", r)
	}
}

func TestSyncDefersLiveSession(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	b := newDevice(t, root, "bob", store)
	t0 := time.Now()

	a.writeSession("proj", "s1", a.line("proj", "u1", "", t0, "one"))
	a.sync()
	b.sync()

	// s1 is open in a running Claude process on bob.
	if err := os.MkdirAll(filepath.Join(b.claude, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	rec := fmt.Sprintf(`{"pid":%d,"sessionId":"s1"}`, os.Getpid())
	if err := os.WriteFile(filepath.Join(b.claude, "sessions", "1.json"), []byte(rec), 0600); err != nil {
		t.Fatal(err)
	}

	a.appendSession("proj", "s1", a.line("proj", "u2", "u1", t0.Add(time.Minute), "remote turn"))
	a.sync()
	before := b.readSession("proj", "s1")
	r := b.sync()
	if len(r.Deferred) != 1 || b.readSession("proj", "s1") != before {
		t.Fatalf("live transcript must not be rewritten: %+v", r)
	}

	// Once the process exits, the update lands.
	_ = os.Remove(filepath.Join(b.claude, "sessions", "1.json"))
	if r := b.sync(); len(r.Downloaded) != 1 {
		t.Fatalf("expected the deferred update after exit: %+v", r)
	}
}

func desktopRecord(sessionID, cliID, cwd string, activity int64, title string) string {
	return fmt.Sprintf(`{"sessionId":%q,"cliSessionId":%q,"cwd":%q,"originCwd":%q,"lastActivityAt":%d,"title":%q,"isArchived":false}`,
		sessionID, cliID, cwd, cwd, activity, title)
}

func (d *device) recordPath(id string) string {
	return filepath.Join(d.desktop, testAcct, testOrg, "local_"+id+".json")
}

func TestSyncDesktopRecords(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	b := newDevice(t, root, "bob", store)

	// Alice's project is a git clone in ~/code; Bob keeps the same repo in ~/Projects.
	aRepo := filepath.Join(a.home, "code", "webapp")
	bRepo := filepath.Join(b.home, "Projects", "webapp")
	for _, repo := range []string{aRepo, bRepo} {
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(aRepo, ".git", "config"), []byte("[remote \"origin\"]\n\turl = git@github.com:BreakPoint/webapp.git\n"), 0600)
	_ = os.WriteFile(filepath.Join(bRepo, ".git", "config"), []byte("[remote \"origin\"]\n\turl = https://github.com/breakpoint/webapp\n"), 0600)

	scratch := filepath.Join(a.home, "Library", "Application Support", "Claude", "scratch-workspaces", "x", "y", "scratch-1")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(a.recordPath("web"), []byte(desktopRecord("local_web", "cli-web", aRepo, 1000, "Web app work")), 0600)
	_ = os.WriteFile(a.recordPath("scr"), []byte(desktopRecord("local_scr", "cli-scr", scratch, 1000, "Scratch")), 0600)
	// App-private files must never leave the device.
	_ = os.WriteFile(filepath.Join(a.desktop, testAcct, testOrg, "scheduled-tasks.json"), []byte(`{}`), 0600)
	_ = os.WriteFile(filepath.Join(a.desktop, testAcct, testOrg, "deleted_local_old"), []byte(``), 0600)

	a.sync()
	for _, k := range store.keys() {
		if strings.Contains(k, "scheduled-tasks") || strings.Contains(k, "deleted_") {
			t.Fatalf("app-private file uploaded: %s", k)
		}
	}
	rb := b.sync()
	if len(rb.NewDesktopSessions) != 2 {
		t.Fatalf("bob should report 2 new sidebar sessions: %+v", rb)
	}

	web, _ := os.ReadFile(b.recordPath("web"))
	if !bytes.Contains(web, []byte(`"cwd":"`+bRepo+`"`)) || bytes.Contains(web, []byte(desktopHintKey)) {
		t.Fatalf("web record not remapped to bob's clone: %s", web)
	}
	scr, _ := os.ReadFile(b.recordPath("scr"))
	bScratch := filepath.Join(b.home, "Library", "Application Support", "Claude", "scratch-workspaces", "x", "y", "scratch-1")
	if !bytes.Contains(scr, []byte(bScratch)) || !dirExists(bScratch) {
		t.Fatalf("scratch record should point at a created folder %s: %s", bScratch, scr)
	}

	// Bob renames the session and is more recent; Alice's app rewrites hers with older activity.
	_ = os.WriteFile(b.recordPath("web"), []byte(desktopRecord("local_web", "cli-web", bRepo, 5000, "Renamed on Bob")), 0600)
	_ = os.WriteFile(a.recordPath("web"), []byte(desktopRecord("local_web", "cli-web", aRepo, 2000, "Stale on Alice")), 0600)
	b.sync()
	a.sync()
	b.sync()
	for _, d := range []*device{a, b} {
		got, _ := os.ReadFile(d.recordPath("web"))
		if !bytes.Contains(got, []byte("Renamed on Bob")) {
			t.Fatalf("%s should keep the most recently active record: %s", d.name, got)
		}
	}
	got, _ := os.ReadFile(a.recordPath("web"))
	if !bytes.Contains(got, []byte(`"cwd":"`+aRepo+`"`)) {
		t.Fatalf("alice's record must point at alice's clone: %s", got)
	}
}

func TestSyncKeepsDesktopReleaseMarkersLocal(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	a.writeSession("proj", "s1", a.line("proj", "u1", "", time.Now(), "x"))
	marker := filepath.Join(a.projectDir("proj"), "s1.desktop-released.json")
	if err := os.WriteFile(marker, []byte(`{"reason":"delete"}`), 0600); err != nil {
		t.Fatal(err)
	}
	a.sync()
	for _, k := range store.keys() {
		if strings.Contains(k, "desktop-released") {
			t.Fatalf("release marker uploaded: %s", k)
		}
	}
}

func TestMergeJSONL(t *testing.T) {
	l := func(uuid string, min int) string {
		return fmt.Sprintf(`{"uuid":%q,"timestamp":"2026-10-25T10:%02d:00Z"}`, uuid, min)
	}
	join := func(ls ...string) []byte { return []byte(strings.Join(ls, "\n") + "\n") }

	base := []string{l("1", 0), l("2", 1)}
	local := join(append(base, l("a", 3), l("a2", 7))...)
	remote := join(append(base, l("b", 5))...)

	merged, changed := MergeJSONL(local, remote)
	if !changed {
		t.Fatal("expected a change")
	}
	want := join(l("1", 0), l("2", 1), l("a", 3), l("b", 5), l("a2", 7))
	if !bytes.Equal(merged, want) {
		t.Fatalf("merged =\n%s\nwant\n%s", merged, want)
	}

	if _, changed := MergeJSONL(local, join(base...)); changed {
		t.Fatal("a remote subset must not change local")
	}
	// Lines without a uuid (summaries, history entries) dedupe by content.
	h1 := `{"display":"ls","timestamp":1761386400000}`
	h2 := `{"display":"pwd","timestamp":1761386500000}`
	got, _ := MergeJSONL(join(h1), join(h1, h2))
	if !bytes.Equal(got, join(h1, h2)) {
		t.Fatalf("history merge = %s", got)
	}
}

func TestCanonicalRemote(t *testing.T) {
	for _, u := range []string{
		"git@github.com:BreakPoint/webapp.git",
		"https://github.com/breakpoint/webapp",
		"https://user:tok@github.com/BreakPoint/webapp.git/",
		"ssh://git@github.com:22/BreakPoint/webapp.git",
	} {
		if got := canonicalRemote(u); got != "github.com/breakpoint/webapp" {
			t.Errorf("canonicalRemote(%q) = %q", u, got)
		}
	}
}

func TestSyncFoldsMovedSession(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	b := newDevice(t, root, "bob", store)
	t0 := time.Now()

	// The session starts in a scratch folder and syncs to bob.
	a.writeSession("scratch", "s1", a.line("scratch", "u1", "", t0, "hello"))
	a.sync()
	b.sync()

	// Claude Code on alice moves it into the repo folder, with a relocated row.
	oldPath := filepath.Join(a.projectDir("scratch"), "s1.jsonl")
	data, _ := os.ReadFile(oldPath)
	repo := filepath.Join(a.home, "code", "repo")
	reloc := fmt.Sprintf(`{"type":"relocated","sessionId":"s1","relocatedCwd":%q}`+"\n", repo)
	moved := string(data) + a.line("repo", "u2", "u1", t0.Add(time.Minute), "after move") + reloc
	_ = os.Remove(oldPath)
	a.writeSession("repo", "s1", moved)
	a.sync()

	// Bob also continued the old copy before hearing about the move.
	b.appendSession("scratch", "s1", b.line("scratch", "ub", "u1", t0.Add(30*time.Second), "bob on old copy"))
	r := b.sync()
	b.sync()

	if _, err := os.Stat(filepath.Join(b.projectDir("scratch"), "s1.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("stale copy should be folded away on bob (report %+v)", r)
	}
	got := b.readSession("repo", "s1")
	if !strings.Contains(got, "after move") || !strings.Contains(got, "bob on old copy") {
		t.Fatalf("bob's current copy must hold every turn: %q", got)
	}
	// Alice ends up with bob's turn too, in the repo folder only.
	a.sync()
	if got := a.readSession("repo", "s1"); !strings.Contains(got, "bob on old copy") {
		t.Fatalf("alice missing bob's turn: %q", got)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("the old copy came back on alice")
	}
}

func TestSyncSkipsDeviceLocalFiles(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	a.writeSession("proj", "s1", a.line("proj", "u1", "", time.Now(), "x"))
	dir := a.projectDir("proj")
	for _, name := range []string{".stradl-123.tmp", "s1.jsonl.compact.tmp.9", "s1.lock", "s1.orphaned-1-ab.jsonl", "s1.jsonl.superseded-5"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a.sync()
	keys := store.keys()
	for _, k := range keys {
		if !strings.HasSuffix(k, "s1.jsonl.age") && !strings.HasPrefix(k, "_metadata/") {
			t.Fatalf("device-local file uploaded: %s (all: %v)", k, keys)
		}
	}
}

func TestLiveSessionIgnoresReusedPID(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, startedAt time.Time) {
		rec := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"startedAt":%d}`, os.Getpid(), name, startedAt.UnixMilli())
		_ = os.WriteFile(filepath.Join(dir, "sessions", name+".json"), []byte(rec), 0600)
	}
	write("fresh", time.Now())
	write("stale", time.Now().Add(-6*time.Hour)) // this PID's process started long after
	live := liveSessionIDs(dir)
	if !live["fresh"] {
		t.Fatal("a running session must count as live")
	}
	if runtime.GOOS == "darwin" && live["stale"] {
		t.Fatal("a record whose PID was reused must not count as live")
	}
}

func TestClampMtimeKeepsPulledSessionsInsideRetention(t *testing.T) {
	root := t.TempDir()
	d := newDevice(t, root, "alice", newCASStore())
	old := time.Now().Add(-90 * 24 * time.Hour)
	got := d.s.clampMtime("projects/x/s.jsonl", old)
	if got.Before(time.Now().Add(-29 * 24 * time.Hour)) {
		t.Fatalf("90-day-old mtime not clamped under the default 30-day retention: %v", got)
	}
	_ = os.WriteFile(filepath.Join(d.claude, "settings.json"), []byte(`{"cleanupPeriodDays":365}`), 0600)
	if got := d.s.clampMtime("projects/x/s.jsonl", old); !got.Equal(old) {
		t.Fatalf("with 365-day retention the real mtime should be kept: %v", got)
	}
	if got := d.s.clampMtime("plans/p.md", old); !got.Equal(old) {
		t.Fatal("only transcripts are clamped")
	}
}

func TestDesktopRecordDropsRuntimeFields(t *testing.T) {
	in := []byte(`{"sessionId":"local_x","cwd":"/nope/does-not-exist","bridgeSessionIds":["b1"],"remoteControlAutoEligible":true,"toolSurfaceSnapshot":{"a":1},"title":"keep me","lastActivityAt":1792963248589}`)
	out := finishDesktopDownload(in, "", nil, nil)
	for _, k := range []string{"bridgeSessionIds", "remoteControlAutoEligible", "toolSurfaceSnapshot"} {
		if bytes.Contains(out, []byte(k)) {
			t.Fatalf("%s survived: %s", k, out)
		}
	}
	if !bytes.Contains(out, []byte(`"title":"keep me"`)) || !bytes.Contains(out, []byte(`1792963248589`)) {
		t.Fatalf("real fields or exact timestamps lost: %s", out)
	}
}

func TestMergeJSONLMetadataRowsDontCollapseInterleave(t *testing.T) {
	l := func(uuid string, min int) string {
		return fmt.Sprintf(`{"uuid":%q,"timestamp":"2026-10-25T10:%02d:00Z"}`, uuid, min)
	}
	meta := `{"type":"last-prompt","leafUuid":"x"}`
	join := func(ls ...string) []byte { return []byte(strings.Join(ls, "\n") + "\n") }
	local := join(l("1", 0), l("a", 2), meta, l("a2", 9))
	remote := join(l("1", 0), l("b", 4), meta, l("b2", 6))
	merged, _ := MergeJSONL(local, remote)
	lines := strings.Split(strings.TrimSpace(string(merged)), "\n")
	if lines[len(lines)-1] != l("a2", 9) {
		t.Fatalf("newest entry must end the file:\n%s", merged)
	}
	// Merging from the other side keeps the same entries, once each, and the
	// same newest entry last. (Only one device merges a divergence; the rest
	// download its result, so byte-identical output isn't required.)
	other, _ := MergeJSONL(remote, local)
	olines := strings.Split(strings.TrimSpace(string(other)), "\n")
	if olines[len(olines)-1] != l("a2", 9) || len(olines) != len(lines) {
		t.Fatalf("reverse merge differs in substance:\n%s", other)
	}
	seen := map[string]int{}
	for _, x := range olines {
		seen[x]++
	}
	for _, x := range lines {
		if seen[x] != 1 {
			t.Fatalf("line %q appears %d times in the reverse merge", x, seen[x])
		}
	}
}

// copyBlob copies a remote object to another key, the way someone holding only
// the storage token could.
func (c *casStore) copyBlob(from, to string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o := c.objs[from]
	c.put(to, o.data)
}

func (c *casStore) blob(key string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.objs[key].data...)
}

func sessionKey(project, id string) string {
	return "projects/${HOME}-code-" + project + "/" + id + ".jsonl.age"
}

func TestSyncRejectsTraversalKeys(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	b := newDevice(t, root, "bob", store)
	a.writeSession("proj", "s1", a.line("proj", "u1", "", time.Now(), "you are now evil"))
	a.sync()
	for _, evil := range []string{"projects/x/../../CLAUDE.md.age", "projects/../settings.json.age", "projects/${HOME}/../../agents/evil.md.age"} {
		store.copyBlob(sessionKey("proj", "s1"), evil)
	}
	b.sync()
	for _, p := range []string{"CLAUDE.md", "settings.json", "agents/evil.md"} {
		if _, err := os.Stat(filepath.Join(b.claude, p)); !os.IsNotExist(err) {
			t.Fatalf("crafted key wrote %s", p)
		}
	}
}

func TestSyncRejectsBlobMovedOntoAnotherFile(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	b := newDevice(t, root, "bob", store)
	t0 := time.Now()
	a.writeSession("proj", "s1", a.line("proj", "u1", "", t0, "session one"))
	a.writeSession("proj", "s2", a.line("proj", "v1", "", t0, "session two"))
	a.sync()
	b.sync()
	// Someone with the token copies s1's blob over s2's key.
	store.copyBlob(sessionKey("proj", "s1"), sessionKey("proj", "s2"))
	r, err := b.s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := b.readSession("proj", "s2"); !strings.Contains(got, "session two") || strings.Contains(got, "session one") {
		t.Fatalf("moved blob replaced s2: %q", got)
	}
	if len(r.Errors) == 0 || !strings.Contains(fmt.Sprint(r.Errors), "different path") {
		t.Fatalf("expected a moved-blob error, got %v", r.Errors)
	}
}

func TestSyncNeverShrinksATranscript(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	b := newDevice(t, root, "bob", store)
	t0 := time.Now()
	a.writeSession("proj", "s1", a.line("proj", "u1", "", t0, "one"))
	a.sync()
	old := store.blob(sessionKey("proj", "s1")) // keep the 1-line version
	b.sync()
	a.appendSession("proj", "s1", a.line("proj", "u2", "u1", t0.Add(time.Minute), "two"))
	a.sync()
	b.sync()
	// Replay the old, shorter ciphertext.
	store.mu.Lock()
	store.put(sessionKey("proj", "s1"), old)
	store.mu.Unlock()
	a.sync()
	b.sync()
	for _, d := range []*device{a, b} {
		if got := d.readSession("proj", "s1"); !strings.Contains(got, `"two"`) {
			t.Fatalf("%s lost a turn to a replayed blob: %q", d.name, got)
		}
	}
}

func TestSyncDesktopStaleRewriteDoesNotRevertNewerRecord(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	b := newDevice(t, root, "bob", store)
	cwdA := filepath.Join(a.home, "code", "w")
	cwdB := filepath.Join(b.home, "code", "w")
	_ = os.MkdirAll(cwdA, 0700)
	_ = os.MkdirAll(cwdB, 0700)
	_ = os.WriteFile(a.recordPath("w"), []byte(desktopRecord("local_w", "cli-w", cwdA, 1000, "Original")), 0600)
	a.sync()
	b.sync()
	_ = os.WriteFile(b.recordPath("w"), []byte(desktopRecord("local_w", "cli-w", cwdB, 9000, "Renamed on Bob")), 0600)
	b.sync()
	a.sync() // alice downloads bob's newer record
	// Alice's running app still holds the old record and saves it back.
	_ = os.WriteFile(a.recordPath("w"), []byte(desktopRecord("local_w", "cli-w", cwdA, 1000, "Original")), 0600)
	a.sync()
	b.sync()
	for _, d := range []*device{a, b} {
		if got, _ := os.ReadFile(d.recordPath("w")); !bytes.Contains(got, []byte("Renamed on Bob")) {
			t.Fatalf("%s reverted to the stale record: %s", d.name, got)
		}
	}
}

func TestSyncStopsOnWrongKey(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	a.writeSession("proj", "s1", a.line("proj", "u1", "", time.Now(), "x"))
	a.sync()

	b := newDevice(t, root, "bob", store)
	keyPath := filepath.Join(b.home, ".claude-sync", "other-key.txt")
	if err := crypto.GenerateKeyFromPassphrase(keyPath, "a different passphrase"); err != nil {
		t.Fatal(err)
	}
	wrong, err := crypto.NewEncryptor(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	b.s.encryptor = wrong
	b.writeSession("proj", "s9", b.line("proj", "z1", "", time.Now(), "never upload me"))
	before := len(store.keys())
	if _, err := b.s.Sync(context.Background()); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("wrong key must stop the sync, got %v", err)
	}
	if len(store.keys()) != before {
		t.Fatal("a device with the wrong key uploaded files")
	}
}

func TestAdoptStorageDropsStateFromAnotherContainer(t *testing.T) {
	st := NewState()
	st.adoptStorage("azure|https://a.blob.core.windows.net|stradl")
	st.Files["projects/x/s.jsonl"] = &FileState{Hash: "h", RemoteETag: "e1"}
	if st.adoptStorage("azure|https://a.blob.core.windows.net|stradl") {
		t.Fatal("same target must keep state")
	}
	if !st.adoptStorage("azure|https://b.blob.core.windows.net|stradl") || len(st.Files) != 0 {
		t.Fatal("a new target must drop file states")
	}
}

func TestSyncCaseVariantFoldersDontOverwrite(t *testing.T) {
	probe := filepath.Join(t.TempDir(), "Abc")
	_ = os.Mkdir(probe, 0700)
	if _, err := os.Stat(strings.ToLower(probe)); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	b := newDevice(t, root, "bob", store)
	t0 := time.Now()
	// Alice keeps the project in ~/Code/x, Bob in ~/code/x (already has the folder).
	aDir := filepath.Join(a.claude, "projects", EncodeClaudePath(filepath.Join(a.home, "Code", "x")))
	bDir := filepath.Join(b.claude, "projects", EncodeClaudePath(filepath.Join(b.home, "code", "x")))
	_ = os.MkdirAll(aDir, 0700)
	_ = os.MkdirAll(bDir, 0700)
	line := func(home, folder, uuid, parent string, at time.Time, text string) string {
		return fmt.Sprintf(`{"uuid":%q,"parentUuid":%q,"cwd":%q,"timestamp":%q,"text":%q}`+"\n", uuid, parent, filepath.Join(home, folder, "x"), at.UTC().Format(time.RFC3339Nano), text)
	}
	_ = os.WriteFile(filepath.Join(aDir, "s1.jsonl"), []byte(line(a.home, "Code", "u1", "", t0, "start")), 0600)
	_ = os.WriteFile(filepath.Join(bDir, "other.jsonl"), []byte(line(b.home, "code", "o1", "", t0, "bob's own")), 0600)
	for i := 0; i < 3; i++ {
		a.sync()
		b.sync()
	}
	appendTo := func(p, s string) {
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
		_, _ = f.WriteString(s)
		_ = f.Close()
	}
	for turn := 2; turn <= 5; turn++ {
		appendTo(filepath.Join(aDir, "s1.jsonl"), line(a.home, "Code", fmt.Sprintf("u%d", turn), fmt.Sprintf("u%d", turn-1), t0.Add(time.Duration(turn)*time.Minute), fmt.Sprintf("alice turn %d", turn)))
		a.sync()
		b.sync()
	}
	a.sync()
	got, _ := os.ReadFile(filepath.Join(aDir, "s1.jsonl"))
	for turn := 2; turn <= 5; turn++ {
		if !strings.Contains(string(got), fmt.Sprintf("alice turn %d", turn)) {
			t.Fatalf("alice lost turn %d:\n%s", turn, got)
		}
	}
}

func TestStripURLCredentials(t *testing.T) {
	cases := map[string]string{
		"https://user:ghp_secret@github.com/o/r.git": "https://github.com/o/r.git",
		"https://github.com/o/r":                     "https://github.com/o/r",
		"git@github.com:o/r.git":                     "git@github.com:o/r.git",
		"ssh://git@github.com:22/o/r.git":            "ssh://github.com:22/o/r.git",
	}
	for in, want := range cases {
		if got := stripURLCredentials(in); got != want {
			t.Errorf("stripURLCredentials(%q) = %q, want %q", in, got, want)
		}
	}
}
