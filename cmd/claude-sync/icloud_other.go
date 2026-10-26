//go:build !darwin || !cgo

package main

// setupICloud does nothing where there are no iCloud placeholders to fetch.
func setupICloud() {}
