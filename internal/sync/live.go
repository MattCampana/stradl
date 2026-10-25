package sync

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// liveSessionIDs returns the IDs of Claude Code sessions whose process is
// still running on this device. Claude Code writes one sessions/<pid>.json
// record per running process. A running process holds its conversation in
// memory and keeps appending to its transcript, so rewriting that file under
// it would either be ignored or fork the conversation. Remote changes to a
// live transcript therefore wait until the process exits.
func liveSessionIDs(claudeDir string) map[string]bool {
	live := make(map[string]bool)
	entries, err := os.ReadDir(filepath.Join(claudeDir, "sessions"))
	if err != nil {
		return live
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(claudeDir, "sessions", e.Name()))
		if err != nil {
			continue
		}
		var rec struct {
			PID       int    `json:"pid"`
			SessionID string `json:"sessionId"`
			StartedAt int64  `json:"startedAt"` // Unix millis
		}
		if json.Unmarshal(data, &rec) != nil || rec.SessionID == "" || rec.PID <= 0 {
			continue
		}
		if processAlive(rec.PID) && !pidReused(rec.PID, rec.StartedAt) {
			live[rec.SessionID] = true
		}
	}
	return live
}

// pidReuseSlack tolerates the gap between a process starting and Claude Code
// recording its session start.
const pidReuseSlack = 2 * time.Minute

// pidReused reports whether pid now belongs to a process that started after
// the session record was written: Claude crashed, left its record behind, and
// the PID went to something else. Without this check one stale record would
// hold that session's sync forever.
func pidReused(pid int, startedAtMillis int64) bool {
	if startedAtMillis <= 0 {
		return false
	}
	start, ok := processStartTime(pid)
	if !ok {
		return false
	}
	return start.After(time.UnixMilli(startedAtMillis).Add(pidReuseSlack))
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// transcriptSessionID extracts the session ID a projects/ path belongs to:
// projects/<dir>/<id>.jsonl, or anything under projects/<dir>/<id>/
// (subagent transcripts, tool results). ok is false for other paths.
func transcriptSessionID(npath string) (string, bool) {
	parts := strings.Split(npath, "/")
	if len(parts) < 3 || parts[0] != "projects" {
		return "", false
	}
	if len(parts) == 3 {
		return strings.TrimSuffix(parts[2], ".jsonl"), strings.HasSuffix(parts[2], ".jsonl")
	}
	return parts[2], true
}
