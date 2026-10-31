package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Every git repository syncs as its own unit, and only when a session needs
// it: a folder full of clones, or a repository full of submodules, never
// drags the others along.

func TestNestedReposSyncOnlyWhenASessionNeedsThem(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	for _, d := range []*device{a, b} {
		// A session works in ~/code/work (a plain folder) and in its app repo.
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "work"), filepath.Join(d.home, "code", "work", "app")}
	}
	for _, r := range []string{"app", "lib", "tool"} {
		dir := filepath.Join(a.home, "code", "work", r)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "init", "-q")
		a.write("code/work/"+r+"/main.go", "package "+r+"\n")
	}
	a.write("code/work/notes.md", "loose file\n")
	a.sync()

	if len(keysWithPrefix(store, FoldersPrefix+"code/work/app/.git/")) == 0 || !keysHave(store, FoldersPrefix+"code/work/app/main.go") {
		t.Fatal("the repo a session works in should sync, history included")
	}
	if !keysHave(store, FoldersPrefix+"code/work/notes.md") {
		t.Fatal("the folder's own files should sync")
	}
	for _, r := range []string{"lib", "tool"} {
		if keys := keysWithPrefix(store, FoldersPrefix+"code/work/"+r+"/"); len(keys) != 0 {
			t.Fatalf("%s isn't needed by any session but synced: %v", r, keys)
		}
	}

	b.sync()
	if b.read("code/work/notes.md") != "loose file\n" || b.read("code/work/app/main.go") != "package app\n" {
		t.Fatal("bob should get the folder and the needed repo")
	}
	if b.exists("code/work/lib") || b.exists("code/work/tool") {
		t.Fatal("bob got repos no session needs")
	}

	// Files an older Stradl uploaded for a repository inside the folder
	// (it synced nested repos along with it) are left in storage: never
	// downloaded, never deleted.
	ctx := context.Background()
	for np, data := range map[string]string{
		FoldersPrefix + "code/work/old/.git/HEAD":  "ref: refs/heads/main\n",
		FoldersPrefix + "code/work/old/readme.txt": "from an older Mac\n",
		// A submodule's .git is a file.
		FoldersPrefix + "code/work/sub/.git":       "gitdir: ../.git/modules/sub\n",
		FoldersPrefix + "code/work/sub/readme.txt": "a submodule\n",
	} {
		if _, err := a.s.put(ctx, np, []byte(data), ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []*device{a, b} {
		if r := d.sync(); len(r.Downloaded) != 0 || len(r.Deleted) != 0 {
			t.Fatalf("%s touched an unneeded repo: %+v", d.name, r)
		}
		if d.exists("code/work/old") || d.exists("code/work/sub") {
			t.Fatalf("%s downloaded an unneeded repo", d.name)
		}
	}
	if !keysHave(store, FoldersPrefix+"code/work/old/readme.txt") {
		t.Fatal("an unneeded repo's files must not be deleted from storage")
	}
}

func TestSubmodulesSyncOnlyWhenNeeded(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
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
	git(t, parent, "add", "-A")
	git(t, parent, "commit", "-q", "-m", "with submodule")

	a.s.cfg.ExtraFolders = []string{parent}
	a.sync()
	if !keysHave(store, FoldersPrefix+"code/p/main.go") || !keysHave(store, FoldersPrefix+"code/p/.gitmodules") {
		t.Fatal("the parent repo should sync")
	}
	if keys := keysWithPrefix(store, FoldersPrefix+"code/p/lib/"); len(keys) != 0 {
		t.Fatalf("a submodule no session works in synced: %v", keys)
	}
	if keys := keysWithPrefix(store, FoldersPrefix+"code/p/.git/modules/"); len(keys) != 0 {
		t.Fatalf("an unneeded submodule's git data synced: %v", keys)
	}

	// A session starts working in the submodule: now it syncs, with its data.
	a.s.cfg.ExtraFolders = []string{parent, filepath.Join(parent, "lib")}
	a.sync()
	if !keysHave(store, FoldersPrefix+"code/p/lib/lib.go") {
		t.Fatal("a needed submodule should sync")
	}
	if len(keysWithPrefix(store, FoldersPrefix+"code/p/.git/modules/lib/")) == 0 {
		t.Fatal("a needed submodule's git data should sync")
	}
}

// cwdLine is a transcript entry recording only a working folder.
func cwdLine(uuid, cwd string) string {
	return fmt.Sprintf(`{"uuid":%q,"cwd":%q}`+"\n", uuid, cwd)
}

func TestEveryFolderASessionWorksInCounts(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	work := filepath.Join(a.home, "code", "work")
	for _, r := range []string{"app", "lib"} {
		dir := filepath.Join(work, r)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "init", "-q")
		a.write("code/work/"+r+"/main.go", "package "+r+"\n")
	}
	// The session starts in the outer folder and moves into app.
	a.writeSession("work", "s1", cwdLine("u1", work)+cwdLine("u2", filepath.Join(work, "app")))
	a.sync()
	if !keysHave(store, FoldersPrefix+"code/work/app/main.go") {
		t.Fatal("a repository the session moved into should sync")
	}
	if keysHave(store, FoldersPrefix+"code/work/lib/main.go") {
		t.Fatal("lib isn't needed yet")
	}
	// Later it moves into lib too: read from where the last pass stopped.
	a.appendSession("work", "s1", cwdLine("u3", filepath.Join(work, "lib")))
	a.sync()
	if !keysHave(store, FoldersPrefix+"code/work/lib/main.go") {
		t.Fatal("a repository the session moved into later should sync")
	}
}

func TestNestedRepoReachesAMacThatHasTheOuterRepo(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	mono := filepath.Join(a.home, "code", "mono")
	if err := os.MkdirAll(mono, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, mono, "init", "-q")
	a.write("code/mono/mono.txt", "mono\n")
	a.writeSession("mono", "s1", cwdLine("u1", mono))
	a.sync()
	b.sync()
	b.sync()
	if b.read("code/mono/mono.txt") != "mono\n" {
		t.Fatal("bob should have the outer repo")
	}

	// A clone inside it (not a submodule) that bob doesn't have yet: bob's
	// copy of mono must not swallow it.
	app := filepath.Join(mono, "app")
	if err := os.MkdirAll(app, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, app, "init", "-q")
	a.write("code/mono/app/app.go", "package app\n")
	a.writeSession("mono", "s2", cwdLine("u2", app))
	a.sync()
	bApp := filepath.Join(b.home, "code", "mono", "app")
	if got := b.s.canonicalFolderIn(bApp, remoteRepoFolders(listAll(t, store))); got != "code/mono/app" {
		t.Fatalf("bob sees %q, want the repository other Macs sync", got)
	}
	b.sync()
	b.sync()
	if b.read("code/mono/app/app.go") != "package app\n" || !b.exists("code/mono/app/.git/HEAD") {
		t.Fatal("bob should get the repository inside mono that a session works in")
	}
}

func TestRenamedSubmoduleDataSyncs(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
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
	git(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", "--name", "vendored", src, "deps/lib")
	git(t, parent, "add", "-A")
	git(t, parent, "commit", "-q", "-m", "with submodule")

	a.s.cfg.ExtraFolders = []string{parent, filepath.Join(parent, "deps", "lib")}
	a.sync()
	if !keysHave(store, FoldersPrefix+"code/p/deps/lib/lib.go") {
		t.Fatal("a needed submodule should sync")
	}
	if len(keysWithPrefix(store, FoldersPrefix+"code/p/.git/modules/vendored/")) == 0 {
		t.Fatal("a needed submodule's git data should sync from where its .git file points")
	}
}

func TestReposInsideABusyFolderWait(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
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
	a.sync()

	a.write("code/mono/.git/index.lock", "")
	a.write("code/mono/app/app.go", "v2\n")
	r := a.sync()
	if len(r.Uploaded) != 0 {
		t.Fatalf("a repository inside a folder git is working in synced: %v", r.Uploaded)
	}
	if !slices.Contains(r.Deferred, FoldersPrefix+"code/mono/") {
		t.Fatalf("deferred = %v", r.Deferred)
	}
	a.remove("code/mono/.git/index.lock")
	if r := a.sync(); !slices.Contains(r.Uploaded, FoldersPrefix+"code/mono/app/app.go") {
		t.Fatalf("uploaded = %v", r.Uploaded)
	}
}

func TestSubmoduleWaitsForGitInItsDataDir(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
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

	a.s.cfg.ExtraFolders = []string{filepath.Join(parent, "lib")}
	a.sync()
	// git keeps a submodule's locks in the parent's .git/modules, outside
	// the folder.
	a.write("code/p/.git/modules/lib/index.lock", "")
	a.write("code/p/lib/lib.go", "package lib // edited\n")
	r := a.sync()
	// The submodule brings its superproject, which waits too.
	if len(r.Uploaded) != 0 || (!slices.Contains(r.Deferred, FoldersPrefix+"code/p/lib/") && !slices.Contains(r.Deferred, FoldersPrefix+"code/p/")) {
		t.Fatalf("uploaded = %v, deferred = %v", r.Uploaded, r.Deferred)
	}
	var busy *gitBusyError
	if _, err := a.s.scanFolder(a.s.folderRoot("code/p/lib")); !errors.As(err, &busy) {
		t.Fatalf("scanning the submodule alone = %v, want git busy", err)
	}
}

// uploadAsOldStradl puts every file of the folder at rel (relative to d's
// home) into storage the way Stradl before v2.3 did for a repository inside
// a synced folder: as plain files of the outer folder, with no marker of its
// own.
func uploadAsOldStradl(t *testing.T, d *device, outer, rel string) {
	t.Helper()
	ctx := context.Background()
	if _, err := d.s.put(ctx, FoldersPrefix+outer+"/"+folderMarker, []byte("{}\n"), ""); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(d.home, filepath.FromSlash(rel))
	err := filepath.WalkDir(base, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		r, _ := filepath.Rel(d.home, p)
		r = filepath.ToSlash(r)
		if gitPerMac(r) {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		_, err = d.s.put(ctx, FoldersPrefix+r, data, "")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A repository an older Stradl uploaded inside a folder and nobody updated
// since follows the usual first-contact rule when a session needs it on a
// Mac with its own clone: the stored copy leads, and nothing of this Mac's
// is lost (its commits on a stradl/ branch, its files in the conflicts
// folder). A `git pull` brings it forward again.
func TestFrozenOldUploadNeverLosesANewerClone(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	carol := newEverythingDevice(t, root, "carol", store)
	alice := newEverythingDevice(t, root, "alice", store)
	src := filepath.Join(carol.home, "code", "work", "lib")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, src, "init", "-q", "-b", "main")
	carol.write("code/work/lib/lib.go", "v1\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "v1")
	uploadAsOldStradl(t, carol, "code/work", "code/work/lib")

	// alice cloned it, moved on to v2, and now a session works in it.
	lib := filepath.Join(alice.home, "code", "work", "lib")
	if err := os.MkdirAll(filepath.Dir(lib), 0755); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(lib), "clone", "-q", src, "lib")
	alice.write("code/work/lib/lib.go", "v2\n")
	git(t, lib, "commit", "-q", "-am", "v2")
	v2 := git(t, lib, "rev-parse", "HEAD")
	alice.s.cfg.ExtraFolders = []string{lib}
	alice.sync()
	alice.sync()
	if !strings.Contains(git(t, lib, "log", "--all", "--format=%H"), strings.TrimSpace(v2)) {
		t.Fatal("alice's v2 commit is no longer reachable")
	}
	if out := git(t, lib, "fsck", "--no-progress"); strings.Contains(out, "missing") || strings.Contains(out, "invalid") {
		t.Fatalf("repository damaged:\n%s", out)
	}
	kept := false
	_ = filepath.WalkDir(filepath.Join(alice.s.stateDir(), "conflicts"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); string(b) == "v2\n" {
				kept = true
			}
		}
		return nil
	})
	if !kept && alice.read("code/work/lib/lib.go") != "v2\n" {
		t.Fatal("alice's v2 lib.go is gone")
	}
}

func TestDeferredSubmoduleHoldsBackItsGitData(t *testing.T) {
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
	a.write("code/libsrc/lib.go", "v1\n")
	a.write("code/libsrc/dir/x.txt", "x\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "v1")
	parent := filepath.Join(a.home, "code", "p")
	if err := os.MkdirAll(parent, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, parent, "init", "-q")
	git(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", src, "lib")
	git(t, parent, "commit", "-q", "-m", "sub")
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "p"), filepath.Join(d.home, "code", "p", "lib")}
	}
	a.sync()
	b.sync()
	b.sync()
	bLib := filepath.Join(b.home, "code", "p", "lib")
	v1 := git(t, bLib, "rev-parse", "HEAD")

	a.write("code/p/lib/lib.go", "v2\n")
	git(t, filepath.Join(parent, "lib"), "commit", "-q", "-am", "v2")
	a.sync()

	// bob's submodule can't be read whole this pass.
	locked := filepath.Join(bLib, "dir")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(locked, 0755) }()
	r, err := b.s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = r
	if got := git(t, bLib, "rev-parse", "HEAD"); got != v1 {
		t.Fatalf("the submodule's HEAD moved to %s while its files waited", got)
	}
	if err := os.Chmod(locked, 0755); err != nil {
		t.Fatal(err)
	}
	b.sync()
	b.sync()
	if b.read("code/p/lib/lib.go") != "v2\n" || git(t, bLib, "rev-parse", "HEAD") == v1 {
		t.Fatal("the submodule should catch up once readable")
	}
}

func TestWorktreeGitDataWaitsForMacOSPermission(t *testing.T) {
	needGit(t)
	if runtime.GOOS != "darwin" {
		t.Skip("macOS privacy prompts")
	}
	// git records the worktree's real path; the temp folder sits behind
	// /var -> /private/var.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newEverythingDevice(t, root, "alice", newCASStore())
	repo := filepath.Join(a.home, "Documents", "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	a.write("Documents/repo/a.txt", "a\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "a")
	if err := os.MkdirAll(filepath.Join(a.home, "code"), 0755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "worktree", "add", "-q", filepath.Join(a.home, "code", "wt"))
	a.write("Documents/repo/.git/worktrees/wt/index.lock", "")

	answer := make(chan error)
	oldRead, oldWait := tccRead, tccWait
	tccRead = func(dir string) error { return <-answer }
	tccWait = 20 * time.Millisecond
	defer func() { tccRead, tccWait = oldRead, oldWait; close(answer) }()
	tccMu.Lock()
	tccProbes = map[string]*tccProbe{}
	tccMu.Unlock()

	_, err = a.s.scanFolder(a.s.folderRoot("code/wt"))
	if !errors.Is(err, errAwaitingPermission) {
		t.Fatalf("scan = %v, want it to wait for macOS before reading Documents", err)
	}
}

func TestCwdsRereadAfterAMergeInsertsLinesEarlier(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	first := cwdLine("u1", "/h/code/work")
	last := cwdLine("u3", "/h/code/work")
	if err := os.WriteFile(p, []byte(first+last), 0600); err != nil {
		t.Fatal(err)
	}
	e := updateCwds(p, cwdEntry{}, int64(len(first+last)), "/h")
	// Another Mac's line lands between them, by time.
	merged := first + cwdLine("u2", "/h/code/work/lib") + last
	if err := os.WriteFile(p, []byte(merged), 0600); err != nil {
		t.Fatal(err)
	}
	e = updateCwds(p, e, int64(len(merged)), "/h")
	if !slices.Contains(e.Cwds, "/h/code/work/lib") {
		t.Fatalf("cwds = %v, missing the merged-in folder", e.Cwds)
	}
	// Appends still resume where the last read stopped.
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(cwdLine("u4", "/h/code/other"))
	_ = f.Close()
	info, _ := os.Stat(p)
	before := e.Scanned
	e = updateCwds(p, e, info.Size(), "/h")
	if !slices.Contains(e.Cwds, "/h/code/other") || e.Scanned <= before {
		t.Fatalf("append: %+v", e)
	}
}
