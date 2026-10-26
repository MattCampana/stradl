package agent

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tawanorg/claude-sync/internal/sync"
)

func TestDashboardInfoLifecycle(t *testing.T) {
	p := PathsFor(t.TempDir())
	if _, err := ReadDashboardInfo(p.Dashboard); !IsNotExist(err) {
		t.Fatalf("read before write: %v, want not-exist", err)
	}
	if err := RemoveDashboardInfo(p.Dashboard, 1); err != nil {
		t.Fatalf("remove before write: %v", err)
	}

	want := DashboardInfo{URL: "http://127.0.0.1:4321/?t=key", PID: os.Getpid()}
	if err := WriteDashboardInfo(p.Dashboard, want); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p.Dashboard)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("dashboard.json mode %v, want 0600: the URL holds the key", fi.Mode().Perm())
	}
	got, err := ReadDashboardInfo(p.Dashboard)
	if err != nil || *got != want {
		t.Fatalf("read back %+v, %v; want %+v", got, err, want)
	}
	entries, _ := os.ReadDir(filepath.Dir(p.Dashboard))
	for _, e := range entries {
		if e.Name() != "dashboard.json" {
			t.Errorf("left %s behind", e.Name())
		}
	}

	// An agent shutting down leaves the file of one that took over alone.
	if err := RemoveDashboardInfo(p.Dashboard, want.PID+1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.Dashboard); err != nil {
		t.Fatal("removed another agent's dashboard file")
	}
	if err := RemoveDashboardInfo(p.Dashboard, want.PID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.Dashboard); !os.IsNotExist(err) {
		t.Fatal("dashboard file still there after its agent removed it")
	}

	// A corrupt file is useless to everyone, so any agent may remove it.
	if err := os.WriteFile(p.Dashboard, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDashboardInfo(p.Dashboard); err == nil {
		t.Fatal("parsed a corrupt dashboard file")
	}
	if err := RemoveDashboardInfo(p.Dashboard, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.Dashboard); !os.IsNotExist(err) {
		t.Fatal("corrupt dashboard file kept")
	}
}

func TestProcessAlive(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Fatal("this process is not alive")
	}
	for _, pid := range []int{0, -1} {
		if ProcessAlive(pid) {
			t.Errorf("ProcessAlive(%d) = true", pid)
		}
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Skip("cannot run /bin/sh:", err)
	}
	if ProcessAlive(cmd.Process.Pid) {
		t.Errorf("exited process %d reported alive", cmd.Process.Pid)
	}
}

func TestRunning(t *testing.T) {
	p := PathsFor(t.TempDir())
	if Running(p) {
		t.Fatal("running without a lock file")
	}
	if _, err := os.Stat(p.Lock); !os.IsNotExist(err) {
		t.Fatal("Running created the lock file")
	}
	release, err := sync.AcquireLock(p.Lock, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !Running(p) {
		t.Fatal("not running while the lock is held")
	}
	release()
	if Running(p) {
		t.Fatal("running after the lock was released")
	}
}

func TestLiveDashboard(t *testing.T) {
	p := PathsFor(t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	live := "http://" + ln.Addr().String() + "/?t=key"
	write := func(u string, pid int) {
		t.Helper()
		if err := WriteDashboardInfo(p.Dashboard, DashboardInfo{URL: u, PID: pid}); err != nil {
			t.Fatal(err)
		}
	}

	if _, ok := LiveDashboard(p); ok {
		t.Fatal("live without a dashboard file")
	}
	write(live, os.Getpid())
	if _, ok := LiveDashboard(p); ok {
		t.Fatal("live while no agent holds the lock")
	}

	release, err := sync.AcquireLock(p.Lock, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if u, ok := LiveDashboard(p); !ok || u != live {
		t.Fatalf("LiveDashboard = %q, %v; want %q", u, ok, live)
	}

	dead := exec.Command("/bin/sh", "-c", "exit 0")
	if err := dead.Run(); err == nil {
		write(live, dead.Process.Pid)
		if _, ok := LiveDashboard(p); ok {
			t.Error("live with the PID of an exited process")
		}
	}
	for _, u := range []string{
		"https://" + ln.Addr().String() + "/?t=key",
		"http://192.0.2.1:80/?t=key",
		"http://example.com:80/?t=key",
		"http://127.0.0.1/?t=key",
		"::not a url",
	} {
		write(u, os.Getpid())
		if _, ok := LiveDashboard(p); ok {
			t.Errorf("live with URL %q", u)
		}
	}

	write(live, os.Getpid())
	_ = ln.Close()
	if _, ok := LiveDashboard(p); ok {
		t.Error("live with nothing listening")
	}
}
