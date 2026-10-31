package sync

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/storage"
)

// Claude accounts: the Circle syncs the accounts the user chose, and keeps
// the rest on the Mac they were made on, deleting nothing.

const (
	judgeAcct = "53d94190-1990-4286-b69d-42cdf507ba7a"
	judgeOrg  = "d1ea0314-c062-49c3-834c-41eb9f47efed"
	bpAcct    = "c067ad00-1bc5-4a8d-b69e-d8ca9ae5d4a3"
	bpOrg     = "e5942ae4-1ce2-408f-b39e-c7a696f66c67"
)

// signIn makes d's Claude Code signed in to acct (as ~/.claude.json says).
func (d *device) signIn(acct, org, email string) {
	d.t.Helper()
	data, _ := json.Marshal(map[string]any{"oauthAccount": map[string]string{
		"accountUuid": acct, "emailAddress": email, "organizationUuid": org, "organizationName": "Org " + org[:4],
	}})
	if err := os.WriteFile(d.s.cfg.ClaudeJSONOverride, data, 0600); err != nil {
		d.t.Fatal(err)
	}
	d.s.noteSignIn(time.Now()) // as the agent's local check does within seconds
}

// writeAccountSession writes a desktop session of acct: its sidebar record
// and its transcript, working in cwd.
func (d *device) writeAccountSession(acct, org, id, cwd string) {
	d.t.Helper()
	p := filepath.Join(d.desktop, acct, org, "local_"+id+".json")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		d.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(desktopRecord("local_"+id, "cli-"+id, cwd, time.Now().UnixMilli(), id)), 0600); err != nil {
		d.t.Fatal(err)
	}
	d.writeSession("p", "cli-"+id, cwdLine("u-"+id, cwd))
}

// hasSession reports whether storage holds the transcript of session id.
func hasSession(store *casStore, id string) bool {
	for _, k := range keysWithPrefix(store, "projects/") {
		if strings.Contains(k, "/"+id+".jsonl") {
			return true
		}
	}
	return false
}

func (d *device) setSync(acct string, on bool) {
	d.t.Helper()
	if err := d.s.SetAccountSync(context.Background(), acct, on, time.Now()); err != nil {
		d.t.Fatal(err)
	}
}

func TestAnAccountThatDoesntSyncStaysOnItsMac(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	a.signIn(judgeAcct, judgeOrg, "mcampana@judge.com")
	a.write("code/judge-app/x.go", "package app\n")
	a.write("code/mine/y.go", "package mine\n")
	a.writeAccountSession(judgeAcct, judgeOrg, "j1", filepath.Join(a.home, "code", "judge-app"))
	a.writeAccountSession(bpAcct, bpOrg, "b1", filepath.Join(a.home, "code", "mine"))
	a.setSync(judgeAcct, false)
	a.sync()

	if !hasSession(store, "cli-b1") {
		t.Fatal("the synced account's transcript should sync")
	}
	for _, k := range []string{DesktopPrefix + bpAcct + "/" + bpOrg + "/local_b1.json", FoldersPrefix + "code/mine/y.go"} {
		if !keysHave(store, k) {
			t.Fatalf("%s should sync", k)
		}
	}
	if hasSession(store, "cli-j1") {
		t.Fatal("the Judge transcript synced")
	}
	if keys := keysWithPrefix(store, FoldersPrefix+"code/judge-app/"); len(keys) != 0 {
		t.Fatalf("a folder only the Judge session works in synced: %v", keys)
	}
	b.sync()
	b.sync()
	if b.exists("code/judge-app/x.go") {
		t.Fatal("a folder only the Judge session works in reached bob")
	}
	if _, err := os.Stat(filepath.Join(b.desktop, judgeAcct, judgeOrg, "local_j1.json")); err == nil {
		t.Fatal("bob got the Judge session's sidebar record")
	}
	if b.read("code/mine/y.go") != "package mine\n" {
		t.Fatal("bob should get the synced account's folder")
	}
}

func TestAnAccountSetToOffBeforeItsFirstSyncNeverUploads(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.signIn(judgeAcct, judgeOrg, "mcampana@judge.com")
	a.setSync(judgeAcct, false)
	a.write("code/judge-app/x.go", "package app\n")
	a.writeAccountSession(judgeAcct, judgeOrg, "j1", filepath.Join(a.home, "code", "judge-app"))
	a.writeSession("cli", "term-1", cwdLine("u1", filepath.Join(a.home, "code", "judge-app"))) // a terminal session
	a.sync()
	if hasSession(store, "cli-j1") || hasSession(store, "term-1") {
		t.Fatal("a session of an account that doesn't sync uploaded")
	}
	for _, prefix := range []string{DesktopPrefix + judgeAcct, FoldersPrefix + "code/judge-app"} {
		if keys := keysWithPrefix(store, prefix); len(keys) != 0 {
			t.Fatalf("an account that doesn't sync uploaded %v", keys)
		}
	}
}

func TestTurningAnAccountOffDeletesNothing(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	a.signIn(judgeAcct, judgeOrg, "mcampana@judge.com")
	a.writeAccountSession(judgeAcct, judgeOrg, "j1", filepath.Join(a.home, "code", "judge-app"))
	a.write("code/judge-app/x.go", "v1\n")
	syncAll(2, a, b)
	if b.read("code/judge-app/x.go") != "v1\n" {
		t.Fatal("setup: bob should have the Judge folder")
	}
	b.setSync(judgeAcct, false) // chosen on bob, for the whole Circle
	a.write("code/judge-app/x.go", "v2\n")
	a.appendSession("p", "cli-j1", cwdLine("u2", filepath.Join(a.home, "code", "judge-app")))
	ra := a.sync()
	rb := b.sync()
	if len(ra.Deleted)+len(rb.Deleted)+len(ra.Trashed)+len(rb.Trashed) != 0 {
		t.Fatalf("deleted or trashed: %v %v %v %v", ra.Deleted, rb.Deleted, ra.Trashed, rb.Trashed)
	}
	if b.read("code/judge-app/x.go") != "v1\n" || !keysHave(store, FoldersPrefix+"code/judge-app/x.go") {
		t.Fatal("bob's copy and the stored one should stay as they were")
	}
	if strings.Contains(b.readSession("p", "cli-j1"), "u2") {
		t.Fatal("the Judge session kept syncing after it was turned off")
	}
}

func TestTerminalSessionsBelongToTheSignedInAccount(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	b.writeSession("p", "from-bob", cwdLine("u0", filepath.Join(b.home, "code", "x")))
	b.sync()
	a.signIn(judgeAcct, judgeOrg, "mcampana@judge.com")
	a.writeSession("p", "mine", cwdLine("u1", filepath.Join(a.home, "code", "x")))
	a.sync()
	reg, _, _, err := a.s.loadAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reg.Sessions["mine"] != judgeAcct {
		t.Fatalf("a terminal session written here belongs to %q", reg.Sessions["mine"])
	}
	if acct, ok := reg.Sessions["from-bob"]; ok {
		t.Fatalf("a session from another Mac was guessed at: %q", acct)
	}
	if a.s.registry().Accounts[judgeAcct].Email != "mcampana@judge.com" {
		t.Fatal("the signed-in account's email should be known")
	}
}

func TestAccountChoicesFromTwoMacsKeepTheLatest(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	ctx := context.Background()
	now := time.Now()
	if err := a.s.SetAccountSync(ctx, judgeAcct, false, now); err != nil {
		t.Fatal(err)
	}
	if err := b.s.SetAccountSync(ctx, judgeAcct, true, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := b.s.SetAccountLabel(ctx, bpAcct, "Breakpoint", now); err != nil {
		t.Fatal(err)
	}
	reg, _, _, err := a.s.loadAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reg.off(judgeAcct) || reg.Accounts[bpAcct].Label != "Breakpoint" {
		t.Fatalf("registry = %+v", reg.Accounts)
	}
	// An older choice arriving late doesn't win.
	old := newAccountRegistry()
	off := false
	old.account(judgeAcct).Sync, old.account(judgeAcct).SyncAt = &off, now.Add(-time.Hour)
	reg.merge(old)
	if reg.off(judgeAcct) {
		t.Fatal("an older choice won")
	}
}

func TestOrgPolicyFilesStayPerMac(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	for _, f := range []string{"policy-limits.json", "policy-limits.json.signature.json", "remote-settings.json", "remote-settings.json.synced-at"} {
		if err := os.WriteFile(filepath.Join(a.claude, f), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a.sync()
	for _, k := range []string{"policy-limits.json", "remote-settings.json"} {
		if len(keysWithPrefix(store, k)) != 0 {
			t.Fatalf("%s synced", k)
		}
	}
}

func TestListAccountsShowsTheChoices(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.signIn(judgeAcct, judgeOrg, "mcampana@judge.com")
	a.writeAccountSession(bpAcct, bpOrg, "b1", filepath.Join(a.home, "code", "mine"))
	a.sync()
	a.setSync(bpAcct, true)
	list := ListAccounts(a.s.cfg)
	if len(list) != 2 || !list[0].Active || list[0].Name != "mcampana@judge.com" || list[0].Sync != "ask" {
		t.Fatalf("list = %+v", list)
	}
	if list[1].ID != bpAcct || list[1].Sync != "on" || !list[1].Here || list[1].Sessions != 1 {
		t.Fatalf("list[1] = %+v", list[1])
	}
	if got, err := FindAccount(list, "MCAMPANA@judge.com"); err != nil || got.ID != judgeAcct {
		t.Fatalf("FindAccount = %+v %v", got, err)
	}
}

func TestASidebarRecordOverridesAGuessedAccount(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.signIn(judgeAcct, judgeOrg, "mcampana@judge.com")
	a.writeSession("p", "cli-s1", cwdLine("u1", filepath.Join(a.home, "code", "x")))
	a.sync() // guessed: Judge, signed in when it was written
	// The desktop app's record shows it belongs to the Breakpoint account.
	p := filepath.Join(a.desktop, bpAcct, bpOrg, "local_s1.json")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(desktopRecord("local_s1", "cli-s1", filepath.Join(a.home, "code", "x"), time.Now().UnixMilli(), "s1")), 0600); err != nil {
		t.Fatal(err)
	}
	a.sync()
	reg, _, _, err := a.s.loadAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reg.Sessions["cli-s1"] != bpAcct {
		t.Fatalf("session belongs to %q, want the record's account", reg.Sessions["cli-s1"])
	}
}

func TestAnAccountChoiceOnAnotherMacMovesTheRemoteFingerprint(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	ctx := context.Background()
	f1, err := a.s.RemoteFingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.setSync(judgeAcct, false)
	if f2, _ := a.s.RemoteFingerprint(ctx); f2 == f1 {
		t.Fatal("an account choice on another Mac didn't move the remote fingerprint")
	}
}

func TestTheSidebarLayoutOfAnAccountThatDoesntSyncStaysHere(t *testing.T) {
	store := newCASStore()
	a, b := newSidebarPair(t, store)
	stubDesktopApp(t, false)
	a.setSync(testAcct, false)
	a.syncSidebar(false)
	if st, _, exists, err := a.s.fetchSidebar(context.Background()); err != nil {
		t.Fatal(err)
	} else if exists && len(st.Scopes) != 0 {
		t.Fatalf("the layout of an account that doesn't sync uploaded: %v", st.Scopes)
	}
	b.syncSidebar(false)
	gs := groupScopeOf(sidebarFile(b).load()["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any))
	if len(gs["assignments"].(map[string]any)) != 0 {
		t.Fatal("bob got alice's group membership for an account that doesn't sync")
	}
	gsA := groupScopeOf(sidebarFile(a).load()["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any))
	if len(gsA["assignments"].(map[string]any)) == 0 {
		t.Fatal("alice's own layout should stay as it is")
	}
}

func TestPromptHistoryKeepsAnOffAccountsPromptsHere(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	a.writeAccountSession(judgeAcct, judgeOrg, "j1", filepath.Join(a.home, "code", "x"))
	a.writeAccountSession(bpAcct, bpOrg, "b1", filepath.Join(a.home, "code", "y"))
	a.sync()
	a.setSync(judgeAcct, false)
	hist := `{"display":"judge secret","pastedContents":{"1":{"id":1,"type":"text","contentHash":"abc123"}},"sessionId":"cli-j1"}` + "\n" +
		`{"display":"my own prompt","pastedContents":{},"sessionId":"cli-b1"}` + "\n"
	if err := os.WriteFile(filepath.Join(a.claude, "history.jsonl"), []byte(hist), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(a.claude, "paste-cache"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.claude, "paste-cache", "abc123.txt"), []byte("pasted judge text"), 0600); err != nil {
		t.Fatal(err)
	}
	a.sync()
	b.sync()
	got, _ := os.ReadFile(filepath.Join(b.claude, "history.jsonl"))
	if strings.Contains(string(got), "judge secret") || !strings.Contains(string(got), "my own prompt") {
		t.Fatalf("bob's history:\n%s", got)
	}
	if len(keysWithPrefix(store, "paste-cache/")) != 0 {
		t.Fatal("a paste only an off account's prompt holds synced")
	}
	mine, _ := os.ReadFile(filepath.Join(a.claude, "history.jsonl"))
	if !strings.Contains(string(mine), "judge secret") {
		t.Fatal("alice's own history should keep every prompt")
	}
}

func TestAnOffAccountsTasksMemoryAndPlansStayHere(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.setSync(judgeAcct, false)
	a.writeAccountSession(judgeAcct, judgeOrg, "j1", filepath.Join(a.home, "code", "x"))
	plan := filepath.Join(a.home, ".claude", "plans", "brave-otter.md")
	a.appendSession("p", "cli-j1", `{"uuid":"u9","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"`+plan+`"}}]}}`+"\n")
	a.write(".claude/plans/brave-otter.md", "the plan\n")
	a.write(".claude/tasks/cli-j1/1.json", "{}\n")
	if err := os.MkdirAll(filepath.Join(a.projectDir("p"), "memory"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.projectDir("p"), "memory", "notes.md"), []byte("judge notes"), 0600); err != nil {
		t.Fatal(err)
	}
	a.sync()
	for _, prefix := range []string{"plans/", "tasks/"} {
		if keys := keysWithPrefix(store, prefix); len(keys) != 0 {
			t.Fatalf("%v synced", keys)
		}
	}
	for _, k := range keysWithPrefix(store, "projects/") {
		if strings.Contains(k, "/memory/") {
			t.Fatalf("the project memory of an account that doesn't sync synced: %s", k)
		}
	}
}

func TestATerminalSessionWrittenBeforeASignInSwitchKeepsItsAccount(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.setSync(judgeAcct, false)
	a.signIn(judgeAcct, judgeOrg, "mcampana@judge.com")
	a.writeSession("cli", "judge-work", cwdLine("u1", filepath.Join(a.home, "code", "jw")))
	time.Sleep(20 * time.Millisecond)
	a.signIn(bpAcct, bpOrg, "matt@breakpointdata.com") // /login before the next sync
	a.sync()
	if hasSession(store, "judge-work") {
		t.Fatal("work written while signed in to an account that doesn't sync uploaded after switching")
	}
}

func TestADesktopSessionWaitsForItsRecord(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.setSync(judgeAcct, false)
	a.signIn(bpAcct, bpOrg, "matt@breakpointdata.com") // the terminal is on another account
	a.writeSession("p", "cli-d1", `{"type":"user","entrypoint":"claude-desktop","uuid":"u1","cwd":"`+filepath.Join(a.home, "code", "x")+`"}`+"\n")
	a.sync() // the app hasn't written the record yet
	if hasSession(store, "cli-d1") {
		t.Fatal("a desktop session uploaded before its record said whose it is")
	}
	p := filepath.Join(a.desktop, judgeAcct, judgeOrg, "local_d1.json")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(desktopRecord("local_d1", "cli-d1", filepath.Join(a.home, "code", "x"), time.Now().UnixMilli(), "d1")), 0600); err != nil {
		t.Fatal(err)
	}
	a.sync()
	if hasSession(store, "cli-d1") {
		t.Fatal("the Judge desktop session uploaded")
	}
}

func TestAccountChoicesSurviveANewContainer(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.setSync(judgeAcct, false)
	a.sync()
	a.s.storage = newCASStore() // a passphrase change emptied it, or a new container
	a.writeAccountSession(judgeAcct, judgeOrg, "j1", filepath.Join(a.home, "code", "x"))
	a.sync()
	if !a.s.registry().off(judgeAcct) {
		t.Fatal("the choice to keep Judge on each Mac was lost")
	}
}

// plainStore is a backend without compare-and-swap (S3, R2, GCS, WebDAV)
// whose "missing" error doesn't say "not found".
type plainStore struct{ inner *casStore }

func (p plainStore) Upload(ctx context.Context, key string, data []byte) error {
	return p.inner.Upload(ctx, key, data)
}
func (p plainStore) Download(ctx context.Context, key string) ([]byte, error) {
	data, err := p.inner.Download(ctx, key)
	if err != nil {
		return nil, errors.New("api error NoSuchKey: The specified key does not exist.")
	}
	return data, nil
}
func (p plainStore) Delete(ctx context.Context, key string) error { return p.inner.Delete(ctx, key) }
func (p plainStore) DeleteBatch(ctx context.Context, keys []string) error {
	return p.inner.DeleteBatch(ctx, keys)
}
func (p plainStore) List(ctx context.Context, prefix string) ([]storage.ObjectInfo, error) {
	return p.inner.List(ctx, prefix)
}
func (p plainStore) Head(ctx context.Context, key string) (*storage.ObjectInfo, error) {
	return p.inner.Head(ctx, key)
}
func (p plainStore) BucketExists(ctx context.Context) (bool, error) { return true, nil }

func TestBackendsWithoutCompareAndSwapSync(t *testing.T) {
	root := t.TempDir()
	inner := newCASStore()
	a := newEverythingDevice(t, root, "alice", inner)
	a.s.storage = plainStore{inner}
	a.writeAccountSession(bpAcct, bpOrg, "b1", filepath.Join(a.home, "code", "y"))
	a.sync() // fails if a missing registry reads as an error
	a.setSync(judgeAcct, false)
	if !a.s.cachedAccounts().off(judgeAcct) {
		t.Fatal("the choice wasn't saved")
	}
}

func TestAnOffAccountsRepoInsideASyncedFolderStaysOut(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	app := filepath.Join(a.home, "code", "app")
	if err := os.MkdirAll(app, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, app, "init", "-q")
	a.write("code/app/a.go", "package app\n")
	a.writeAccountSession(judgeAcct, judgeOrg, "j1", app)
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code")}
	}
	a.write("code/notes.md", "outer\n")
	syncAll(2, a, b)
	if b.read("code/app/a.go") != "package app\n" {
		t.Fatal("setup: bob should have the repo")
	}
	a.setSync(judgeAcct, false)
	// bob cleans up his clone of the work repo, and a new Mac joins.
	if err := os.RemoveAll(filepath.Join(b.home, "code", "app")); err != nil {
		t.Fatal(err)
	}
	c := newEverythingDevice(t, root, "carol", store)
	c.s.cfg.ExtraFolders = []string{filepath.Join(c.home, "code")}
	rb := b.sync()
	c.sync()
	c.sync()
	if len(rb.Deleted) != 0 || !keysHave(store, FoldersPrefix+"code/app/a.go") {
		t.Fatalf("bob's cleanup deleted the off account's repo from storage: %v", rb.Deleted)
	}
	if c.exists("code/app/a.go") {
		t.Fatal("a new Mac downloaded the off account's repo as part of ~/code")
	}
	if c.read("code/notes.md") != "outer\n" {
		t.Fatal("carol should get the outer folder")
	}
}

func TestAWorktreeSessionsMainRepoBelongsToItsAccount(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	repo := filepath.Join(a.home, "code", "jr")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	a.write("code/jr/a.txt", "a\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "a")
	wt := filepath.Join(repo, ".claude", "worktrees", "w1")
	git(t, repo, "worktree", "add", "-q", "-b", "claude/w1", wt)
	a.signIn(judgeAcct, judgeOrg, "mcampana@judge.com")
	a.writeSession("cli", "jw", cwdLine("u1", wt))
	a.sync()
	a.setSync(judgeAcct, false)
	a.write("code/jr/b.txt", "new\n")
	a.sync()
	if keysHave(store, FoldersPrefix+"code/jr/b.txt") {
		t.Fatal("the main repo of an off account's worktree session kept syncing")
	}
}

func TestTurningAnAccountOffAndOnKeepsDeletesWorking(t *testing.T) {
	needGit(t)
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	app := filepath.Join(a.home, "code", "app")
	if err := os.MkdirAll(app, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, app, "init", "-q")
	a.write("code/app/a.go", "a\n")
	a.write("code/app/b.go", "b\n")
	a.writeAccountSession(judgeAcct, judgeOrg, "j1", app)
	for _, d := range []*device{a, b} {
		d.s.cfg.ExtraFolders = []string{filepath.Join(d.home, "code")}
	}
	syncAll(2, a, b)
	a.setSync(judgeAcct, false)
	syncAll(1, a, b)
	a.setSync(judgeAcct, true)
	a.remove("code/app/b.go")
	syncAll(2, a, b)
	if b.exists("code/app/b.go") || keysHave(store, FoldersPrefix+"code/app/b.go") {
		t.Fatal("a delete after turning the account back on was undone")
	}
}

func TestTheProbeFollowsAnAccountChoice(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	b.writeAccountSession(judgeAcct, judgeOrg, "j1", filepath.Join(b.home, "code", "x"))
	syncAll(1, b, a)
	ctx := context.Background()
	a.setSync(judgeAcct, false)
	a.sync()
	// The agent's probe lives across passes, built while Judge is off.
	probe := newScopeSyncer(a.s.cfg)
	probe.storage = store
	if _, err := probe.RemoteFingerprint(ctx); err != nil {
		t.Fatal(err)
	}
	a.setSync(judgeAcct, true)
	a.sync()
	f1, _ := probe.RemoteFingerprint(ctx)
	b.appendSession("p", "cli-j1", cwdLine("u2", filepath.Join(b.home, "code", "x")))
	b.sync()
	if f2, _ := probe.RemoteFingerprint(ctx); f2 == f1 {
		t.Fatal("the probe kept the old choice: a change to a Judge session didn't move it")
	}
}

func TestAccountsKnownEvenWithTheSidebarNotSyncing(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	off := false
	a.s.cfg.DesktopSync = &off
	a.setSync(judgeAcct, false)
	a.signIn(bpAcct, bpOrg, "matt@breakpointdata.com")
	p := filepath.Join(a.desktop, judgeAcct, judgeOrg, "local_d1.json")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(desktopRecord("local_d1", "cli-d1", filepath.Join(a.home, "code", "x"), time.Now().UnixMilli(), "d1")), 0600); err != nil {
		t.Fatal(err)
	}
	a.writeSession("p", "cli-d1", `{"type":"user","entrypoint":"claude-desktop","uuid":"u1","cwd":"`+filepath.Join(a.home, "code", "x")+`"}`+"\n")
	a.sync()
	if hasSession(store, "cli-d1") {
		t.Fatal("a Judge desktop session uploaded because the sidebar doesn't sync")
	}
}

func TestAnOldSessionResumedUnderAnOffAccountStaysHere(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	// Synced before Stradl knew accounts: no account on record.
	a.writeSession("cli", "old", cwdLine("u1", filepath.Join(a.home, "code", "x")))
	syncAll(1, a, b)
	a.setSync(judgeAcct, false)
	a.signIn(judgeAcct, judgeOrg, "mcampana@judge.com")
	time.Sleep(10 * time.Millisecond)
	a.appendSession("cli", "old", cwdLine("u2", filepath.Join(a.home, "code", "x")))
	a.sync()
	b.sync()
	if got := b.readSession("cli", "old"); strings.Contains(got, "u2") {
		t.Fatal("work resumed under an account that doesn't sync reached bob")
	}
}

func TestASessionStartedDuringASwitchWaits(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.setSync(judgeAcct, false)
	a.signIn(bpAcct, bpOrg, "matt@breakpointdata.com")
	// Breakpoint was last confirmed a minute ago; then a session starts,
	// then the switch to Judge: the session can't be placed.
	h := a.s.readSignIns()
	h[len(h)-1].Seen = time.Now().Add(-time.Minute).UTC()
	b, _ := json.Marshal(h)
	_ = os.WriteFile(filepath.Join(a.s.stateDir(), signInsFile), b, 0600)
	a.writeSession("cli", "unsure", cwdLine("u1", filepath.Join(a.home, "code", "q")))
	time.Sleep(20 * time.Millisecond)
	data, _ := json.Marshal(map[string]any{"oauthAccount": map[string]string{"accountUuid": judgeAcct, "emailAddress": "mcampana@judge.com", "organizationUuid": judgeOrg}})
	if err := os.WriteFile(a.s.cfg.ClaudeJSONOverride, data, 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	a.writeSession("cli", "after", cwdLine("u2", filepath.Join(a.home, "code", "q")))
	a.sync()
	if hasSession(store, "unsure") || hasSession(store, "after") {
		t.Fatal("a session around a switch to an account that doesn't sync uploaded")
	}
	held := HeldSessions(a.s.cfg)
	if len(held) != 1 || held[0].ID != "unsure" {
		t.Fatalf("held = %+v, want only the session written during the switch", held)
	}
	if a.s.registry().Sessions["after"] != judgeAcct {
		t.Fatal("a session started after the switch belongs to Judge for certain")
	}
}

func TestSessionsWithoutAClaudeLoginStillSync(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.setSync(judgeAcct, false)
	// An API key: ~/.claude.json reads fine, without a claude.ai account.
	if err := os.WriteFile(a.s.cfg.ClaudeJSONOverride, []byte(`{"primaryApiKey":"x"}`), 0600); err != nil {
		t.Fatal(err)
	}
	a.s.noteSignIn(time.Now())
	time.Sleep(10 * time.Millisecond)
	a.writeSession("cli", "apikey", cwdLine("u1", filepath.Join(a.home, "code", "k")))
	a.sync()
	if !hasSession(store, "apikey") {
		t.Fatal("a session made without a claude.ai account should sync")
	}
}

func TestHeldSessionsCanBeAssigned(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	// Written before this Mac's sign-in history began.
	a.writeSession("cli", "old-work", cwdLine("u1", filepath.Join(a.home, "code", "w")))
	hist := `{"display":"old prompt","sessionId":"old-work"}` + "\n"
	if err := os.WriteFile(filepath.Join(a.claude, "history.jsonl"), []byte(hist), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	a.setSync(judgeAcct, false)
	a.signIn(bpAcct, bpOrg, "matt@breakpointdata.com")
	a.sync()
	if hasSession(store, "old-work") {
		t.Fatal("a session older than the sign-in history uploaded while an account is off")
	}
	other := newEverythingDevice(t, root, "carol", store)
	other.sync()
	if got, _ := os.ReadFile(filepath.Join(other.claude, "history.jsonl")); strings.Contains(string(got), "old prompt") {
		t.Fatal("a held session's prompt reached another Mac")
	}
	held := HeldSessions(a.s.cfg)
	if len(held) != 1 || held[0].ID != "old-work" {
		t.Fatalf("held = %+v", held)
	}
	if err := a.s.AssignSessions(context.Background(), bpAcct, []string{"old-work"}); err != nil {
		t.Fatal(err)
	}
	a.sync()
	if !hasSession(store, "old-work") || len(HeldSessions(a.s.cfg)) != 0 {
		t.Fatal("an assigned session should sync by its account's choice")
	}
}

func TestPlansStayHereWithFolderSyncOff(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	off := false
	a.s.cfg.FolderSync = &off
	a.setSync(judgeAcct, false)
	a.writeAccountSession(judgeAcct, judgeOrg, "j1", filepath.Join(a.home, "code", "x"))
	plan := filepath.Join(a.home, ".claude", "plans", "quiet-heron.md")
	a.appendSession("p", "cli-j1", `{"uuid":"u9","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"`+plan+`"}}]}}`+"\n")
	a.write(".claude/plans/quiet-heron.md", "the plan\n")
	a.sync()
	if len(keysWithPrefix(store, "plans/")) != 0 {
		t.Fatal("an off account's plan synced with folder sync off")
	}
}

func TestAPendingSidebarKeepsAnOffAccountsLayout(t *testing.T) {
	store := newCASStore()
	a, b := newSidebarPair(t, store)
	stubDesktopApp(t, false)
	a.syncSidebar(false) // the base holds the whole layout
	a.setSync(testAcct, false)
	// Bob adds a group in another account, which syncs.
	f := sidebarFile(b)
	m := f.load()
	ep := m["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any)
	ep["dframe-group-scopes"].(map[string]any)[bpAcct+"/"+bpOrg] = map[string]any{
		"groups": []any{map[string]any{"id": "GBP", "name": "Breakpoint"}}, "assignments": map[string]any{}, "order": map[string]any{},
	}
	f.save(m)
	b.syncSidebar(false)
	if res := a.syncSidebar(true); !res.PendingRestart {
		t.Fatalf("setup: alice should get a pending layout: %+v", res)
	}
	if _, err := ApplyPendingSidebar(a.s.cfg, a.s.sidebarPath()); err != nil {
		t.Fatal(err)
	}
	ep = sidebarFile(a).load()["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any)
	if len(groupScopeOf(ep)["assignments"].(map[string]any)) == 0 {
		t.Fatal("alice's layout of an account that doesn't sync was erased when the app quit")
	}
	if _, ok := ep["dframe-group-scopes"].(map[string]any)[bpAcct+"/"+bpOrg]; !ok {
		t.Fatal("bob's new group should arrive")
	}
}

func TestHistoryDoesntPingPongBetweenMacsWithOffPrompts(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	for _, d := range []*device{a, b} {
		d.writeAccountSession(judgeAcct, judgeOrg, "j-"+d.name, filepath.Join(d.home, "code", "x"))
	}
	a.setSync(judgeAcct, false)
	for _, d := range []*device{a, b} {
		hist := `{"display":"secret ` + d.name + `","sessionId":"cli-j-` + d.name + `"}` + "\n" + `{"display":"shared ` + d.name + `"}` + "\n"
		if err := os.WriteFile(filepath.Join(d.claude, "history.jsonl"), []byte(hist), 0600); err != nil {
			t.Fatal(err)
		}
	}
	syncAll(3, a, b)
	ra, rb := a.sync(), b.sync()
	if len(ra.Uploaded)+len(rb.Uploaded)+len(ra.Merged)+len(rb.Merged) != 0 {
		t.Fatalf("still moving after settling: %v %v %v %v", ra.Uploaded, rb.Uploaded, ra.Merged, rb.Merged)
	}
}

func TestAStaleCacheNeverUndoesANewerChoice(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	a.setSync(judgeAcct, false) // alice's copy: off
	b.sync()
	b.setSync(judgeAcct, true) // newer, on bob
	// The container is replaced; alice syncs first and brings back her
	// older choice, then bob brings back his newer one.
	fresh := newCASStore()
	a.s.storage, b.s.storage = fresh, fresh
	a.sync()
	b.sync()
	a.sync()
	if got := a.s.registry().Accounts[judgeAcct]; got == nil || got.Sync == nil || !*got.Sync {
		t.Fatalf("bob's newer choice to sync Judge was lost or undone: %+v", got)
	}
}

func TestTheClaudeAppsAccountShowsAsSignedInFromTheStart(t *testing.T) {
	root := t.TempDir()
	a := newEverythingDevice(t, root, "alice", newCASStore())
	a.signIn(judgeAcct, judgeOrg, "mcampana@judge.com") // the terminal
	const fresh = "9a8b7c6d-1111-4222-8333-444455556666"
	// The Claude app just signed in to an account with no session yet.
	if err := os.MkdirAll(a.desktop, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(a.desktop), "config.json"), []byte(`{"lastKnownAccountUuid":"`+fresh+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	list := ListAccounts(a.s.cfg)
	if len(list) != 2 || list[0].ID != fresh || !list[0].Active || list[0].Terminal {
		t.Fatalf("list = %+v, want the app's new account first and signed in", list)
	}
	if list[1].ID != judgeAcct || list[1].Active || !list[1].Terminal {
		t.Fatalf("list[1] = %+v, want Judge as the terminal's account", list[1])
	}
}

func TestAnAccountsEmailIsLearnedFromTheClaudeApp(t *testing.T) {
	root := t.TempDir()
	a := newEverythingDevice(t, root, "alice", newCASStore())
	cfg := filepath.Join(filepath.Dir(a.desktop), "local-agent-mode-sessions", bpAcct, bpOrg, "local_x", ".claude", ".claude.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"oauthAccount":{"accountUuid":"`+bpAcct+`","emailAddress":"matt@breakpointdata.com"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a.writeAccountSession(bpAcct, bpOrg, "b1", filepath.Join(a.home, "code", "y"))
	a.sync()
	list := ListAccounts(a.s.cfg)
	if len(list) != 1 || list[0].Email != "matt@breakpointdata.com" || list[0].Name != "matt@breakpointdata.com" {
		t.Fatalf("list = %+v, want the account shown by the email the app knows", list)
	}
}

func TestSidebarRecordsBringNoFoldersWhileTheSidebarDoesntSync(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	off := false
	a.s.cfg.DesktopSync = &off
	// A record whose transcript Claude Code already cleaned up.
	p := filepath.Join(a.desktop, bpAcct, bpOrg, "local_r1.json")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(desktopRecord("local_r1", "cli-r1", filepath.Join(a.home, "code", "old-a"), time.Now().UnixMilli(), "r1")), 0600); err != nil {
		t.Fatal(err)
	}
	a.write("code/old-a/x.go", "package a\n")
	a.sync()
	if keys := keysWithPrefix(store, FoldersPrefix+"code/old-a/"); len(keys) != 0 {
		t.Fatalf("a folder only a sidebar record names synced with the sidebar off: %v", keys)
	}
}

func TestAKnownFolderStopsSyncingOnceOnlyAnOffAccountWorksThere(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	// Synced before Stradl knew accounts: the folder has no account.
	a.write("code/judge-app/x.go", "v1\n")
	a.writeSession("cli", "old", cwdLine("u1", filepath.Join(a.home, "code", "judge-app")))
	syncAll(1, a, b)
	if !keysHave(store, FoldersPrefix+"code/judge-app/x.go") {
		t.Fatal("setup: the folder should sync")
	}
	a.setSync(judgeAcct, false)
	a.signIn(judgeAcct, judgeOrg, "mcampana@judge.com")
	time.Sleep(10 * time.Millisecond)
	a.appendSession("cli", "old", cwdLine("u2", filepath.Join(a.home, "code", "judge-app"))) // claude --resume
	a.write("code/judge-app/x.go", "v2 judge work\n")
	a.sync()
	a.sync()
	b.sync()
	if got := b.read("code/judge-app/x.go"); got != "v1\n" {
		t.Fatalf("work in a folder only an off account uses reached bob: %q", got)
	}
}

func TestEditsToAnOffAccountsLayoutSurviveAPendingLayout(t *testing.T) {
	store := newCASStore()
	a, b := newSidebarPair(t, store)
	stubDesktopApp(t, false)
	a.syncSidebar(false)
	a.setSync(testAcct, false)
	a.syncSidebar(false) // a pass with nothing new saves the base
	f := sidebarFile(b)
	m := f.load()
	ep := m["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any)
	ep["dframe-group-scopes"].(map[string]any)[bpAcct+"/"+bpOrg] = map[string]any{
		"groups": []any{map[string]any{"id": "GBP", "name": "Breakpoint"}}, "assignments": map[string]any{}, "order": map[string]any{},
	}
	f.save(m)
	b.syncSidebar(false)
	if res := a.syncSidebar(true); !res.PendingRestart {
		t.Fatalf("setup: alice should get a pending layout: %+v", res)
	}
	// Alice reorganizes her off account's groups while the app runs.
	fa := sidebarFile(a)
	m = fa.load()
	ep = m["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any)
	gs := groupScopeOf(ep)
	gs["groups"] = append(gs["groups"].([]any), map[string]any{"id": "GNEW", "name": "New"})
	gs["assignments"] = map[string]any{}
	fa.save(m)
	if _, err := ApplyPendingSidebar(a.s.cfg, a.s.sidebarPath()); err != nil {
		t.Fatal(err)
	}
	ep = fa.load()["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any)
	gs = groupScopeOf(ep)
	if len(gs["assignments"].(map[string]any)) != 0 {
		t.Fatal("moves alice made in an off account's layout were undone when the app quit")
	}
	found := false
	for _, g := range gs["groups"].([]any) {
		found = found || g.(map[string]any)["id"] == "GNEW"
	}
	if !found {
		t.Fatal("a group alice added to an off account's layout was lost when the app quit")
	}
	if _, ok := ep["dframe-group-scopes"].(map[string]any)[bpAcct+"/"+bpOrg]; !ok {
		t.Fatal("bob's new group should arrive")
	}
	// Turning the account back on keeps alice's layout (the base remembers
	// what storage still has).
	a.setSync(testAcct, true)
	a.syncSidebar(false)
	gs = groupScopeOf(fa.load()["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any))
	if len(gs["assignments"].(map[string]any)) != 0 {
		t.Fatal("turning the account back on undid alice's moves")
	}
}

func TestANewMacGetsNoneOfAnOffAccountsPlansPastesOrPrompts(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	a.writeAccountSession(judgeAcct, judgeOrg, "j1", filepath.Join(a.home, "code", "x"))
	a.writeAccountSession(bpAcct, bpOrg, "b1", filepath.Join(a.home, "code", "y"))
	for id, name := range map[string]string{"j1": "judge-plan", "b1": "bp-plan"} {
		plan := filepath.Join(a.home, ".claude", "plans", name+".md")
		a.appendSession("p", "cli-"+id, `{"uuid":"w-`+id+`","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"`+plan+`"}}]}}`+"\n")
		a.write(".claude/plans/"+name+".md", name+"\n")
	}
	a.write(".claude/paste-cache/jjj111.txt", "judge paste")
	a.write(".claude/paste-cache/bbb222.txt", "bp paste")
	hist := `{"display":"judge secret","pastedContents":{"1":{"id":1,"type":"text","contentHash":"jjj111"}},"sessionId":"cli-j1"}` + "\n" +
		`{"display":"my own prompt","pastedContents":{"1":{"id":1,"type":"text","contentHash":"bbb222"}},"sessionId":"cli-b1"}` + "\n"
	a.write(".claude/history.jsonl", hist)
	a.sync() // Judge syncs for a while...
	a.setSync(judgeAcct, false)
	a.sync()

	c := newEverythingDevice(t, root, "carol", store) // ...then a new Mac joins
	syncAll(3, c)
	for _, rel := range []string{".claude/plans/judge-plan.md", ".claude/paste-cache/jjj111.txt"} {
		if c.exists(rel) {
			t.Fatalf("the new Mac got %s, an off account's", rel)
		}
	}
	if got := c.read(".claude/history.jsonl"); strings.Contains(got, "judge secret") || !strings.Contains(got, "my own prompt") {
		t.Fatalf("the new Mac's history:\n%s", got)
	}
	for _, rel := range []string{".claude/plans/bp-plan.md", ".claude/paste-cache/bbb222.txt"} {
		if !c.exists(rel) {
			t.Fatalf("the new Mac should get %s, a synced account's", rel)
		}
	}
}

func TestAnExtraFolderStaysOnWhenAnOffAccountWorksInIt(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	a.s.cfg.ExtraFolders = []string{filepath.Join(a.home, "code", "x")}
	a.write("code/x/f.txt", "v1\n")
	syncAll(2, a, b)
	if b.read("code/x/f.txt") != "v1\n" {
		t.Fatal("setup: bob should get the extra folder")
	}
	a.setSync(judgeAcct, false)
	a.writeAccountSession(judgeAcct, judgeOrg, "j1", filepath.Join(a.home, "code", "x"))
	a.sync()
	b.write("code/x/f.txt", "v2 from bob\n")
	syncAll(2, b, a)
	if got := a.read("code/x/f.txt"); got != "v2 from bob\n" {
		t.Fatalf("a folder alice named in extra_folders stopped syncing on bob: alice has %q", got)
	}
}
