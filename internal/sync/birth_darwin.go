//go:build darwin

package sync

import (
	"os"
	"syscall"
	"time"
)

// fileBirth returns when a file was created (its modification time where
// the system doesn't say).
func fileBirth(info os.FileInfo) time.Time {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Birthtimespec.Sec > 0 {
		return time.Unix(st.Birthtimespec.Sec, st.Birthtimespec.Nsec)
	}
	return info.ModTime()
}
