package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
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
	if len(r.Uploaded) != 0 || !slices.Contains(r.Deferred, FoldersPrefix+"code/p/lib/") {
		t.Fatalf("uploaded = %v, deferred = %v", r.Uploaded, r.Deferred)
	}
}
