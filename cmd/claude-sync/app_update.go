package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	if f, err := os.CreateTemp("/Applications", ".stradl-write-test-"); err == nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "/Applications/Stradl.app", false
	}
	return filepath.Join(home, "Applications", "Stradl.app"), false
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
