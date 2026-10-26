//go:build !darwin

package sync

import "os"

func isDataless(os.FileInfo) bool { return false }
