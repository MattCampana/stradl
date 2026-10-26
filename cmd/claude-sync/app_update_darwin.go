//go:build darwin && cgo

package main

import (
	"context"
	"strings"
	"time"

	"github.com/tawanorg/claude-sync/internal/agent"
)

// ensureAppInstalled installs Stradl.app from this version's release when it
// is missing: a Mac that updated from 2.1.x got only the new command (the old
// updater knows nothing of the app), and this agent is the first 2.2 code to
// run there. Release builds only; problems are logged and retried at the next
// agent start.
func ensureAppInstalled(logger *agent.Logger) {
	if strings.Contains(version, "-") || checkReleaseRepo() != nil {
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
