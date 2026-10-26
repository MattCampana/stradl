package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/agent"
	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/dashboard"
	"github.com/tawanorg/claude-sync/internal/sync"
)

// browse opens a dashboard URL the way a browser does: the key visit sets
// the cookie and redirects to the page.
func browse(t *testing.T, u string) (int, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestAgentDashboardLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := agent.PathsFor(home)

	u, stop := serveAgentDashboard(p, dashboard.NewFake(time.Now()), nil)
	if !strings.HasPrefix(u, "http://127.0.0.1:") || !strings.Contains(u, "/?t=") {
		t.Fatalf("dashboard URL %q", u)
	}
	fi, err := os.Stat(p.Dashboard)
	if err != nil {
		t.Fatalf("dashboard.json not written: %v", err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("dashboard.json mode %v, want 0600", fi.Mode().Perm())
	}
	var info map[string]any
	data, _ := os.ReadFile(p.Dashboard)
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	if info["url"] != u || info["pid"] != float64(os.Getpid()) {
		t.Errorf("dashboard.json = %s, want url %q and pid %d", data, u, os.Getpid())
	}
	if code, body := browse(t, u); code != http.StatusOK || !strings.Contains(body, "Musical chairs for your Claude sessions.") {
		t.Fatalf("dashboard page: %d", code)
	}

	stop()
	if _, err := os.Stat(p.Dashboard); !os.IsNotExist(err) {
		t.Error("dashboard.json left behind after the agent stopped")
	}
	if _, err := http.Get(u); err == nil {
		t.Error("dashboard still answering after the agent stopped")
	}
	stop() // twice is harmless
}

// A dashboard that can't start leaves the agent running without one, and
// clears a file a crashed agent left.
func TestAgentDashboardFailureIsNotFatal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := agent.PathsFor(home)
	if err := agent.WriteDashboardInfo(p.Dashboard, agent.DashboardInfo{URL: "http://127.0.0.1:1/?t=old", PID: 1}); err != nil {
		t.Fatal(err)
	}
	old := dashboardServerOptions
	dashboardServerOptions = dashboard.ServerOptions{Addr: "0.0.0.0:0"} // refused: not loopback
	defer func() { dashboardServerOptions = old }()

	u, stop := serveAgentDashboard(p, dashboard.NewFake(time.Now()), nil)
	defer stop()
	if u != "" {
		t.Fatalf("URL %q from a server that should not have started", u)
	}
	if _, err := os.Stat(p.Dashboard); !os.IsNotExist(err) {
		t.Error("stale dashboard.json kept")
	}
}

// `stradl dashboard` opens the running agent's dashboard rather than
// serving a second one.
func TestDashboardCommandUsesAgentDashboard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := agent.PathsFor(home)
	release, err := sync.AcquireLock(p.Lock, false, 0) // stands in for the agent
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	u, stop := serveAgentDashboard(p, dashboard.NewFake(time.Now()), nil)
	defer stop()

	var opened []string
	oldOpen := openBrowser
	openBrowser = func(u string) error { opened = append(opened, u); return nil }
	defer func() { openBrowser = oldOpen }()
	quiet = true
	defer func() { quiet = false }()

	out := captureStdout(t, func() {
		if err := execute(dashboardCmd(), "--no-open"); err != nil {
			t.Fatal(err)
		}
	})
	if strings.TrimSpace(out) != u {
		t.Fatalf("--no-open printed %q, want the agent's URL %q", out, u)
	}
	if err := execute(dashboardCmd()); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 1 || opened[0] != u {
		t.Fatalf("opened %v, want the agent's URL", opened)
	}
}

// With no agent, `stradl dashboard` serves one itself until stopped.
func TestDashboardCommandServesDemo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	quiet = true
	defer func() { quiet = false }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = oldStdout }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		cmd := dashboardCmd()
		cmd.SetArgs([]string{"--demo", "--no-open"})
		done <- cmd.ExecuteContext(ctx)
	}()

	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	u := strings.TrimSpace(line)
	if code, body := browse(t, u); code != http.StatusOK || !strings.Contains(body, `src="/static/app.js"`) {
		t.Fatalf("demo dashboard page: %d", code)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dashboard kept serving after its context ended")
	}
	if _, err := http.Get(u); err == nil {
		t.Error("demo dashboard still answering after it stopped")
	}
	_ = w.Close()
}

// samePointerProvider returns one shared Overview, as a caching provider does.
type samePointerProvider struct {
	*dashboard.Fake
	ov *dashboard.Overview
}

func (p *samePointerProvider) Overview(context.Context) (*dashboard.Overview, error) {
	return p.ov, nil
}

func TestLiveProviderLeavesTheProvidersOverviewAlone(t *testing.T) {
	before := time.Now().Add(-time.Hour)
	shared := &dashboard.Overview{Agent: dashboard.AgentInfo{LastAttemptAt: &before, LastSyncAt: &before}}
	syncing := true
	at, syncErr := time.Now(), errors.New("storage token expired")
	l := &liveProvider{
		Provider: &samePointerProvider{Fake: dashboard.NewFake(time.Now()), ov: shared},
		syncing:  func() bool { return syncing },
		last:     func() (time.Time, error) { return at, syncErr },
	}
	ov, err := l.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ov.Agent.Syncing || ov.Agent.LastError == "" || !ov.Agent.LastAttemptAt.Equal(at) || !ov.Agent.LastSyncAt.Equal(before) {
		t.Errorf("overlay: %+v", ov.Agent)
	}
	if shared.Agent.Syncing || shared.Agent.LastError != "" || !shared.Agent.LastAttemptAt.Equal(before) {
		t.Errorf("the provider's own overview was changed: %+v", shared.Agent)
	}

	syncing, syncErr = false, nil
	ov, _ = l.Overview(context.Background())
	if ov.Agent.Syncing || ov.Agent.LastError != "" || !ov.Agent.LastSyncAt.Equal(at) {
		t.Errorf("after a clean sync: %+v", ov.Agent)
	}
}

func TestStandaloneSyncRunsInTheBackground(t *testing.T) {
	installSyncSpy(t, false) // temp HOME with a config, quiet
	release := make(chan struct{})
	calls := 0
	old := syncOnce
	syncOnce = func(ctx context.Context, cfg *config.Config, q, wait bool) (*sync.SyncReport, error) {
		calls++
		<-release
		return &sync.SyncReport{}, nil
	}
	defer func() { syncOnce = old }()

	finished := make(chan struct{}, 2)
	ss := &standaloneSync{done: func() { finished <- struct{}{} }}
	if err := ss.start(); err != nil {
		t.Fatal(err)
	}
	if !ss.running.Load() {
		t.Fatal("not running right after start")
	}
	if err := ss.start(); err != nil { // joins the running one
		t.Fatal(err)
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("sync never finished")
	}
	at, err := ss.last()
	if at.IsZero() || err != nil || calls != 1 {
		t.Fatalf("last = %v, %v after %d calls; want one clean sync", at, err, calls)
	}
}
