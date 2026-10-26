package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
)

type FileState struct {
	Path     string    `json:"path"`
	Hash     string    `json:"hash"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"mod_time"`
	Uploaded time.Time `json:"uploaded,omitempty"`

	// RemoteETag is the ETag of the remote version this device's copy was last
	// reconciled with. Comparing ETags detects remote changes without trusting
	// any device's clock.
	RemoteETag string `json:"remote_etag,omitempty"`

	// LocalDeleted marks a file deleted on this device after it synced. The
	// delete stays local: the remote copy is kept for the other devices and is
	// not re-downloaded here unless another device changes it.
	LocalDeleted bool `json:"local_deleted,omitempty"`

	// Mode is the file's permission bits when it was reconciled (0 when not
	// tracked, as for desktop records and state written before modes synced).
	// A chmod alone, such as making a script executable, then counts as a
	// local change.
	Mode uint32 `json:"mode,omitempty"`
}

// cwdEntry caches the first working folder recorded in a top-level transcript,
// keyed by its sync path. Size lets a transcript with no cwd yet be read again
// once it grows; a found cwd never changes.
type cwdEntry struct {
	Size int64  `json:"size"`
	Cwd  string `json:"cwd,omitempty"`
}

// adoptStorage ties the state to storage target id. When the state belonged
// to a different target its file states are dropped: their ETags would make
// every file look changed remotely, and stale copies from the new target
// would overwrite newer local files. With no state, files present on both
// sides merge instead. It reports whether anything was dropped.
func (s *SyncState) adoptStorage(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.StorageID == id {
		return false
	}
	dropped := s.StorageID != "" && len(s.Files) > 0
	if dropped {
		s.Files = make(map[string]*FileState)
		s.MCPBaseline = nil
	}
	s.StorageID = id
	return dropped
}

// SetReconciled records that npath's local copy (described by info and hash)
// now matches the remote version etag.
func (s *SyncState) SetReconciled(npath string, info os.FileInfo, hash, etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Files[npath] = &FileState{
		Path:       npath,
		Hash:       hash,
		Size:       info.Size(),
		ModTime:    info.ModTime(),
		Uploaded:   time.Now(),
		RemoteETag: etag,
		Mode:       trackedMode(npath, info),
	}
}

// trackedMode returns the permission bits recorded for npath, or 0 for a
// desktop record: the desktop app rewrites those with its own mode.
func trackedMode(npath string, info os.FileInfo) uint32 {
	if strings.HasPrefix(npath, DesktopPrefix) || info.Mode()&os.ModeSymlink != 0 {
		return 0 // a link's own mode is meaningless
	}
	return uint32(info.Mode().Perm())
}

// setMode updates the recorded mode of a tracked file (0 forgets it).
func (s *SyncState) setMode(npath string, mode uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.Files[npath]; ok {
		f.Mode = mode
	}
}

// cachedCwd returns the cached first cwd of a transcript.
func (s *SyncState) cachedCwd(npath string) (cwdEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.TranscriptCwds[npath]
	return e, ok
}

// setCachedCwd records the first cwd of a transcript.
func (s *SyncState) setCachedCwd(npath string, e cwdEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.TranscriptCwds == nil {
		s.TranscriptCwds = make(map[string]cwdEntry)
	}
	s.TranscriptCwds[npath] = e
}

// pruneCwds forgets cached transcripts that are no longer present.
func (s *SyncState) pruneCwds(present map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p := range s.TranscriptCwds {
		if !present[p] {
			delete(s.TranscriptCwds, p)
		}
	}
}

// MarkLocalDeleted records that npath was deleted on this device while the
// remote copy (etag) stays in place.
func (s *SyncState) MarkLocalDeleted(npath, etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.Files[npath]; ok {
		f.LocalDeleted = true
		if etag != "" {
			f.RemoteETag = etag
		}
	}
}

// MarkHeldBack records that the remote copy of npath (etag) is kept off this
// device, whether or not it was ever here, until that copy changes.
func (s *SyncState) MarkHeldBack(npath, etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.Files[npath]
	if !ok {
		f = &FileState{}
		s.Files[npath] = f
	}
	f.LocalDeleted = true
	f.RemoteETag = etag
}

// Snapshot returns a copy of the tracked file states keyed by path.
func (s *SyncState) Snapshot() map[string]FileState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]FileState, len(s.Files))
	for k, v := range s.Files {
		out[k] = *v
	}
	return out
}

type SyncState struct {
	Files    map[string]*FileState `json:"files"`
	LastSync time.Time             `json:"last_sync"`
	DeviceID string                `json:"device_id"`
	LastPush time.Time             `json:"last_push,omitempty"`
	LastPull time.Time             `json:"last_pull,omitempty"`

	// MCPBaseline stores the last-synced normalized MCP server configs for three-way merge.
	MCPBaseline json.RawMessage `json:"mcp_baseline,omitempty"`

	// StorageID identifies the storage target these file states belong to.
	// Their ETags and hashes mean nothing against another container, so the
	// file states are dropped when the target changes (see NewSyncer).
	StorageID string `json:"storage_id,omitempty"`

	// TranscriptCwds caches each top-level transcript's first working folder,
	// one of the sources project folders are discovered from, so a sync reads
	// only transcripts it hasn't seen before.
	TranscriptCwds map[string]cwdEntry `json:"transcript_cwds,omitempty"`

	// savePath is the custom path to save state to (if set)
	savePath string     `json:"-"`
	mu       sync.Mutex `json:"-"`
}

func LoadState() (*SyncState, error) {
	return loadStateFromPath(config.StateFilePath())
}

// LoadStateFromDir loads state from a custom directory (for testing)
func LoadStateFromDir(dir string) (*SyncState, error) {
	statePath := filepath.Join(dir, config.StateFile)
	state, err := loadStateFromPath(statePath)
	if err != nil {
		return nil, err
	}
	state.savePath = statePath
	return state, nil
}

func loadStateFromPath(statePath string) (*SyncState, error) {
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return NewState(), nil
		}
		return nil, fmt.Errorf("failed to read state: %w", err)
	}

	var state SyncState
	if err := json.Unmarshal(data, &state); err != nil {
		// state.json is a regenerable sync cache (hashes, timestamps), not user
		// config. A corrupt cache must never brick push/pull: back it up and start
		// fresh so the next sync simply re-scans, without touching real config.
		backup := fmt.Sprintf("%s.corrupt-%d", statePath, time.Now().Unix())
		if renameErr := os.Rename(statePath, backup); renameErr == nil {
			fmt.Fprintf(os.Stderr, "Warning: state file was corrupt (%v); backed up to %s and starting fresh\n", err, backup)
		} else {
			fmt.Fprintf(os.Stderr, "Warning: state file was corrupt (%v); starting fresh\n", err)
		}
		return NewState(), nil
	}

	if state.Files == nil {
		state.Files = make(map[string]*FileState)
	}

	return &state, nil
}

func NewState() *SyncState {
	hostname, _ := os.Hostname()
	return &SyncState{
		Files:    make(map[string]*FileState),
		DeviceID: hostname,
	}
}

func (s *SyncState) Save() error {
	statePath := s.savePath
	if statePath == "" {
		statePath = config.StateFilePath()
	}

	// Ensure directory exists
	dir := filepath.Dir(statePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	// Hold the lock while serializing: a long sync saves its progress while
	// workers are still recording files.
	s.mu.Lock()
	data, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to serialize state: %w", err)
	}

	// Write atomically: write to a temp file in the same directory, then rename.
	// A crash or concurrent run mid-write can never leave a half-written/corrupt
	// state.json this way (rename is atomic on the same filesystem).
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op if rename succeeded

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write state: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to set state permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to flush state: %w", err)
	}
	if err := os.Rename(tmpPath, statePath); err != nil {
		return fmt.Errorf("failed to write state: %w", err)
	}

	return nil
}

func (s *SyncState) UpdateFile(relativePath string, info os.FileInfo, hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Files[relativePath] = &FileState{
		Path:    relativePath,
		Hash:    hash,
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}
}

func (s *SyncState) MarkUploaded(relativePath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.Files[relativePath]; ok {
		f.Uploaded = time.Now()
	}
}

func (s *SyncState) GetFile(relativePath string) *FileState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Files[relativePath]
}

func (s *SyncState) RemoveFile(relativePath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Files, relativePath)
}

// IsEmpty returns true if no files have been synced yet (first sync)
func (s *SyncState) IsEmpty() bool {
	return len(s.Files) == 0 && s.LastSync.IsZero()
}

// GetMCPBaseline returns the last-synced MCP server configs used for three-way merge.
func (s *SyncState) GetMCPBaseline() (MCPServers, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.MCPBaseline) == 0 {
		return nil, nil
	}
	var servers MCPServers
	if err := json.Unmarshal(s.MCPBaseline, &servers); err != nil {
		return nil, fmt.Errorf("failed to parse MCP baseline: %w", err)
	}
	return servers, nil
}

// SetMCPBaseline stores the normalized MCP server configs as the baseline for future merges.
func (s *SyncState) SetMCPBaseline(servers MCPServers) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if servers == nil {
		s.MCPBaseline = nil
		return nil
	}
	data, err := json.Marshal(servers)
	if err != nil {
		return fmt.Errorf("failed to serialize MCP baseline: %w", err)
	}
	s.MCPBaseline = data
	return nil
}

func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// isWithin reports whether target is root itself or lives underneath it.
// Both are cleaned first so ".." segments cannot slip through.
func isWithin(root, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, "../"))
}

func GetLocalFiles(claudeDir string, syncPaths []string, excludeFn ...func(string) bool) (map[string]os.FileInfo, error) {
	files := make(map[string]os.FileInfo)

	// Use the first exclude function if provided
	var isExcluded func(string) bool
	if len(excludeFn) > 0 && excludeFn[0] != nil {
		isExcluded = excludeFn[0]
	}

	for _, syncPath := range syncPaths {
		fullPath := filepath.Join(claudeDir, syncPath)

		// Sync paths are user-configurable via sync_paths, so a traversing
		// entry could otherwise walk outside the sync root and turn files like
		// ~/.ssh/id_rsa into remote objects. Skip anything that escapes.
		if !isWithin(claudeDir, fullPath) {
			continue
		}

		info, err := os.Stat(fullPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed to stat %s: %w", syncPath, err)
		}

		if info.IsDir() {
			err := filepath.Walk(fullPath, func(path string, fi os.FileInfo, err error) error {
				if err != nil {
					return err
				}

				relPath, _ := filepath.Rel(claudeDir, path)
				// Normalize to forward slashes for consistent matching
				relPath = filepath.ToSlash(relPath)

				// Skip excluded directories entirely (never the root itself)
				if fi.IsDir() {
					if relPath != "." && isExcluded != nil && isExcluded(relPath) {
						return filepath.SkipDir
					}
					return nil
				}
				// Skip symlinks, and sockets, pipes and devices (such as git's
				// fsmonitor socket), which have no content to sync
				if !fi.Mode().IsRegular() {
					return nil
				}
				// Skip excluded files
				if isExcluded != nil && isExcluded(relPath) {
					return nil
				}

				files[relPath] = fi
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("failed to walk %s: %w", syncPath, err)
			}
		} else {
			// Skip symlinks and other non-regular files
			if !info.Mode().IsRegular() {
				continue
			}
			// Skip excluded files
			if isExcluded != nil && isExcluded(syncPath) {
				continue
			}
			files[syncPath] = info
		}
	}

	return files, nil
}

type FileChange struct {
	Path      string
	Action    string // "add", "modify", "delete"
	LocalHash string
	LocalSize int64
	LocalTime time.Time
}

func (s *SyncState) DetectChanges(claudeDir string, syncPaths []string, excludeFn ...func(string) bool) ([]FileChange, error) {
	var changes []FileChange

	localFiles, err := GetLocalFiles(claudeDir, syncPaths, excludeFn...)
	if err != nil {
		return nil, err
	}

	// Check for new or modified files
	for relPath, info := range localFiles {
		fullPath := filepath.Join(claudeDir, relPath)
		hash, err := HashFile(fullPath)
		if err != nil {
			return nil, fmt.Errorf("failed to hash %s: %w", relPath, err)
		}

		existing := s.GetFile(relPath)
		if existing == nil {
			changes = append(changes, FileChange{
				Path:      relPath,
				Action:    "add",
				LocalHash: hash,
				LocalSize: info.Size(),
				LocalTime: info.ModTime(),
			})
		} else if existing.Hash != hash {
			changes = append(changes, FileChange{
				Path:      relPath,
				Action:    "modify",
				LocalHash: hash,
				LocalSize: info.Size(),
				LocalTime: info.ModTime(),
			})
		}
	}

	// Check for deleted files - snapshot keys under lock to avoid race condition
	s.mu.Lock()
	knownPaths := make([]string, 0, len(s.Files))
	for relPath := range s.Files {
		knownPaths = append(knownPaths, relPath)
	}
	s.mu.Unlock()

	for _, relPath := range knownPaths {
		if _, exists := localFiles[relPath]; !exists {
			changes = append(changes, FileChange{
				Path:   relPath,
				Action: "delete",
			})
		}
	}

	return changes, nil
}
