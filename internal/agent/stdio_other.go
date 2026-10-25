//go:build !darwin

package agent

import "os"

// redirectStdio is only needed under launchd.
func redirectStdio(*os.File, os.FileInfo) {}
