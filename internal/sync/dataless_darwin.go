//go:build darwin

package sync

import (
	"os"
	"syscall"
)

// sfDataless is SF_DATALESS from <sys/stat.h>: the file's contents are only
// in the cloud.
const sfDataless = 0x40000000

func isDataless(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Flags&sfDataless != 0
}
