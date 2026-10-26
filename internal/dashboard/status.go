package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tawanorg/claude-sync/internal/sync"
)

// healthyWithin is how recently a Mac must have synced cleanly to count as
// healthy.
const healthyWithin = 24 * time.Hour

// agentStatus mirrors the fields of the agent's status.json the dashboard
// shows (internal/agent.Status; the agent imports this package, so the type
// can't be shared).
type agentStatus struct {
	LastSyncAt            time.Time `json:"lastSyncAt"`
	LastAttemptAt         time.Time `json:"lastAttemptAt"`
	LastError             string    `json:"lastError"`
	LastErrorKind         string    `json:"lastErrorKind"`
	SessionsTotal         int       `json:"sessionsTotal"`
	PendingDesktopRestart int       `json:"pendingDesktopRestart"`
	Paused                bool      `json:"paused"`
	AgentVersion          string    `json:"agentVersion"`
	Device                string    `json:"device"`
	// Syncing is not written by agents yet; read in case a later one does.
	Syncing bool `json:"syncing"`
}

// notConfigured is the agent's status error until `stradl init` has run
// (agent.NotConfigured). It is a state, not a failure.
const notConfigured = "not set up: run stradl init"

// readAgentStatus loads status.json; a missing or corrupt file reads as the
// zero status.
func readAgentStatus(path string) agentStatus {
	var st agentStatus
	if path == "" {
		return st
	}
	if data, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(data, &st) != nil {
			return agentStatus{}
		}
	}
	return st
}

// failed reports whether the agent's last attempt failed.
func (st agentStatus) failed() bool {
	return st.LastError != "" && st.LastError != notConfigured && !st.LastAttemptAt.IsZero()
}

// errorSummaries mirror agent.ErrorKind.Summary for the kinds that have a
// short, actionable line. Other errors are shown by their own text, which is
// more useful on a dashboard than "see log".
var errorSummaries = map[string]string{
	"key-mismatch": "Passphrase doesn't match your other Macs",
	"auth":         "Storage token expired or invalid",
	"no-container": "Storage container not found",
	"offline":      "Offline: will retry",
}

// errorSummary is the short line for the agent's last error.
func errorSummary(st agentStatus) string {
	if st.LastError == "" {
		return ""
	}
	if s, ok := errorSummaries[st.LastErrorKind]; ok {
		return s
	}
	return truncate(collapseSpace(st.LastError), 120)
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// agentInfo builds AgentInfo from the status file and the live agent hooks.
func (p *localProvider) agentInfo(st agentStatus) AgentInfo {
	info := AgentInfo{
		Running:               p.opts.AgentRunning != nil && p.opts.AgentRunning(),
		Paused:                st.Paused,
		Syncing:               st.Syncing || p.standaloneSyncing(),
		LastSyncAt:            timePtr(st.LastSyncAt),
		LastAttemptAt:         timePtr(st.LastAttemptAt),
		LastError:             st.LastError,
		LastErrorSummary:      errorSummary(st),
		Version:               st.AgentVersion,
		PendingDesktopRestart: st.PendingDesktopRestart,
	}
	if p.opts.Syncing != nil && p.opts.Syncing() {
		info.Syncing = true
	}
	if p.opts.StatusPath != "" {
		// Pausing is a flag file any process can create next to status.json.
		if _, err := os.Stat(filepath.Join(filepath.Dir(p.opts.StatusPath), "paused")); err == nil {
			info.Paused = true
		}
	}
	if info.Version == "" {
		info.Version = sync.AppVersion
	}
	return info
}

// --- devices ---------------------------------------------------------------------

// buildDevices turns heartbeats into the device list, this Mac first. This
// Mac is always listed, from its status file and local records when it has
// not published a heartbeat yet, and its own entry is refreshed from them
// since they are never older than its last heartbeat.
func buildDevices(beats []sync.DeviceHeartbeat, thisID, thisName string, st agentStatus, localRecent []sync.RecentChange, sessions int, now time.Time, loc *time.Location) ([]Device, *deviceActivity) {
	type entry struct {
		d      Device
		recent []sync.RecentChange
	}
	var (
		list    []entry
		thisIdx = -1
	)
	for _, hb := range beats {
		e := entry{
			d: Device{
				ID:         hb.ID,
				Name:       hb.Name,
				This:       thisID != "" && hb.ID == thisID,
				Version:    hb.Version,
				LastSyncAt: hb.LastSyncAt,
				Healthy:    hb.Errors == 0 && fresh(hb.LastSyncAt, now),
				Sessions:   hb.Sessions,
			},
			recent: hb.Recent,
		}
		if e.d.Name == "" {
			e.d.Name = "Mac " + hb.ID
		}
		if e.d.This {
			if thisIdx >= 0 {
				continue
			}
			thisIdx = len(list)
		}
		list = append(list, e)
	}
	if thisIdx < 0 {
		list = append(list, entry{d: Device{ID: thisID, Name: thisName, This: true, Version: st.AgentVersion, Sessions: sessions}})
		thisIdx = len(list) - 1
		if list[thisIdx].d.Version == "" {
			list[thisIdx].d.Version = sync.AppVersion
		}
	}
	me := &list[thisIdx]
	if thisName != "" {
		me.d.Name = thisName
	}
	if st.LastSyncAt.After(me.d.LastSyncAt) {
		me.d.LastSyncAt = st.LastSyncAt
	}
	if sessions > 0 {
		me.d.Sessions = sessions
	}
	me.recent = append(append([]sync.RecentChange(nil), me.recent...), localRecent...)
	if st.LastAttemptAt.IsZero() && st.LastSyncAt.IsZero() {
		me.d.Healthy = me.d.Healthy && fresh(me.d.LastSyncAt, now)
	} else {
		me.d.Healthy = !st.failed() && fresh(me.d.LastSyncAt, now)
	}

	sort.SliceStable(list, func(i, j int) bool {
		if list[i].d.This != list[j].d.This {
			return list[i].d.This
		}
		if !list[i].d.LastSyncAt.Equal(list[j].d.LastSyncAt) {
			return list[i].d.LastSyncAt.After(list[j].d.LastSyncAt)
		}
		return strings.ToLower(list[i].d.Name) < strings.ToLower(list[j].d.Name)
	})

	devices := make([]Device, 0, len(list))
	activity := newDeviceActivity(loc)
	for _, e := range list {
		e.d.RecentSessionIDs = recentIDs(e.recent, now)
		activity.add(e.d.Name, e.recent, now)
		devices = append(devices, e.d)
	}
	return devices, activity
}

func fresh(t, now time.Time) bool {
	return !t.IsZero() && now.Sub(t) < healthyWithin
}

// --- sync state --------------------------------------------------------------------

// syncView answers what the sync engine thinks of each local file, from its
// dry-run preview when storage answered, else from the sync state alone.
type syncView struct {
	previewOK bool
	previewAt time.Time // when the preview started
	upload    map[string]bool
	restore   map[string]bool
	download  map[string]bool
	merge     map[string]bool
	deferred  map[string]bool
	del       map[string]bool
	trash     map[string]bool
	scope     *sync.ScopeSummary
}

func newSyncView(scope *sync.ScopeSummary, preview *sync.SyncReport, previewAt time.Time) *syncView {
	set := func(list []string) map[string]bool {
		m := make(map[string]bool, len(list))
		for _, p := range list {
			m[p] = true
		}
		return m
	}
	v := &syncView{scope: scope, previewAt: previewAt}
	if preview != nil {
		v.previewOK = true
		v.upload = set(preview.Uploaded)
		v.restore = set(preview.Restored)
		v.download = set(preview.Downloaded)
		v.merge = set(preview.Merged)
		v.deferred = set(preview.Deferred)
		v.del = set(preview.Deleted)
		v.trash = set(preview.Trashed)
	}
	return v
}

func (v *syncView) pending(np string) bool {
	return v.upload[np] || v.restore[np] || v.download[np] || v.merge[np] || v.deferred[np] || v.del[np] || v.trash[np]
}

// localChange reports a local change the preview doesn't know about: the
// file differs from its last sync and, when there is a preview, changed
// after the preview started (a preview is reused for a while; the scan is
// always fresh).
func (v *syncView) localChange(f sync.ScopeFile) bool {
	return !f.Reconciled && (!v.previewOK || f.ModTime.After(v.previewAt))
}

// sessionState is a transcript's sync state. A conflict copy needs the
// person's attention, so it shows first; a session open in Claude is held
// (remote changes to it wait until it closes) whatever else is pending.
func (v *syncView) sessionState(np string, live, conflict bool) string {
	var f sync.ScopeFile
	var inScope bool
	if v.scope != nil {
		f, inScope = v.scope.Files[np]
	}
	switch {
	case conflict:
		return "conflict"
	case v.deferred[np] || live:
		return "held"
	case v.merge[np]:
		return "merging"
	case v.download[np]:
		return "pending-download"
	case v.upload[np] || v.restore[np]:
		return "pending-upload"
	case inScope && v.localChange(f):
		return "pending-upload" // changed since it last synced, or never synced
	case inScope && (v.previewOK && f.Tracked || f.Reconciled):
		return "synced"
	}
	return "not-synced"
}

// syncedFiles counts the local files in scope that match their remote copy.
func (v *syncView) syncedFiles() int {
	if v.scope == nil {
		return 0
	}
	n := 0
	for np, f := range v.scope.Files {
		if v.pending(np) || v.localChange(f) {
			continue
		}
		if v.previewOK && f.Tracked || f.Reconciled {
			n++
		}
	}
	return n
}

// pendingSummary is what the next sync would do. Without a preview it lists
// the local changes the sync state reveals; downloads are then unknown.
func (v *syncView) pendingSummary() Pending {
	pd := Pending{Files: []PendingFile{}}
	if v.scope != nil {
		pd.Conflicts = len(v.scope.Conflicts)
	}
	add := func(set map[string]bool, action string) {
		paths := make([]string, 0, len(set))
		for p := range set {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			if len(pd.Files) == maxPendingFiles {
				return
			}
			pd.Files = append(pd.Files, PendingFile{Path: p, Action: action})
		}
	}
	// Local changes the preview doesn't cover (all of them, without one).
	local := make(map[string]bool)
	if v.scope != nil {
		for np, f := range v.scope.Files {
			if !v.pending(np) && v.localChange(f) {
				local[np] = true
			}
		}
	}
	if !v.previewOK {
		pd.Upload = len(local)
		add(local, "upload")
		return pd
	}
	pd.Known = true
	pd.Upload = len(v.upload) + len(v.restore) + len(local)
	pd.Download = len(v.download)
	pd.Merge = len(v.merge)
	pd.Deferred = len(v.deferred)
	pd.Delete = len(v.del)
	pd.Trash = len(v.trash)
	add(v.merge, "merge")
	add(v.download, "download")
	add(v.upload, "upload")
	add(local, "upload")
	add(v.restore, "restore")
	add(v.deferred, "deferred")
	add(v.del, "delete")
	add(v.trash, "trash")
	return pd
}

const maxPendingFiles = 200
