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

func TestSyncAllowMassDeleteReachesTheEngine(t *testing.T) {
	installSyncSpy(t, false)
	var got []bool
	old := syncOnce
	syncOnce = func(ctx context.Context, cfg *config.Config, q, wait bool) (*sync.SyncReport, error) {
		got = append(got, cfg.AllowMassDelete)
		return &sync.SyncReport{}, nil
	}
	t.Cleanup(func() { syncOnce = old })
	if err := execute(syncCmd()); err != nil {
		t.Fatal(err)
	}
	if err := execute(syncCmd(), "--allow-mass-delete"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("AllowMassDelete per run = %v, want [false true]", got)
	}
}

func TestPrintSyncReportShowsDeletesTrashAndWarnings(t *testing.T) {
	out := captureStdout(t, func() {
		printSyncReport(&sync.SyncReport{
			Deleted:  []string{"_folders/code/app/old.go"},
			Trashed:  []string{"skills/x/SKILL.md", "plans/p.md"},
			Warnings: []string{"~/code/big: 15 of 20 synced files would be deleted at once, so none were."},
		}, time.Second)
	})
	for _, want := range []string{"1 deleted on every Mac", "2 moved to ~/.stradl/trash", "15 of 20 synced files"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Everything is in sync") {
		t.Errorf("a pass with deletions isn't idle:\n%s", out)
	}
	if !pendingWork(&sync.SyncReport{Warnings: []string{"w"}}) || pendingWork(&sync.SyncReport{}) {
		t.Error("pendingWork should count warnings and nothing else when idle")
	}
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
