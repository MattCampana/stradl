//go:build !darwin

package sync

import (
	"os"
	"time"
)

// fileBirth returns when a file was created; without a creation time on
// this system, its modification time.
func fileBirth(info os.FileInfo) time.Time { return info.ModTime() }
