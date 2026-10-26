package sync

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Findings from reviewing the repository split: how leftovers of older
// versions, stray .git folders, build output and wandering sessions are
// handled.

// oldVersionNestedRepo syncs ~/code/work with lib inside it as one folder,
// as Stradl before v2.3 did, then makes lib a git repository on both Macs.
func oldVersionNestedRepo(t *testing.T) (a, b *device, store *casStore) {
	needGit(t)
	root := t.TempDir()
	store = newCASStore()
	a = newEverythingDevice(t, root, "alice", store)
	b = newEverythingDevice(t, root, "matthew", store)
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "work")}
	}
	a.write("code/work/notes.md", "outer\n")
	for _, f := range []string{"f0", "f1", "f2"} {
		a.write("code/work/lib/"+f+".go", "package lib\n")
	}
	syncAll(2, a, b)
	for _, d := range []*device{a, b} {
		git(t, filepath.Join(d.home, "code", "work", "lib"), "init", "-q")
	}
	if _, err := a.s.put(context.Background(), FoldersPrefix+"code/work/lib/.git/HEAD", []byte("ref: refs/heads/main\n"), ""); err != nil {
		t.Fatal(err)
	}
	return a, b, store
}

func TestARepoDeletedWhileUnneededArrivesOnceNeeded(t *testing.T) {
	a, b, store := oldVersionNestedRepo(t)
	if err := os.RemoveAll(filepath.Join(b.home, "code", "work", "lib")); err != nil {
		t.Fatal(err)
	}
	if r := b.sync(); len(r.Deleted) != 0 {
		t.Fatalf("deleting an unneeded repo must stay local: %v", r.Deleted)
	}
	a.s.cfg.ExtraFolders = append(a.s.cfg.ExtraFolders, filepath.Join(a.home, "code", "work", "lib"))
	a.write("code/work/lib/f0.go", "package lib // v2\n")
	a.sync()
	r := b.sync()
	b.sync()
	if b.read("code/work/lib/f0.go") != "package lib // v2\n" {
		t.Fatalf("bob never gets the repo alice's session needs; warnings: %q", r.Warnings)
	}
	a.sync()
	if !a.exists("code/work/lib/f1.go") || !keysHave(store, FoldersPrefix+"code/work/lib/f1.go") || len(a.trashed()) != 0 {
		t.Fatalf("alice's repo lost files: trashed %v", a.trashed())
	}
}

func TestLeftoverGitKeysDontHideAPlainFolder(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.s.cfg.ExtraFolders = []string{filepath.Join(a.home, "code", "work")}
	// x was a repository once (an older Mac uploaded its git data); here
	// it's a plain folder.
	ctx := context.Background()
	for np, data := range map[string]string{
		FoldersPrefix + "code/work/x/.git/HEAD":            "ref: refs/heads/main\n",
		FoldersPrefix + "code/work/x/.git/objects/ab/cdef": "blob",
		FoldersPrefix + "code/work/.stradl-folder":         "{}\n",
	} {
		if _, err := a.s.put(ctx, np, []byte(data), ""); err != nil {
			t.Fatal(err)
		}
	}
	a.write("code/work/x/doc.md", "plain\n")
	// y has a stray .git with no HEAD.
	a.write("code/work/y/.git/refs/heads/old", "0123\n")
	a.write("code/work/y/doc.md", "plain too\n")
	a.sync()
	for _, k := range []string{"code/work/x/doc.md", "code/work/y/doc.md"} {
		if !keysHave(store, FoldersPrefix+k) {
			t.Fatalf("%s should sync with its folder", k)
		}
	}
	if a.exists("code/work/x/.git") {
		t.Fatal("leftover git data was downloaded into a plain folder")
	}
	if keysHave(store, FoldersPrefix+"code/work/y/.git/refs/heads/old") {
		t.Fatal("a stray .git synced")
	}
}

func TestSessionsWanderingOutsideTheirFolderAddNothing(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	app := filepath.Join(a.home, "code", "app")
	pkg := filepath.Join(app, "node_modules", "pkg")
	for _, dir := range []string{app, pkg} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "init", "-q")
	}
	a.write("code/app/main.go", "package main\n")
	a.write("code/app/node_modules/pkg/index.js", "x\n")
	a.write(".ssh/config", "Host *\n")
	a.write("elsewhere/repo/.git/HEAD", "ref: refs/heads/main\n")
	a.write("elsewhere/repo/a.txt", "a\n")
	a.writeSession("app", "s1", cwdLine("u1", app)+
		cwdLine("u2", filepath.Join(a.home, ".ssh"))+
		cwdLine("u3", pkg)+
		cwdLine("u4", filepath.Join(a.home, "elsewhere", "repo"))+
		`{"uuid":"u5","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"`+filepath.Join(a.home, ".ssh", "config")+`"}}]}}`+"\n")
	got := a.s.discoverFolders(nil, true)
	if strings.Join(got, ",") != "code/app" {
		t.Fatalf("folders = %v, want only the session's own folder", got)
	}
	// A session started in a credential folder doesn't sync it either.
	a.writeSession("ssh", "s2", cwdLine("u1", filepath.Join(a.home, ".ssh")))
	if got := a.s.discoverFolders(nil, true); slices.Contains(got, ".ssh") {
		t.Fatalf("folders = %v", got)
	}
}

func TestFilesASessionEditsInANestedRepoSync(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	work := filepath.Join(a.home, "code", "work")
	lib := filepath.Join(work, "lib")
	if err := os.MkdirAll(lib, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, lib, "init", "-q")
	a.write("code/work/lib/x.go", "package lib\n")
	a.writeSession("work", "s1", cwdLine("u1", work)+
		`{"uuid":"u2","cwd":"`+work+`","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"`+filepath.Join(lib, "x.go")+`","old_string":"a","new_string":"b"}}]}}`+"\n")
	a.sync()
	if !keysHave(store, FoldersPrefix+"code/work/lib/x.go") {
		t.Fatal("a repository whose files the session edited should sync")
	}

	// folder_exclude takes it out again.
	a.s.cfg.FolderExclude = []string{"lib"}
	if got := a.s.discoverFolders(nil, true); slices.Contains(got, "code/work/lib") {
		t.Fatalf("folders = %v, want lib excluded", got)
	}
}

func TestSuperprojectArrivesWithEmptySubmoduleFolders(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	src := filepath.Join(a.home, "code", "libsrc")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, src, "init", "-q")
	a.write("code/libsrc/lib.go", "package lib\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "lib")
	parent := filepath.Join(a.home, "code", "p")
	if err := os.MkdirAll(parent, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, parent, "init", "-q")
	a.write("code/p/main.go", "package main\n")
	git(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", src, "lib")
	git(t, parent, "commit", "-q", "-m", "with submodule")
	a.s.cfg.ExtraFolders = []string{parent}
	a.sync()
	syncAll(2, b)
	bp := filepath.Join(b.home, "code", "p")
	if b.read("code/p/main.go") != "package main\n" {
		t.Fatal("bob should get the superproject")
	}
	if fi, err := os.Stat(filepath.Join(bp, "lib")); err != nil || !fi.IsDir() {
		t.Fatal("the submodule's folder should exist, empty, as in a clone without submodules")
	}
	if st := git(t, bp, "status", "--porcelain"); strings.Contains(st, "D lib") {
		t.Fatalf("git shows the submodule deleted:\n%s", st)
	}
}

func TestASubmoduleAloneBringsItsGitData(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	src := filepath.Join(a.home, "code", "libsrc")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, src, "init", "-q")
	a.write("code/libsrc/lib.go", "package lib\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "lib")
	parent := filepath.Join(a.home, "code", "p")
	if err := os.MkdirAll(parent, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, parent, "init", "-q")
	git(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", src, "lib")
	git(t, parent, "commit", "-q", "-m", "sub")
	// Only the submodule is a session's folder.
	a.s.cfg.ExtraFolders = []string{filepath.Join(parent, "lib")}
	a.sync()
	syncAll(2, b)
	want := git(t, filepath.Join(parent, "lib"), "rev-parse", "HEAD")
	if got := git(t, filepath.Join(b.home, "code", "p", "lib"), "rev-parse", "HEAD"); got != want {
		t.Fatalf("bob's submodule HEAD = %q, want %q", got, want)
	}
}

func TestAnUnreadableUnneededCloneDoesntHoldBackItsFolder(t *testing.T) {
	needGit(t)
	a, b, store := oldVersionNestedRepo(t)
	_ = a
	lib := filepath.Join(b.home, "code", "work", "lib")
	if err := os.Chmod(lib, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(lib, 0755) }()
	b.write("code/work/notes.md", "outer, edited\n")
	r, err := b.s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Errors) != 0 || !slices.Contains(r.Uploaded, FoldersPrefix+"code/work/notes.md") {
		t.Fatalf("errors %v, uploaded %v", r.Errors, r.Uploaded)
	}
	_ = store
}

func TestFolderStatsLeaveOutReposThatNoLongerSync(t *testing.T) {
	a, _, _ := oldVersionNestedRepo(t)
	a.sync() // lib is a repository no session needs now
	if err := os.RemoveAll(filepath.Join(a.home, "code", "work", "lib")); err != nil {
		t.Fatal(err)
	}
	stats, err := FolderStats(a.s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range stats {
		if st.Path == "code/work" && st.Files != 1 {
			t.Fatalf("code/work counts %d files, want only notes.md", st.Files)
		}
	}
}

func TestProbeIgnoresReposInsideABusyFolder(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	a := newEverythingDevice(t, root, "alice", newCASStore())
	mono := filepath.Join(a.home, "code", "mono")
	app := filepath.Join(mono, "app")
	for _, dir := range []string{mono, app} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "init", "-q")
	}
	a.write("code/mono/app/app.go", "v1\n")
	a.s.cfg.ExtraFolders = []string{mono, app}
	a.write("code/mono/.git/index.lock", "")
	f1, err := a.s.LocalFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	a.write("code/mono/app/app.go", "v2, longer\n")
	if f2, _ := a.s.LocalFingerprint(); f2 != f1 {
		t.Fatal("an edit sync would sit out moved the fingerprint")
	}
}
