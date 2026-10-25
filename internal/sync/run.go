package sync

import (
	"context"
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
	return s.Sync(ctx)
}
