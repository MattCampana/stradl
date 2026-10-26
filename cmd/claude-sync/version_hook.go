package main

import "github.com/tawanorg/claude-sync/internal/sync"

// The sync engine reports the running version in this Mac's device heartbeat.
func init() { sync.AppVersion = version }
