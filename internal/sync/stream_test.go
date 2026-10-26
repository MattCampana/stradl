package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/tawanorg/claude-sync/internal/storage"
)

// streamStore adds storage.Streaming to casStore and records which path each
// transfer took.
type streamStore struct {
	*casStore
	mu         gosync.Mutex
	streamUp   map[string]int
	streamDown map[string]int
	memDown    map[string]int
}

var _ storage.Streaming = (*streamStore)(nil)

func newStreamStore() *streamStore {
	return &streamStore{casStore: newCASStore(), streamUp: map[string]int{}, streamDown: map[string]int{}, memDown: map[string]int{}}
}

func (s *streamStore) count(m map[string]int, key string) {
	s.mu.Lock()
	m[key]++
	s.mu.Unlock()
}

func (s *streamStore) counts(m map[string]int, key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return m[key]
}

func (s *streamStore) UploadStream(_ context.Context, key string, r io.Reader, ifMatch string) (string, error) {
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return "", err // nothing is committed, like Azure's block upload
	}
	s.count(s.streamUp, key)
	c := s.casStore
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, exists := c.objs[key]
	if (ifMatch == "" && exists) || (ifMatch != "" && (!exists || cur.etag != ifMatch)) {
		return "", fmt.Errorf("%s: %w", key, storage.ErrPreconditionFailed)
	}
	c.seq++
	etag := fmt.Sprintf(`"0x%04d"`, c.seq)
	c.objs[key] = casObj{data: buf.Bytes(), etag: etag, mod: time.Now()}
	return etag, nil
}

func (s *streamStore) DownloadStream(_ context.Context, key string) (io.ReadCloser, string, error) {
	c := s.casStore
	c.mu.Lock()
	o, ok := c.objs[key]
	c.mu.Unlock()
	if !ok {
		return nil, "", fmt.Errorf("%s: %w", key, storage.ErrNotFound)
	}
	s.count(s.streamDown, key)
	return io.NopCloser(bytes.NewReader(o.data)), o.etag, nil // stored slices are never modified in place
}

func (s *streamStore) DownloadWithETag(ctx context.Context, key string) ([]byte, string, error) {
	s.count(s.memDown, key)
	return s.casStore.DownloadWithETag(ctx, key)
}

func (s *streamStore) Download(ctx context.Context, key string) ([]byte, error) {
	d, _, err := s.DownloadWithETag(ctx, key)
	return d, err
}

// withStreamLimits lowers the streaming and in-memory merge thresholds for
// one test.
func withStreamLimits(t *testing.T, stream, merge int64) {
	t.Helper()
	oldStream, oldMerge := streamThreshold, mergeMemoryLimit
	streamThreshold, mergeMemoryLimit = stream, merge
	t.Cleanup(func() { streamThreshold, mergeMemoryLimit = oldStream, oldMerge })
}

func fileHash(t *testing.T, p string) string {
	t.Helper()
	h, err := HashFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// padLine is a transcript line long enough to push files over a lowered
// streaming threshold.
func (d *device) padLine(project, uuid, parent string, at time.Time) string {
	return d.line(project, uuid, parent, at, strings.Repeat("words ", 120))
}

func TestStreamingPathsRoundTrip(t *testing.T) {
	withStreamLimits(t, 1024, 64<<10)
	root := t.TempDir()
	store := newStreamStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "data")}
	}
	t0 := time.Now().Add(-time.Hour)

	// A transcript over the threshold whose cwd fields need mapping, and a
	// binary file in a project folder.
	var tr strings.Builder
	for i := 0; i < 4; i++ {
		tr.WriteString(a.padLine("app", fmt.Sprintf("u%d", i), "", t0.Add(time.Duration(i)*time.Minute)))
	}
	a.writeSession("app", "s1", tr.String())
	blob := make([]byte, 8<<10)
	_, _ = rand.NewChaCha8([32]byte{1}).Read(blob)
	a.write("code/data/blob.bin", string(blob))
	a.sync()
	trKey, binKey := sessionKey("app", "s1"), FoldersPrefix+"code/data/blob.bin.age"
	if store.counts(store.streamUp, trKey) != 1 || store.counts(store.streamUp, binKey) != 1 {
		t.Fatalf("large files should stream up: %v", store.streamUp)
	}
	b.sync()
	if store.counts(store.streamDown, binKey) != 1 || store.counts(store.memDown, binKey) != 0 {
		t.Fatalf("large files should stream down: stream %v, memory %v", store.streamDown, store.memDown)
	}
	got := b.readSession("app", "s1")
	if !strings.Contains(got, filepath.Join(b.home, "code", "app")) || strings.Contains(got, a.home) || strings.Contains(got, "${HOME}") {
		t.Fatalf("streamed transcript not mapped to bob's home")
	}
	if fileHash(t, filepath.Join(b.home, "code", "data", "blob.bin")) != fileHash(t, filepath.Join(a.home, "code", "data", "blob.bin")) {
		t.Fatal("streamed binary differs")
	}
	for _, d := range []*device{a, b} {
		if r := d.sync(); r.Changed() {
			t.Fatalf("%s: idle sync moved data: %+v", d.name, r)
		}
	}

	// Continued on bob: alice gets the longer transcript by stream.
	b.appendSession("app", "s1", b.padLine("app", "u9", "u3", t0.Add(30*time.Minute)))
	b.sync()
	if r := a.sync(); len(r.Downloaded) != 1 || strings.Count(a.readSession("app", "s1"), "\n") != 5 {
		t.Fatalf("alice should download the continued transcript: %+v", r)
	}

	// Both continue it: merged line by line (under the in-memory limit).
	a.appendSession("app", "s1", a.padLine("app", "ua", "u9", t0.Add(40*time.Minute)))
	b.appendSession("app", "s1", b.padLine("app", "ub", "u9", t0.Add(41*time.Minute)))
	a.sync()
	if r := b.sync(); len(r.Merged) != 1 || len(r.Conflicts) != 0 {
		t.Fatalf("bob should merge: %+v", r)
	}
	a.sync()
	for _, d := range []*device{a, b} {
		if n := strings.Count(d.readSession("app", "s1"), "\n"); n != 7 {
			t.Fatalf("%s has %d lines after the merge, want 7", d.name, n)
		}
	}

	// Both change the binary: the newer copy wins, the other is kept.
	v2, v3 := bytes.Repeat([]byte{2}, 4096), bytes.Repeat([]byte{3}, 4096)
	a.write("code/data/blob.bin", string(v2))
	b.write("code/data/blob.bin", string(v3))
	_ = os.Chtimes(filepath.Join(a.home, "code", "data", "blob.bin"), t0.Add(time.Minute), t0.Add(time.Minute))
	a.sync()
	if r := b.sync(); len(r.Conflicts) != 1 {
		t.Fatalf("bob's newer binary should win with a conflict copy: %+v", r)
	}
	a.sync()
	if a.read("code/data/blob.bin") != string(v3) {
		t.Fatal("alice should end with bob's newer binary")
	}
	copies, _ := filepath.Glob(filepath.Join(b.home, "code", "data", "blob.bin.conflict.*.other-mac"))
	if len(copies) != 1 {
		t.Fatalf("bob should keep alice's version as a conflict copy: %v", copies)
	}
	if data, _ := os.ReadFile(copies[0]); !bytes.Equal(data, v2) {
		t.Fatal("conflict copy holds the wrong version")
	}
}

func TestStreamingTranscriptAboveMergeLimitKeepsBothCopies(t *testing.T) {
	withStreamLimits(t, 1024, 2048)
	root := t.TempDir()
	store := newStreamStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	t0 := time.Now().Add(-time.Hour)
	var tr strings.Builder
	for i := 0; i < 4; i++ {
		tr.WriteString(a.padLine("app", fmt.Sprintf("u%d", i), "", t0.Add(time.Duration(i)*time.Minute)))
	}
	a.writeSession("app", "s1", tr.String())
	syncAll(1, a, b)

	a.appendSession("app", "s1", a.padLine("app", "ua", "u3", t0.Add(40*time.Minute)))
	b.appendSession("app", "s1", b.padLine("app", "ub", "u3", t0.Add(41*time.Minute)))
	a.sync()
	if r := b.sync(); len(r.Conflicts) != 1 {
		t.Fatalf("above the merge limit the newest copy wins with a conflict copy: %+v", r)
	}
	copies, _ := filepath.Glob(filepath.Join(b.projectDir("app"), "s1.jsonl.conflict.*"))
	if len(copies) != 1 {
		t.Fatalf("the losing transcript must be kept: %v", copies)
	}
	// Neither turn is lost: one is in the transcript, the other in the copy.
	kept, _ := os.ReadFile(copies[0])
	both := b.readSession("app", "s1") + string(kept)
	if !strings.Contains(both, `"ua"`) || !strings.Contains(both, `"ub"`) {
		t.Fatal("a turn was lost")
	}
}

func TestStreamingRejectsBlobMovedOntoAnotherFile(t *testing.T) {
	withStreamLimits(t, 1024, 64<<10)
	root := t.TempDir()
	store := newStreamStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "data")}
	}
	a.write("code/data/one.bin", strings.Repeat("1", 4096))
	a.write("code/data/two.bin", strings.Repeat("2", 4096))
	syncAll(1, a, b)
	store.copyBlob(FoldersPrefix+"code/data/one.bin.age", FoldersPrefix+"code/data/two.bin.age")
	r, err := b.s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b.read("code/data/two.bin") != strings.Repeat("2", 4096) {
		t.Fatal("a moved blob replaced two.bin")
	}
	if len(r.Errors) == 0 || !strings.Contains(fmt.Sprint(r.Errors), "different path") {
		t.Fatalf("expected a moved-blob error, got %v", r.Errors)
	}
}

func TestStreaming200MBRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 200 MB")
	}
	root := t.TempDir()
	store := newStreamStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "big")}
	}
	src := filepath.Join(a.home, "code", "big", "data.bin")
	if err := os.MkdirAll(filepath.Dir(src), 0755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.NewChaCha8([32]byte{42}) // seeded, fast, reproducible
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for i := 0; i < 200; i++ {
		_, _ = rng.Read(buf)
		h.Write(buf)
		if _, err := f.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	want := hex.EncodeToString(h.Sum(nil))

	a.sync()
	b.sync()
	key := FoldersPrefix + "code/big/data.bin.age"
	if store.counts(store.streamUp, key) != 1 || store.counts(store.streamDown, key) != 1 || store.counts(store.memDown, key) != 0 {
		t.Fatalf("the 200 MB file must stream both ways: up %d, down %d, in memory %d",
			store.counts(store.streamUp, key), store.counts(store.streamDown, key), store.counts(store.memDown, key))
	}
	if got := fileHash(t, filepath.Join(b.home, "code", "big", "data.bin")); got != want {
		t.Fatalf("bob's copy differs: %s != %s", got, want)
	}
	if st := a.s.state.GetFile(strings.TrimSuffix(key, ".age")); st == nil || st.Hash != want {
		t.Fatal("alice's state should record the hash computed while streaming")
	}
	if r := b.sync(); r.Changed() {
		t.Fatalf("idle sync moved data: %+v", r)
	}
}

// azuriteStore returns a real Azure client on a fresh Azurite container, or
// skips the test when no emulator is listening.
func azuriteStore(t *testing.T) storage.Storage {
	t.Helper()
	if testing.Short() {
		t.Skip("Azurite tests skipped in -short mode")
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:10000", 500*time.Millisecond)
	if err != nil {
		t.Skip("Azurite not running on 127.0.0.1:10000")
	}
	_ = conn.Close()
	const account, key = "devstoreaccount1", "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	endpoint := "http://127.0.0.1:10000/" + account
	name := fmt.Sprintf("engine%d", time.Now().UnixNano())
	cred, err := container.NewSharedKeyCredential(account, key)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := container.NewClientWithSharedKeyCredential(endpoint+"/"+name, cred, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Create(context.Background(), nil); err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Delete(context.Background(), nil) })
	store, err := storage.New(&storage.StorageConfig{Provider: storage.ProviderAzure, Bucket: name, Endpoint: endpoint, AzureAccount: account, AzureAccountKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestEverythingAgainstAzurite runs the whole path against the real Azure
// client: a file above the default streaming threshold, settings mapped across
// homes, and a mirrored delete with Azure's conditional delete.
func TestEverythingAgainstAzurite(t *testing.T) {
	store := azuriteStore(t)
	if _, ok := store.(storage.Streaming); !ok {
		t.Fatal("the Azure client should stream")
	}
	root := t.TempDir()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "app")}
	}
	big := make([]byte, streamThreshold+5<<20)
	_, _ = rand.NewChaCha8([32]byte{9}).Read(big)
	a.write("code/app/assets/video.bin", string(big))
	a.write("code/app/main.go", "package main\n")
	a.write("code/app/old.go", "package main // old\n")
	a.write(".claude/settings.json", fmt.Sprintf(`{"statusLine":{"command":"%s/.claude/status.sh"}}`+"\n", a.home))
	a.sync()
	b.sync()
	if fileHash(t, filepath.Join(b.home, "code", "app", "assets", "video.bin")) != fileHash(t, filepath.Join(a.home, "code", "app", "assets", "video.bin")) {
		t.Fatal("the streamed file differs on bob")
	}
	if got := b.read(".claude/settings.json"); !strings.Contains(got, b.home+"/.claude/status.sh") {
		t.Fatalf("settings not mapped: %s", got)
	}

	a.remove("code/app/old.go")
	if r := a.sync(); len(r.Deleted) != 1 {
		t.Fatalf("alice should delete old.go everywhere: %+v", r)
	}
	if r := b.sync(); len(r.Trashed) != 1 || b.exists("code/app/old.go") {
		t.Fatalf("bob should move old.go to the trash: %+v", r)
	}
	for _, d := range []*device{a, b} {
		if r := d.sync(); r.Changed() {
			t.Fatalf("%s: idle sync moved data: %+v", d.name, r)
		}
	}
}
