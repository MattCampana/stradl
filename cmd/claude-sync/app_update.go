package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/tawanorg/claude-sync/internal/agent"
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

// ensureAppInstalled installs Stradl.app from this version's release when it
// is missing: a Mac that updated from 2.1.x got only the new command (the old
// updater knows nothing of the app), and this agent is the first 2.2 code to
// run there. Release builds only; problems are logged and retried at the next
// agent start.
func ensureAppInstalled(logger *agent.Logger) {
	if runtime.GOOS != "darwin" || strings.Contains(version, "-") || checkReleaseRepo() != nil {
		return
	}
	if _, installed := appPath(); installed {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	release, err := getLatestRelease(ctx)
	if err != nil || strings.TrimPrefix(release.TagName, "v") != version {
		return // offline, or this isn't the latest release: stradl update covers it
	}
	zipURL, ok := release.assetURL(appAsset)
	if !ok {
		return
	}
	if err := installApp(ctx, release, zipURL); err != nil {
		logger.Printf("couldn't install Stradl.app: %v", err)
		return
	}
	logger.Printf("installed Stradl.app %s", version)
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

	old := filepath.Join(stage, "Stradl.app.old")
	if installed {
		if err := os.Rename(dest, old); err != nil {
			return err
		}
	}
	if err := os.Rename(fresh, dest); err != nil {
		if installed {
			_ = os.Rename(old, dest)
		}
		return err
	}
	verb := "Updated"
	if !installed {
		verb = "Installed"
	}
	fmt.Printf("%s✓%s %s %s\n", colorGreen, colorReset, verb, dest)
	return nil
}
