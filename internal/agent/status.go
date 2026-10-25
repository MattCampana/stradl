package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Status is the agent's view of the last sync, persisted to status.json for
// the menu bar and `stradl agent status`.
type Status struct {
	LastSyncAt     time.Time `json:"lastSyncAt,omitzero"`
	LastDurationMs int64     `json:"lastDurationMs"`
	LastError      string    `json:"lastError"`

	Uploaded   int `json:"uploaded"`
	Downloaded int `json:"downloaded"`
	Merged     int `json:"merged"`
	Deferred   int `json:"deferred"`
	Conflicts  int `json:"conflicts"`

	// SessionsTotal counts transcripts (projects/<dir>/<id>.jsonl) on this
	// device after the sync.
	SessionsTotal int `json:"sessionsTotal"`

	// PendingDesktopRestart counts desktop sidebar sessions that arrived
	// from other devices since the Claude app last started. The app reads its
	// sidebar only at launch, so they appear after it restarts.
	PendingDesktopRestart int `json:"pendingDesktopRestart"`

	Paused       bool   `json:"paused"`
	AgentVersion string `json:"agentVersion"`
	Device       string `json:"device"`
	PID          int    `json:"pid,omitempty"`

	// PendingDesktop records when each batch behind PendingDesktopRestart
	// arrived, so a Claude launch clears exactly the batches it picked up.
	PendingDesktop []PendingBatch `json:"pendingDesktop,omitempty"`
}

// PendingBatch is a group of desktop sessions written in one sync.
type PendingBatch struct {
	At    time.Time `json:"at"`
	Count int       `json:"count"`
}

// ReadStatus loads a status file. It always returns a usable Status: on a
// missing file (IsNotExist(err)) or a corrupt one it is the zero Status.
func ReadStatus(path string) (*Status, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return &Status{}, err
	}
	var st Status
	if err := json.Unmarshal(data, &st); err != nil {
		return &Status{}, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	return &st, nil
}

// WriteStatus replaces the status file atomically, so a reader never sees a
// half-written file.
func WriteStatus(path string, st *Status) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// addPending records n desktop sessions that arrived at t.
func (s *Status) addPending(t time.Time, n int) {
	if n <= 0 {
		return
	}
	s.PendingDesktop = append(s.PendingDesktop, PendingBatch{At: t, Count: n})
	s.recount()
}

// clearPendingBefore drops batches the Claude app has loaded since: those
// recorded before it started. Timestamps from ps have one-second
// resolution, so a batch from the same second as the launch is kept; the
// worst case is one unnecessary restart prompt, never a missed one.
func (s *Status) clearPendingBefore(appStart time.Time) bool {
	kept := s.PendingDesktop[:0]
	for _, b := range s.PendingDesktop {
		if !appStart.After(b.At.Truncate(time.Second)) {
			kept = append(kept, b)
		}
	}
	changed := len(kept) != len(s.PendingDesktop)
	s.PendingDesktop = kept
	if len(kept) == 0 {
		s.PendingDesktop = nil
	}
	s.recount()
	return changed
}

func (s *Status) recount() {
	n := 0
	for _, b := range s.PendingDesktop {
		n += b.Count
	}
	s.PendingDesktopRestart = n
}

// IsNotExist reports whether err means the status file doesn't exist yet.
func IsNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
