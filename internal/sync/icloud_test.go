package sync

import (
	"io/fs"
	"os"
	"path/filepath"
	gosync "sync"
	"syscall"
	"testing"
)

// cloudOnly makes the files at the given paths look like iCloud placeholders
// until the returned function "downloads" them, and records the downloads the
// sync asks for.
func cloudOnly(t *testing.T, paths ...string) (download func(), requested func() []string) {
	t.Helper()
	var mu gosync.Mutex
	cloud := make(map[string]bool)
	for _, p := range paths {
		cloud[p] = true
	}
	var asked []string
	oldIn, oldReq := fileInICloud, RequestICloudDownload
	fileInICloud = func(abs string, info os.FileInfo) bool {
		mu.Lock()
		defer mu.Unlock()
		return cloud[abs]
	}
	RequestICloudDownload = func(abs string) {
		mu.Lock()
		asked = append(asked, abs)
		mu.Unlock()
	}
	t.Cleanup(func() { fileInICloud, RequestICloudDownload = oldIn, oldReq })
	download = func() {
		mu.Lock()
		cloud = map[string]bool{}
		mu.Unlock()
	}
	requested = func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}
	return download, requested
}

func TestICloudOnlyFilesWaitInsteadOfFailing(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := named(t, newEverythingDevice(t, root, "alice", store))
	b := named(t, newEverythingDevice(t, root, "matthew", store))
	foldersOn("p", a, b)

	a.write("code/p/shared.txt", "alice\n")
	a.write("code/p/only-alice.txt", "a\n")
	a.sync()

	// Bob's copy of shared.txt is still only in iCloud.
	b.write("code/p/shared.txt", "bob, still in iCloud\n")
	b.write("code/p/local.txt", "here\n")
	bobShared := filepath.Join(b.home, "code", "p", "shared.txt")
	download, requested := cloudOnly(t, bobShared)

	r := b.sync() // fails the test on any file error
	if len(r.InICloud) != 1 || r.InICloud[0] != FoldersPrefix+"code/p/shared.txt" {
		t.Fatalf("the cloud-only file should wait: %+v", r)
	}
	if got := requested(); len(got) == 0 || got[0] != bobShared {
		t.Fatalf("its download wasn't requested: %v", got)
	}
	if got := b.read("code/p/shared.txt"); got != "bob, still in iCloud\n" {
		t.Fatalf("bob's cloud-only copy was touched: %q", got)
	}
	if b.read("code/p/only-alice.txt") != "a\n" || !keysHave(store, FoldersPrefix+"code/p/local.txt") {
		t.Fatal("the rest of the folder should sync meanwhile")
	}

	// Downloaded: now it goes through first contact like any file.
	download()
	r = b.sync()
	if len(r.InICloud) != 0 {
		t.Fatalf("nothing should wait any more: %+v", r)
	}
	if got := b.read("code/p/shared.txt"); got != "alice\n" {
		t.Fatalf("the synced copy should stay the reference: %q", got)
	}
	if got := b.read("code/p/shared.txt.from-matthew-s-mac"); got != "bob, still in iCloud\n" {
		t.Fatalf("bob's copy should be kept beside it: %q", got)
	}
}

func TestICloudReadErrorsAreRecognized(t *testing.T) {
	_, requested := cloudOnly(t)
	if p, ok := inICloud(&fs.PathError{Op: "read", Path: "/Users/m/Desktop/x", Err: syscall.EDEADLK}); !ok || p != "/Users/m/Desktop/x" {
		t.Fatalf("EDEADLK read = %q, %v", p, ok)
	}
	if got := requested(); len(got) != 1 || got[0] != "/Users/m/Desktop/x" {
		t.Fatalf("download not requested: %v", got)
	}
	if _, ok := inICloud(&fs.PathError{Op: "read", Path: "/x", Err: syscall.EACCES}); ok {
		t.Fatal("a permission error isn't iCloud")
	}
	if _, ok := inICloud(&ICloudError{Path: "/y"}); !ok {
		t.Fatal("ICloudError is iCloud")
	}
}

func keysHave(store *casStore, key string) bool {
	for _, k := range store.keys() {
		if k == key+".age" || k == key {
			return true
		}
	}
	return false
}

func TestCloudOnlyFileDeletedElsewhereWaitsToBeTrashed(t *testing.T) {
	root := t.TempDir()
	store := newCASStore()
	a := newEverythingDevice(t, root, "alice", store)
	b := newEverythingDevice(t, root, "matthew", store)
	foldersOn("p", a, b)
	a.write("code/p/keep.txt", "k\n")
	a.write("code/p/gone.txt", "g\n")
	syncAll(1, a, b)

	a.remove("code/p/gone.txt")
	a.sync()
	bobGone := filepath.Join(b.home, "code", "p", "gone.txt")
	download, requested := cloudOnly(t, bobGone)
	r := b.sync()
	if len(r.InICloud) != 1 || len(r.Trashed) != 0 || !b.exists("code/p/gone.txt") {
		t.Fatalf("a cloud-only file should wait to be trashed: %+v", r)
	}
	if got := requested(); len(got) == 0 || got[0] != bobGone {
		t.Fatalf("its download wasn't requested: %v", got)
	}
	download()
	if r := b.sync(); len(r.Trashed) != 1 || b.exists("code/p/gone.txt") {
		t.Fatalf("once downloaded it goes to the trash: %+v", r)
	}
}

func TestICloudRenameErrorRequestsDownload(t *testing.T) {
	_, requested := cloudOnly(t)
	err := &os.LinkError{Op: "rename", Old: "/Users/m/Desktop/a", New: "/Users/m/.stradl/trash/a", Err: syscall.EDEADLK}
	if p, ok := inICloud(err); !ok || p != "/Users/m/Desktop/a" {
		t.Fatalf("rename EDEADLK = %q, %v", p, ok)
	}
	if got := requested(); len(got) != 1 || got[0] != "/Users/m/Desktop/a" {
		t.Fatalf("download not requested: %v", got)
	}
}
