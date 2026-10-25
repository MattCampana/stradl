package sync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrLocked is returned by AcquireLock when another sync holds the lock and
// the caller asked not to wait.
var ErrLocked = errors.New("another sync is already running")

// AcquireLock takes an exclusive advisory lock on path, so a background sync,
// a manual `sync` and the menu bar app never reconcile the same files at the
// same moment. The kernel drops the lock when the process exits, so a crash
// can never leave it stuck. With wait=false it fails fast with ErrLocked; with
// wait=true it polls until timeout.
func AcquireLock(path string, wait bool, timeout time.Duration) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("failed to create lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open lock file: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("failed to lock %s: %w", path, err)
		}
		if !wait || time.Now().After(deadline) {
			_ = f.Close()
			return nil, ErrLocked
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Record the holder for diagnostics only; the flock is what matters.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
