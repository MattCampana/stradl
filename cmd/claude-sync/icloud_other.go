//go:build !darwin || !cgo

package main

// allowICloudDownloads is a no-op where there are no iCloud placeholders.
func allowICloudDownloads() error { return nil }
