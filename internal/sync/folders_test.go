package sync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/storage"
)

// newEverythingDevice is newDevice under the "everything" scope: all of
// ~/.claude, MCP servers aside, plus project folders, with this device's home
// as the folder base.
func newEverythingDevice(t *testing.T, root, name string, store storage.Storage) *device {
	t.Helper()
	d := newDevice(t, root, name, store)
	d.s.cfg.Scope = config.ScopeEverything
	d.s.cfg.HomeDirOverride = d.home
	d.s.cfg.EncryptionKey = filepath.Join(d.home, ".claude-sync", "age-key.txt")
	d.s.ageID = loadAgeIdentity(d.s.cfg.EncryptionKey)
	if d.s.ageID == nil {
		t.Fatal("test key unreadable")
	}
	return d
}

// write creates a file at a path relative to the device's home.
func (d *device) write(rel, content string) {
	d.t.Helper()
	p := filepath.Join(d.home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		d.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		d.t.Fatal(err)
	}
}

// read returns a file relative to the device's home, or "" if missing.
func (d *device) read(rel string) string {
	b, err := os.ReadFile(filepath.Join(d.home, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	return string(b)
}

func (d *device) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(d.home, filepath.FromSlash(rel)))
	return err == nil
}

func (d *device) remove(rel string) {
	d.t.Helper()
	if err := os.Remove(filepath.Join(d.home, filepath.FromSlash(rel))); err != nil {
		d.t.Fatal(err)
	}
}

// trashed lists the files in the device's trash, relative to the day folder.
func (d *device) trashed() []string {
	var out []string
	trash := filepath.Join(d.home, ".claude-sync", "trash")
	_ = filepath.Walk(trash, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(trash, p)
		parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
		if len(parts) == 2 {
			out = append(out, parts[1])
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// syncAll syncs every device in turn, rounds times.
func syncAll(rounds int, devs ...*device) {
	for i := 0; i < rounds; i++ {
		for _, d := range devs {
			d.sync()
		}
	}
}

func keysWithPrefix(store *casStore, prefix string) []string {
	var out []string
	for _, k := range store.keys() {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

func TestEverythingNeverUploadsPerMacState(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	for _, rel := range []string{
		".claude/.credentials.json", ".claude/sessions/123.json", ".claude/session-env/x/env",
		".claude/shell-snapshots/snap.sh", ".claude/cache/c.json", ".claude/statsig/s",
		".claude/telemetry/t.json", ".claude/debug/d.txt", ".claude/logs/l.log", ".claude/ide/1.lock",
		".claude/backups/b.json", ".claude/todos/x.lock", ".claude/plugins/p/node_modules/m.js",
		".claude/plugins/p/.venv/bin/python", ".claude/skills/s/__pycache__/a.pyc", ".claude/.DS_Store",
	} {
		a.write(rel, "secret or per-Mac\n")
	}
	keep := []string{
		".claude/settings.json", ".claude/CLAUDE.md", ".claude/skills/s/SKILL.md", ".claude/agents/a.md",
		".claude/commands/c.md", ".claude/plugins/installed_plugins.json", ".claude/plugins/p/plugin.json",
		".claude/file-history/x/1", ".claude/todos/t.json", ".claude/keybindings.json", ".claude/output-styles/o.md",
	}
	for _, rel := range keep {
		a.write(rel, "{}\n")
	}
	a.sync()
	keys := store.keys()
	for _, k := range keys {
		for _, bad := range []string{"credentials", "sessions/", "session-env", "shell-snapshots", "cache/", "statsig", "telemetry", "debug", "logs", "ide/", "backups", ".lock", "node_modules", ".venv", "__pycache__", ".DS_Store"} {
			if strings.Contains(k, bad) {
				t.Errorf("per-Mac file uploaded: %s", k)
			}
		}
	}
	for _, rel := range keep {
		want := strings.TrimPrefix(rel, ".claude/") + ".age"
		found := false
		for _, k := range keys {
			found = found || k == want
		}
		if !found {
			t.Errorf("%s not uploaded (keys: %v)", rel, keys)
		}
	}
}

func TestEverythingMapsSettingsPathsAcrossHomes(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)

	hook := filepath.Join(a.home, ".claude", "hooks", "notify.sh")
	a.write(".claude/settings.json", fmt.Sprintf(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":%q}]}]},"statusLine":{"command":"%s/.claude/statusline.sh"}}`+"\n", hook, a.home))
	a.write(".claude/hooks/notify.sh", "#!/bin/sh\necho done\n")
	if err := os.Chmod(filepath.Join(a.home, ".claude", "hooks", "notify.sh"), 0755); err != nil {
		t.Fatal(err)
	}
	a.write(".claude/CLAUDE.md", "Notes live in "+a.home+"/notes.\n")
	syncAll(1, a, b)

	got := b.read(".claude/settings.json")
	if !strings.Contains(got, filepath.Join(b.home, ".claude", "hooks", "notify.sh")) || strings.Contains(got, a.home) || strings.Contains(got, "${HOME}") {
		t.Fatalf("settings.json not mapped to bob's home: %s", got)
	}
	if !strings.Contains(b.read(".claude/CLAUDE.md"), b.home+"/notes") {
		t.Fatalf("CLAUDE.md not mapped: %s", b.read(".claude/CLAUDE.md"))
	}
	fi, err := os.Stat(filepath.Join(b.home, ".claude", "hooks", "notify.sh"))
	if err != nil || fi.Mode().Perm()&0100 == 0 {
		t.Fatalf("hook script should stay executable on bob: %v %v", fi.Mode(), err)
	}
	if blob := string(store.blob("settings.json.age")); strings.Contains(blob, a.home) {
		t.Fatal("remote copy holds alice's home path in the clear")
	}
	// An idle round moves nothing on either side.
	for _, d := range []*device{a, b} {
		if r := d.sync(); r.Changed() {
			t.Fatalf("%s: idle sync moved data: %+v", d.name, r)
		}
	}
}

func TestFolderSyncCreatesMissingFolder(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	t0 := time.Now()

	// A session works in ~/code/plain, which only alice has.
	a.write("code/plain/README.md", "# plain\n")
	a.write("code/plain/src/main.go", "package main\n")
	a.writeSession("plain", "s1", a.line("plain", "u1", "", t0, "hi"))
	a.sync()
	if keys := keysWithPrefix(store, FoldersPrefix+"code/plain/"); len(keys) != 3 {
		t.Fatalf("want README, main.go and the folder marker remotely, got %v", keys)
	}
	r := b.sync()
	if b.read("code/plain/README.md") != "# plain\n" || b.read("code/plain/src/main.go") != "package main\n" {
		t.Fatalf("bob didn't get the folder: %+v", r)
	}
	// Folder files are never rewritten: a home path inside one stays verbatim.
	a.write("code/plain/notes.txt", "built in "+a.home+"\n")
	a.sync()
	b.sync()
	if got := b.read("code/plain/notes.txt"); got != "built in "+a.home+"\n" {
		t.Fatalf("folder file was rewritten: %q", got)
	}
	// Bob's edit comes back to alice.
	b.write("code/plain/README.md", "# plain, edited on bob\n")
	b.sync()
	a.sync()
	if got := a.read("code/plain/README.md"); got != "# plain, edited on bob\n" {
		t.Fatalf("alice = %q", got)
	}
}

// git runs git in dir with a private, empty configuration.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main", "-c", "core.fsmonitor=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func TestFolderSyncCarriesARealGitRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	t0 := time.Now()

	repoA := filepath.Join(a.home, "code", "repo")
	if err := os.MkdirAll(repoA, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, repoA, "init", "-q")
	a.write("code/repo/app.py", "print('one')\n")
	a.write("code/repo/run.sh", "#!/bin/sh\npython3 app.py\n")
	_ = os.Chmod(filepath.Join(repoA, "run.sh"), 0755)
	if err := os.Symlink("app.py", filepath.Join(repoA, "main.py")); err != nil { // a tracked link
		t.Fatal(err)
	}
	git(t, repoA, "add", "-A")
	git(t, repoA, "commit", "-q", "-m", "first")
	a.write("code/repo/app.py", "print('two')\n")
	git(t, repoA, "commit", "-q", "-am", "second")
	// A session in a subfolder syncs the whole repository.
	a.writeSession("repo-sub", "s1", fmt.Sprintf(`{"uuid":"u1","cwd":%q,"timestamp":%q}`+"\n", filepath.Join(repoA, "sub"), t0.UTC().Format(time.RFC3339Nano)))
	a.sync()
	if keys := keysWithPrefix(store, FoldersPrefix+"code/repo/.git/"); len(keys) == 0 {
		t.Fatal(".git was not uploaded")
	}
	b.sync()

	repoB := filepath.Join(b.home, "code", "repo")
	git(t, repoB, "fsck", "--strict")
	if log := git(t, repoB, "log", "--format=%s"); log != "second\nfirst\n" {
		t.Fatalf("bob's history = %q", log)
	}
	if st := git(t, repoB, "status", "--porcelain"); st != "" {
		t.Fatalf("bob's working tree isn't clean: %q", st)
	}
	if fi, err := os.Stat(filepath.Join(repoB, "run.sh")); err != nil || fi.Mode().Perm()&0100 == 0 {
		t.Fatalf("run.sh lost its executable bit: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(repoB, "main.py")); err != nil || target != "app.py" {
		t.Fatalf("main.py should be a link to app.py on bob: %q %v", target, err)
	}

	// Bob commits; alice gets it.
	b.write("code/repo/app.py", "print('three')\n")
	git(t, repoB, "commit", "-q", "-am", "third, on bob")
	b.sync()
	a.sync()
	git(t, repoA, "fsck", "--strict")
	if log := git(t, repoA, "log", "--format=%s"); log != "third, on bob\nsecond\nfirst\n" {
		t.Fatalf("alice's history = %q", log)
	}
	if st := git(t, repoA, "status", "--porcelain"); st != "" {
		t.Fatalf("alice's working tree isn't clean: %q", st)
	}
	if got := a.read("code/repo/app.py"); got != "print('three')\n" {
		t.Fatalf("alice's app.py = %q", got)
	}
}

func TestFolderSyncWaitsWhileGitHoldsALock(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.s.cfg.ExtraFolders = []string{filepath.Join(a.home, "code", "repo")}
	a.write("code/repo/.git/HEAD", "ref: refs/heads/main\n")
	a.write("code/repo/main.go", "package main\n")
	a.write("code/repo/.git/index.lock", "")

	r := a.sync()
	if len(r.Deferred) != 1 || r.Deferred[0] != FoldersPrefix+"code/repo/" {
		t.Fatalf("folder should be deferred while git holds index.lock: %+v", r)
	}
	if keys := keysWithPrefix(store, FoldersPrefix); len(keys) != 0 {
		t.Fatalf("nothing in a busy repository may upload: %v", keys)
	}
	if p, err := a.s.Preview(context.Background()); err != nil || len(p.Deferred) != 1 {
		t.Fatalf("preview should show the deferral: %+v %v", p, err)
	}

	// Any lock under .git counts, refs included.
	a.remove("code/repo/.git/index.lock")
	a.write("code/repo/.git/refs/heads/main.lock", "")
	if r := a.sync(); len(r.Deferred) != 1 {
		t.Fatalf("a ref lock should defer the folder too: %+v", r)
	}
	// A lock git left behind long ago gets a warning.
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(filepath.Join(a.home, "code", "repo", ".git", "refs", "heads", "main.lock"), old, old)
	if r := a.sync(); len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "refs/heads/main.lock") {
		t.Fatalf("a stale lock should be reported: %+v", r)
	}
	a.remove("code/repo/.git/refs/heads/main.lock")
	a.sync()
	keys := keysWithPrefix(store, FoldersPrefix+"code/repo/")
	if len(keys) != 3 { // HEAD, main.go, marker
		t.Fatalf("after git finished the folder should sync: %v", keys)
	}
	for _, k := range keys {
		if strings.HasSuffix(k, ".lock.age") {
			t.Fatalf("a lock file was uploaded: %s", k)
		}
	}
}

func TestFolderSyncLeavesBuildArtifactsUnlessAsked(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.s.cfg.ExtraFolders = []string{filepath.Join(a.home, "code", "app")}
	a.s.cfg.FolderExclude = []string{"dist/**"}
	for _, rel := range []string{
		"node_modules/left-pad/index.js", ".venv/bin/python", "venv/lib/x.py", "pkg/__pycache__/m.cpython-312.pyc",
		"pkg/stale.pyc", ".pytest_cache/v", ".mypy_cache/m", ".next/build", ".nuxt/x", ".gradle/c", ".DS_Store",
		"dist/bundle.js",
	} {
		a.write("code/app/"+rel, "built\n")
	}
	a.write("code/app/src/index.ts", "export {}\n")
	a.write("code/app/yarn.lock", "# lockfile\n") // a project file, not a git lock
	a.sync()
	got := keysWithPrefix(store, FoldersPrefix+"code/app/")
	want := []string{FoldersPrefix + "code/app/.stradl-folder.age", FoldersPrefix + "code/app/src/index.ts.age", FoldersPrefix + "code/app/yarn.lock.age"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v\nwant %v", got, want)
	}

	a.s.cfg.SyncBuildArtifacts = true
	a.sync()
	got = keysWithPrefix(store, FoldersPrefix+"code/app/")
	if len(got) != 3+11 {
		t.Fatalf("with sync_build_artifacts every artifact but the folder_exclude match should sync: %v", got)
	}
}

func TestFolderMirrorDeleteGoesToTheOtherMacsTrash(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "proj")}
	}
	a.write("code/proj/a.txt", "a\n")
	a.write("code/proj/old/b.txt", "b\n")
	a.write("code/proj/c.txt", "c\n")
	a.write(".claude/skills/gone/SKILL.md", "# skill\n")
	syncAll(1, a, b)
	if b.read("code/proj/old/b.txt") != "b\n" || b.read(".claude/skills/gone/SKILL.md") == "" {
		t.Fatal("setup: bob should have everything")
	}

	a.remove("code/proj/old/b.txt")
	a.remove(".claude/skills/gone/SKILL.md")
	if p, _ := a.s.Preview(context.Background()); len(p.Deleted) != 2 {
		t.Fatalf("preview should list 2 deletions: %+v", p)
	}
	ra := a.sync()
	if len(ra.Deleted) != 2 {
		t.Fatalf("alice should delete both remotely: %+v", ra)
	}
	if store.blob(FoldersPrefix+"code/proj/old/b.txt.age") != nil && len(store.blob(FoldersPrefix+"code/proj/old/b.txt.age")) > 0 {
		t.Fatal("remote copy survived")
	}
	if p, _ := b.s.Preview(context.Background()); len(p.Trashed) != 2 {
		t.Fatalf("bob's preview should list 2 files for the trash: %+v", p)
	}
	rb := b.sync()
	if len(rb.Trashed) != 2 {
		t.Fatalf("bob should trash both: %+v", rb)
	}
	if b.exists("code/proj/old/b.txt") || b.exists("code/proj/old") || b.exists(".claude/skills/gone") {
		t.Fatal("bob still has the deleted files (or their empty folders)")
	}
	if got := b.trashed(); strings.Join(got, ",") != ".claude/skills/gone/SKILL.md,code/proj/old/b.txt" {
		t.Fatalf("bob's trash = %v", got)
	}
	if b.read("code/proj/a.txt") != "a\n" {
		t.Fatal("bob lost an untouched file")
	}
	// Nothing comes back on later rounds.
	syncAll(2, a, b)
	if a.exists("code/proj/old/b.txt") || b.exists("code/proj/old/b.txt") {
		t.Fatal("a deleted file came back")
	}
}

func TestFolderEditBeatsDelete(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "proj")}
	}
	for _, n := range []string{"one", "two", "three", "four"} {
		a.write("code/proj/"+n+".txt", n+"\n")
	}
	syncAll(1, a, b)

	// Edited on bob first, then deleted on alice: the edit wins.
	b.write("code/proj/one.txt", "one, edited\n")
	b.sync()
	a.remove("code/proj/one.txt")
	if r := a.sync(); len(r.Deleted) != 0 || a.read("code/proj/one.txt") != "one, edited\n" {
		t.Fatalf("alice should get bob's edit back instead of deleting: %+v", r)
	}

	// Deleted on alice first, then edited on bob: the edit is uploaded again.
	a.remove("code/proj/two.txt")
	a.sync()
	b.write("code/proj/two.txt", "two, edited\n")
	if r := b.sync(); len(r.Trashed) != 0 || len(r.Uploaded) != 1 {
		t.Fatalf("bob's edit should be uploaded, not trashed: %+v", r)
	}
	a.sync()
	if a.read("code/proj/two.txt") != "two, edited\n" {
		t.Fatal("alice should get the edited file back")
	}

	// Bob uploads an edit between alice's listing and her delete: the
	// conditional delete fails and alice downloads the edit.
	a.remove("code/proj/three.txt")
	var once gosync.Once
	store.beforeDeleteIf = func(key string) {
		once.Do(func() {
			store.beforeDeleteIf = nil
			b.write("code/proj/three.txt", "three, raced in\n")
			b.sync()
		})
	}
	if r := a.sync(); len(r.Deleted) != 0 {
		t.Fatalf("the delete must lose to the concurrent edit: %+v", r)
	}
	if a.read("code/proj/three.txt") != "three, raced in\n" {
		t.Fatalf("alice = %q", a.read("code/proj/three.txt"))
	}
	syncAll(1, a, b)
	if b.read("code/proj/three.txt") != "three, raced in\n" {
		t.Fatal("bob lost his edit")
	}
}

func TestMassDeleteGuard(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "big"), filepath.Join(d.home, "code", "other")}
	}
	for i := 0; i < 20; i++ {
		a.write(fmt.Sprintf("code/big/f%02d.txt", i), "x\n")
		a.write(fmt.Sprintf("code/other/g%02d.txt", i), "y\n")
	}
	syncAll(1, a, b)
	before := len(keysWithPrefix(store, FoldersPrefix+"code/big/"))

	for i := 0; i < 15; i++ {
		a.remove(fmt.Sprintf("code/big/f%02d.txt", i))
	}
	r := a.sync()
	if len(r.Deleted) != 0 || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "~/code/big") {
		t.Fatalf("the guard should hold all 15 deletions back with one warning: %+v", r)
	}
	if got := len(keysWithPrefix(store, FoldersPrefix+"code/big/")); got != before {
		t.Fatalf("remote lost files: %d of %d left", got, before)
	}
	if rb := b.sync(); len(rb.Trashed) != 0 || b.read("code/big/f00.txt") == "" {
		t.Fatalf("bob must keep everything: %+v", rb)
	}

	// A few deletions elsewhere still go through.
	a.remove("code/other/g00.txt")
	if r := a.sync(); len(r.Deleted) != 1 {
		t.Fatalf("a single delete in another folder should proceed: %+v", r)
	}
	if rb := b.sync(); len(rb.Trashed) != 1 {
		t.Fatalf("bob should trash the single delete: %+v", rb)
	}

	// Asked explicitly, the mass delete proceeds.
	a.s.cfg.AllowMassDelete = true
	if r := a.sync(); len(r.Deleted) != 15 || len(r.Warnings) != 0 {
		t.Fatalf("--allow-mass-delete should delete all 15: %+v", r)
	}
	if rb := b.sync(); len(rb.Trashed) != 15 {
		t.Fatalf("bob should trash the 15: %+v", rb)
	}

	// A wiped container: every mirrored file vanishes remotely at once. The
	// global guard keeps every Mac's copies (union files are restored).
	for _, k := range store.keys() {
		_ = store.Delete(context.Background(), k)
	}
	rb := b.sync()
	if len(rb.Trashed) != 0 || len(rb.Warnings) == 0 {
		t.Fatalf("bob must not trash everything after a wipe: %+v", rb)
	}
	if b.read("code/other/g05.txt") == "" {
		t.Fatal("bob lost files to a wiped container")
	}
}

// Deletions a v2.0.0 Mac recorded as local-only (LocalDeleted) are mirrored
// once it upgrades, without tripping the guard: they aren't sudden, and
// transcripts make up most of what every Mac syncs.
func TestGuardLetsEarlierDeletionsThrough(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	t0 := time.Now()
	for i := 0; i < 30; i++ {
		a.writeSession("p", fmt.Sprintf("s%02d", i), a.line("p", fmt.Sprintf("u%d", i), "", t0, "x"))
	}
	for i := 0; i < 20; i++ {
		a.write(fmt.Sprintf(".claude/plans/p%02d.md", i), "plan\n")
	}
	syncAll(1, a, b)
	// Alice's Claude Code cleaned up 15 plans while deletes stayed local.
	objs := listAll(t, store)
	for i := 0; i < 15; i++ {
		np := fmt.Sprintf("plans/p%02d.md", i)
		a.remove(".claude/" + np)
		for _, o := range objs {
			if o.Key == np+".age" {
				a.s.state.MarkLocalDeleted(np, o.ETag)
			}
		}
	}
	if r := a.sync(); len(r.Deleted) != 15 || len(r.Warnings) != 0 {
		t.Fatalf("earlier local deletions should now be mirrored: %+v", r)
	}
	if r := b.sync(); len(r.Trashed) != 15 || len(r.Warnings) != 0 {
		t.Fatalf("bob should trash them: %+v", r)
	}
}

func TestFolderInsideRepoSyncsTheRepoRoot(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.write("code/mono/.git/HEAD", "ref: refs/heads/main\n")
	a.write("code/mono/top.txt", "top\n")
	a.write("code/mono/svc/api/main.go", "package api\n")
	a.writeSession("x", "s1", fmt.Sprintf(`{"uuid":"u1","cwd":%q}`+"\n", filepath.Join(a.home, "code", "mono", "svc", "api")))
	a.sync()
	if got := remoteFolderMarkers(listAll(t, store)); strings.Join(got, ",") != "code/mono" {
		t.Fatalf("folders = %v, want the repository root only", got)
	}
	if len(keysWithPrefix(store, FoldersPrefix+"code/mono/top.txt")) != 1 {
		t.Fatal("files above the session's folder should sync with the repository")
	}
}

func listAll(t *testing.T, store storage.Storage) []storage.ObjectInfo {
	t.Helper()
	objs, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

func TestDiscoverFoldersDedupesNestedAndRejectsUnsafeFolders(t *testing.T) {
	root := t.TempDir()
	a := newEverythingDevice(t, root, "alice", newCASStore())
	h := a.home
	a.s.cfg.ExtraFolders = []string{
		filepath.Join(h, "work"), filepath.Join(h, "work", "app"), filepath.Join(h, "work", "app", "deep"),
		h, filepath.Join(h, ".claude", "x"), filepath.Join(h, ".stradl"), filepath.Join(h, "Library", "Mobile Documents", "doc"),
		filepath.Join(h, "Library", "CloudStorage", "Drive"), filepath.Join(h, ".Trash", "old"), "/tmp/elsewhere",
		filepath.Join(h, "Library", "Application Support", "Claude", "scratch-workspaces", "a", "b", "s1"),
		filepath.Join(h, "Library", "Preferences"), filepath.Join(h, "Projects", "solo"),
	}
	got := a.s.discoverFolders(nil, true)
	want := []string{"Library/Application Support/Claude/scratch-workspaces/a/b/s1", "Projects/solo", "work"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("folders = %q\nwant      %q", got, want)
	}

	// Nested folders collapse to the outermost; case variants to one spelling,
	// preferring the one in remote storage.
	if got := chooseFolders([]string{"a/b", "a", "c/d", "c/d/e", "cc"}, nil, false, nil); strings.Join(got, ",") != "a,c/d,cc" {
		t.Fatalf("nested = %v", got)
	}
	// A git repository inside another folder stays its own.
	if got := chooseFolders([]string{"a", "a/repo", "a/plain"}, nil, false, map[string]bool{"a/repo": true}); strings.Join(got, ",") != "a,a/repo" {
		t.Fatalf("nested repository = %v", got)
	}
	if got := chooseFolders([]string{"code/X", "Code/x"}, map[string]bool{"code/X": true}, true, nil); strings.Join(got, ",") != "code/X" {
		t.Fatalf("case variants = %v", got)
	}
	if got := chooseFolders([]string{"code/X", "Code/x"}, nil, false, nil); len(got) != 2 {
		t.Fatalf("a case-sensitive volume keeps both: %v", got)
	}
}

func TestFolderCaseVariantsConverge(t *testing.T) {
	probe := filepath.Join(t.TempDir(), "Abc")
	_ = os.Mkdir(probe, 0700)
	if _, err := os.Stat(strings.ToLower(probe)); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	// Alice keeps the project in ~/Code/proj, Bob in ~/code/proj.
	a.s.cfg.ExtraFolders = []string{filepath.Join(a.home, "Code", "proj")}
	b.s.cfg.ExtraFolders = []string{filepath.Join(b.home, "code", "proj")}
	a.write("Code/proj/from-alice.txt", "a\n")
	b.write("code/proj/from-bob.txt", "b\n")
	a.write("Code/proj/shared.txt", "alice's\n")
	syncAll(3, a, b)

	for _, d := range []*device{a, b} {
		for _, f := range []string{"from-alice.txt", "from-bob.txt", "shared.txt"} {
			if d.read("code/proj/"+f) == "" {
				t.Fatalf("%s is missing %s", d.name, f)
			}
		}
		matches, _ := filepath.Glob(filepath.Join(d.home, "*", "proj"))
		if len(matches) != 1 {
			t.Fatalf("%s has the folder twice: %v", d.name, matches)
		}
	}
	// One spelling remotely.
	if lower := keysWithPrefix(store, FoldersPrefix+"code/"); len(lower) != 0 {
		t.Fatalf("both spellings uploaded: %v", lower)
	}
	b.write("code/proj/shared.txt", "edited on bob\n")
	syncAll(1, b, a)
	if got := a.read("Code/proj/shared.txt"); got != "edited on bob\n" {
		t.Fatalf("alice = %q", got)
	}
}

func TestFolderSyncOffAndOtherScopesIgnoreFolders(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.s.cfg.ExtraFolders = []string{filepath.Join(a.home, "code", "p")}
	a.write("code/p/x.txt", "x\n")
	off := false
	a.s.cfg.FolderSync = &off
	a.sync()
	a.s.cfg.FolderSync = nil
	a.s.cfg.Scope = config.ScopeFull
	a.sync()
	if keys := keysWithPrefix(store, FoldersPrefix); len(keys) != 0 {
		t.Fatalf("folders synced with folder sync off or outside everything: %v", keys)
	}
}

func TestFolderFingerprints(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "p")}
	}
	a.write("code/p/x.txt", "x\n")
	base := mustLocalFP(t, a)
	a.write("code/p/x.txt", "xy\n")
	if mustLocalFP(t, a) == base {
		t.Fatal("editing a folder file must move the local fingerprint")
	}
	base = mustLocalFP(t, a)
	_ = os.Chmod(filepath.Join(a.home, "code", "p", "x.txt"), 0755)
	if mustLocalFP(t, a) == base {
		t.Fatal("a chmod must move the local fingerprint")
	}
	base = mustLocalFP(t, a)
	a.write("code/p/.git/index.lock", "")
	if mustLocalFP(t, a) == base {
		t.Fatal("git starting work must move the local fingerprint")
	}
	a.remove("code/p/.git/index.lock")

	remoteBase := mustRemoteFP(t, b)
	a.sync()
	if mustRemoteFP(t, b) == remoteBase {
		t.Fatal("alice's folder upload must move bob's remote fingerprint")
	}
	b.sync()
	remoteBase = mustRemoteFP(t, b)
	a.write("code/p/x.txt", "changed\n")
	a.sync()
	if mustRemoteFP(t, b) == remoteBase {
		t.Fatal("a folder edit on alice must move bob's remote fingerprint")
	}
}

func TestModesFollowChmod(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "p")}
	}
	a.write("code/p/tool.sh", "#!/bin/sh\n")
	a.write("code/p/secret.env", "K=V\n")
	_ = os.Chmod(filepath.Join(a.home, "code", "p", "secret.env"), 0600)
	syncAll(1, a, b)
	mode := func(d *device, rel string) os.FileMode {
		fi, err := os.Stat(filepath.Join(d.home, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}
	if m := mode(b, "code/p/tool.sh"); m != 0644 {
		t.Fatalf("tool.sh = %o, want 0644", m)
	}
	if m := mode(b, "code/p/secret.env"); m != 0600 {
		t.Fatalf("a private file must stay private: %o", m)
	}
	_ = os.Chmod(filepath.Join(a.home, "code", "p", "tool.sh"), 0755)
	syncAll(1, a, b)
	if m := mode(b, "code/p/tool.sh"); m != 0755 {
		t.Fatalf("chmod +x on alice should reach bob: %o", m)
	}
	_ = os.Chmod(filepath.Join(b.home, "code", "p", "tool.sh"), 0644)
	syncAll(1, b, a)
	if m := mode(a, "code/p/tool.sh"); m != 0644 {
		t.Fatalf("chmod -x on bob should reach alice: %o", m)
	}
}

func TestFolderSymlinks(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "p")}
	}
	a.write("code/p/AGENTS.md", "# agents\n")
	link := func(d *device, target, rel string) {
		t.Helper()
		p := filepath.Join(d.home, filepath.FromSlash(rel))
		_ = os.Remove(p)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	readlink := func(d *device, rel string) string {
		target, err := os.Readlink(filepath.Join(d.home, filepath.FromSlash(rel)))
		if err != nil {
			return ""
		}
		return target
	}
	link(a, "AGENTS.md", "code/p/CLAUDE.md")
	link(a, "/nonexistent/elsewhere", "code/p/dangling")
	syncAll(1, a, b)
	if readlink(b, "code/p/CLAUDE.md") != "AGENTS.md" || readlink(b, "code/p/dangling") != "/nonexistent/elsewhere" {
		t.Fatalf("links not recreated on bob: %q %q", readlink(b, "code/p/CLAUDE.md"), readlink(b, "code/p/dangling"))
	}
	if b.read("code/p/CLAUDE.md") != "# agents\n" {
		t.Fatal("bob's link should resolve to the synced file")
	}
	for _, d := range []*device{a, b} {
		if r := d.sync(); r.Changed() {
			t.Fatalf("%s: idle sync moved data: %+v", d.name, r)
		}
	}

	// Retargeted on bob, deleted on alice.
	b.write("code/p/README.md", "# readme\n")
	link(b, "README.md", "code/p/CLAUDE.md")
	b.sync()
	a.remove("code/p/dangling")
	a.sync()
	b.sync()
	if readlink(a, "code/p/CLAUDE.md") != "README.md" {
		t.Fatalf("alice's link should follow bob's change: %q", readlink(a, "code/p/CLAUDE.md"))
	}
	if b.exists("code/p/dangling") {
		t.Fatal("the deleted link should be gone from bob")
	}

	// A remote file below a folder that is a link here is never written
	// through the link.
	outside := filepath.Join(root, "outside")
	_ = os.MkdirAll(outside, 0755)
	a.write("code/p/sub/file.txt", "x\n")
	link(b, outside, "code/p/sub")
	_, _ = a.s.Sync(context.Background())
	r, err := b.s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "file.txt")); !os.IsNotExist(err) {
		t.Fatal("a file was written through a symbolic link")
	}
	if !strings.Contains(fmt.Sprint(r.Errors), "symbolic link") {
		t.Fatalf("expected a refusal, got %v", r.Errors)
	}
}

// ctxStore fails writes once their context is done, as real backends do.
type ctxStore struct{ *casStore }

func (c ctxStore) UploadIf(ctx context.Context, key string, data []byte, ifMatch string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return c.casStore.UploadIf(ctx, key, data, ifMatch)
}

// A pass cut short (the agent's time limit, a sleeping Mac) keeps what it
// finished: the next pass doesn't compare those files again.
func TestInterruptedSyncKeepsProgress(t *testing.T) {
	root := t.TempDir()
	cas := newCASStore()
	a := newEverythingDevice(t, root, "alice", ctxStore{cas})
	a.s.cfg.ExtraFolders = []string{filepath.Join(a.home, "code", "big")}
	for i := 0; i < 40; i++ {
		a.write(fmt.Sprintf("code/big/f%02d.txt", i), fmt.Sprintf("file %d\n", i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	var mu gosync.Mutex
	n := 0
	cas.beforeUploadIf = func(key string) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(key, "/f") {
			if n++; n == 15 {
				cancel()
			}
		}
	}
	first, err := a.s.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cas.beforeUploadIf = nil
	if len(first.Uploaded) == 0 || len(first.Errors) == 0 {
		t.Fatalf("the first pass should finish some files and fail the rest: %d uploaded, %d errors", len(first.Uploaded), len(first.Errors))
	}
	// A fresh syncer reads the saved state, as the next agent pass would.
	state, err := LoadStateFromDir(a.s.cfg.StateDirOverride)
	if err != nil {
		t.Fatal(err)
	}
	a.s.state = state
	second := a.sync()
	if len(second.Merged) != 0 || len(first.Uploaded)+len(second.Uploaded) != 40 {
		t.Fatalf("the second pass should upload only the rest: first %d, second %d uploaded, %d merged",
			len(first.Uploaded), len(second.Uploaded), len(second.Merged))
	}
}

func TestPurgeTrashKeepsThirtyDays(t *testing.T) {
	d := newEverythingDevice(t, t.TempDir(), "alice", newCASStore())
	trash := d.s.trashDir()
	old := time.Now().AddDate(0, 0, -40).Format(trashDayLayout)
	recent := time.Now().AddDate(0, 0, -3).Format(trashDayLayout)
	for _, day := range []string{old, recent} {
		_ = os.MkdirAll(filepath.Join(trash, day, "code"), 0700)
		_ = os.WriteFile(filepath.Join(trash, day, "code", "f"), []byte("x"), 0600)
	}
	d.s.purgeTrash(time.Now())
	if _, err := os.Stat(filepath.Join(trash, old)); !os.IsNotExist(err) {
		t.Fatal("40-day-old trash should be purged")
	}
	if _, err := os.Stat(filepath.Join(trash, recent, "code", "f")); err != nil {
		t.Fatal("recent trash must be kept")
	}
}

func TestConflictsInsideGitAndPluginsGoAside(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code", "r")}
	}
	a.write("code/r/.git/HEAD", "ref: refs/heads/main\n")
	a.write("code/r/.git/refs/heads/main", "1111\n")
	a.write("code/r/notes.txt", "base\n")
	a.write(".claude/plugins/known_marketplaces.json", "{}\n")
	syncAll(1, a, b)

	a.write("code/r/.git/refs/heads/main", "aaaa\n")
	a.write("code/r/notes.txt", "alice\n")
	a.write(".claude/plugins/known_marketplaces.json", `{"a":1}`+"\n")
	b.write("code/r/.git/refs/heads/main", "bbbb\n")
	b.write("code/r/notes.txt", "bob\n")
	b.write(".claude/plugins/known_marketplaces.json", `{"b":1}`+"\n")
	a.sync()
	if r := b.sync(); len(r.Conflicts) != 3 {
		t.Fatalf("want 3 conflicts on bob: %+v", r)
	}
	if beside, _ := filepath.Glob(filepath.Join(b.home, "code", "r", ".git", "refs", "heads", "main.conflict.*")); len(beside) != 0 {
		t.Fatalf("a conflict copy inside .git: %v", beside)
	}
	if beside, _ := filepath.Glob(filepath.Join(b.home, ".claude", "plugins", "*.conflict.*")); len(beside) != 0 {
		t.Fatalf("a conflict copy inside plugins/: %v", beside)
	}
	aside, _ := filepath.Glob(filepath.Join(b.home, ".claude-sync", "conflicts", "code", "r", ".git", "refs", "heads", "main.conflict.*"))
	plug, _ := filepath.Glob(filepath.Join(b.home, ".claude-sync", "conflicts", ".claude", "plugins", "known_marketplaces.json.conflict.*"))
	if len(aside) != 1 || len(plug) != 1 {
		t.Fatalf("conflict copies should be in ~/.stradl/conflicts: %v %v", aside, plug)
	}
	if beside, _ := filepath.Glob(filepath.Join(b.home, "code", "r", "notes.txt.conflict.*")); len(beside) != 1 {
		t.Fatalf("an ordinary file keeps its conflict copy beside it: %v", beside)
	}
}
