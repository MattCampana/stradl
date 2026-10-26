package sync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	gosync "sync"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
)

// AppVersion is the running Stradl version, set by the CLI at startup. It is
// reported in this device's heartbeat.
var AppVersion = "dev"

// DevicesPrefix holds one heartbeat per device: <prefix><id>.json.age. The
// engine ignores everything under _metadata/, so heartbeats never sync as
// files and never trigger another device's sync.
const DevicesPrefix = "_metadata/devices/"

const (
	deviceIDFile   = "device-id"
	deviceNameFile = "device-name" // optional: overrides the computer name
	recentFile     = "recent.json"

	// RecentWindow is how long a device remembers the sessions it changed.
	RecentWindow = 7 * 24 * time.Hour

	heartbeatTimeout = 15 * time.Second
)

var deviceIDRe = regexp.MustCompile(`^[0-9a-f]{12}$`)

// RecentChange records when a device last changed a session on one day. A
// session changed on several days has one entry per day.
type RecentChange struct {
	SessionID string    `json:"sessionId"`
	At        time.Time `json:"at"`
}

// DeviceHeartbeat is what each device publishes after every successful sync,
// so the dashboard on any Mac can show which Macs are syncing and what each
// one worked on recently.
type DeviceHeartbeat struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Version    string         `json:"version"`
	OS         string         `json:"os"`
	Arch       string         `json:"arch"`
	LastSyncAt time.Time      `json:"lastSyncAt"`
	Sessions   int            `json:"sessions"` // transcripts on the device after the sync
	Recent     []RecentChange `json:"recent"`   // sessions it changed in the last RecentWindow
	Uploaded   int            `json:"uploaded"`
	Downloaded int            `json:"downloaded"`
	Merged     int            `json:"merged"`
	Errors     int            `json:"errors"`
}

// StateDir is the directory holding this device's Stradl state (~/.stradl,
// or cfg.StateDirOverride in tests).
func StateDir(cfg *config.Config) string {
	if cfg != nil && cfg.StateDirOverride != "" {
		return cfg.StateDirOverride
	}
	return config.ConfigDirPath()
}

var deviceIDMu gosync.Mutex

// DeviceID returns this device's stable random ID, creating it on first use.
// Unlike the hostname it survives renames and never collides between Macs
// that share a name.
func DeviceID(cfg *config.Config) (string, error) {
	deviceIDMu.Lock()
	defer deviceIDMu.Unlock()
	p := filepath.Join(StateDir(cfg), deviceIDFile)
	if data, err := os.ReadFile(p); err == nil {
		if id := strings.TrimSpace(string(data)); deviceIDRe.MatchString(id) {
			return id, nil
		}
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	if err := writeFileAtomic(p, []byte(id+"\n"), 0600); err != nil {
		return "", fmt.Errorf("failed to save device ID: %w", err)
	}
	return id, nil
}

var (
	computerNameOnce gosync.Once
	computerNameVal  string
)

// DeviceName returns the name this device shows on the dashboard: the
// contents of device-name in the state directory when present, else the Mac's
// computer name ("Matt's MacBook Pro"), else its hostname.
func DeviceName(cfg *config.Config) string {
	if data, err := os.ReadFile(filepath.Join(StateDir(cfg), deviceNameFile)); err == nil {
		if name := strings.TrimSpace(string(data)); name != "" {
			return name
		}
	}
	computerNameOnce.Do(func() { computerNameVal = computerName() })
	return computerNameVal
}

func computerName() string {
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("scutil", "--get", "ComputerName").Output(); err == nil {
			if name := strings.TrimSpace(string(out)); name != "" {
				return name
			}
		}
	}
	h, err := os.Hostname()
	if err != nil {
		return "this device"
	}
	return strings.TrimSuffix(h, ".local")
}

// ThisDevice returns this device's ID and display name.
func ThisDevice(cfg *config.Config) (id, name string, err error) {
	id, err = DeviceID(cfg)
	return id, DeviceName(cfg), err
}

// LiveSessionIDs returns the IDs of Claude Code sessions whose process is
// still running on this device (see liveSessionIDs).
func LiveSessionIDs(claudeDir string) map[string]bool {
	return liveSessionIDs(claudeDir)
}

// ClaudeDir is the ~/.claude directory this syncer works on.
func (s *Syncer) ClaudeDir() string { return s.claudeDir }

// IsDeviceLocalFile reports whether a file under ~/.claude belongs to this
// device alone and never syncs (temp, lock and set-aside files, desktop
// release markers); see isDeviceLocalFile.
func IsDeviceLocalFile(name string) bool { return isDeviceLocalFile(name) }

// --- recent changes --------------------------------------------------------------

var sessionIDRe = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

// changedSessionID maps a synced path to the session it belongs to. Top-level
// transcripts count whatever their name; files under projects/<dir>/<id>/
// count only when <id> looks like a session ID, so per-project folders such
// as memory/ are never mistaken for sessions.
func changedSessionID(npath string) (string, bool) {
	id, ok := transcriptSessionID(npath)
	if !ok || id == "" {
		return "", false
	}
	if strings.Count(npath, "/") > 2 && !sessionIDRe.MatchString(id) {
		return "", false
	}
	return id, true
}

// ReadRecent returns the sessions this device changed in the last
// RecentWindow, newest first. A missing or unreadable file reads as empty.
func ReadRecent(cfg *config.Config, now time.Time) []RecentChange {
	data, err := os.ReadFile(filepath.Join(StateDir(cfg), recentFile))
	if err != nil {
		return nil
	}
	var list []RecentChange
	if json.Unmarshal(data, &list) != nil {
		return nil
	}
	return pruneRecent(list, now)
}

// pruneRecent drops entries older than RecentWindow and keeps the latest
// entry per session and local calendar day, newest first.
func pruneRecent(list []RecentChange, now time.Time) []RecentChange {
	type key struct{ id, day string }
	latest := make(map[key]time.Time, len(list))
	floor := now.Add(-RecentWindow)
	for _, c := range list {
		if c.SessionID == "" || c.At.Before(floor) {
			continue
		}
		k := key{c.SessionID, c.At.Local().Format("2006-01-02")}
		if cur, ok := latest[k]; !ok || c.At.After(cur) {
			latest[k] = c.At
		}
	}
	out := make([]RecentChange, 0, len(latest))
	for k, at := range latest {
		out = append(out, RecentChange{SessionID: k.id, At: at})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

// recordRecent adds the sessions this device changed in report (uploads and
// merges) to recent.json and returns the pruned list. Each change is dated by
// the transcript's modification time, which is when this device last wrote
// it, never later than now.
func recordRecent(cfg *config.Config, claudeDir string, report *SyncReport, now time.Time) ([]RecentChange, error) {
	list := ReadRecent(cfg, now)
	for _, np := range append(append([]string(nil), report.Uploaded...), report.Merged...) {
		id, ok := changedSessionID(np)
		if !ok {
			continue
		}
		at := now
		if info, err := os.Stat(filepath.Join(claudeDir, filepath.FromSlash(np))); err == nil && info.ModTime().Before(now) {
			at = info.ModTime()
		}
		list = append(list, RecentChange{SessionID: id, At: at})
	}
	list = pruneRecent(list, now)
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return list, err
	}
	return list, writeFileAtomic(filepath.Join(StateDir(cfg), recentFile), append(data, '\n'), 0600)
}

// countTranscripts counts the sessions (projects/<dir>/<id>.jsonl) on this
// device.
func countTranscripts(claudeDir string) int {
	projects := filepath.Join(claudeDir, "projects")
	dirs, err := os.ReadDir(projects)
	if err != nil {
		return 0
	}
	n := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(projects, d.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".jsonl") && !isDeviceLocalFile(e.Name()) {
				n++
			}
		}
	}
	return n
}

// --- heartbeats --------------------------------------------------------------------

// AfterSync records which sessions this device changed in report (for its
// recent list) and publishes its heartbeat. RunSync calls it after every
// successful sync. It is best effort: a failure here never fails the sync it
// follows.
func (s *Syncer) AfterSync(ctx context.Context, report *SyncReport) {
	if report == nil || ctx.Err() != nil {
		return
	}
	now := time.Now()
	recent, err := recordRecent(s.cfg, s.claudeDir, report, now)
	if err != nil {
		s.log("Warning: failed to record recent sessions: %v", err)
	}
	hctx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
	defer cancel()
	if err := s.publishHeartbeat(hctx, report, recent, now); err != nil {
		s.log("Warning: failed to publish device heartbeat: %v", err)
	}
}

func (s *Syncer) publishHeartbeat(ctx context.Context, report *SyncReport, recent []RecentChange, now time.Time) error {
	id, name, err := ThisDevice(s.cfg)
	if err != nil {
		return err
	}
	if recent == nil {
		recent = []RecentChange{}
	}
	hb := DeviceHeartbeat{
		ID:         id,
		Name:       name,
		Version:    AppVersion,
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		LastSyncAt: now.UTC(),
		Sessions:   countTranscripts(s.claudeDir),
		Recent:     recent,
		Uploaded:   len(report.Uploaded) + len(report.Restored),
		Downloaded: len(report.Downloaded),
		Merged:     len(report.Merged),
		Errors:     len(report.Errors),
	}
	data, err := json.Marshal(hb)
	if err != nil {
		return err
	}
	key := DevicesPrefix + id + ".json.age"
	compressed, err := gzipCompress(bindKey(data, key))
	if err != nil {
		return err
	}
	enc, err := s.encryptor.Encrypt(compressed)
	if err != nil {
		return err
	}
	// Each device owns its own key, so a plain write never races another Mac.
	return s.storage.Upload(ctx, key, enc)
}

// Heartbeats returns the latest heartbeat of every device syncing into the
// container, most recently synced first. Heartbeats that can't be read (another
// key, a corrupt or moved blob) are skipped.
func (s *Syncer) Heartbeats(ctx context.Context) ([]DeviceHeartbeat, error) {
	objects, err := s.storage.List(ctx, DevicesPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to list device heartbeats: %w", err)
	}
	var out []DeviceHeartbeat
	for _, obj := range objects {
		id, ok := strings.CutSuffix(strings.TrimPrefix(obj.Key, DevicesPrefix), ".json.age")
		if !ok || !strings.HasPrefix(obj.Key, DevicesPrefix) || !deviceIDRe.MatchString(id) {
			continue
		}
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		hb, err := s.readHeartbeat(ctx, obj.Key)
		if err != nil || hb.ID != id {
			continue
		}
		out = append(out, *hb)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSyncAt.Equal(out[j].LastSyncAt) {
			return out[i].LastSyncAt.After(out[j].LastSyncAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *Syncer) readHeartbeat(ctx context.Context, key string) (*DeviceHeartbeat, error) {
	enc, err := s.storage.Download(ctx, key)
	if err != nil {
		return nil, err
	}
	data, err := s.encryptor.Decrypt(enc)
	if err != nil {
		return nil, err
	}
	if isGzipped(data) {
		if data, err = gzipDecompress(data); err != nil {
			return nil, err
		}
	}
	if data, err = unbindKey(data, key); err != nil {
		return nil, err
	}
	var hb DeviceHeartbeat
	if err := json.Unmarshal(data, &hb); err != nil {
		return nil, err
	}
	return &hb, nil
}

// --- sync scope ------------------------------------------------------------------

// ScopeFile is one local file in the sync scope.
type ScopeFile struct {
	Size    int64
	ModTime time.Time
	// Tracked: the file has synced before (it has sync state and was not
	// deleted here since).
	Tracked bool
	// Reconciled: the local copy still has the size and modification time it
	// had when it last synced, so it has no local changes to upload.
	Reconciled bool
}

// ScopeSummary describes the local side of the sync scope.
type ScopeSummary struct {
	Files     map[string]ScopeFile // keyed by sync path (projects/..., _desktop/sessions/...)
	Bytes     int64
	Conflicts []string // .conflict copies in the scope, by sync path
}

// ScopeStats counts the local files in the sync scope and their total size.
func (s *Syncer) ScopeStats() (files int, bytes int64, err error) {
	sum, err := s.ScopeScan()
	if err != nil {
		return 0, 0, err
	}
	return len(sum.Files), sum.Bytes, nil
}

// ScopeScan lists the local files Sync considers, with what the sync state
// says about each. It reads no file contents and makes no network call.
func (s *Syncer) ScopeScan() (*ScopeSummary, error) {
	var known map[string]FileState
	if s.state != nil {
		known = s.state.Snapshot()
	}
	sum := &ScopeSummary{Files: make(map[string]ScopeFile)}
	for _, root := range s.roots() {
		var exclude func(string) bool
		if root.prefix == "" {
			exclude = s.isExcluded
		}
		files, err := GetLocalFiles(root.dir, root.paths, exclude)
		if err != nil {
			return nil, fmt.Errorf("failed to scan %s: %w", root.dir, err)
		}
		for rel, info := range files {
			rel = strings.TrimPrefix(rel, "./")
			if root.prefix == "" && strings.Contains(path.Base(rel), ".conflict.") {
				sum.Conflicts = append(sum.Conflicts, rel)
				continue
			}
			if !root.inScope(rel) {
				continue
			}
			np := root.prefix + rel
			f := ScopeFile{Size: info.Size(), ModTime: info.ModTime()}
			if st, ok := known[np]; ok && !st.LocalDeleted {
				f.Tracked = true
				f.Reconciled = st.Size == info.Size() && st.ModTime.Equal(info.ModTime())
			}
			sum.Files[np] = f
			sum.Bytes += info.Size()
		}
	}
	sort.Strings(sum.Conflicts)
	return sum, nil
}

// ScopeScanFor runs ScopeScan for cfg without a storage client or encryption
// key, against the sync state on disk.
func ScopeScanFor(cfg *config.Config) (*ScopeSummary, error) {
	s := newScopeSyncer(cfg)
	var (
		state *SyncState
		err   error
	)
	if cfg.StateDirOverride != "" {
		state, err = LoadStateFromDir(cfg.StateDirOverride)
	} else {
		state, err = LoadState()
	}
	if err != nil {
		return nil, err
	}
	// File states recorded against another storage target say nothing about
	// this one (NewSyncer drops them for the same reason).
	if state.StorageID != "" && state.StorageID != StorageIdentity(cfg.GetStorageConfig()) {
		state = NewState()
	}
	s.state = state
	return s.ScopeScan()
}
