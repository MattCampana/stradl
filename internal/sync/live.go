package sync

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
		}
		if json.Unmarshal(data, &rec) != nil || rec.SessionID == "" || rec.PID <= 0 {
			continue
		}
		if processAlive(rec.PID) {
			live[rec.SessionID] = true
		}
	}
	return live
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
