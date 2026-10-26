package sync

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
)

// RunSync builds a Syncer for cfg and runs one reconcile pass under the
// device-wide sync lock. With wait=false it returns ErrLocked immediately
// when another sync is in progress (the caller can simply skip: that sync
// covers the same files).
func RunSync(ctx context.Context, cfg *config.Config, quiet, wait bool) (*SyncReport, error) {
	lockPath := config.LockFilePath()
	if cfg.StateDirOverride != "" {
		lockPath = filepath.Join(cfg.StateDirOverride, config.LockFile)
	}
	release, err := AcquireLock(lockPath, wait, 2*time.Minute)
	if err != nil {
		return nil, err
	}
	defer release()

	s, err := NewSyncer(cfg, quiet)
	if err != nil {
		return nil, err
	}
	report, err := s.Sync(ctx)
	if err != nil {
		return report, err
	}
	if cfg.IsMCPSyncEnabled() {
		// MCP server configs live in ~/.claude.json and merge three ways
		// against the baseline in state.json, so they sync under the same lock.
		if _, err := s.PullMCP(ctx); err != nil {
			report.Errors = append(report.Errors, fmt.Errorf("MCP servers: %w", err))
		} else if _, err := s.PushMCP(ctx); err != nil {
			report.Errors = append(report.Errors, fmt.Errorf("MCP servers: %w", err))
		}
	}
	// The desktop sidebar layout (groups, membership, pins) merges as a whole
	// document; changes for a running app wait for it to quit.
	if sres, err := s.SyncSidebar(ctx, nil); err != nil {
		report.Errors = append(report.Errors, fmt.Errorf("desktop sidebar: %w", err))
	} else {
		report.SidebarPending = sres.PendingRestart
		report.SidebarChanged = sres.GroupsChanged + sres.AssignmentsChanged
	}
	// Best effort: remember what this Mac changed and tell the other Macs'
	// dashboards about this sync. Never fails the sync.
	s.AfterSync(ctx, report)
	return report, nil
}
