package sync

import (
	"fmt"
	"os"
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

func TestFirstContactKeepsThisMacsCopyInAPlainFolder(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	foldersOn("p", a, b)

	a.write("code/p/notes.txt", "alice's notes\n")
	a.write("code/p/only-alice.txt", "a\n")
	a.sync()
	// Bob had the folder before syncing it, with an older copy of notes.txt:
	// newest-wins would replace it, first contact must not.
	b.write("code/p/notes.txt", "bob's notes\n")
	b.write("code/p/only-bob.txt", "b\n")
	backdate(t, b, "code/p/notes.txt", time.Hour)
	b.sync()

	if got := b.read("code/p/notes.txt"); got != "bob's notes\n" {
		t.Fatalf("bob's own copy was replaced: %q", got)
	}
	copies := conflictCopies(t, filepath.Join(b.home, "code", "p"), "notes.txt")
	if len(copies) != 1 {
		t.Fatalf("alice's copy should sit beside bob's: %v", copies)
	}
	if data, _ := os.ReadFile(copies[0]); string(data) != "alice's notes\n" {
		t.Fatalf("conflict copy = %q", data)
	}
	if b.read("code/p/only-alice.txt") != "a\n" {
		t.Fatal("a file only alice had didn't reach bob")
	}
	a.sync()
	if a.read("code/p/notes.txt") != "bob's notes\n" || a.read("code/p/only-bob.txt") != "b\n" {
		t.Fatal("the Macs didn't converge after first contact")
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

func TestFirstContactCloneTakesTheOtherMacsEdits(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	foldersOn("repo", a, b)
	repoA, repoB := twoClones(t, a, b)

	// Alice has uncommitted work in x.py and syncs first. Bob's clone is
	// fresh (and newer), with work of his own in y.py.
	a.write("code/repo/x.py", "x = 3  # alice, not committed\n")
	backdate(t, a, "code/repo/x.py", time.Hour)
	a.sync()
	b.write("code/repo/y.py", "y = 2  # bob, not committed\n")
	b.sync()

	if got := b.read("code/repo/x.py"); got != "x = 3  # alice, not committed\n" {
		t.Fatalf("an untouched clone file should take alice's edit: %q", got)
	}
	if c := conflictCopies(t, repoB, "x.py"); len(c) != 0 {
		t.Fatalf("git still holds bob's untouched x.py, no copy belongs in the repo: %v", c)
	}
	if c := asideCopies(b, "code/repo/x.py"); len(c) != 1 {
		t.Fatalf("bob's x.py should be kept in the conflicts folder: %v", c)
	}
	if got := b.read("code/repo/y.py"); got != "y = 2  # bob, not committed\n" {
		t.Fatalf("bob's own edit was replaced: %q", got)
	}
	if c := conflictCopies(t, repoB, "y.py"); len(c) != 1 {
		t.Fatalf("alice's y.py should sit beside bob's: %v", c)
	}
	git(t, repoB, "fsck", "--strict")

	a.sync()
	if got := a.read("code/repo/y.py"); got != "y = 2  # bob, not committed\n" {
		t.Fatalf("alice didn't get bob's edit: %q", got)
	}
	git(t, repoA, "fsck", "--strict")
	for _, repo := range []string{repoA, repoB} {
		st := git(t, repo, "status", "--porcelain", "--untracked-files=no")
		if !strings.Contains(st, " M x.py") || !strings.Contains(st, " M y.py") {
			t.Fatalf("%s: both edits should show as uncommitted changes: %q", repo, st)
		}
	}
}

func TestFirstContactNeverRollsBackAMacThatIsAhead(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	foldersOn("repo", a, b)
	repoA, repoB := twoClones(t, a, b)

	// Bob committed a change alice never saw; alice's copy of the file is the
	// older commit (untouched on her side) and syncs first.
	b.write("code/repo/x.py", "x = 9  # bob's commit\n")
	git(t, repoB, "commit", "-q", "-am", "third, on bob")
	backdate(t, b, "code/repo/x.py", time.Hour)
	for _, rel := range []string{"code/repo/.git/index", "code/repo/.git/refs/heads/main", "code/repo/.git/HEAD"} {
		backdate(t, b, rel, time.Hour)
	}
	a.sync()
	b.sync()

	if got := b.read("code/repo/x.py"); got != "x = 9  # bob's commit\n" {
		t.Fatalf("bob's committed file was rolled back: %q", got)
	}
	if log := git(t, repoB, "log", "-1", "--format=%s"); log != "third, on bob\n" {
		t.Fatalf("bob's branch moved: %q", log)
	}
	git(t, repoB, "fsck", "--strict")

	a.sync()
	git(t, repoA, "fsck", "--strict")
	if log := git(t, repoA, "log", "-1", "--format=%s"); log != "third, on bob\n" {
		t.Fatalf("alice should fast-forward to bob's commit: %q", log)
	}
	if st := git(t, repoA, "status", "--porcelain", "--untracked-files=no"); st != "" {
		t.Fatalf("alice's checkout should be clean: %q", st)
	}
}

func TestBehindCloneFastForwards(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	foldersOn("repo", a, b)
	repoA, repoB := twoClones(t, a, b)

	// Alice moves on after bob cloned; bob's untouched files and refs are
	// newer on disk but behind in history.
	a.write("code/repo/y.py", "y = 5\n")
	git(t, repoA, "commit", "-q", "-am", "third, on alice")
	for _, rel := range []string{"code/repo/y.py", "code/repo/.git/index", "code/repo/.git/refs/heads/main"} {
		backdate(t, a, rel, time.Hour)
	}
	a.sync()
	r := b.sync()

	git(t, repoB, "fsck", "--strict")
	if log := git(t, repoB, "log", "-1", "--format=%s"); log != "third, on alice\n" {
		t.Fatalf("bob should fast-forward: %q (%+v)", log, r)
	}
	if st := git(t, repoB, "status", "--porcelain", "--untracked-files=no"); st != "" {
		t.Fatalf("bob's checkout should be clean: %q", st)
	}
	if branches := git(t, repoB, "branch", "--list", keptBranchPrefix+"*"); branches != "" {
		t.Fatalf("a fast-forward loses nothing and needs no kept branch: %q", branches)
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
