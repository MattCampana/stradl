//go:build darwin

package agent

import (
	"os"
	"syscall"
)

// redirectStdio points stdout and stderr at f when they currently write to
// the file described by old (the log launchd opened for us, just rotated).
func redirectStdio(f *os.File, old os.FileInfo) {
	for _, std := range []*os.File{os.Stdout, os.Stderr} {
		if fi, err := std.Stat(); err == nil && os.SameFile(fi, old) {
			_ = syscall.Dup2(int(f.Fd()), int(std.Fd()))
		}
	}
}
