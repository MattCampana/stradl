package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// appAsset is the release asset holding Stradl.app, the Mac app.
const appAsset = "Stradl.app.zip"

// appPath returns where Stradl.app is installed, or where it goes:
// /Applications when it's there or writable, else ~/Applications.
func appPath() (path string, installed bool) {
	home, _ := os.UserHomeDir()
	for _, dir := range []string{"/Applications", filepath.Join(home, "Applications")} {
		p := filepath.Join(dir, "Stradl.app")
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p, true
		}
	}
	if syscall.Access("/Applications", 0x2 /* W_OK */) == nil {
		return "/Applications/Stradl.app", false
	}
	return filepath.Join(home, "Applications", "Stradl.app"), false
}

// installedAppVersion reads the version of the Stradl.app at path ("" when
// unknown).
func installedAppVersion(path string) string {
	out, err := exec.Command("/usr/bin/plutil", "-extract", "CFBundleShortVersionString", "raw", "-o", "-",
		filepath.Join(path, "Contents", "Info.plist")).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// appPids lists the running Stradl.app processes started from app.
func appPids(ctx context.Context, app string) []int {
	exe := filepath.Join(app, "Contents", "MacOS", "Stradl")
	// No -a: an app that started this update (an ancestor) is never quit
	// by it; its bundle is swapped and it runs on until relaunched.
	out, err := exec.CommandContext(ctx, "/usr/bin/pgrep", "-f", "^"+regexp.QuoteMeta(exe)+"( |$)").Output()
	if err != nil {
		return nil // pgrep exits 1 when nothing matches
	}
	return parsePids(string(out))
}

func parsePids(out string) []int {
	var pids []int
	for _, f := range strings.Fields(out) {
		if pid, err := strconv.Atoi(f); err == nil && pid > 0 && pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	return pids
}

// quitApp asks a running Stradl.app to quit, as if the user chose Quit, and
// waits for it; one still running after 10 s is stopped with SIGTERM. It
// reports whether the app was running.
func quitApp(ctx context.Context, app string) bool {
	pids := appPids(ctx, app)
	for _, pid := range pids {
		script := "ObjC.import('AppKit'); $.NSRunningApplication.runningApplicationWithProcessIdentifier(" + strconv.Itoa(pid) + ").terminate"
		_ = exec.CommandContext(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-e", script).Run()
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(pids) > 0 && time.Now().Before(deadline) && len(appPids(ctx, app)) > 0 {
		time.Sleep(250 * time.Millisecond)
	}
	for _, pid := range appPids(ctx, app) {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	return len(pids) > 0
}

// reopenApp opens Stradl.app again when it was open before the update.
func reopenApp(app string, wasOpen bool) {
	if wasOpen {
		if err := exec.Command("/usr/bin/open", app).Run(); err == nil {
			fmt.Printf("%s✓%s Reopened Stradl\n", colorGreen, colorReset)
		}
	}
}

// updateApp installs or replaces Stradl.app from release, verified against
// its checksums.txt. It never fails the update of the command itself: a
// problem is printed as a warning.
func updateApp(ctx context.Context, release *GitHubRelease) {
	if runtime.GOOS != "darwin" {
		return
	}
	zipURL, ok := release.assetURL(appAsset)
	if !ok {
		return // a release without the app
	}
	if err := installApp(ctx, release, zipURL); err != nil {
		printWarning("Stradl.app wasn't updated: " + err.Error())
	}
}

func installApp(ctx context.Context, release *GitHubRelease, zipURL string) error {
	checksumsURL, ok := release.assetURL(checksumsAsset)
	if !ok {
		return fmt.Errorf("release %s has no %s", release.TagName, checksumsAsset)
	}
	data, err := downloadAsset(ctx, zipURL)
	if err != nil {
		return err
	}
	sums, err := downloadAsset(ctx, checksumsURL)
	if err != nil {
		return err
	}
	want, err := checksumFor(sums, appAsset)
	if err != nil {
		return err
	}
	if err := verifySHA256(data, want); err != nil {
		return err
	}

	dest, installed := appPath()
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	// Unpack beside the destination, so the swap is two renames on one volume.
	stage, err := os.MkdirTemp(dir, ".stradl-app-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	zip := filepath.Join(stage, appAsset)
	if err := os.WriteFile(zip, data, 0600); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "/usr/bin/ditto", "-x", "-k", zip, stage).CombinedOutput(); err != nil {
		return fmt.Errorf("couldn't unpack %s: %v %s", appAsset, err, out)
	}
	fresh := filepath.Join(stage, "Stradl.app")
	if fi, err := os.Stat(fresh); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s doesn't contain Stradl.app", appAsset)
	}
	_ = exec.CommandContext(ctx, "/usr/bin/xattr", "-dr", "com.apple.quarantine", fresh).Run()

	// An open Stradl.app is asked to quit first, as if the user chose Quit,
	// and opens again on the new version once it's in place.
	wasOpen := false
	if installed {
		// Whoever reads this command's output may be gone after the app
		// quits: a write to the closed pipe must not kill the update
		// before it reopens the app.
		signal.Ignore(syscall.SIGPIPE)
		wasOpen = quitApp(ctx, dest)
	}
	old := filepath.Join(stage, "Stradl.app.old")
	if installed {
		if err := os.Rename(dest, old); err != nil {
			reopenApp(dest, wasOpen)
			return err
		}
	}
	if err := os.Rename(fresh, dest); err != nil {
		if installed {
			_ = os.Rename(old, dest)
		}
		reopenApp(dest, wasOpen)
		return err
	}
	reopenApp(dest, wasOpen)
	verb := "Updated"
	if !installed {
		verb = "Installed"
	}
	fmt.Printf("%s✓%s %s %s\n", colorGreen, colorReset, verb, dest)
	return nil
}
