package profiles

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	bp    = "c067ad00-1bc5-4a8d-b69e-d8ca9ae5d4a3"
	judge = "53d94190-1990-4286-b69d-42cdf507ba7a"
	owd   = "0a1b2c3d-1111-4222-8333-444455556666"
)

func testEnv(t *testing.T) Env {
	t.Helper()
	root := t.TempDir()
	e := Env{
		MainDir:     filepath.Join(root, "Claude"),
		ProfilesDir: filepath.Join(root, "Stradl", "Profiles"),
		StateDir:    filepath.Join(root, ".stradl"),
		Instances:   func() map[int]string { return nil },
	}
	if err := os.MkdirAll(filepath.Join(e.MainDir, sessionsDir), 0700); err != nil {
		t.Fatal(err)
	}
	return e
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func signIn(t *testing.T, dir, acct string) {
	writeFile(t, filepath.Join(dir, appConfigName), `{"lastKnownAccountUuid":"`+acct+`","oauth:tokenCache":"opaque"}`)
}

func TestTheMainProfileIsTheAppsOwnFolder(t *testing.T) {
	e := testEnv(t)
	signIn(t, e.MainDir, bp)
	list := e.List()
	if len(list) != 1 || !list[0].Main || list[0].ID != MainID || list[0].Account != bp || !list[0].SignedIn {
		t.Fatalf("list = %+v", list)
	}
	if p, ok := ForAccount(list, bp); !ok || !p.Main {
		t.Fatalf("Breakpoint should open in the main profile: %+v %v", p, ok)
	}
	if _, ok := ForAccount(list, judge); ok {
		t.Fatal("Judge has no profile yet")
	}
}

func TestANewProfileSharesTheSidebarRecords(t *testing.T) {
	e := testEnv(t)
	writeFile(t, filepath.Join(e.MainDir, desktopConfigName), `{"mcpServers":{"notes":{}}}`)
	p, err := e.Ensure(judge)
	if err != nil {
		t.Fatal(err)
	}
	if p.Main || p.ID != judge || p.For != judge || p.SignedIn || p.Account != judge {
		t.Fatalf("profile = %+v", p)
	}
	link := filepath.Join(p.Dir, sessionsDir)
	if dest, err := os.Readlink(link); err != nil || dest != filepath.Join(e.MainDir, sessionsDir) {
		t.Fatalf("records link = %q %v", dest, err)
	}
	// A record the profile's app writes lands in the main folder.
	writeFile(t, filepath.Join(link, judge, "org", "local_1.json"), `{}`)
	if _, err := os.Stat(filepath.Join(e.MainDir, sessionsDir, judge, "org", "local_1.json")); err != nil {
		t.Fatal("a record written in the profile isn't in the main folder")
	}
	if data, _ := os.ReadFile(filepath.Join(p.Dir, desktopConfigName)); !strings.Contains(string(data), "notes") {
		t.Fatal("the new profile should start with the main profile's preferences")
	}
	again, err := e.Ensure(judge)
	if err != nil || again.Dir != p.Dir {
		t.Fatalf("Ensure made a second profile: %+v %v", again, err)
	}
}

func TestConvertMakesAProfileForEveryOtherAccountOnce(t *testing.T) {
	e := testEnv(t)
	signIn(t, e.MainDir, bp)
	if e.Converted() {
		t.Fatal("not converted yet")
	}
	made, err := e.Convert([]string{bp, judge, owd, "not-an-id"})
	if err != nil {
		t.Fatal(err)
	}
	if len(made) != 2 || made[0].ID != judge || made[1].ID != owd {
		t.Fatalf("made = %+v", made)
	}
	if !e.Converted() {
		t.Fatal("conversion not recorded")
	}
	if made, err := e.Convert([]string{bp, judge, owd}); err != nil || len(made) != 0 {
		t.Fatalf("second Convert made %+v (%v)", made, err)
	}
	// Signed in to Judge in its own profile, Judge opens there.
	signIn(t, filepath.Join(e.ProfilesDir, judge), judge)
	if p, ok := ForAccount(e.List(), judge); !ok || p.ID != judge || !p.SignedIn {
		t.Fatalf("Judge's profile = %+v", p)
	}
}

func TestAProfileSignedInToAnotherAccountServesThatOne(t *testing.T) {
	e := testEnv(t)
	signIn(t, e.MainDir, bp)
	p, err := e.New()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.ID, "new-") || p.Account != "" {
		t.Fatalf("new profile = %+v", p)
	}
	signIn(t, p.Dir, owd)
	if got, ok := ForAccount(e.List(), owd); !ok || got.ID != p.ID {
		t.Fatalf("OWD should open in the profile it signed in to: %+v", got)
	}
	// Breakpoint's own profile, signed in, wins over the main one.
	bpp, err := e.create(bp, bp)
	if err != nil {
		t.Fatal(err)
	}
	signIn(t, bpp.Dir, bp)
	if got, _ := ForAccount(e.List(), bp); got.ID != bp {
		t.Fatalf("Breakpoint opens in %+v", got)
	}
	// A profile made for Judge but signed in to OWD isn't Judge's.
	jp, _ := e.create(judge, judge)
	signIn(t, jp.Dir, owd)
	if got, ok := ForAccount(e.List(), judge); ok {
		t.Fatalf("Judge opens in %+v", got)
	}
}

func TestLinkTakesInARecordsFolderTheAppMade(t *testing.T) {
	e := testEnv(t)
	dir := filepath.Join(e.ProfilesDir, judge)
	own := filepath.Join(dir, sessionsDir)
	writeFile(t, filepath.Join(own, judge, "o", "local_new.json"), `{"lastActivityAt":5}`)
	writeFile(t, filepath.Join(own, judge, "o", "local_both.json"), `{"lastActivityAt":9,"title":"profile"}`)
	writeFile(t, filepath.Join(own, judge, "o", "local_old.json"), `{"lastActivityAt":1,"title":"profile"}`)
	main := filepath.Join(e.MainDir, sessionsDir)
	writeFile(t, filepath.Join(main, judge, "o", "local_both.json"), `{"lastActivityAt":7,"title":"main"}`)
	writeFile(t, filepath.Join(main, judge, "o", "local_old.json"), `{"lastActivityAt":3,"title":"main"}`)
	if err := e.Link(dir); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(main, judge, "o", name))
		return string(b)
	}
	if !strings.Contains(read("local_new.json"), "5") || !strings.Contains(read("local_both.json"), "profile") || !strings.Contains(read("local_old.json"), "main") {
		t.Fatalf("merged: new=%s both=%s old=%s", read("local_new.json"), read("local_both.json"), read("local_old.json"))
	}
	if fi, err := os.Lstat(own); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the records folder should now be a link")
	}
	aside, _ := filepath.Glob(own + ".stradl-*")
	if len(aside) != 1 {
		t.Fatalf("the app's folder should be kept aside, not deleted: %v", aside)
	}
}

func TestDataDirAndLaunchArgs(t *testing.T) {
	if got := DataDir([]string{"--user-data-dir=/x/Profiles/a b"}, "/main"); got != "/x/Profiles/a b" {
		t.Fatalf("DataDir = %q", got)
	}
	if got := DataDir([]string{"--user-data-dir", "/x/p"}, "/main"); got != "/x/p" {
		t.Fatalf("DataDir = %q", got)
	}
	if got := DataDir(nil, "/main/"); got != "/main" {
		t.Fatalf("DataDir = %q", got)
	}
	if got := strings.Join(LaunchArgs(Profile{Main: true}), " "); got != "-n -a Claude" {
		t.Fatalf("main launch = %q", got)
	}
	if got := strings.Join(LaunchArgs(Profile{Dir: "/p/a b"}), " "); got != "-n -a Claude --args --user-data-dir=/p/a b" {
		t.Fatalf("profile launch = %q", got)
	}
}

func TestOpenBringsARunningProfileForwardOrStartsIt(t *testing.T) {
	e := testEnv(t)
	var calls []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, filepath.Base(name)+" "+strings.Join(args, " "))
		return nil, nil
	}
	p, err := e.Ensure(judge)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Open(context.Background(), run, p); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "open -n -a Claude --args --user-data-dir="+p.Dir {
		t.Fatalf("calls = %q", calls)
	}
	calls = nil
	p.Running, p.PID = true, 4242
	if err := e.Open(context.Background(), run, p); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "runningApplicationWithProcessIdentifier(4242).activate") {
		t.Fatalf("calls = %q", calls)
	}
}

func TestRunningInstancesAreMatchedToTheirProfiles(t *testing.T) {
	e := testEnv(t)
	p, _ := e.Ensure(judge)
	e.Instances = func() map[int]string { return map[int]string{100: e.MainDir, 200: p.Dir + "/"} }
	for _, got := range e.List() {
		want := map[string]int{MainID: 100, judge: 200}[got.ID]
		if !got.Running || got.PID != want {
			t.Fatalf("%s: running=%v pid=%d, want %d", got.ID, got.Running, got.PID, want)
		}
	}
}
