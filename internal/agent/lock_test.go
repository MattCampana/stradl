package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/sync"
)

func TestWaitForLockTakesOverWhenFree(t *testing.T) {
	busy := 3
	tries, waits := 0, 0
	released := false
	try := func() (func(), error) {
		tries++
		if busy > 0 {
			busy--
			return nil, sync.ErrLocked
		}
		return func() { released = true }, nil
	}
	release, err := WaitForLock(context.Background(), try, time.Millisecond, func() { waits++ })
	if err != nil || release == nil {
		t.Fatalf("release=%v err=%v", release != nil, err)
	}
	if tries != 4 || waits != 1 {
		t.Fatalf("tries=%d waits=%d, want 4 tries and one waiting message", tries, waits)
	}
	release()
	if !released {
		t.Fatal("returned release func is not the lock's")
	}
}

func TestWaitForLockStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	try := func() (func(), error) { return nil, sync.ErrLocked }
	done := make(chan error, 1)
	go func() {
		_, err := WaitForLock(ctx, try, time.Hour, cancel) // cancel as soon as it starts waiting
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForLock ignored cancellation")
	}
}

func TestWaitForLockReturnsOtherErrors(t *testing.T) {
	boom := errors.New("permission denied")
	waited := false
	_, err := WaitForLock(context.Background(), func() (func(), error) { return nil, boom }, time.Millisecond, func() { waited = true })
	if !errors.Is(err, boom) || waited {
		t.Fatalf("err=%v waited=%v", err, waited)
	}
}

// TestWaitForLockRealLock runs a second instance against a held flock: it
// waits, then takes over once the first lets go.
func TestWaitForLockRealLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.lock")
	first, err := sync.AcquireLock(path, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := LockHolder(path); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("LockHolder = %q", got)
	}

	waiting := make(chan struct{})
	got := make(chan func(), 1)
	go func() {
		release, err := WaitForLock(context.Background(), func() (func(), error) {
			return sync.AcquireLock(path, false, 0)
		}, 5*time.Millisecond, func() { close(waiting) })
		if err != nil {
			t.Error(err)
		}
		got <- release
	}()

	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("second instance never noticed the held lock")
	}
	select {
	case <-got:
		t.Fatal("second instance took the lock while the first held it")
	case <-time.After(50 * time.Millisecond):
	}

	first()
	select {
	case release := <-got:
		release()
	case <-time.After(5 * time.Second):
		t.Fatal("second instance never took over")
	}
	if LockHolder(filepath.Join(t.TempDir(), "missing")) != "" {
		t.Fatal("missing lock file has a holder")
	}
}
