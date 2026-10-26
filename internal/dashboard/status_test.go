package dashboard

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/sync"
)

func TestErrorSummary(t *testing.T) {
	long := "upload failed: " + strings.Repeat("x", 300)
	cases := []struct {
		st   agentStatus
		want string
	}{
		{agentStatus{}, ""},
		{agentStatus{LastError: "AuthenticationFailed", LastErrorKind: "auth"}, "Storage token expired or invalid"},
		{agentStatus{LastError: "no such host", LastErrorKind: "offline"}, "Offline: will retry"},
		{agentStatus{LastError: "wrong key", LastErrorKind: "key-mismatch"}, "Passphrase doesn't match your other Macs"},
		{agentStatus{LastError: "ContainerNotFound", LastErrorKind: "no-container"}, "Storage container not found"},
		{agentStatus{LastError: "disk  full\nnow", LastErrorKind: "other"}, "disk full now"},
		{agentStatus{LastError: notConfigured}, notConfigured},
	}
	for _, c := range cases {
		if got := errorSummary(c.st); got != c.want {
			t.Errorf("errorSummary(%+v) = %q, want %q", c.st, got, c.want)
		}
	}
	if got := errorSummary(agentStatus{LastError: long}); len([]rune(got)) != 120 || !strings.HasSuffix(got, "…") {
		t.Errorf("long error summary = %q", got)
	}
}

func TestReadAgentStatus(t *testing.T) {
	dir := t.TempDir()
	if st := readAgentStatus(filepath.Join(dir, "missing.json")); st != (agentStatus{}) {
		t.Errorf("missing file = %+v", st)
	}
	bad := filepath.Join(dir, "bad.json")
	writeFile(t, bad, "{not json")
	if st := readAgentStatus(bad); st != (agentStatus{}) {
		t.Errorf("corrupt file = %+v", st)
	}
	good := filepath.Join(dir, "status.json")
	writeFile(t, good, `{"lastSyncAt":"2026-10-25T10:00:00Z","lastAttemptAt":"2026-10-25T10:05:00Z","lastError":"boom","lastErrorKind":"other",
"uploaded":3,"sessionsTotal":12,"pendingDesktopRestart":1,"paused":true,"agentVersion":"2.0.0","device":"mac","pid":42}`)
	st := readAgentStatus(good)
	if !st.LastSyncAt.Equal(ts("2026-10-25T10:00:00Z")) || st.LastError != "boom" || st.SessionsTotal != 12 || !st.Paused || st.AgentVersion != "2.0.0" || !st.failed() {
		t.Errorf("status = %+v", st)
	}
	if (agentStatus{LastError: notConfigured, LastAttemptAt: time.Now()}).failed() {
		t.Error("waiting for stradl init is not a failure")
	}
}

func TestAgentInfo(t *testing.T) {
	dir := t.TempDir()
	syncing := false
	p := newLocalProvider(LocalOptions{
		StatusPath:   filepath.Join(dir, "status.json"),
		AgentRunning: func() bool { return true },
		Syncing:      func() bool { return syncing },
	})
	info := p.agentInfo(agentStatus{LastSyncAt: ts("2026-10-25T10:00:00Z"), PendingDesktopRestart: 3})
	if !info.Running || info.Paused || info.Syncing || info.LastSyncAt == nil || info.LastAttemptAt != nil || info.PendingDesktopRestart != 3 || info.Version != sync.AppVersion {
		t.Errorf("info = %+v", info)
	}
	syncing = true
	writeFile(t, filepath.Join(dir, "paused"), "")
	if info := p.agentInfo(agentStatus{AgentVersion: "2.1.0"}); !info.Syncing || !info.Paused || info.Version != "2.1.0" {
		t.Errorf("info = %+v (the pause flag file and the syncing hook count)", info)
	}
	if info := newLocalProvider(LocalOptions{StatusPath: filepath.Join(dir, "status.json")}).agentInfo(agentStatus{}); info.Running {
		t.Error("no AgentRunning hook: not running")
	}
}

func TestBuildDevices(t *testing.T) {
	now := ts("2026-10-25T12:00:00Z")
	beats := []sync.DeviceHeartbeat{
		{ID: "aaaaaaaaaaaa", Name: "Laptop", LastSyncAt: now.Add(-time.Hour), Sessions: 4,
			Recent: []sync.RecentChange{{SessionID: "s1", At: now.Add(-2 * time.Hour)}}},
		{ID: "bbbbbbbbbbbb", Name: "Studio", LastSyncAt: now.Add(-30 * time.Hour), Sessions: 9},
		{ID: "cccccccccccc", Name: "Mini", LastSyncAt: now.Add(-10 * time.Minute), Errors: 2},
		{ID: "dddddddddddd", Name: "", LastSyncAt: now.Add(-20 * time.Minute)},
	}
	st := agentStatus{LastSyncAt: now.Add(-5 * time.Minute), LastAttemptAt: now.Add(-5 * time.Minute)}
	local := []sync.RecentChange{{SessionID: "s2", At: now.Add(-time.Minute)}}
	devices, act := buildDevices(beats, "aaaaaaaaaaaa", "Matt's Laptop", st, local, 6, now, time.UTC)

	var names []string
	for _, d := range devices {
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "Matt's Laptop,Mini,Mac dddddddddddd,Studio" {
		t.Errorf("order = %v (this Mac first, then most recently synced)", names)
	}
	me := devices[0]
	if !me.This || !me.LastSyncAt.Equal(st.LastSyncAt) || me.Sessions != 6 || !me.Healthy {
		t.Errorf("this Mac = %+v (its status file is fresher than its heartbeat)", me)
	}
	if strings.Join(me.RecentSessionIDs, ",") != "s2,s1" {
		t.Errorf("this Mac's recent = %v (its local record joins its heartbeat)", me.RecentSessionIDs)
	}
	if devices[1].Healthy || devices[3].Healthy || !devices[2].Healthy {
		t.Errorf("health = %v/%v/%v (errors, a day without syncing, fine)", devices[1].Healthy, devices[3].Healthy, devices[2].Healthy)
	}
	if devices[3].RecentSessionIDs == nil {
		t.Error("RecentSessionIDs is nil, want an empty list")
	}
	if got := act.forSession("s1"); len(got) != 1 || got[0] != "Matt's Laptop" {
		t.Errorf("s1 changed by %v", got)
	}

	// A failed last attempt makes this Mac unhealthy.
	failed := agentStatus{LastSyncAt: now.Add(-time.Hour), LastAttemptAt: now.Add(-time.Minute), LastError: "boom"}
	if devices, _ := buildDevices(beats, "aaaaaaaaaaaa", "Matt's Laptop", failed, nil, 6, now, time.UTC); devices[0].Healthy {
		t.Error("this Mac is healthy despite a failed sync")
	}
}
