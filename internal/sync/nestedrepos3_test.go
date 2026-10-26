package sync

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Third review round: Macs on different versions, submodules, symlinks.

// v222Pair syncs ~/code/work/r2 (a repository) on two Macs while it is its
// own root, then takes away its marker and lists only code/work as synced,
// leaving the state Stradl v2.2.2 left: r2 synced as part of code/work.
func v222Pair(t *testing.T) (a, b *device, store *casStore) {
	needGit(t)
	root := t.TempDir()
	store = newCASStore()
	a = newEverythingDevice(t, root, "alice", store)
	b = newEverythingDevice(t, root, "matthew", store)
	repo := filepath.Join(a.home, "code", "work", "r2")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	a.write("code/work/notes.md", "outer\n")
	a.write("code/work/r2/lib.go", "v0\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "S")
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "work"), filepath.Join(d.home, "code", "work", "r2")}
	}
	syncAll(2, a, b)
	dropMarker(store, "code/work/r2")
	for _, d := range []*device{a, b} {
		_ = d.s.saveFolderList([]string{"code/work"}, nil)
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "work")}
	}
	return a, b, store
}

func dropMarker(store *casStore, rel string) {
	_ = store.Delete(context.Background(), FoldersPrefix+rel+"/"+folderMarker+".age")
}

// syncAsV222 runs a pass as a Mac still on v2.2.2 would: r2 syncs with
// code/work, and no marker of its own is left behind.
func syncAsV222(d *device, store *casStore) {
	saved := d.s.cfg.ExtraFolders
	d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "work"), filepath.Join(d.home, "code", "work", "r2")}
	defer func() { d.s.cfg.ExtraFolders = saved }()
	d.sync()
	dropMarker(store, "code/work/r2")
	_ = d.s.saveFolderList([]string{"code/work"}, nil)
}

func TestStaleNeverRollsBackWhatAMacStillOnTheOldVersionMovedAhead(t *testing.T) {
	a, b, store := v222Pair(t)
	ra := filepath.Join(a.home, "code", "work", "r2")
	rb := filepath.Join(b.home, "code", "work", "r2")
	b.write("code/work/r2/lib.go", "v1\n")
	git(t, rb, "commit", "-q", "-am", "X")
	syncAsV222(b, store)
	a.sync()
	git(t, ra, "fetch", "-q", rb, "main") // both pull the same upstream
	git(t, ra, "merge", "-q", "--ff-only", "FETCH_HEAD")
	a.sync()
	b.write("code/work/r2/lib.go", "v2\n")
	git(t, rb, "commit", "-q", "-am", "Y")
	y := strings.TrimSpace(git(t, rb, "rev-parse", "HEAD"))
	syncAsV222(b, store)

	a.s.cfg.ExtraFolders = append(a.s.cfg.ExtraFolders, ra) // a session on alice needs r2 now
	a.sync()
	if got := strings.TrimSpace(a.read("code/work/r2/.git/refs/heads/main")); got != y {
		t.Errorf("alice's main = %.7s, want Y %.7s", got, y)
	}
	if got := a.read("code/work/r2/lib.go"); got != "v2\n" {
		t.Errorf("alice's lib.go = %q", got)
	}
	syncAsV222(b, store)
	if got := strings.TrimSpace(git(t, rb, "rev-parse", "main")); got != y {
		t.Errorf("bob's main rolled back to %.7s", got)
	}
}

func TestFrozenFilesOnlyTheOldUploadHasStayOut(t *testing.T) {
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
	carol.write("code/work/lib/old.go", "old\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "v1")
	carol.write("code/work/lib/.git/MERGE_HEAD", strings.TrimSpace(git(t, src, "rev-parse", "HEAD"))+"\n")
	uploadAsOldStradl(t, carol, "code/work", "code/work/lib")

	lib := filepath.Join(alice.home, "code", "work", "lib")
	if err := os.MkdirAll(filepath.Dir(lib), 0755); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(lib), "clone", "-q", src, "lib")
	git(t, lib, "rm", "-q", "old.go")
	git(t, lib, "commit", "-q", "-m", "v2: drop old.go")
	alice.s.cfg.ExtraFolders = []string{lib}
	alice.sync()
	alice.sync() // lib has its marker now
	// A file only the frozen copy has arrives (as an untracked file), but
	// another Mac's merge in progress never does, and alice's history and
	// files stay hers.
	if alice.exists("code/work/lib/.git/MERGE_HEAD") {
		t.Fatal("another Mac's old merge-in-progress state landed in alice's clone")
	}
	if alice.read("code/work/lib/lib.go") != "v1\n" || strings.Contains(git(t, lib, "log", "--oneline", "-1"), "v1") {
		t.Fatalf("alice's clone changed: %s", git(t, lib, "log", "--oneline", "-1"))
	}
	if out := git(t, lib, "fsck", "--no-progress"); strings.Contains(out, "error") || strings.Contains(out, "missing") {
		t.Fatalf("alice's repository is damaged:\n%s", out)
	}
}

func TestOldUploadOfAnUnneededSubmoduleStaysOut(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	carol := newEverythingDevice(t, root, "carol", store)
	bob := newEverythingDevice(t, root, "matthew", store)
	src := filepath.Join(carol.home, "code", "libsrc")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, src, "init", "-q")
	carol.write("code/libsrc/lib.go", "package lib\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "lib")
	p := filepath.Join(carol.home, "code", "p")
	if err := os.MkdirAll(p, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, p, "init", "-q")
	carol.write("code/p/main.go", "package main\n")
	git(t, p, "-c", "protocol.file.allow=always", "submodule", "add", "-q", src, "lib")
	git(t, p, "commit", "-q", "-m", "sub")
	uploadAsOldStradl(t, carol, "code/p", "code/p")

	syncAll(3, bob)
	if bob.read("code/p/main.go") != "package main\n" {
		t.Fatal("bob should get the superproject")
	}
	if bob.exists("code/p/lib/lib.go") {
		t.Fatal("an older Stradl's copy of a submodule no session needs landed in its folder")
	}
	if fi, err := os.Stat(filepath.Join(bob.home, "code", "p", "lib")); err != nil || !fi.IsDir() {
		t.Fatal("the submodule's folder should exist, empty")
	}
}

func TestSidebarDownloadNeverRecreatesATrackedFolder(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "code", "app")
	rec := []byte(`{"sessionId":"local_1","cwd":"` + cwd + `"}`)
	finishDesktopDownload(rec, home, nil, func(string) bool { return true })
	if _, err := os.Stat(cwd); err == nil {
		t.Fatal("recreated a project folder sync tracks")
	}
	finishDesktopDownload(rec, home, nil, func(string) bool { return false })
	if _, err := os.Stat(cwd); err != nil {
		t.Fatal("an untracked folder (a scratch folder) should be created")
	}
}

func TestSymlinkedPathsNeverBecomeFolders(t *testing.T) {
	needGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newEverythingDevice(t, root, "alice", newCASStore())
	app := filepath.Join(a.home, "code", "app")
	lib := filepath.Join(a.home, "code", "lib")
	for _, d := range []string{app, lib} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
		git(t, d, "init", "-q")
	}
	if err := os.Symlink("../lib", filepath.Join(app, "shared")); err != nil {
		t.Fatal(err)
	}
	a.write("code/lib/x.go", "package lib\n")
	a.writeSession("app", "s1", cwdLine("u1", app)+
		`{"uuid":"u2","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"`+filepath.Join(app, "shared", "x.go")+`"}}]}}`+"\n")
	got := a.s.discoverFolders(nil, true)
	if strings.Join(got, ",") != "code/app" {
		t.Fatalf("folders = %v, want only code/app", got)
	}
}

func TestCredentialFoldersInsideAFolderStayPerMac(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.write(".config/nvim/init.lua", "-- nvim\n")
	a.write(".config/gh/hosts.yml", "oauth_token: secret\n")
	a.s.cfg.ExtraFolders = []string{filepath.Join(a.home, ".config")}
	a.sync()
	if !keysHave(store, FoldersPrefix+".config/nvim/init.lua") {
		t.Fatal("the folder should sync")
	}
	if keysHave(store, FoldersPrefix+".config/gh/hosts.yml") {
		t.Fatal("a login token synced")
	}
	// One an older Stradl uploaded doesn't come down either.
	if _, err := a.s.put(context.Background(), FoldersPrefix+".config/gh/hosts.yml", []byte("oauth_token: theirs\n"), ""); err != nil {
		t.Fatal(err)
	}
	b := newEverythingDevice(t, root, "matthew", store)
	b.s.cfg.ExtraFolders = []string{filepath.Join(b.home, ".config")}
	b.sync()
	if b.read(".config/nvim/init.lua") != "-- nvim\n" || b.exists(".config/gh/hosts.yml") {
		t.Fatal("bob should get nvim's config and not another Mac's login")
	}
}

func TestOuterExcludesApplyInsideANestedRepo(t *testing.T) {
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
	a.write("code/work/lib/main.go", "package lib\n")
	a.write("code/work/lib/.env", "SECRET=1\n")
	a.s.cfg.ExtraFolders = []string{work, lib}
	a.s.cfg.FolderExclude = []string{"lib/.env"}
	a.sync()
	if !keysHave(store, FoldersPrefix+"code/work/lib/main.go") || keysHave(store, FoldersPrefix+"code/work/lib/.env") {
		t.Fatal("folder_exclude written for the outer folder should still leave lib/.env out")
	}
}

func TestProbeWatchesReposKeptOnlyByAMarker(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	a := newEverythingDevice(t, root, "alice", newCASStore())
	lib := filepath.Join(a.home, "code", "work", "lib")
	if err := os.MkdirAll(lib, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, lib, "init", "-q")
	a.write("code/work/lib/x.go", "v1\n")
	a.s.cfg.ExtraFolders = []string{filepath.Join(a.home, "code", "work")}
	// The last sync covered lib because another Mac's marker names it.
	if err := a.s.saveFolderList([]string{"code/work", "code/work/lib"}, nil); err != nil {
		t.Fatal(err)
	}
	f1, err := a.s.LocalFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	a.write("code/work/lib/x.go", "v2, longer\n")
	if f2, _ := a.s.LocalFingerprint(); f2 == f1 {
		t.Fatal("an edit in a repo the last sync covered didn't move the fingerprint")
	}
}

func TestSubmoduleIndexStaysPerMac(t *testing.T) {
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
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "p"), filepath.Join(d.home, "code", "p", "lib")}
	}
	a.sync()
	if keysHave(store, FoldersPrefix+"code/p/.git/modules/lib/index") {
		t.Fatal("a submodule's index synced")
	}
	syncAll(2, b)
	if st := git(t, filepath.Join(b.home, "code", "p", "lib"), "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Fatalf("bob's submodule isn't clean (its index wasn't rebuilt):\n%s", st)
	}
}

func TestGitDirSplit(t *testing.T) {
	for _, c := range []struct{ in, dir, inside string }{
		{".git/HEAD", ".git", "HEAD"},
		{"a/.git/refs/heads/main", "a/.git", "refs/heads/main"},
		{"p/.git/modules/lib/refs/heads/main", "p/.git/modules/lib", "refs/heads/main"},
		{"p/.git/modules/deps/lib/index", "p/.git/modules/deps/lib", "index"},
		{"p/.git/modules/a/modules/b/HEAD", "p/.git/modules/a/modules/b", "HEAD"},
		{"p/.git/modules/lib/objects/ab/cd", "p/.git/modules/lib", "objects/ab/cd"},
		{"p/.git/modules/infra/modules/HEAD", "p/.git/modules/infra/modules", "HEAD"},
		{"p/.git/modules/app/config/HEAD", "p/.git/modules/app/config", "HEAD"},
		{"p/.git/modules/app/config/refs/heads/main", "p/.git/modules/app/config", "refs/heads/main"},
		{"p/.git/modules/lib/logs/refs/heads/main", "p/.git/modules/lib", "logs/refs/heads/main"},
		{"p/.git/modules/tools/hooks/HEAD", "p/.git/modules/tools/hooks", "HEAD"},
		{"p/.git/modules/lib/refs/heads/config", "p/.git/modules/lib", "refs/heads/config"},
	} {
		dir, inside, ok := gitDirSplit(c.in)
		if !ok || dir != c.dir || inside != c.inside {
			t.Errorf("gitDirSplit(%q) = %q, %q, %v", c.in, dir, inside, ok)
		}
	}
	if m := innermostModule("a/modules/b/HEAD"); m != "a/modules/b" {
		t.Errorf("innermostModule = %q", m)
	}
	if m := innermostModule("deps/lib/refs/heads/x"); m != "deps/lib" {
		t.Errorf("innermostModule = %q", m)
	}
	r := &syncRoot{modules: map[string]bool{"a": true}}
	if r.owns(".git/modules/a/modules/b/HEAD") || !r.owns(".git/modules/a/HEAD") {
		t.Error("a nested submodule's data belongs to it, not to the one holding it")
	}
	if !slices.Contains([]bool{r.wantsModuleDir("a/refs")}, true) || r.wantsModuleDir("a/modules/b") {
		t.Error("wantsModuleDir should stop at a nested submodule nobody needs")
	}
}
