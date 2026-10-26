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
	// LastSyncAt is when the last successful sync finished (no error and no
	// file that failed); LastDurationMs is how long that sync took.
	LastSyncAt     time.Time `json:"lastSyncAt,omitzero"`
	LastDurationMs int64     `json:"lastDurationMs"`
	// LastAttemptAt is when the last sync attempt finished, failed or not.
	LastAttemptAt time.Time `json:"lastAttemptAt,omitzero"`
	// LastError is the last attempt's failure ("" after a clean sync), and
	// LastErrorKind sorts it by what the user can do about it.
	LastError     string    `json:"lastError"`
	LastErrorKind ErrorKind `json:"lastErrorKind,omitempty"`

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
	// SidebarPending is true while sidebar layout changes from other Macs
	// (groups, membership, pins) wait for the Claude app to quit.
	SidebarPending bool `json:"sidebarPending,omitempty"`

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
	data, err := encodeStatus(st)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

func encodeStatus(st *Status) ([]byte, error) {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// AttemptFailed reports whether the last sync attempt failed. Waiting for
// `stradl init` is not a failure.
func (s *Status) AttemptFailed() bool {
	return s.LastError != "" && s.LastError != NotConfigured && !s.LastAttemptAt.IsZero()
}

// ErrorSummary is the short line for LastError, e.g. "Offline: will retry",
// or "" when there is no error. Status files written before LastErrorKind
// existed are sorted by their message.
func (s *Status) ErrorSummary() string {
	if s.LastError == "" {
		return ""
	}
	kind := s.LastErrorKind
	if kind == "" {
		kind = classifyMessage(s.LastError)
	}
	return kind.Summary()
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
	var kept []PendingBatch // a new slice: never rewrite one a copy may share
	for _, b := range s.PendingDesktop {
		if !appStart.After(b.At.Truncate(time.Second)) {
			kept = append(kept, b)
		}
	}
	changed := len(kept) != len(s.PendingDesktop)
	s.PendingDesktop = kept
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
