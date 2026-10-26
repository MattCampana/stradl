package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"syscall"
	"time"
)

// DashboardInfo is ~/.stradl/dashboard.json: where the running agent serves
// its dashboard. The menu bar and `stradl dashboard` open URL; PID tells a
// stale file (left by an agent that crashed) from a live one. The URL holds
// the dashboard's key, so the file is private to the user (0600).
type DashboardInfo struct {
	URL string `json:"url"`
	PID int    `json:"pid"`
}

// WriteDashboardInfo replaces the dashboard file atomically.
func WriteDashboardInfo(path string, info DashboardInfo) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

// ReadDashboardInfo loads the dashboard file. IsNotExist(err) means no
// agent has published a dashboard.
func ReadDashboardInfo(path string) (*DashboardInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var info DashboardInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	return &info, nil
}

// RemoveDashboardInfo deletes the dashboard file if it still belongs to
// pid, so an agent shutting down never removes the file of one that has
// already taken over.
func RemoveDashboardInfo(path string, pid int) error {
	info, err := ReadDashboardInfo(path)
	if err != nil {
		if IsNotExist(err) {
			return nil
		}
		return os.Remove(path) // unreadable: nobody can use it
	}
	if info.PID != pid {
		return nil
	}
	if err := os.Remove(path); err != nil && !IsNotExist(err) {
		return err
	}
	return nil
}

// LiveDashboard returns the URL of the dashboard a running agent serves for
// p. ok is false unless the dashboard file names a live process, an agent
// holds the single-instance lock, the URL is a loopback http address and
// something answers there; a file left by an agent that crashed fails one
// of those, so its key never reaches whatever reused the port or PID.
func LiveDashboard(p Paths) (string, bool) {
	info, err := ReadDashboardInfo(p.Dashboard)
	if err != nil || info.URL == "" || !ProcessAlive(info.PID) || !Running(p) {
		return "", false
	}
	u, err := url.Parse(info.URL)
	if err != nil || u.Scheme != "http" || u.Port() == "" {
		return "", false
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		return "", false
	}
	conn, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err != nil {
		return "", false
	}
	_ = conn.Close()
	return info.URL, true
}

// ProcessAlive reports whether a process with this PID exists.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Running reports whether an agent is running for p: whether something
// holds the single-instance lock. It only asks for a shared lock on the
// existing file, so it never creates the file or records a PID in it.
func Running(p Paths) bool {
	f, err := os.Open(p.Lock)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}
