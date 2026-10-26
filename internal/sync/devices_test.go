package sync

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
)

const (
	sessA = "11111111-1111-4111-8111-111111111111"
	sessB = "22222222-2222-4222-8222-222222222222"
	sessC = "33333333-3333-4333-8333-333333333333"
)

func TestDeviceIDIsStable(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{StateDirOverride: dir}
	id, err := DeviceID(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !deviceIDRe.MatchString(id) {
		t.Fatalf("device ID %q is not 12 hex characters", id)
	}
	info, err := os.Stat(filepath.Join(dir, deviceIDFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("device-id mode = %v, want 0600", info.Mode().Perm())
	}
	if again, _ := DeviceID(cfg); again != id {
		t.Errorf("second call = %q, want %q", again, id)
	}
	// A damaged file is replaced rather than published.
	if err := os.WriteFile(filepath.Join(dir, deviceIDFile), []byte("../../etc\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if fixed, _ := DeviceID(cfg); !deviceIDRe.MatchString(fixed) {
		t.Errorf("damaged ID replaced by %q", fixed)
	}
}

func TestDeviceNameOverride(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{StateDirOverride: dir}
	if DeviceName(cfg) == "" {
		t.Error("no device name")
	}
	if err := os.WriteFile(filepath.Join(dir, deviceNameFile), []byte("  Studio Mac \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := DeviceName(cfg); got != "Studio Mac" {
		t.Errorf("DeviceName = %q, want the device-name file", got)
	}
}

func TestChangedSessionID(t *testing.T) {
	cases := map[string]string{
		"projects/-w/" + sessA + ".jsonl":                 sessA,
		"projects/-w/short-id.jsonl":                      "short-id",
		"projects/-w/" + sessA + "/subagents/agent.jsonl": sessA,
		"projects/-w/memory/MEMORY.md":                    "",
		"history.jsonl":                                   "",
		DesktopPrefix + "a/b/local_x.json":                "",
	}
	for np, want := range cases {
		got, ok := changedSessionID(np)
		if got != want || ok != (want != "") {
			t.Errorf("changedSessionID(%q) = %q, %v; want %q", np, got, ok, want)
		}
	}
}

func TestRecordRecent(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 10, 25, 15, 0, 0, 0, loc)
	state := t.TempDir()
	claude := t.TempDir()
	cfg := &config.Config{StateDirOverride: state}

	touch := func(np string, mt time.Time) {
		p := filepath.Join(claude, filepath.FromSlash(np))
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	aPath := "projects/-w/" + sessA + ".jsonl"
	touch(aPath, now.Add(-time.Hour))
	touch("projects/-w/"+sessB+"/subagents/agent.jsonl", now.Add(-2*time.Hour))
	touch("projects/-w/memory/MEMORY.md", now.Add(-time.Hour))

	earlier := []RecentChange{
		{SessionID: sessA, At: now.Add(-3 * time.Hour)},      // same day: replaced by the newer change
		{SessionID: sessA, At: now.Add(-48 * time.Hour)},     // two days ago: kept
		{SessionID: sessC, At: now.Add(-8 * 24 * time.Hour)}, // older than a week: dropped
	}
	data, _ := json.Marshal(earlier)
	if err := os.WriteFile(filepath.Join(state, recentFile), data, 0600); err != nil {
		t.Fatal(err)
	}

	report := &SyncReport{
		Uploaded: []string{aPath, "projects/-w/memory/MEMORY.md", "history.jsonl"},
		Merged:   []string{"projects/-w/" + sessB + "/subagents/agent.jsonl"},
		// Downloads are other Macs' changes, not this one's.
		Downloaded: []string{"projects/-w/" + sessC + ".jsonl"},
	}
	got, err := recordRecent(cfg, claude, report, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []RecentChange{
		{SessionID: sessA, At: now.Add(-time.Hour)},
		{SessionID: sessB, At: now.Add(-2 * time.Hour)},
		{SessionID: sessA, At: now.Add(-48 * time.Hour)},
	}
	if len(got) != len(want) {
		t.Fatalf("recent = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].SessionID != want[i].SessionID || !got[i].At.Equal(want[i].At) {
			t.Errorf("recent[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if back := ReadRecent(cfg, now); len(back) != len(want) {
		t.Errorf("ReadRecent = %+v", back)
	}
	if info, err := os.Stat(filepath.Join(state, recentFile)); err != nil || info.Mode().Perm() != 0600 {
		t.Errorf("recent.json: %v, %v", info, err)
	}
}

func TestHeartbeatRoundTrip(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	mac1 := newDevice(t, root, "alice", store)
	mac2 := newDevice(t, root, "bob", store)
	for _, d := range []*device{mac1, mac2} {
		if err := os.WriteFile(filepath.Join(d.s.cfg.StateDirOverride, deviceNameFile), []byte(d.name+"'s Mac"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	now := time.Now()

	mac1.writeSession("app", sessA, mac1.line("app", "u1", "", now, "hello"))
	rep1, err := mac1.s.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mac1.s.AfterSync(ctx, rep1)

	if _, err := mac2.s.Sync(ctx); err != nil { // downloads alice's session
		t.Fatal(err)
	}
	mac2.writeSession("app", sessB, mac2.line("app", "u2", "", now, "from bob"))
	rep2, err := mac2.s.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mac2.s.AfterSync(ctx, rep2)

	// Unreadable heartbeats are skipped: garbage, one moved to another
	// device's key, and keys that aren't device IDs.
	id1, _ := DeviceID(mac1.s.cfg)
	_ = store.Upload(ctx, DevicesPrefix+"abcdefabcdef.json.age", []byte("garbage"))
	if blob, err := store.Download(ctx, DevicesPrefix+id1+".json.age"); err == nil {
		_ = store.Upload(ctx, DevicesPrefix+"0123456789ab.json.age", blob)
		_ = store.Upload(ctx, DevicesPrefix+"not-an-id.json.age", blob)
	}

	beats, err := mac1.s.Heartbeats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(beats) != 2 {
		t.Fatalf("heartbeats = %+v, want 2", beats)
	}
	byName := map[string]DeviceHeartbeat{}
	for _, hb := range beats {
		byName[hb.Name] = hb
	}
	a, b := byName["alice's Mac"], byName["bob's Mac"]
	if a.ID != id1 || a.Sessions != 1 || a.Uploaded != 1 || a.Version != AppVersion || a.OS == "" || a.LastSyncAt.IsZero() {
		t.Errorf("alice = %+v", a)
	}
	if len(a.Recent) != 1 || a.Recent[0].SessionID != sessA {
		t.Errorf("alice recent = %+v", a.Recent)
	}
	if b.Sessions != 2 || len(b.Recent) != 1 || b.Recent[0].SessionID != sessB {
		t.Errorf("bob = %+v (he downloaded alice's session but changed only his own)", b)
	}

	// Heartbeats never sync as files.
	prev, err := mac1.s.Preview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range append(append([]string{}, prev.Downloaded...), prev.Uploaded...) {
		if strings.HasPrefix(p, "_metadata") {
			t.Errorf("preview touches %s", p)
		}
	}
}

func TestScopeScan(t *testing.T) {
	root := t.TempDir()
	d := newDevice(t, root, "alice", newCASStore())
	ctx := context.Background()
	now := time.Now().Add(-time.Hour)
	d.writeSession("app", sessA, d.line("app", "u1", "", now, "hello"))
	d.writeSession("app", sessB, d.line("app", "u2", "", now, "two"))
	dir := d.projectDir("app")
	for _, name := range []string{sessA + ".jsonl.conflict.20261024-101010.other-mac", ".stradl-1.tmp", "x.lock"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	d.appendSession("app", sessB, d.line("app", "u3", "u2", now, "more"))

	sum, err := d.s.ScopeScan()
	if err != nil {
		t.Fatal(err)
	}
	npA := "projects/" + filepath.Base(dir) + "/" + sessA + ".jsonl"
	npB := "projects/" + filepath.Base(dir) + "/" + sessB + ".jsonl"
	if len(sum.Files) != 2 {
		t.Fatalf("files = %v, want the two transcripts", sum.Files)
	}
	if f := sum.Files[npA]; !f.Tracked || !f.Reconciled {
		t.Errorf("synced transcript: %+v", f)
	}
	if f := sum.Files[npB]; !f.Tracked || f.Reconciled {
		t.Errorf("transcript changed since its sync: %+v", f)
	}
	if len(sum.Conflicts) != 1 || !strings.HasSuffix(sum.Conflicts[0], ".other-mac") {
		t.Errorf("conflicts = %v", sum.Conflicts)
	}
	files, bytes, err := d.s.ScopeStats()
	if err != nil || files != 2 || bytes != sum.Bytes || bytes == 0 {
		t.Errorf("ScopeStats = %d, %d, %v", files, bytes, err)
	}

	// The same answer without a storage client.
	cfg := *d.s.cfg
	viaCfg, err := ScopeScanFor(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(viaCfg.Files) != 2 || viaCfg.Files[npA] != sum.Files[npA] {
		t.Errorf("ScopeScanFor = %+v", viaCfg.Files)
	}
}

func TestLiveSessionIDsExported(t *testing.T) {
	claude := t.TempDir()
	rec := `{"pid":` + itoaTest(os.Getpid()) + `,"sessionId":"` + sessA + `","startedAt":` + itoaTest(int(time.Now().UnixMilli())) + `}`
	if err := os.MkdirAll(filepath.Join(claude, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claude, "sessions", "1.json"), []byte(rec), 0600); err != nil {
		t.Fatal(err)
	}
	if live := LiveSessionIDs(claude); !live[sessA] || len(live) != 1 {
		t.Errorf("LiveSessionIDs = %v", live)
	}
}

func itoaTest(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
