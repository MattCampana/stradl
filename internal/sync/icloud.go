package sync

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// With iCloud's Desktop & Documents and "Optimize Mac Storage", a project
// folder can hold files whose contents live only in iCloud (dataless
// placeholders). Reading one makes macOS download it first, which can take
// minutes or, while iCloud is busy, much longer. The sync never waits for
// that: a cloud-only file sits the pass out, a background download is
// requested (RequestICloudDownload), and a later pass syncs it once it has
// arrived. On the sync path such a read fails at once with EDEADLK, because
// main turns downloads off for the process and on only for the downloader's
// threads.

// RequestICloudDownload asks for the cloud-only file at abs to be downloaded
// in the background. It must return at once. main sets it on macOS; the
// default does nothing.
var RequestICloudDownload = func(abs string) {}

// fileInICloud reports a file whose contents are only in iCloud (tests
// replace it).
var fileInICloud = func(abs string, info os.FileInfo) bool { return isDataless(info) }

// ICloudError is a file the pass skipped because its contents are only in
// iCloud; its download was requested.
type ICloudError struct{ Path string }

func (e *ICloudError) Error() string {
	return fmt.Sprintf("%s is only in iCloud: downloading it", e.Path)
}

// Is makes errors.Is(err, syscall.EDEADLK) match it, as a read would.
func (e *ICloudError) Is(target error) bool { return target == syscall.EDEADLK }

// inICloud reports whether err means a file is only in iCloud (a placeholder
// found before reading, or a read macOS refused with EDEADLK), requests its
// download when it names the file, and returns that path.
func inICloud(err error) (string, bool) {
	var ie *ICloudError
	if errors.As(err, &ie) {
		return ie.Path, true
	}
	if !errors.Is(err, syscall.EDEADLK) {
		return "", false
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		RequestICloudDownload(pe.Path)
		return pe.Path, true
	}
	return "", true
}

// checkInICloud returns an *ICloudError, after requesting the download, when
// the file described by info is only in iCloud.
func checkInICloud(abs string, info os.FileInfo) error {
	if info != nil && fileInICloud(abs, info) {
		RequestICloudDownload(abs)
		return &ICloudError{Path: abs}
	}
	return nil
}
