//go:build !darwin

package sync

import "time"

// processStartTime is only implemented on macOS; elsewhere a live PID is
// trusted as is.
func processStartTime(int) (time.Time, bool) { return time.Time{}, false }
