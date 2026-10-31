package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tawanorg/claude-sync/internal/profiles"
)

const owdAcct = "0a1b2c3d-1111-4222-8333-444455556666"

// Claude app profiles (see package profiles): each keeps its own sidebar
// layout file, merged with storage like another Mac's, and shares the main
// folder's sidebar records.

func TestEachProfileKeepsItsOwnLayout(t *testing.T) {
	store := newCASStore()
	a, b := newSidebarPair(t, store)
	stubDesktopApp(t, false)
	prof, err := profiles.FromConfig(a.s.cfg).Ensure(judgeAcct) // starts with alice's layout
	if err != nil {
		t.Fatal(err)
	}
	pf := appFile{t, filepath.Join(prof.Dir, sidebarConfigName)}
	a.syncSidebar(false)
	// Alice groups Judge's sessions in Judge's profile.
	m := pf.load()
	m["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any)["dframe-group-scopes"].(map[string]any)[judgeAcct+"/"+judgeOrg] = map[string]any{
		"groups": []any{map[string]any{"id": "GJ", "name": "Judge"}}, "assignments": map[string]any{}, "order": map[string]any{},
	}
	pf.save(m)
	a.syncSidebar(false)
	b.syncSidebar(false)
	scopes := func(f appFile) map[string]any {
		return f.load()["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any)["dframe-group-scopes"].(map[string]any)
	}
	if _, ok := scopes(sidebarFile(b))[judgeAcct+"/"+judgeOrg]; !ok {
		t.Fatal("a group made in a profile should reach the other Macs")
	}

	// Judge's profile is open; the main one isn't. Bob adds a group.
	old := DesktopAppRunning
	DesktopAppRunning = func(dir string) bool { return filepath.Clean(dir) == filepath.Clean(prof.Dir) }
	t.Cleanup(func() { DesktopAppRunning = old })
	m = sidebarFile(b).load()
	m["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any)["dframe-group-scopes"].(map[string]any)[bpAcct+"/"+bpOrg] = map[string]any{
		"groups": []any{map[string]any{"id": "GBP", "name": "Breakpoint"}}, "assignments": map[string]any{}, "order": map[string]any{},
	}
	sidebarFile(b).save(m)
	b.syncSidebar(false)
	res, err := a.s.SyncSidebar(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.PendingRestart || !res.AppliedLocally {
		t.Fatalf("want the main profile written and Judge's pending: %+v", res)
	}
	if _, ok := scopes(sidebarFile(a))[bpAcct+"/"+bpOrg]; !ok {
		t.Fatal("the main profile, closed, should get bob's group now")
	}
	if _, ok := scopes(pf)[bpAcct+"/"+bpOrg]; ok {
		t.Fatal("an open profile's file was written underneath it")
	}
	if _, err := os.Stat(filepath.Join(a.s.cfg.StateDirOverride, "sidebar-pending."+judgeAcct+".json")); err != nil {
		t.Fatal("the open profile's layout should wait in its own pending file")
	}
	if !SidebarPendingExists(a.s.cfg) {
		t.Fatal("a profile's pending layout should count")
	}
	// Judge's profile quits.
	DesktopAppRunning = func(string) bool { return false }
	if applied, err := ApplyPendingSidebars(a.s.cfg); err != nil || !applied {
		t.Fatalf("apply: %v %v", applied, err)
	}
	if _, ok := scopes(pf)[bpAcct+"/"+bpOrg]; !ok {
		t.Fatal("the profile should get bob's group once it quits")
	}
	if SidebarPendingExists(a.s.cfg) {
		t.Fatal("nothing should be pending any more")
	}
}

func TestAccountsSignedInToProfilesShowAsSignedIn(t *testing.T) {
	root := t.TempDir()
	a := newEverythingDevice(t, root, "alice", newCASStore())
	if err := os.MkdirAll(a.desktop, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(a.desktop), "config.json"), []byte(`{"lastKnownAccountUuid":"`+bpAcct+`","oauth:tokenCache":"x"}`), 0600); err != nil {
		t.Fatal(err)
	}
	env := profiles.FromConfig(a.s.cfg)
	jp, err := env.Ensure(judgeAcct)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jp.Dir, "config.json"), []byte(`{"lastKnownAccountUuid":"`+judgeAcct+`","oauth:tokenCache":"x"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Ensure(owdAcct); err != nil { // made, not signed in yet
		t.Fatal(err)
	}
	// Emails come from any profile's agent-mode sessions.
	cfgPath := filepath.Join(jp.Dir, "local-agent-mode-sessions", judgeAcct, judgeOrg, "local_x", ".claude", ".claude.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(`{"oauthAccount":{"accountUuid":"`+judgeAcct+`","emailAddress":"mcampana@judge.com"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	old := appInstances
	appInstances = func(string) map[int]string { return map[int]string{77: jp.Dir} }
	t.Cleanup(func() { appInstances = old })
	a.sync()
	got := map[string]Account{}
	for _, acc := range ListAccounts(a.s.cfg) {
		got[acc.ID] = acc
	}
	if b := got[bpAcct]; !b.Active || b.Profile != profiles.MainID || b.SignIn || b.Open {
		t.Fatalf("Breakpoint = %+v", b)
	}
	if j := got[judgeAcct]; !j.Active || j.Profile != judgeAcct || j.SignIn || !j.Open || j.Email != "mcampana@judge.com" {
		t.Fatalf("Judge = %+v", j)
	}
	if _, ok := got[owdAcct]; ok {
		t.Fatal("a profile no one signed in to yet named an account")
	}
}
