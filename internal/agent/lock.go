package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/tawanorg/claude-sync/internal/sync"
)

// LockPoll is how often an agent started while another one runs checks
// whether it can take over.
const LockPoll = 3 * time.Second

// WaitForLock takes the single-instance lock through try, which fails with
// sync.ErrLocked while another agent holds it. Rather than exiting, which
// launchd's KeepAlive (SuccessfulExit=false) would read as "stay stopped",
// it polls every interval and takes over once the other agent is gone.
// waiting is called once, the first time the lock is busy. It returns
// ctx.Err() if ctx ends first.
func WaitForLock(ctx context.Context, try func() (release func(), err error), every time.Duration, waiting func()) (func(), error) {
	told := false
	for {
		release, err := try()
		if !errors.Is(err, sync.ErrLocked) {
			return release, err
		}
		if !told {
			told = true
			if waiting != nil {
				waiting()
			}
		}
		t := time.NewTimer(every)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// LockHolder returns the PID the agent holding the lock at path recorded,
// or "" when unknown. It is for messages only; the flock is what counts.
func LockHolder(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
