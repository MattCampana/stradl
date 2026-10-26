//go:build darwin && cgo

package main

/*
#include <sys/resource.h>
*/
import "C"

// allowICloudDownloads lets this process read files that iCloud keeps only in
// the cloud (Desktop & Documents with "Optimize Mac Storage"): macOS then
// downloads a file when Stradl reads it, instead of failing the read with
// "resource deadlock avoided", which is what a background agent gets by
// default. Everything else on the Mac is unaffected; the setting is this
// process's own.
func allowICloudDownloads() error {
	if r, err := C.setiopolicy_np(C.IOPOL_TYPE_VFS_MATERIALIZE_DATALESS_FILES, C.IOPOL_SCOPE_PROCESS, C.IOPOL_MATERIALIZE_DATALESS_FILES_ON); r != 0 {
		return err
	}
	return nil
}
