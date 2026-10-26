package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/sync"
)

// stubSyncOnce replaces the sync command's engine call. Each call returns the
// next of results (nil = a clean sync); it records each call's wait flag.
func stubSyncOnce(t *testing.T, results ...error) *[]bool {
	t.Helper()
	installSyncSpy(t, false) // temp HOME, a config, quiet
	var waits []bool
	old := syncOnce
	syncOnce = func(ctx context.Context, cfg *config.Config, q, wait bool) (*sync.SyncReport, error) {
		waits = append(waits, wait)
		if len(waits) > len(results) {
			t.Fatalf("sync ran %d times, want %d", len(waits), len(results))
		}
		if err := results[len(waits)-1]; err != nil {
			return nil, err
		}
		return &sync.SyncReport{}, nil
	}
	t.Cleanup(func() { syncOnce = old })
	return &waits
}

func TestSyncWaitsForRunningSync(t *testing.T) {
	t.Run("no other sync", func(t *testing.T) {
		waits := stubSyncOnce(t, nil)
		if err := execute(syncCmd()); err != nil {
			t.Fatal(err)
		}
		if len(*waits) != 1 || (*waits)[0] {
			t.Errorf("calls (wait flags) = %v, want one that doesn't wait", *waits)
		}
	})

	t.Run("waits by default", func(t *testing.T) {
		waits := stubSyncOnce(t, sync.ErrLocked, nil)
		if err := execute(syncCmd()); err != nil {
			t.Fatal(err)
		}
		if len(*waits) != 2 || !(*waits)[1] {
			t.Errorf("calls (wait flags) = %v, want a second one that waits", *waits)
		}
	})

	t.Run("still locked after waiting", func(t *testing.T) {
		stubSyncOnce(t, sync.ErrLocked, sync.ErrLocked)
		err := execute(syncCmd())
		if err == nil || !strings.Contains(err.Error(), "still running") {
			t.Errorf("sync = %v, want a clear failure", err)
		}
	})

	t.Run("--no-wait skips", func(t *testing.T) {
		waits := stubSyncOnce(t, sync.ErrLocked)
		if err := execute(syncCmd(), "--no-wait"); err != nil {
			t.Fatal(err)
		}
		if len(*waits) != 1 {
			t.Errorf("sync ran %d times, want 1", len(*waits))
		}
	})

	t.Run("--wait still accepted", func(t *testing.T) {
		stubSyncOnce(t, nil)
		if err := execute(syncCmd(), "--wait"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("other errors", func(t *testing.T) {
		stubSyncOnce(t, errors.New("boom"))
		if err := execute(syncCmd()); err == nil || err.Error() != "boom" {
			t.Errorf("sync = %v, want boom", err)
		}
	})
}

func TestMigrateHoldsSyncLock(t *testing.T) {
	installSyncSpy(t, false) // a config whose key is missing
	release, err := sync.AcquireLock(config.LockFilePath(), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- execute(migrateCmd()) }()

	select {
	case err := <-done:
		release()
		t.Fatalf("migrate ran while a sync held the lock: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	release()

	select {
	case err := <-done:
		// Past the lock, the missing key stops it.
		if err == nil || errors.Is(err, sync.ErrLocked) || strings.Contains(err.Error(), "still running") {
			t.Errorf("migrate = %v, want it to get the lock and then fail on the missing key", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("migrate never took the lock after it was released")
	}
}
