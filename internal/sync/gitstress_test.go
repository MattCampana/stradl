package sync

// Stress tests: real git repositories on two Macs, synced file by
// file. Each test checks git fsck --strict on both Macs, that every commit a
// Mac made stays reachable from some ref (kept stradl/ branches count), and
// that no uncommitted edit vanished from every Mac.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tawanorg/claude-sync/internal/storage"
)

func gsRun(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main", "-c", "core.fsmonitor=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME="+dir), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func gsGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gsRun(dir, nil, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return out
}

func gsSHA(t *testing.T, dir, rev string) string {
	t.Helper()
	return strings.TrimSpace(gsGit(t, dir, "rev-parse", rev))
}

// gsSync runs passes in order, reporting (not aborting on) sync errors.
func gsSync(t *testing.T, rounds int, devs ...*device) {
	t.Helper()
	for i := 0; i < rounds; i++ {
		for _, d := range devs {
			r, err := d.s.Sync(context.Background())
			if err != nil {
				t.Errorf("%s: sync: %v", d.name, err)
				continue
			}
			if len(r.Errors) > 0 {
				t.Errorf("%s: sync errors: %v", d.name, r.Errors)
			}
		}
	}
}

type gsWorld struct {
	t            *testing.T
	root         string
	store        *casStore
	a, b         *device
	repoA, repoB string
}

// gsShared: alice makes code/repo (c0, c1), bob receives it through sync.
func gsShared(t *testing.T, bobStore func(*casStore) storage.Storage) *gsWorld {
	t.Helper()
	needGit(t)
	w := &gsWorld{t: t, root: t.TempDir(), store: newCASStore()}
	w.a = gsDevice(t, w.root, "alice", w.store)
	var bs storage.Storage = w.store
	if bobStore != nil {
		bs = bobStore(w.store)
	}
	w.b = gsDevice(t, w.root, "matthew", bs)
	foldersOn("repo", w.a, w.b)
	w.repoA = filepath.Join(w.a.home, "code", "repo")
	w.repoB = filepath.Join(w.b.home, "code", "repo")
	if err := os.MkdirAll(w.repoA, 0755); err != nil {
		t.Fatal(err)
	}
	gsGit(t, w.repoA, "init", "-q")
	for name, content := range map[string]string{"x.py": "x = 0\n", "y.py": "y = 0\n", "f.txt": "f base\n", "h.txt": "h base\n"} {
		w.a.write("code/repo/"+name, content)
	}
	gsGit(t, w.repoA, "add", "-A")
	gsGit(t, w.repoA, "commit", "-q", "-m", "c0")
	w.a.write("code/repo/x.py", "x = 1\n")
	gsGit(t, w.repoA, "commit", "-q", "-am", "c1")
	gsSync(t, 2, w.a, w.b)
	if gsSHA(t, w.repoB, "HEAD") != gsSHA(t, w.repoA, "HEAD") {
		t.Fatal("setup: bob didn't get alice's repo")
	}
	return w
}

// check runs fsck and reachability on both Macs, and logs status.
func (w *gsWorld) check(label string, commits map[string]string) {
	w.t.Helper()
	gsCheckRepos(w.t, label, map[string]string{"alice": w.repoA, "bob": w.repoB}, commits)
}

func gsCheckRepos(t *testing.T, label string, repos map[string]string, commits map[string]string) {
	t.Helper()
	for name, dir := range repos {
		if out, err := gsRun(dir, nil, "fsck", "--strict", "--no-dangling", "--no-progress"); err != nil {
			t.Errorf("[%s] %s: git fsck --strict: %v\n%s", label, name, err, out)
		}
		all, _ := gsRun(dir, nil, "rev-list", "--all")
		reach := map[string]bool{}
		for _, l := range strings.Fields(all) {
			reach[l] = true
		}
		refs, _ := gsRun(dir, nil, "for-each-ref", "--format=%(refname) %(objectname:short)")
		for cname, sha := range commits {
			if !reach[sha] {
				t.Errorf("[%s] %s: commit %q %s unreachable from every ref; refs:\n%s", label, name, cname, sha[:7], refs)
			}
		}
		st, _ := gsRun(dir, []string{"GIT_OPTIONAL_LOCKS=0"}, "status", "--porcelain=v1", "-b")
		t.Logf("[%s] %s status:\n%s", label, name, st)
	}
}

// staged reports changes between HEAD and the index nobody staged.
func (w *gsWorld) noPhantomStaging(label string) {
	w.t.Helper()
	for name, dir := range map[string]string{"alice": w.repoA, "bob": w.repoB} {
		out, err := gsRun(dir, []string{"GIT_OPTIONAL_LOCKS=0"}, "diff", "--cached", "--name-status", "HEAD")
		if err != nil {
			w.t.Errorf("[%s] %s: diff --cached: %v %s", label, name, err, out)
		} else if strings.TrimSpace(out) != "" {
			w.t.Errorf("[%s] %s: index differs from HEAD although nobody staged anything (index and HEAD from different Macs):\n%s", label, name, out)
		}
	}
}

// gsDevice is newEverythingDevice with its own device name (the .from- slug).
func gsDevice(t *testing.T, root, name string, store storage.Storage) *device {
	t.Helper()
	d := newEverythingDevice(t, root, name, store)
	if err := os.WriteFile(filepath.Join(d.home, ".claude-sync", "device-name"), []byte(name+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return d
}

// gsKept reports whether content survives at rel, or as a conflict copy beside
// it or in the conflicts folder, on any device.
func gsKept(t *testing.T, rel, content string, devs ...*device) bool {
	for _, d := range devs {
		if d.read(rel) == content {
			return true
		}
		abs := filepath.Join(d.home, filepath.FromSlash(rel))
		copies := append(conflictCopies(t, filepath.Dir(abs), filepath.Base(abs)), asideCopies(d, rel)...)
		shared, _ := filepath.Glob(abs + ".from-*")
		copies = append(copies, shared...)
		for _, c := range copies {
			if data, _ := os.ReadFile(c); string(data) == content {
				return true
			}
		}
	}
	return false
}

// --- 1. git gc ---------------------------------------------------------------

// 1a: alice commits (loose ref + objects); bob commits and runs git gc
// (packs objects, packs refs, deletes loose refs).
func TestGS1aGcOnOneMacWhileOtherHasLooseRefs(t *testing.T) {
	for _, aliceFirst := range []bool{true, false} {
		name := "alice-syncs-first"
		if !aliceFirst {
			name = "bob-syncs-first"
		}
		t.Run(name, func(t *testing.T) {
			w := gsShared(t, nil)
			a, b := w.a, w.b
			a.write("code/repo/x.py", "x = 'alice'\n")
			gsGit(t, w.repoA, "commit", "-q", "-am", "alice c2a")
			c2a := gsSHA(t, w.repoA, "HEAD")
			b.write("code/repo/y.py", "y = 'bob'\n")
			gsGit(t, w.repoB, "commit", "-q", "-am", "bob c2b")
			c2b := gsSHA(t, w.repoB, "HEAD")
			gsGit(t, w.repoB, "gc", "-q")
			if b.exists("code/repo/.git/refs/heads/main") {
				t.Log("gc kept bob's loose main")
			}
			if aliceFirst {
				gsSync(t, 3, a, b)
			} else {
				gsSync(t, 3, b, a)
			}
			w.check("1a", map[string]string{"c2a": c2a, "c2b": c2b})
		})
	}
}

// 1c: alice amends c1 and prunes the old commit right away; bob had
// committed on top of the old c1.
func TestGS1cPruneOnOneMacWhileOtherBuildsOnPrunedCommit(t *testing.T) {
	w := gsShared(t, nil)
	a, b := w.a, w.b
	a.write("code/repo/x.py", "x = 'amended'\n")
	gsGit(t, w.repoA, "commit", "-q", "--amend", "-am", "c1 amended")
	c1p := gsSHA(t, w.repoA, "HEAD")
	gsGit(t, w.repoA, "reflog", "expire", "--expire=now", "--all")
	gsGit(t, w.repoA, "gc", "-q", "--prune=now")
	b.write("code/repo/y.py", "y = 'bob on c1'\n")
	gsGit(t, w.repoB, "commit", "-q", "-am", "bob c2 on old c1")
	c2b := gsSHA(t, w.repoB, "HEAD")
	gsSync(t, 3, a, b)
	w.check("1c", map[string]string{"c1-amended": c1p, "bob-c2": c2b})
}

// gsFlaky fails downloads of pack files while fail is set (network drop).
type gsFlaky struct {
	*casStore
	fail *atomic.Bool
}

func (f gsFlaky) DownloadWithETag(ctx context.Context, key string) ([]byte, string, error) {
	if f.fail.Load() && strings.Contains(key, "/objects/pack/") {
		return nil, "", errors.New("connection reset")
	}
	return f.casStore.DownloadWithETag(ctx, key)
}

func (f gsFlaky) Download(ctx context.Context, key string) ([]byte, error) {
	d, _, err := f.DownloadWithETag(ctx, key)
	return d, err
}

// 1d: alice gc's; bob's next pass loses the network while downloading the
// pack. Loose objects shouldn't be trashed before the pack holding them lands.
func TestGS1dInterruptedPassAfterGc(t *testing.T) {
	fail := &atomic.Bool{}
	w := gsShared(t, func(c *casStore) storage.Storage { return gsFlaky{c, fail} })
	gsGit(t, w.repoA, "gc", "-q")
	gsSync(t, 1, w.a)
	fail.Store(true)
	r, err := w.b.s.Sync(context.Background())
	fail.Store(false)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("bob's failing pass: trashed %d, errors %d", len(r.Trashed), len(r.Errors))
	// Isolate from the pruned-.git/refs bug (TestGS1e): put the folder back.
	_ = os.MkdirAll(filepath.Join(w.repoB, ".git", "refs", "heads"), 0755)
	if out, err := gsRun(w.repoB, nil, "fsck", "--strict", "--no-progress"); err != nil {
		t.Errorf("after an interrupted pass bob's repository is broken until the next one:\n%s", firstLines(out, 5))
	}
	gsSync(t, 1, w.b)
	_ = os.MkdirAll(filepath.Join(w.repoB, ".git", "refs", "heads"), 0755)
	w.check("1d-after-retry", nil)
}

// 1e: alice gc's (every ref packed, refs/ holds only empty folders); a Mac
// that never had the repository receives it.
func TestGS1eNewMacReceivesGcdRepo(t *testing.T) {
	w := gsShared(t, nil)
	gsGit(t, w.repoA, "gc", "-q")
	gsSync(t, 2, w.a, w.b)
	c := gsDevice(t, w.root, "carol", w.store)
	foldersOn("repo", c)
	gsSync(t, 1, c)
	repoC := filepath.Join(c.home, "code", "repo")
	if _, err := os.Stat(filepath.Join(repoC, ".git", "refs")); err != nil {
		t.Errorf("carol's .git/refs is missing: %v", err)
	}
	gsCheckRepos(t, "1e", map[string]string{"alice": w.repoA, "bob": w.repoB, "carol": repoC}, nil)
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// --- 3. checkout vs uncommitted edit -----------------------------------------------

func TestGS3CheckoutOnOneMacWhileOtherEdits(t *testing.T) {
	w := gsShared(t, nil)
	a, b := w.a, w.b
	gsGit(t, w.repoA, "checkout", "-q", "-b", "feature")
	a.write("code/repo/f.txt", "f on feature\n")
	a.write("code/repo/g.txt", "g only on feature\n")
	a.remove("code/repo/h.txt")
	gsGit(t, w.repoA, "add", "-A")
	gsGit(t, w.repoA, "commit", "-q", "-m", "feature")
	feat := gsSHA(t, w.repoA, "HEAD")
	gsGit(t, w.repoA, "checkout", "-q", "main")
	gsSync(t, 2, a, b)

	gsGit(t, w.repoA, "checkout", "-q", "feature")
	b.write("code/repo/f.txt", "f: bob's uncommitted edit\n")
	b.write("code/repo/h.txt", "h: bob's uncommitted edit\n")
	_, _ = gsRun(w.repoB, nil, "status") // an editor refreshes the index
	gsSync(t, 3, a, b)

	for _, e := range [][2]string{{"code/repo/f.txt", "f: bob's uncommitted edit\n"}, {"code/repo/h.txt", "h: bob's uncommitted edit\n"}} {
		if !gsKept(t, e[0], e[1], a, b) {
			t.Errorf("bob's uncommitted edit to %s vanished from every Mac", e[0])
		}
	}
	for name, dir := range map[string]string{"alice": w.repoA, "bob": w.repoB} {
		head, _ := gsRun(dir, nil, "symbolic-ref", "-q", "HEAD")
		t.Logf("%s HEAD -> %s", name, strings.TrimSpace(head))
	}
	w.check("3", map[string]string{"feature": feat})
	w.noPhantomStaging("3")
}

// --- 5. branch deletes ----------------------------------------------------------

// 5c: alice deletes topic while bob checks it out.
func TestGS5cDeleteBranchOtherMacJustCheckedOut(t *testing.T) {
	w := gsShared(t, nil)
	a, b := w.a, w.b
	gsGit(t, w.repoA, "checkout", "-q", "-b", "topic")
	a.write("code/repo/t.txt", "topic\n")
	gsGit(t, w.repoA, "add", "-A")
	gsGit(t, w.repoA, "commit", "-q", "-m", "topic")
	gsGit(t, w.repoA, "checkout", "-q", "main")
	gsSync(t, 2, a, b)

	gsGit(t, w.repoA, "branch", "-D", "topic")
	gsGit(t, w.repoB, "checkout", "-q", "topic")
	gsSync(t, 3, a, b)
	for name, dir := range map[string]string{"alice": w.repoA, "bob": w.repoB} {
		head, _ := gsRun(dir, nil, "symbolic-ref", "-q", "HEAD")
		if out, err := gsRun(dir, nil, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
			t.Errorf("%s: HEAD (%s) no longer resolves: %s", name, strings.TrimSpace(head), out)
		}
	}
	w.check("5c", nil)
}

// --- 6. worktrees, submodules, detached HEAD --------------------------------------

// 6a: a linked worktree inside the repo, as Claude Code makes them.
func TestGS6aLinkedWorktreeInsideRepo(t *testing.T) {
	w := gsShared(t, nil)
	gsGit(t, w.repoA, "worktree", "add", "-q", ".claude/worktrees/wt", "-b", "wt")
	w.a.write("code/repo/.claude/worktrees/wt/notes.md", "work in the worktree\n")
	gsSync(t, 2, w.a, w.b)
	// The worktree's files arrive; its .git pointer (an absolute path into
	// alice's home) and the main repository's worktrees/ admin folder stay
	// on alice, so bob's copy is a plain folder and git on bob never prunes
	// alice's worktree.
	if got := w.b.read("code/repo/.claude/worktrees/wt/notes.md"); got != "work in the worktree\n" {
		t.Fatalf("the worktree's files didn't reach bob: %q", got)
	}
	if w.b.exists("code/repo/.claude/worktrees/wt/.git") || w.b.exists("code/repo/.git/worktrees") {
		t.Fatal("alice's worktree pointers reached bob")
	}
	_, _ = gsRun(w.repoB, nil, "worktree", "prune")
	gsSync(t, 2, w.b, w.a)
	if out, err := gsRun(filepath.Join(w.repoA, ".claude", "worktrees", "wt"), nil, "status", "--short"); err != nil {
		t.Fatalf("alice's worktree broke: %s", out)
	}
	w.check("6a", nil)
}

// 6c: both Macs commit on a detached HEAD (as in a submodule, or mid-rebase).
func TestGS6cDetachedHeadDiverges(t *testing.T) {
	w := gsShared(t, nil)
	gsGit(t, w.repoA, "checkout", "-q", "--detach")
	gsSync(t, 2, w.a, w.b)
	w.a.write("code/repo/x.py", "x = 'alice detached'\n")
	gsGit(t, w.repoA, "commit", "-q", "-am", "alice detached")
	da := gsSHA(t, w.repoA, "HEAD")
	w.b.write("code/repo/y.py", "y = 'bob detached'\n")
	gsGit(t, w.repoB, "commit", "-q", "-am", "bob detached")
	db := gsSHA(t, w.repoB, "HEAD")
	gsSync(t, 3, w.a, w.b)
	w.check("6c", map[string]string{"alice-detached": da, "bob-detached": db})
}

// --- 7. stash -------------------------------------------------------------------

func TestGS7StashOnBothMacs(t *testing.T) {
	w := gsShared(t, nil)
	w.a.write("code/repo/x.py", "x = 'alice wip'\n")
	gsGit(t, w.repoA, "stash", "-q")
	sa := gsSHA(t, w.repoA, "refs/stash")
	w.b.write("code/repo/y.py", "y = 'bob wip'\n")
	gsGit(t, w.repoB, "stash", "-q")
	sb := gsSHA(t, w.repoB, "refs/stash")
	gsSync(t, 3, w.a, w.b)
	for name, dir := range map[string]string{"alice": w.repoA, "bob": w.repoB} {
		list, _ := gsRun(dir, nil, "stash", "list")
		t.Logf("%s stash list:\n%s", name, list)
	}
	w.check("7", map[string]string{"alice-stash": sa, "bob-stash": sb})
}
