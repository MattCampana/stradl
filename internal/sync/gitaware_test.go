package sync

import (
	"fmt"
	"os"
	"runtime"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil || gitBinary() == "" {
		t.Skip("git not installed")
	}
}

// foldersOn makes code/<name> a project folder on each device.
func foldersOn(name string, devs ...*device) {
	for _, d := range devs {
		d.s.cfg.ExtraFolders = append(d.s.cfg.ExtraFolders, filepath.Join(d.home, "code", name))
	}
}

func conflictCopies(t *testing.T, dir, base string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, base+".conflict.*"))
	return m
}

func asideCopies(d *device, rel string) []string {
	m, _ := filepath.Glob(filepath.Join(d.home, ".claude-sync", "conflicts", filepath.FromSlash(rel)+".conflict.*"))
	return m
}

// backdate moves a file's mtime into the past, so "newest wins" would pick
// the other side.
func backdate(t *testing.T, d *device, rel string, by time.Duration) {
	t.Helper()
	old := time.Now().Add(-by)
	if err := os.Chtimes(filepath.Join(d.home, filepath.FromSlash(rel)), old, old); err != nil {
		t.Fatal(err)
	}
}

// named gives a test device a display name, which names its .from- copies.
func named(t *testing.T, d *device) *device {
	t.Helper()
	if err := os.WriteFile(filepath.Join(d.s.cfg.StateDirOverride, deviceNameFile), []byte(d.name+"'s Mac\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestFirstContactKeepsTheSyncedCopyAndSharesTheOther(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := named(t, newEverythingDevice(t, root, "alice", store))
	b := named(t, newEverythingDevice(t, root, "matthew", store))
	foldersOn("p", a, b)

	a.write("code/p/notes.txt", "alice's notes\n")
	a.write("code/p/only-alice.txt", "a\n")
	a.sync()
	// Bob had the folder before syncing it, with a newer copy of notes.txt:
	// newest-wins would let it replace alice's; first contact keeps the
	// copy already synced and shares bob's beside it.
	b.write("code/p/notes.txt", "bob's notes\n")
	b.write("code/p/only-bob.txt", "b\n")
	backdate(t, a, "code/p/notes.txt", time.Hour)
	b.sync()

	if got := b.read("code/p/notes.txt"); got != "alice's notes\n" {
		t.Fatalf("the synced copy should stay the reference: %q", got)
	}
	if got := b.read("code/p/notes.txt.from-matthew-s-mac"); got != "bob's notes\n" {
		t.Fatalf("bob's copy should sit beside it: %q", got)
	}
	if b.read("code/p/only-alice.txt") != "a\n" {
		t.Fatal("a file only alice had didn't reach bob")
	}
	syncAll(1, b, a)
	if a.read("code/p/notes.txt") != "alice's notes\n" || a.read("code/p/notes.txt.from-matthew-s-mac") != "bob's notes\n" || a.read("code/p/only-bob.txt") != "b\n" {
		t.Fatal("alice should have both copies and bob's file")
	}
	// Deleting the shared copy once it's merged deletes it everywhere.
	a.remove("code/p/notes.txt.from-matthew-s-mac")
	syncAll(1, a, b)
	if b.exists("code/p/notes.txt.from-matthew-s-mac") {
		t.Fatal("the merged copy should be gone from bob too")
	}
}

// twoClones gives alice a repository with two commits and bob a clone of it
// at the same commit, each in code/repo, with nothing synced yet.
func twoClones(t *testing.T, a, b *device) (repoA, repoB string) {
	t.Helper()
	repoA = filepath.Join(a.home, "code", "repo")
	if err := os.MkdirAll(repoA, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, repoA, "init", "-q")
	a.write("code/repo/x.py", "x = 1\n")
	a.write("code/repo/y.py", "y = 1\n")
	a.write("code/repo/.gitignore", "dist/\n")
	git(t, repoA, "add", "-A")
	git(t, repoA, "commit", "-q", "-m", "first")
	a.write("code/repo/x.py", "x = 2\n")
	git(t, repoA, "commit", "-q", "-am", "second")
	repoB = filepath.Join(b.home, "code", "repo")
	if err := os.MkdirAll(filepath.Dir(repoB), 0755); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(repoB), "clone", "-q", "--no-hardlinks", repoA, "repo")
	return repoA, repoB
}

func TestFirstContactCloneTakesTheSyncedEditsAndSharesItsOwn(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := named(t, newEverythingDevice(t, root, "alice", store))
	b := named(t, newEverythingDevice(t, root, "matthew", store))
	foldersOn("repo", a, b)
	repoA, repoB := twoClones(t, a, b)

	// Alice has uncommitted work in x.py and syncs first. Bob's clone is
	// fresh, with work of his own in y.py and a build of his own.
	a.write("code/repo/x.py", "x = 3  # alice, not committed\n")
	a.write("code/repo/dist/app.js", "alice's build\n")
	a.sync()
	b.write("code/repo/y.py", "y = 2  # bob, not committed\n")
	b.write("code/repo/dist/app.js", "bob's build\n")
	b.sync()

	if got := b.read("code/repo/x.py"); got != "x = 3  # alice, not committed\n" {
		t.Fatalf("an untouched clone file should take alice's edit: %q", got)
	}
	if b.exists("code/repo/x.py.from-matthew-s-mac") || len(asideCopies(b, "code/repo/x.py")) != 1 {
		t.Fatal("git holds bob's untouched x.py: it goes to the conflicts folder, not the repo")
	}
	if got := b.read("code/repo/y.py"); got != "y = 1\n" {
		t.Fatalf("y.py should be the synced copy: %q", got)
	}
	if got := b.read("code/repo/y.py.from-matthew-s-mac"); got != "y = 2  # bob, not committed\n" {
		t.Fatalf("bob's edit should sit beside it: %q", got)
	}
	if b.exists("code/repo/dist/app.js.from-matthew-s-mac") || len(asideCopies(b, "code/repo/dist/app.js")) != 1 {
		t.Fatal("an ignored build file goes to the conflicts folder")
	}
	git(t, repoB, "fsck", "--strict")

	syncAll(1, b, a)
	if got := a.read("code/repo/y.py.from-matthew-s-mac"); got != "y = 2  # bob, not committed\n" {
		t.Fatalf("alice should see bob's edit: %q", got)
	}
	git(t, repoA, "fsck", "--strict")
	for _, repo := range []string{repoA, repoB} {
		if st := git(t, repo, "status", "--porcelain", "--untracked-files=no"); st != " M x.py\n" {
			t.Fatalf("%s: alice's edit should be the one uncommitted change: %q", repo, st)
		}
	}
}

func TestFirstContactKeepsTheJoiningMacsCommits(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := named(t, newEverythingDevice(t, root, "alice", store))
	b := named(t, newEverythingDevice(t, root, "matthew", store))
	foldersOn("repo", a, b)
	repoA, repoB := twoClones(t, a, b)

	// Bob committed a change alice never saw, then alice's copy syncs first.
	b.write("code/repo/x.py", "x = 9  # bob's commit\n")
	git(t, repoB, "commit", "-q", "-am", "third, on bob")
	bobSHA := strings.TrimSpace(git(t, repoB, "rev-parse", "HEAD"))
	a.sync()
	b.sync()
	syncAll(1, b, a)

	for _, repo := range []string{repoA, repoB} {
		git(t, repo, "fsck", "--strict")
		if log := git(t, repo, "log", "-1", "--format=%s"); log != "second\n" {
			t.Fatalf("%s: main should follow the synced copy: %q", repo, log)
		}
		if st := git(t, repo, "status", "--porcelain", "--untracked-files=no"); st != "" {
			t.Fatalf("%s: HEAD, index and files should agree: %q", repo, st)
		}
		kept := git(t, repo, "branch", "--list", "--format=%(refname:short) %(objectname)", keptBranchPrefix+"*")
		if kept != keptBranchPrefix+"main-"+bobSHA[:7]+" "+bobSHA+"\n" {
			t.Fatalf("%s: bob's commit should be on a kept branch: %q", repo, kept)
		}
	}
}

func TestFirstContactBehindCloneNeedsNoKeptBranch(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	foldersOn("repo", a, b)
	repoA, repoB := twoClones(t, a, b)

	// Alice moves on after bob cloned, with git gc packing her refs.
	a.write("code/repo/y.py", "y = 5\n")
	git(t, repoA, "commit", "-q", "-am", "third, on alice")
	git(t, repoA, "gc", "-q")
	a.sync()
	b.sync()

	git(t, repoB, "fsck", "--strict")
	if log := git(t, repoB, "log", "-1", "--format=%s"); log != "third, on alice\n" {
		t.Fatalf("bob should be on alice's commit: %q", log)
	}
	if st := git(t, repoB, "status", "--porcelain", "--untracked-files=no"); st != "" {
		t.Fatalf("bob's checkout should be clean: %q", st)
	}
	if branches := git(t, repoB, "branch", "--list", keptBranchPrefix+"*"); branches != "" {
		t.Fatalf("bob's commits are all in alice's history: %q", branches)
	}
}

func TestDivergedBranchKeepsBothCommits(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	foldersOn("repo", a, b)
	repoA, _ := twoClones(t, a, b)
	if err := os.RemoveAll(filepath.Join(b.home, "code", "repo")); err != nil {
		t.Fatal(err)
	}
	syncAll(1, a, b)
	repoB := filepath.Join(b.home, "code", "repo")

	// Both commit on main between syncs.
	a.write("code/repo/x.py", "x = 'alice'\n")
	git(t, repoA, "commit", "-q", "-am", "alice's commit")
	aliceSHA := strings.TrimSpace(git(t, repoA, "rev-parse", "HEAD"))
	for _, rel := range []string{"code/repo/x.py", "code/repo/.git/index", "code/repo/.git/refs/heads/main", "code/repo/.git/logs/HEAD", "code/repo/.git/logs/refs/heads/main", "code/repo/.git/ORIG_HEAD"} {
		if a.exists(rel) {
			backdate(t, a, rel, time.Hour)
		}
	}
	b.write("code/repo/y.py", "y = 'bob'\n")
	git(t, repoB, "commit", "-q", "-am", "bob's commit")
	a.sync()
	b.sync() // keeps alice's commit as a branch, uploaded on its next pass
	syncAll(1, b, a)

	for _, repo := range []string{repoA, repoB} {
		git(t, repo, "fsck", "--strict")
		all := git(t, repo, "log", "--all", "--format=%s")
		if !strings.Contains(all, "alice's commit") || !strings.Contains(all, "bob's commit") {
			t.Fatalf("%s: a commit became unreachable: %q", repo, all)
		}
		if head := git(t, repo, "log", "-1", "--format=%s"); head != "bob's commit\n" {
			t.Fatalf("%s: main should hold the newer commit: %q", repo, head)
		}
		kept := git(t, repo, "branch", "--list", "--format=%(refname:short) %(objectname)", keptBranchPrefix+"*")
		if kept != keptBranchPrefix+"main-"+aliceSHA[:7]+" "+aliceSHA+"\n" {
			t.Fatalf("%s: alice's commit should be on a kept branch: %q", repo, kept)
		}
	}
}

func TestGitMetadata(t *testing.T) {
	for p, want := range map[string]bool{
		"_folders/code/r/.git/index":               true,
		"_folders/code/r/.git/refs/heads/main":     true,
		"_folders/code/r/.git/objects/ab/cdef":     false,
		"_folders/code/r/.git/objects/pack/p.idx":  false,
		"_folders/code/r/src/main.go":              false,
		".git/HEAD":                                true,
		"_folders/code/r/.git/modules/m/objects/x": false,
	} {
		if got := gitMetadata(p); got != want {
			t.Errorf("gitMetadata(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestRemoteControlledRecordStaysOffOtherMacs(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	b := newDevice(t, root, "matthew", store)
	now := time.Now()
	cwd := filepath.Join(a.home, "code")
	record := func(rc bool, at time.Time) string {
		return fmt.Sprintf(`{"sessionId":"local_s1","cwd":%q,"title":"phone","lastActivityAt":%d,"remoteControlUserEnabled":%v,"bridgeSessionIds":["bridge-1"]}`, cwd, at.UnixMilli(), rc)
	}
	write := func(d *device, content string) {
		if err := os.WriteFile(d.recordPath("s1"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(d *device) bool { _, err := os.Stat(d.recordPath("s1")); return err == nil }

	// Alice serves the session to her phone: bob's app already lists it.
	write(a, record(true, now))
	a.sync()
	r := b.sync()
	if exists(b) || len(r.NewDesktopSessions) != 0 || len(r.RemoteControlled) != 1 {
		t.Fatalf("a remote-controlled record must stay off bob: %+v", r)
	}
	if r := b.sync(); len(r.Downloaded)+len(r.RemoteControlled) != 0 {
		t.Fatalf("an unchanged held record isn't looked at again: %+v", r)
	}

	// Remote Control off: the record arrives, without alice's bridge.
	write(a, record(false, now.Add(time.Minute)))
	a.sync()
	r = b.sync()
	if !exists(b) || len(r.NewDesktopSessions) != 1 {
		t.Fatalf("the record should arrive once Remote Control is off: %+v", r)
	}
	if data, _ := os.ReadFile(b.recordPath("s1")); strings.Contains(string(data), "bridge-1") {
		t.Fatalf("alice's bridge reached bob: %s", data)
	}

	// On again: bob's copy goes to the trash rather than showing twice.
	write(a, record(true, now.Add(2*time.Minute)))
	a.sync()
	if r := b.sync(); exists(b) || len(r.RemoteControlled) != 1 {
		t.Fatalf("bob's copy should be set aside: %+v", r)
	}
	// Bob's app re-saves the copy it holds in memory: it must not replace
	// alice's live record.
	write(b, record(false, now.Add(-time.Hour)))
	b.sync()
	if exists(b) {
		t.Fatal("the re-saved copy should be set aside again")
	}
	if r := a.sync(); len(r.Downloaded) != 0 {
		t.Fatalf("bob's stale copy reached alice: %+v", r)
	}
	if data, _ := os.ReadFile(a.recordPath("s1")); !strings.Contains(string(data), `"remoteControlUserEnabled":true`) {
		t.Fatalf("alice's record changed: %s", data)
	}
}

func TestCommitAfterGCReachesTheOtherMac(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	foldersOn("repo", a, b)
	repoA, _ := twoClones(t, a, b)
	if err := os.RemoveAll(filepath.Join(b.home, "code", "repo")); err != nil {
		t.Fatal(err)
	}
	syncAll(1, a, b)
	repoB := filepath.Join(b.home, "code", "repo")

	// gc packs main; the next commit writes a loose main that storage has
	// never seen, while storage's packed-refs still holds the old commit.
	git(t, repoA, "gc", "-q")
	syncAll(1, a, b)
	a.write("code/repo/y.py", "y = 7\n")
	git(t, repoA, "commit", "-q", "-am", "after gc")
	syncAll(2, a, b)

	for _, repo := range []string{repoA, repoB} {
		git(t, repo, "fsck", "--strict")
		if log := git(t, repo, "log", "-1", "--format=%s"); log != "after gc\n" {
			t.Fatalf("%s: main should be on the new commit: %q", repo, log)
		}
		if branches := git(t, repo, "branch", "--list", keptBranchPrefix+"*"); branches != "" {
			t.Fatalf("%s: nothing diverged: %q", repo, branches)
		}
	}
}

func TestCommitThenStatusElsewhereKeepsIndexesHonest(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	foldersOn("repo", a, b)
	repoA, _ := twoClones(t, a, b)
	if err := os.RemoveAll(filepath.Join(b.home, "code", "repo")); err != nil {
		t.Fatal(err)
	}
	syncAll(2, a, b)
	repoB := filepath.Join(b.home, "code", "repo")

	a.write("code/repo/x.py", "x = 'c3'\n")
	git(t, repoA, "commit", "-q", "-am", "c3")
	git(t, repoB, "status") // a shell prompt rewrites bob's index
	syncAll(2, a, b)
	for _, repo := range []string{repoA, repoB} {
		if st := git(t, repo, "status", "--porcelain"); st != "" {
			t.Fatalf("%s: index and HEAD disagree: %q", repo, st)
		}
	}
	// Bob's next commit must not revert alice's.
	b.write("code/repo/z.py", "z = 1\n")
	git(t, repoB, "add", "z.py")
	git(t, repoB, "commit", "-q", "-m", "z")
	if got := git(t, repoB, "show", "HEAD:x.py"); got != "x = 'c3'\n" {
		t.Fatalf("bob's commit reverted x.py: %q", got)
	}
}

func TestVanishedFolderIsNotDeletedEverywhere(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	foldersOn("p", a, b)
	a.write("code/p/one.txt", "1\n")
	a.write("code/p/two.txt", "2\n")
	syncAll(1, a, b)

	if err := os.Rename(filepath.Join(a.home, "code", "p"), filepath.Join(a.home, "code", "p-renamed")); err != nil {
		t.Fatal(err)
	}
	r := a.sync()
	if len(r.Deleted) != 0 || len(r.Warnings) == 0 {
		t.Fatalf("a vanished folder must warn, not delete: %+v", r)
	}
	b.sync()
	if b.read("code/p/one.txt") != "1\n" || len(b.trashed()) != 0 {
		t.Fatal("bob lost the folder")
	}
}

func TestSymlinkedDotfileInClaudeIsLeftAlone(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	a.write(".claude/CLAUDE.md", "alice's rules\n")
	a.write(".claude/skills/s/SKILL.md", "alice's skill\n")
	a.sync()

	dot := filepath.Join(b.home, "dotfiles")
	b.write("dotfiles/CLAUDE.md", "bob's rules\n")
	if err := os.MkdirAll(filepath.Join(dot, "skills"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, l := range [][2]string{{"CLAUDE.md", "CLAUDE.md"}, {"skills", "skills"}} {
		if err := os.Symlink(filepath.Join(dot, l[0]), filepath.Join(b.claude, l[1])); err != nil {
			t.Fatal(err)
		}
	}
	b.sync()
	b.sync()
	if fi, err := os.Lstat(filepath.Join(b.claude, "CLAUDE.md")); err != nil || !isSymlink(fi) {
		t.Fatal("bob's CLAUDE.md link was replaced")
	}
	if b.read("dotfiles/CLAUDE.md") != "bob's rules\n" || b.exists("dotfiles/skills/s/SKILL.md") {
		t.Fatal("sync wrote through bob's links")
	}
	if r := a.sync(); len(r.Trashed) != 0 || a.read(".claude/skills/s/SKILL.md") != "alice's skill\n" {
		t.Fatalf("alice lost her skill: %+v", r)
	}
}

func TestRemoteControlHoldSurvivesAMerge(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newDevice(t, root, "alice", store)
	b := newDevice(t, root, "matthew", store)
	now := time.Now()
	rec := func(rc bool, at time.Time, who string) string {
		return fmt.Sprintf(`{"sessionId":"local_s1","cwd":%q,"title":%q,"lastActivityAt":%d,"remoteControlUserEnabled":%v,"bridgeSessionIds":["b"]}`, filepath.Join(a.home, "code"), who, at.UnixMilli(), rc)
	}
	put := func(d *device, c string) {
		if err := os.WriteFile(d.recordPath("s1"), []byte(c), 0600); err != nil {
			t.Fatal(err)
		}
	}
	put(a, rec(false, now, "alice"))
	syncAll(1, a, b)
	put(a, rec(true, now.Add(time.Minute), "alice"))
	a.sync()
	b.sync() // bob's copy goes to the trash
	put(a, rec(true, now.Add(2*time.Minute), "alice"))
	a.sync()
	put(b, rec(false, now.Add(time.Hour), "bob re-saved")) // newer stamp than alice's
	b.sync()
	a.sync()
	if data, _ := os.ReadFile(a.recordPath("s1")); !strings.Contains(string(data), `"remoteControlUserEnabled":true`) {
		t.Fatalf("bob's copy replaced alice's live record: %s", data)
	}
	if _, err := os.Stat(b.recordPath("s1")); err == nil {
		t.Fatal("bob shows the session twice")
	}
}

func TestNewFileDownloadNeverReplacesAFileCreatedMeanwhile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(p, []byte("made here during the pass\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeLocalPerm(p, []byte("from the other Mac\n"), nil, 0644, dir); err != errLocalChanged {
		t.Fatalf("want errLocalChanged, got %v", err)
	}
	if data, _ := os.ReadFile(p); string(data) != "made here during the pass\n" {
		t.Fatalf("file replaced: %q", data)
	}
}

func TestProtectedFolderWaitsForMacOSWithoutStallingTheSync(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS privacy prompts")
	}
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.write("Desktop/site/index.html", "<h1>hi</h1>\n")
	a.s.cfg.ExtraFolders = []string{filepath.Join(a.home, "Desktop", "site")}
	a.writeSession("proj", "s1", a.line("proj", "u1", "", time.Now(), "hello"))

	answer := make(chan error)
	oldRead, oldWait := tccRead, tccWait
	tccRead = func(dir string) error { return <-answer }
	tccWait = 50 * time.Millisecond
	defer func() { tccRead, tccWait = oldRead, oldWait }()
	tccMu.Lock()
	tccProbes = map[string]*tccProbe{}
	tccMu.Unlock()

	r := a.sync() // macOS's question is unanswered
	if len(keysWithPrefix(store, FoldersPrefix+"Desktop/")) != 0 {
		t.Fatal("read a protected folder before macOS answered")
	}
	if len(keysWithPrefix(store, "projects/")) == 0 {
		t.Fatal("the session didn't sync while the folder waited")
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "Desktop") {
		t.Fatalf("want one Desktop warning: %v", r.Warnings)
	}
	close(answer) // Allow
	time.Sleep(20 * time.Millisecond)
	r = a.sync()
	if len(r.Warnings) != 0 || len(keysWithPrefix(store, FoldersPrefix+"Desktop/site/")) == 0 {
		t.Fatalf("the folder should sync once allowed: %+v", r)
	}
}
