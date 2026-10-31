package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/tawanorg/claude-sync/internal/agent"
	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/sync"
)

// applyPendingSidebar applies sidebar layout changes (groups, membership,
// pins) that arrived from other Macs while the Claude app was running. It
// takes the sync lock, because ApplyPendingSidebar rewrites state the sync
// pass also reads, and does nothing while the app runs. It reports whether
// changes are still waiting.
func applyPendingSidebar(ctx context.Context, logger *agent.Logger) bool {
	cfg, err := config.Load()
	if err != nil {
		return false
	}
	if cfg.EffectiveDesktopDir() == "" {
		return false
	}
	lockPath := config.LockFilePath()
	if cfg.StateDirOverride != "" {
		lockPath = filepath.Join(cfg.StateDirOverride, config.LockFile)
	}
	// Between Claude quitting and reopening, don't keep it closed for long
	// behind a running sync: wait only as long as the caller allows.
	wait := 2 * time.Minute
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl) - 5*time.Second; left < wait {
			wait = max(left, time.Second)
		}
	}
	release, err := sync.AcquireLock(lockPath, true, wait)
	if err != nil {
		logger.Printf("sidebar: waiting for the sync lock: %v", err)
		return sync.SidebarPendingExists(cfg)
	}
	defer release()
	applied, err := sync.ApplyPendingSidebars(cfg)
	switch {
	case err != nil:
		logger.Printf("sidebar: couldn't apply changes from your other Macs: %v", err)
	case applied:
		logger.Printf("sidebar: applied groups and pins from your other Macs")
	}
	return sync.SidebarPendingExists(cfg)
}
