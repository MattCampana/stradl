//go:build !(darwin && cgo)

package main

import "github.com/tawanorg/claude-sync/internal/agent"

// The menu bar needs Cocoa through cgo; other builds run headless.
const trayAvailable = false

func runTray(opts agent.Options) error { return runHeadless(opts) }
