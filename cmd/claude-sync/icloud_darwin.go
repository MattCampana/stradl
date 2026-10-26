//go:build darwin && cgo

package main

/*
#include <sys/resource.h>
*/
import "C"

import (
	"os"
	"runtime"
	gosync "sync"
	"syscall"

	"github.com/tawanorg/claude-sync/internal/sync"
)

// Files iCloud keeps only in the cloud (see internal/sync/icloud.go): reads
// on the sync path must never wait for iCloud, so this process doesn't
// download them on a read (it fails at once with EDEADLK, and the sync skips
// the file), and a few downloader threads, and only they, fetch them.

// icloudWorkers is how many cloud-only files download at once; iCloud itself
// is the bottleneck, so a couple are plenty.
const icloudWorkers = 2

// icloudQueue bounds the files waiting to download. A request that doesn't
// fit is dropped; a later pass asks again.
const icloudQueue = 4096

// setupICloud turns downloads on read off for the process and lets the sync
// engine ask the background downloader for cloud-only files.
func setupICloud() {
	C.setiopolicy_np(C.IOPOL_TYPE_VFS_MATERIALIZE_DATALESS_FILES, C.IOPOL_SCOPE_PROCESS, C.IOPOL_MATERIALIZE_DATALESS_FILES_OFF)
	d := &icloudDownloader{queued: make(map[string]bool), ch: make(chan string, icloudQueue)}
	sync.RequestICloudDownload = d.request
}

// icloudDownloader downloads cloud-only files on its own threads, each file
// once at a time however often it is asked for.
type icloudDownloader struct {
	start  gosync.Once
	mu     gosync.Mutex
	queued map[string]bool // waiting or downloading
	ch     chan string
}

// request queues abs for download and returns at once.
func (d *icloudDownloader) request(abs string) {
	d.start.Do(func() {
		for range icloudWorkers {
			go d.work()
		}
	})
	d.mu.Lock()
	if d.queued[abs] {
		d.mu.Unlock()
		return
	}
	d.queued[abs] = true
	d.mu.Unlock()
	select {
	case d.ch <- abs:
	default:
		d.done(abs)
	}
}

func (d *icloudDownloader) done(abs string) {
	d.mu.Lock()
	delete(d.queued, abs)
	d.mu.Unlock()
}

// work downloads queued files. The download policy is per thread, so the
// goroutine keeps its thread for good and turns downloads on for it alone; a
// read that waits on iCloud for a long time holds up only this thread.
func (d *icloudDownloader) work() {
	runtime.LockOSThread()
	C.setiopolicy_np(C.IOPOL_TYPE_VFS_MATERIALIZE_DATALESS_FILES, C.IOPOL_SCOPE_THREAD, C.IOPOL_MATERIALIZE_DATALESS_FILES_ON)
	var b [1]byte
	for abs := range d.ch {
		// Reading one byte downloads a whole file; listing a folder
		// downloads the folder. Both stay on this thread: the syscall
		// package and os.ReadDir run their calls on the calling thread.
		if fd, err := syscall.Open(abs, syscall.O_RDONLY|syscall.O_CLOEXEC, 0); err == nil {
			var st syscall.Stat_t
			if syscall.Fstat(fd, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFDIR {
				_ = syscall.Close(fd)
				_, _ = os.ReadDir(abs)
			} else {
				_, _ = syscall.Read(fd, b[:])
				_ = syscall.Close(fd)
			}
		}
		d.done(abs)
	}
}
