package sync

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	reg, _, err := a.s.loadAccounts(context.Background())
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
	reg, _, err := a.s.loadAccounts(ctx)
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
	reg, _, err := a.s.loadAccounts(context.Background())
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
