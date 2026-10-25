package sync

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func mustLocalFP(t *testing.T, d *device) string {
	t.Helper()
	fp, err := LocalFingerprint(d.s.cfg)
	if err != nil {
		t.Fatalf("LocalFingerprint: %v", err)
	}
	// The method form must agree with the config-only form.
	if m, err := d.s.LocalFingerprint(); err != nil || m != fp {
		t.Fatalf("method fingerprint %q (err %v) != package fingerprint %q", m, err, fp)
	}
	return fp
}

func mustRemoteFP(t *testing.T, d *device) string {
	t.Helper()
	fp, err := d.s.RemoteFingerprint(context.Background())
	if err != nil {
		t.Fatalf("RemoteFingerprint: %v", err)
	}
	return fp
}

func TestLocalFingerprintTracksSyncedFiles(t *testing.T) {
	d := newDevice(t, t.TempDir(), "alice", newCASStore())
	now := time.Now()
	d.writeSession("app", "s1", d.line("app", "u1", "", now, "hello"))

	base := mustLocalFP(t, d)
	if again := mustLocalFP(t, d); again != base {
		t.Fatal("fingerprint changed with no file changes")
	}

	// Files Sync ignores must not move the fingerprint: out of the sessions
	// scope, conflict copies, and desktop app files that aren't records.
	if err := os.WriteFile(filepath.Join(d.claude, "settings.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	conflict := filepath.Join(d.projectDir("app"), "s1.jsonl.conflict.20260101-000000")
	if err := os.WriteFile(conflict, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.desktop, "index.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := mustLocalFP(t, d); got != base {
		t.Fatal("fingerprint changed for files outside the sync scope")
	}

	// Appending to a transcript changes size and mtime.
	d.appendSession("app", "s1", d.line("app", "u2", "u1", now, "more"))
	appended := mustLocalFP(t, d)
	if appended == base {
		t.Fatal("fingerprint unchanged after appending to a transcript")
	}

	// Same size, new mtime still counts.
	p := filepath.Join(d.projectDir("app"), "s1.jsonl")
	later := now.Add(time.Hour)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	touched := mustLocalFP(t, d)
	if touched == appended {
		t.Fatal("fingerprint unchanged after mtime change")
	}

	// A new desktop sidebar record is in scope.
	rec := d.recordPath("abc123")
	if err := os.WriteFile(rec, []byte(desktopRecord("local_abc123", "s1", d.home, 1, "t")), 0600); err != nil {
		t.Fatal(err)
	}
	withRecord := mustLocalFP(t, d)
	if withRecord == touched {
		t.Fatal("fingerprint unchanged after a new desktop record")
	}

	// Deleting a file changes it back.
	if err := os.Remove(rec); err != nil {
		t.Fatal(err)
	}
	if got := mustLocalFP(t, d); got != touched {
		t.Fatal("fingerprint did not return to its earlier value after deleting the new file")
	}
}

func TestLocalFingerprintTracksLiveSessions(t *testing.T) {
	d := newDevice(t, t.TempDir(), "alice", newCASStore())
	d.writeSession("app", "s1", d.line("app", "u1", "", time.Now(), "hello"))
	base := mustLocalFP(t, d)

	sessions := filepath.Join(d.claude, "sessions")
	if err := os.MkdirAll(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	rec := filepath.Join(sessions, "1.json")
	live := []byte(`{"pid":` + strconv.Itoa(os.Getpid()) + `,"sessionId":"s1"}`)
	if err := os.WriteFile(rec, live, 0600); err != nil {
		t.Fatal(err)
	}
	if got := mustLocalFP(t, d); got == base {
		t.Fatal("fingerprint unchanged when a session went live")
	}
	if err := os.Remove(rec); err != nil {
		t.Fatal(err)
	}
	if got := mustLocalFP(t, d); got != base {
		t.Fatal("fingerprint did not return when the session ended")
	}
}

func TestRemoteFingerprintTracksRemoteChanges(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	alice := newDevice(t, root, "alice", store)
	bob := newDevice(t, root, "bob", store)
	now := time.Now()

	empty := mustRemoteFP(t, bob)
	alice.writeSession("app", "s1", alice.line("app", "u1", "", now, "hello"))
	alice.sync()

	afterUpload := mustRemoteFP(t, bob)
	if afterUpload == empty {
		t.Fatal("remote fingerprint unchanged after another device uploaded")
	}
	if again := mustRemoteFP(t, bob); again != afterUpload {
		t.Fatal("remote fingerprint changed with no remote changes")
	}
	// Both devices see the same remote, whatever their home directory.
	if a := mustRemoteFP(t, alice); a != afterUpload {
		t.Fatal("devices disagree on the remote fingerprint")
	}

	// Reserved metadata and foreign objects don't count.
	_ = store.Upload(context.Background(), MtimeManifestKey, []byte("x"))
	_ = store.Upload(context.Background(), "_external/mcp-servers.json.age", []byte("x"))
	_ = store.Upload(context.Background(), "README.txt", []byte("x"))
	if got := mustRemoteFP(t, bob); got != afterUpload {
		t.Fatal("remote fingerprint changed for objects Sync ignores")
	}

	// Bob syncing (download only) leaves the remote untouched.
	bob.sync()
	if got := mustRemoteFP(t, bob); got != afterUpload {
		t.Fatal("remote fingerprint changed after a download-only sync")
	}

	// A continuation on another device rewrites the object's ETag.
	alice.appendSession("app", "s1", alice.line("app", "u2", "u1", now, "more"))
	alice.sync()
	if got := mustRemoteFP(t, bob); got == afterUpload {
		t.Fatal("remote fingerprint unchanged after another device updated a transcript")
	}
}
