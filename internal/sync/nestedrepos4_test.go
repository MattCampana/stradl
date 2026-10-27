package sync

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Fourth review round: the frozen-copy rule decided by git, submodule names,
// symlinks kept as spelled, worktrees.

// frozenLib uploads carol's ~/code/work/lib the way Stradl before v2.3 did
// and gives alice her own clone of it (from carol's), then runs prep in it.
func frozenLib(t *testing.T, prep func(carolLib, aliceLib string)) (carol, alice *device, store *casStore) {
	needGit(t)
	root := t.TempDir()
	store = newCASStore()
	carol = newEverythingDevice(t, root, "carol", store)
	alice = newEverythingDevice(t, root, "alice", store)
	src := filepath.Join(carol.home, "code", "work", "lib")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, src, "init", "-q", "-b", "main")
	carol.write("code/work/lib/lib.go", "v1\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "v1")
	lib := filepath.Join(alice.home, "code", "work", "lib")
	if err := os.MkdirAll(filepath.Dir(lib), 0755); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(lib), "clone", "-q", "--single-branch", "-b", "main", src, "lib")
	prep(src, lib)
	uploadAsOldStradl(t, carol, "code/work", "code/work/lib")
	alice.s.cfg.ExtraFolders = []string{lib}
	return carol, alice, store
}

func TestDivergedFrozenCopyKeepsBothSidesAndAHealthyRepo(t *testing.T) {
	var c, a string
	_, alice, _ := frozenLib(t, func(carolLib, aliceLib string) {
		if err := os.WriteFile(filepath.Join(carolLib, "lib.go"), []byte("carol\n"), 0644); err != nil {
			t.Fatal(err)
		}
		git(t, carolLib, "commit", "-q", "-am", "C (unpushed)")
		c = strings.TrimSpace(git(t, carolLib, "rev-parse", "HEAD"))
		if err := os.WriteFile(filepath.Join(aliceLib, "lib.go"), []byte("alice\n"), 0644); err != nil {
			t.Fatal(err)
		}
		git(t, aliceLib, "commit", "-q", "-am", "A")
		a = strings.TrimSpace(git(t, aliceLib, "rev-parse", "HEAD"))
	})
	syncAll(2, alice)
	lib := filepath.Join(alice.home, "code", "work", "lib")
	if out := git(t, lib, "fsck", "--no-progress"); strings.Contains(out, "missing") || strings.Contains(out, "invalid") {
		t.Fatalf("repository damaged:\n%s", out)
	}
	all := git(t, lib, "log", "--all", "--format=%H")
	if !strings.Contains(all, c) || !strings.Contains(all, a) {
		t.Fatalf("both commits should stay reachable; have:\n%s", all)
	}
}

func TestAFolderWithOnlyNotesStillGetsTheStoredRepo(t *testing.T) {
	carol, _, store := frozenLib(t, func(string, string) {})
	dave := newEverythingDevice(t, filepath.Dir(carol.home), "dave", store)
	dave.write("code/work/lib/notes.md", "my note\n")
	dave.s.cfg.ExtraFolders = []string{filepath.Join(dave.home, "code", "work", "lib")}
	syncAll(3, dave)
	if dave.read("code/work/lib/lib.go") != "v1\n" || !dave.exists("code/work/lib/.git/HEAD") {
		t.Fatal("dave never got the repository his session works in")
	}
}

func TestSubmoduleWithAGitLikeNameSyncsItsData(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	src := filepath.Join(a.home, "code", "cfgsrc")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, src, "init", "-q")
	a.write("code/cfgsrc/main.tf", "x\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "tf")
	p := filepath.Join(a.home, "code", "p")
	if err := os.MkdirAll(p, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, p, "init", "-q")
	git(t, p, "-c", "protocol.file.allow=always", "submodule", "add", "-q", src, "infra/modules")
	git(t, p, "commit", "-q", "-m", "sub")
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "p", "infra", "modules")}
	}
	a.sync()
	if !keysHave(store, FoldersPrefix+"code/p/.git/modules/infra/modules/HEAD") {
		t.Fatal("the submodule's git data didn't sync")
	}
	syncAll(2, b)
	want := git(t, filepath.Join(p, "infra", "modules"), "rev-parse", "HEAD")
	if got := git(t, filepath.Join(b.home, "code", "p", "infra", "modules"), "rev-parse", "HEAD"); got != want {
		t.Fatalf("bob's submodule HEAD = %q, want %q", got, want)
	}
}

func TestSymlinkedFoldersKeepTheirSpelling(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newEverythingDevice(t, root, "alice", newCASStore())
	// ~/code is a link to another volume; ~/work a link to ~/code.
	ext := filepath.Join(root, "Volumes", "SSD", "code")
	if err := os.MkdirAll(filepath.Join(ext, "app"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ext, filepath.Join(a.home, "code")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(a.home, "code"), filepath.Join(a.home, "work")); err != nil {
		t.Fatal(err)
	}
	a.write("code/app/x.go", "package app\n")
	a.s.cfg.ExtraFolders = []string{filepath.Join(a.home, "code", "app"), filepath.Join(a.home, "work", "app")}
	got := a.s.discoverFolders(nil, true)
	if !slices.Contains(got, "code/app") || !slices.Contains(got, "work/app") {
		t.Fatalf("folders = %v, want both spellings kept as given", got)
	}
}

func TestAGitmodulesEntryForPlainFilesDoesntHideThem(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	p := filepath.Join(a.home, "code", "p")
	if err := os.MkdirAll(p, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, p, "init", "-q")
	// docs was a submodule once, converted to plain files; .gitmodules kept it.
	a.write("code/p/.gitmodules", "[submodule \"docs\"]\n\tpath = docs\n\turl = https://example.com/docs\n")
	a.write("code/p/docs/readme.md", "docs\n")
	a.s.cfg.ExtraFolders = []string{filepath.Join(p, "docs")}
	a.sync()
	if !keysHave(store, FoldersPrefix+"code/p/docs/readme.md") {
		t.Fatal("a plain folder a session works in should sync")
	}
}

func TestFolderTrackedNeedsAFolderThatSynced(t *testing.T) {
	root := t.TempDir()
	a := newEverythingDevice(t, root, "alice", newCASStore())
	a.write("code/app/x.go", "x\n")
	a.s.cfg.ExtraFolders = []string{filepath.Join(a.home, "code", "app")}
	a.sync()
	if !a.s.folderTracked(filepath.Join(a.home, "code", "app")) || !a.s.folderTracked(filepath.Join(a.home, "code", "app", "sub")) {
		t.Fatal("a synced folder (and anything in it) is tracked")
	}
	if a.s.folderTracked(filepath.Join(a.home, "code", "newproj")) {
		t.Fatal("a new folder beside a synced one isn't")
	}
}

func TestAWorktreeSessionBringsItsRepository(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	repo := filepath.Join(a.home, "code", "work", "c01")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	a.write("code/work/c01/a.txt", "a\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "a")
	wt := filepath.Join(repo, ".claude", "worktrees", "w1")
	git(t, repo, "worktree", "add", "-q", "-b", "claude/w1", wt)
	a.write("code/work/c01/.claude/worktrees/w1/b.txt", "b\n")
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-q", "-m", "b")
	a.write("code/work/c01/.claude/worktrees/w1/wip.txt", "uncommitted\n")
	a.s.cfg.ExtraFolders = []string{wt}
	a.sync()
	if !keysHave(store, FoldersPrefix+"code/work/c01/.git/refs/heads/claude/w1") {
		t.Fatal("the worktree session's branch never reached storage")
	}
	if !keysHave(store, FoldersPrefix+"code/work/c01/.claude/worktrees/w1/wip.txt") {
		t.Fatal("the worktree session's own files never reached storage")
	}
}

func TestABranchConflictWaitsForTheOtherMacsCommit(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	repo := filepath.Join(a.home, "code", "r")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	a.write("code/r/a.txt", "a\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "a")
	a.s.cfg.ExtraFolders = []string{repo}
	a.sync()
	a.write("code/r/a.txt", "a2\n")
	git(t, repo, "commit", "-q", "-am", "a2")
	mine := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	// Another Mac moved main to a commit whose objects aren't here yet.
	np := FoldersPrefix + "code/r/.git/refs/heads/main"
	st := a.s.state.GetFile(np)
	if _, err := a.s.put(t.Context(), np, []byte("1111111111111111111111111111111111111111\n"), st.RemoteETag); err != nil {
		t.Fatal(err)
	}
	r := a.sync()
	if !slices.Contains(r.Deferred, np) {
		t.Fatalf("deferred = %v, want the branch to wait", r.Deferred)
	}
	if got := strings.TrimSpace(git(t, repo, "rev-parse", "main")); got != mine {
		t.Fatalf("main moved to %s", got)
	}
}

func TestOperationStateStaysPerMac(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	repo := filepath.Join(a.home, "code", "r")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	a.write("code/r/.git/MERGE_HEAD", "0123456789012345678901234567890123456789\n")
	a.write("code/r/.git/rebase-merge/head-name", "refs/heads/main\n")
	a.write("code/r/x", "x\n")
	a.s.cfg.ExtraFolders = []string{repo}
	a.sync()
	if keysHave(store, FoldersPrefix+"code/r/.git/MERGE_HEAD") || len(keysWithPrefix(store, FoldersPrefix+"code/r/.git/rebase-merge/")) != 0 {
		t.Fatal("a merge or rebase under way synced")
	}
}

func TestAStoredCopyAheadIsNotFrozen(t *testing.T) {
	var y string
	_, alice, _ := frozenLib(t, func(carolLib, _ string) {
		// carol (still on an older Stradl) kept working past alice's clone.
		if err := os.WriteFile(filepath.Join(carolLib, "lib.go"), []byte("v2\n"), 0644); err != nil {
			t.Fatal(err)
		}
		git(t, carolLib, "commit", "-q", "-am", "Y")
		y = strings.TrimSpace(git(t, carolLib, "rev-parse", "HEAD"))
	})
	syncAll(2, alice)
	lib := filepath.Join(alice.home, "code", "work", "lib")
	if got := strings.TrimSpace(git(t, lib, "rev-parse", "main")); got != y {
		t.Fatalf("alice's main = %.7s, want carol's newer %.7s", got, y)
	}
	if got := alice.read("code/work/lib/lib.go"); got != "v2\n" {
		t.Fatalf("lib.go = %q, want carol's newer v2", got)
	}
}

func TestClaudeWorktreesStayPerMac(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	repo := filepath.Join(a.home, "code", "r")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	a.write("code/r/a.txt", "a\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "a")
	git(t, repo, "worktree", "add", "-q", "--detach", filepath.Join(repo, ".claude", "worktrees", "review-1"))
	a.s.cfg.ExtraFolders = []string{repo}
	// An older Stradl uploaded a worktree's files (no .git key: the link
	// never syncs).
	if _, err := a.s.put(t.Context(), FoldersPrefix+"code/r/.claude/worktrees/old/a.txt", []byte("a\n"), ""); err != nil {
		t.Fatal(err)
	}
	r := a.sync()
	if len(keysWithPrefix(store, FoldersPrefix+"code/r/.claude/worktrees/review-1/")) != 0 {
		t.Fatal("a worktree no session works in synced")
	}
	if a.exists("code/r/.claude/worktrees/old") || len(r.Deleted) != 0 {
		t.Fatalf("an old upload of a worktree came down or was deleted: %v", r.Deleted)
	}
}

func TestWorktreeSessionReachesTheOtherMac(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	repo := filepath.Join(a.home, "code", "x")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	a.write("code/x/a.txt", "a\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "a")
	a.writeSession("x", "s0", cwdLine("u0", repo))
	syncAll(2, a, b)
	wt := filepath.Join(repo, ".claude", "worktrees", "w1")
	git(t, repo, "worktree", "add", "-q", "-b", "claude/w1", wt)
	a.write("code/x/.claude/worktrees/w1/wip.txt", "uncommitted\n")
	a.writeSession("x", "s1", cwdLine("u1", wt))
	syncAll(3, a, b)
	if got := b.read("code/x/.claude/worktrees/w1/wip.txt"); got != "uncommitted\n" {
		t.Fatalf("bob's copy of the worktree session's folder = %q", got)
	}
	// The app removes the worktree when the session ends: no warnings, no
	// deletes on the other Mac.
	git(t, repo, "worktree", "remove", "--force", wt)
	r := a.sync()
	for _, w := range r.Warnings {
		if strings.Contains(w, "worktrees") {
			t.Fatalf("warned about a removed worktree: %s", w)
		}
	}
	b.sync()
	if !b.exists("code/x/.claude/worktrees/w1/wip.txt") {
		t.Fatal("bob's copy was deleted")
	}
}

func TestACloneAnOldMacDeletedIsntTrashedWhenASessionNeedsIt(t *testing.T) {
	a, _, store := oldVersionNestedRepo(t)
	a.sync() // lib is a repository no session needs on alice now
	// bob, still on v2.2.2, deletes his lib: v2.2.2 mirrors that delete.
	for _, k := range keysWithPrefix(store, FoldersPrefix+"code/work/lib/") {
		_ = store.Delete(t.Context(), k)
	}
	if r := a.sync(); len(r.Trashed) != 0 {
		t.Fatalf("alice touched her unneeded lib: %v", r.Trashed)
	}
	// Weeks later a session on alice works in lib.
	a.s.cfg.ExtraFolders = append(a.s.cfg.ExtraFolders, filepath.Join(a.home, "code", "work", "lib"))
	a.sync()
	if len(a.trashed()) != 0 || !a.exists("code/work/lib/f0.go") {
		t.Fatalf("the session's clone went to the trash: %v", a.trashed())
	}
	if !keysHave(store, FoldersPrefix+"code/work/lib/f0.go") {
		t.Fatal("lib should upload again")
	}
}

func TestACloneAnOldMacDeletedSurvivesAFirstPassThatWaits(t *testing.T) {
	a, _, store := oldVersionNestedRepo(t)
	a.sync()
	for _, k := range keysWithPrefix(store, FoldersPrefix+"code/work/lib/") {
		_ = store.Delete(t.Context(), k)
	}
	a.sync()
	a.s.cfg.ExtraFolders = append(a.s.cfg.ExtraFolders, filepath.Join(a.home, "code", "work", "lib"))
	a.write("code/work/lib/.git/index.lock", "") // git is busy as the session starts
	a.sync()
	a.remove("code/work/lib/.git/index.lock")
	a.sync()
	if len(a.trashed()) != 0 || !a.exists("code/work/lib/f0.go") {
		t.Fatalf("the session's clone went to the trash: %v", a.trashed())
	}
}
