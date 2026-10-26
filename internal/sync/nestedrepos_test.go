package sync

import (
	"context"
	"os"
	"path/filepath"
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
	} {
		if _, err := a.s.put(ctx, np, []byte(data), ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []*device{a, b} {
		if r := d.sync(); len(r.Downloaded) != 0 || len(r.Deleted) != 0 {
			t.Fatalf("%s touched an unneeded repo: %+v", d.name, r)
		}
		if d.exists("code/work/old") {
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
