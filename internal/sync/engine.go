package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	gosync "sync"
	"time"

	"github.com/tawanorg/claude-sync/internal/storage"
)

// MtimeManifestKey is the remote key of the shared modification-time
// manifest, keyed by portable remote key so devices with different home
// directories agree on entries. Updated with compare-and-swap.
const MtimeManifestKey = "_metadata/mtimes.json.age"

// maxRaceRetries bounds how often one file is re-merged after losing a
// compare-and-swap race to another device within a single sync.
const maxRaceRetries = 4

// syncRoot is a local directory whose files join the sync namespace under
// prefix: ~/.claude with no prefix, and the desktop session index under
// DesktopPrefix.
type syncRoot struct {
	dir     string
	prefix  string
	paths   []string
	include func(rel string) bool
}

type localFile struct {
	root *syncRoot
	rel  string
	abs  string
	info os.FileInfo
}

// SyncReport describes one reconcile pass.
type SyncReport struct {
	Uploaded   []string // local changes sent to remote
	Downloaded []string // remote changes written locally
	Merged     []string // both sides changed; merged (transcripts) or newest kept
	Restored   []string // remote copy had vanished; re-uploaded from this device
	Deferred   []string // remote changes held back while a live session owns the file
	Conflicts  []string // non-mergeable files changed on both sides; loser kept as .conflict copy
	Skipped    []string // remote keys using a path token this device can't map

	// NewDesktopSessions lists desktop sidebar records that did not exist on
	// this device before. The desktop app loads its sidebar at launch, so they
	// show up after it restarts.
	NewDesktopSessions []string

	Errors []error
}

// Changed reports whether the pass moved any data.
func (r *SyncReport) Changed() bool {
	return len(r.Uploaded)+len(r.Downloaded)+len(r.Merged)+len(r.Restored) > 0
}

// reportMu guards SyncReport slices written from worker goroutines.
type reportMu struct {
	gosync.Mutex
	r *SyncReport
}

func (m *reportMu) add(list *[]string, p string) { m.Lock(); *list = append(*list, p); m.Unlock() }
func (m *reportMu) fail(p string, err error) {
	m.Lock()
	m.r.Errors = append(m.r.Errors, fmt.Errorf("%s: %w", p, err))
	m.Unlock()
}

func (s *Syncer) roots() []*syncRoot {
	roots := []*syncRoot{{
		dir:   s.claudeDir,
		paths: s.syncPaths(),
		include: func(rel string) bool {
			return !strings.Contains(rel, ".conflict.") && !s.isExcluded(rel)
		},
	}}
	if dir := s.cfg.EffectiveDesktopDir(); dir != "" && dirExists(dir) {
		roots = append(roots, &syncRoot{dir: dir, prefix: DesktopPrefix, paths: []string{"."}, include: isDesktopRecord})
	}
	return roots
}

func (s *Syncer) rootFor(roots []*syncRoot, npath string) (*syncRoot, string) {
	for _, r := range roots {
		if r.prefix != "" && strings.HasPrefix(npath, r.prefix) {
			return r, strings.TrimPrefix(npath, r.prefix)
		}
	}
	for _, r := range roots {
		if r.prefix == "" && !strings.HasPrefix(npath, "_") {
			return r, npath
		}
	}
	return nil, ""
}

// inScope reports whether rel is covered by the root's sync paths and filter.
func (r *syncRoot) inScope(rel string) bool {
	if r.include != nil && !r.include(rel) {
		return false
	}
	for _, p := range r.paths {
		if p == "." || rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

func (s *Syncer) engineRemoteKey(npath string) string {
	if strings.HasPrefix(npath, DesktopPrefix) {
		return npath + ".age"
	}
	return s.remoteKey(npath)
}

// npathForKey maps a remote key to its sync-namespace path.
func (s *Syncer) npathForKey(key string) (npath string, ok bool, unknownToken bool) {
	if !strings.HasSuffix(key, ".age") {
		return "", false, false
	}
	k := strings.TrimSuffix(key, ".age")
	if strings.HasPrefix(k, "_external/") || strings.HasPrefix(k, "_metadata/") {
		return "", false, false
	}
	if strings.HasPrefix(k, DesktopPrefix) {
		return k, true, false
	}
	if strings.HasPrefix(k, "_") {
		return "", false, false
	}
	lp, ok := s.localPath(key)
	if !ok {
		return "", false, true
	}
	return lp, true, false
}

// Sync reconciles this device with remote storage in both directions under
// union semantics: every device ends up with every file any device created.
//
//   - New on either side: copied to the other.
//   - Changed on one side: that side's version wins.
//   - Changed on both: transcripts and history are merged line by line,
//     desktop session records keep the most recently active version, and any
//     other file keeps the newer copy and saves the other as .conflict.
//   - Deleted locally: stays deleted here; the remote copy is kept for the
//     other devices (and comes back here only if another device changes it).
//   - Vanished remotely: re-uploaded from this device.
//
// Remote changes are detected by ETag, not timestamps, so clock skew between
// devices can't cause lost or phantom updates. Uploads use compare-and-swap
// when the backend supports it; losing a race to another device triggers a
// fresh merge instead of an overwrite.
func (s *Syncer) Sync(ctx context.Context) (*SyncReport, error) {
	report := &SyncReport{}
	rm := &reportMu{r: report}
	roots := s.roots()

	objects, err := s.storage.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("failed to list remote objects: %w", err)
	}
	remote := make(map[string]storage.ObjectInfo, len(objects))
	for _, obj := range objects {
		np, ok, unknown := s.npathForKey(obj.Key)
		if unknown {
			report.Skipped = append(report.Skipped, obj.Key)
			continue
		}
		if !ok {
			continue
		}
		root, rel := s.rootFor(roots, np)
		if root == nil || !root.inScope(rel) {
			continue
		}
		// Prefer the canonical (normalized) key over a legacy duplicate.
		if existing, dup := remote[np]; dup && existing.Key == s.engineRemoteKey(np) {
			continue
		}
		remote[np] = obj
	}

	local := make(map[string]*localFile)
	for _, root := range roots {
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
			if !root.inScope(rel) {
				continue
			}
			local[root.prefix+rel] = &localFile{root: root, rel: rel, abs: filepath.Join(root.dir, filepath.FromSlash(rel)), info: info}
		}
	}

	known := s.state.Snapshot()
	paths := make(map[string]bool, len(local)+len(remote))
	for p := range local {
		paths[p] = true
	}
	for p := range remote {
		paths[p] = true
	}
	for p := range known {
		if root, rel := s.rootFor(roots, p); root != nil && root.inScope(rel) {
			paths[p] = true
		}
	}

	live := liveSessionIDs(s.claudeDir)
	loaded, manifestETag := s.loadMtimes(ctx)
	manifest := &mtimeBook{m: loaded}
	homeDir := s.home
	repos := newRepoIndex(homeDir)

	type job struct {
		npath string
		run   func() error
	}
	var jobs []job

	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	for _, np := range sorted {
		np := np
		lf := local[np]
		robj, hasRemote := remote[np]
		st, hasState := known[np]

		var lhash string
		if lf != nil {
			if hasState && !st.LocalDeleted && st.Size == lf.info.Size() && st.ModTime.Equal(lf.info.ModTime()) {
				lhash = st.Hash // unchanged size and mtime: skip re-hashing
			} else if h, err := HashFile(lf.abs); err == nil {
				lhash = h
			} else {
				rm.fail(np, err)
				continue
			}
		}

		localChanged := lf != nil && (!hasState || st.LocalDeleted || lhash != st.Hash)
		remoteChanged := hasRemote && (!hasState || remoteDiffers(st, robj))
		isLive := false
		if id, ok := transcriptSessionID(np); ok && live[id] {
			isLive = true
		}

		switch {
		case lf != nil && !hasRemote:
			// New here, or the remote copy vanished: upload create-only.
			restored := hasState && st.RemoteETag != "" && !localChanged
			jobs = append(jobs, job{np, func() error {
				if err := s.reconcileUpload(ctx, np, lf, "", rm, repos, manifest, homeDir); err != nil {
					return err
				}
				if restored {
					rm.add(&report.Restored, np)
				}
				return nil
			}})

		case lf == nil && hasRemote:
			if hasState && !remoteChanged {
				// Deleted on this device; keep it deleted here only.
				if !st.LocalDeleted {
					s.state.MarkLocalDeleted(np, robj.ETag)
				}
				continue
			}
			if isLive {
				rm.add(&report.Deferred, np)
				continue
			}
			isNewDesktop := strings.HasPrefix(np, DesktopPrefix)
			jobs = append(jobs, job{np, func() error {
				root, rel := s.rootFor(roots, np)
				target := &localFile{root: root, rel: rel, abs: filepath.Join(root.dir, filepath.FromSlash(rel))}
				if err := s.reconcileDownload(ctx, np, target, robj, nil, rm, repos, manifest, homeDir); err != nil {
					return err
				}
				if isNewDesktop {
					rm.add(&report.NewDesktopSessions, np)
				}
				return nil
			}})

		case lf != nil && hasRemote:
			switch {
			case hasState && !localChanged && !remoteChanged:
				// In sync.
			case hasState && localChanged && !remoteChanged:
				ifMatch := st.RemoteETag
				if ifMatch == "" {
					ifMatch = robj.ETag // legacy state without ETags
				}
				jobs = append(jobs, job{np, func() error {
					return s.reconcileUpload(ctx, np, lf, ifMatch, rm, repos, manifest, homeDir)
				}})
			case hasState && !localChanged && remoteChanged:
				if isLive {
					rm.add(&report.Deferred, np)
					continue
				}
				expect := lf.info
				jobs = append(jobs, job{np, func() error {
					return s.reconcileDownload(ctx, np, lf, robj, expect, rm, repos, manifest, homeDir)
				}})
			default:
				// Changed on both sides, or present on both before this device
				// ever synced it.
				if isLive {
					rm.add(&report.Deferred, np)
					continue
				}
				jobs = append(jobs, job{np, func() error {
					return s.reconcileBoth(ctx, np, lf, rm, repos, manifest, homeDir)
				}})
			}

		case lf == nil && !hasRemote:
			if hasState {
				s.state.RemoveFile(np)
			}
		}
	}

	// Run with a bounded worker pool.
	sem := make(chan struct{}, defaultWorkers)
	var wg gosync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := j.run(); errors.Is(err, errLocalChanged) {
				rm.add(&report.Deferred, j.npath)
			} else if err != nil {
				rm.fail(j.npath, err)
			}
		}(j)
	}
	wg.Wait()

	if len(report.Uploaded)+len(report.Merged)+len(report.Restored) > 0 {
		if err := s.saveMtimes(ctx, manifest.snapshot(), manifestETag); err != nil {
			s.log("Warning: failed to update mtime manifest: %v", err)
		}
	}

	now := time.Now()
	s.state.LastSync = now
	s.state.LastPull = now
	if len(report.Uploaded)+len(report.Merged)+len(report.Restored) > 0 {
		s.state.LastPush = now
	}
	if err := s.state.Save(); err != nil {
		return report, fmt.Errorf("failed to save state: %w", err)
	}
	return report, nil
}

// remoteDiffers reports whether the remote object changed since st was
// reconciled. ETags are authoritative; state written before ETags were
// tracked falls back to the legacy timestamp comparison.
func remoteDiffers(st FileState, obj storage.ObjectInfo) bool {
	if st.RemoteETag != "" && obj.ETag != "" {
		return st.RemoteETag != obj.ETag
	}
	return obj.LastModified.After(st.Uploaded)
}

// --- remote I/O helpers ------------------------------------------------------

func (s *Syncer) conditional() (storage.Conditional, bool) {
	c, ok := s.storage.(storage.Conditional)
	return c, ok
}

// fetch downloads and decodes a remote object into plaintext for this device
// (decrypted, decompressed, paths resolved). It returns the ETag of exactly
// the version read when the backend reports one.
func (s *Syncer) fetch(ctx context.Context, npath, key string, repos *repoIndex, home string) ([]byte, string, error) {
	var (
		enc  []byte
		etag string
		err  error
	)
	if c, ok := s.conditional(); ok {
		enc, etag, err = c.DownloadWithETag(ctx, key)
	} else {
		enc, err = s.storage.Download(ctx, key)
	}
	if err != nil {
		return nil, "", err
	}
	data, err := s.encryptor.Decrypt(enc)
	if err != nil {
		return nil, "", fmt.Errorf("failed to decrypt: %w", err)
	}
	if isGzipped(data) {
		if data, err = gzipDecompress(data); err != nil {
			return nil, "", fmt.Errorf("failed to decompress: %w", err)
		}
	}
	if strings.HasPrefix(npath, DesktopPrefix) {
		data = s.paths.ResolveContent("record.json", data)
		data = finishDesktopDownload(data, home, repos)
	} else if IsPortableContentPath(npath) {
		data = s.paths.ResolveContent(npath, data)
	}
	return data, etag, nil
}

// put encodes plaintext for remote storage and uploads it, conditionally when
// the backend supports compare-and-swap. ifMatch "" means create-only.
func (s *Syncer) put(ctx context.Context, npath string, plain []byte, ifMatch string) (string, error) {
	data := plain
	if strings.HasPrefix(npath, DesktopPrefix) {
		data = prepareDesktopUpload(data)
		data = s.paths.NormalizeContent("record.json", data)
	} else if IsPortableContentPath(npath) {
		data = s.paths.NormalizeContent(npath, data)
	}
	compressed, err := gzipCompress(data)
	if err != nil {
		return "", fmt.Errorf("failed to compress: %w", err)
	}
	enc, err := s.encryptor.Encrypt(compressed)
	if err != nil {
		return "", fmt.Errorf("failed to encrypt: %w", err)
	}
	key := s.engineRemoteKey(npath)
	if c, ok := s.conditional(); ok {
		return c.UploadIf(ctx, key, enc, ifMatch)
	}
	if err := s.storage.Upload(ctx, key, enc); err != nil {
		return "", err
	}
	// Without compare-and-swap, still learn the new ETag so the next sync
	// detects remote changes by ETag rather than by clock.
	if info, err := s.storage.Head(ctx, key); err == nil && info != nil {
		return info.ETag, nil
	}
	return "", nil
}

// --- reconcile actions --------------------------------------------------------

func (s *Syncer) reconcileUpload(ctx context.Context, npath string, lf *localFile, ifMatch string, rm *reportMu, repos *repoIndex, manifest *mtimeBook, home string) error {
	info, err := os.Stat(lf.abs) // stat before reading: a later append makes the next scan re-hash
	if err != nil {
		return err
	}
	data, err := os.ReadFile(lf.abs)
	if err != nil {
		return err
	}
	etag, err := s.put(ctx, npath, data, ifMatch)
	if errors.Is(err, storage.ErrPreconditionFailed) {
		// Another device got there first: merge with its version instead.
		return s.reconcileBoth(ctx, npath, lf, rm, repos, manifest, home)
	}
	if err != nil {
		return err
	}
	s.state.SetReconciled(npath, info, sha256Hex(data), etag)
	s.recordMtime(manifest, npath, info.ModTime())
	rm.add(&rm.r.Uploaded, npath)
	return nil
}

func (s *Syncer) reconcileDownload(ctx context.Context, npath string, lf *localFile, obj storage.ObjectInfo, expect os.FileInfo, rm *reportMu, repos *repoIndex, manifest *mtimeBook, home string) error {
	data, etag, err := s.fetch(ctx, npath, obj.Key, repos, home)
	if err != nil {
		return err
	}
	if etag == "" {
		etag = obj.ETag
	}
	if err := writeLocal(lf.abs, data, expect, s.claudeDir, lf.root.dir); err != nil {
		return err
	}
	if mt := manifest.get(s.engineRemoteKey(npath)); !mt.IsZero() {
		_ = os.Chtimes(lf.abs, mt, mt)
	}
	info, err := os.Stat(lf.abs)
	if err != nil {
		return err
	}
	s.state.SetReconciled(npath, info, sha256Hex(data), etag)
	rm.add(&rm.r.Downloaded, npath)
	return nil
}

// reconcileBoth resolves a file that changed on both sides (or exists on both
// sides with no shared history), retrying when another device wins a race.
func (s *Syncer) reconcileBoth(ctx context.Context, npath string, lf *localFile, rm *reportMu, repos *repoIndex, manifest *mtimeBook, home string) error {
	key := s.engineRemoteKey(npath)
	for attempt := 0; attempt < maxRaceRetries; attempt++ {
		info, err := os.Stat(lf.abs)
		if err != nil {
			return err
		}
		localData, err := os.ReadFile(lf.abs)
		if err != nil {
			return err
		}
		remoteData, etag, err := s.fetch(ctx, npath, key, repos, home)
		if errors.Is(err, storage.ErrNotFound) {
			// Remote copy vanished meanwhile: plain create-only upload.
			etag, err := s.put(ctx, npath, localData, "")
			if errors.Is(err, storage.ErrPreconditionFailed) {
				continue
			}
			if err != nil {
				return err
			}
			s.state.SetReconciled(npath, info, sha256Hex(localData), etag)
			rm.add(&rm.r.Uploaded, npath)
			return nil
		}
		if err != nil {
			return err
		}

		if bytes.Equal(localData, remoteData) {
			s.state.SetReconciled(npath, info, sha256Hex(localData), etag)
			return nil
		}

		var (
			result      []byte // content both sides should end with
			writeHere   bool   // local copy must change
			uploadThere bool   // remote copy must change
			conflict    bool
		)
		switch {
		case IsMergeableJSONL(npath):
			merged, gotRemote := MergeJSONL(localData, remoteData)
			_, gotLocal := MergeJSONL(remoteData, localData)
			result, writeHere, uploadThere = merged, gotRemote, gotLocal
		case strings.HasPrefix(npath, DesktopPrefix):
			if PreferRemoteDesktopMeta(localData, remoteData) {
				result, writeHere = remoteData, true
			} else {
				result, uploadThere = localData, true
			}
		default:
			// Non-mergeable: newest wins, the other copy is kept beside it.
			remoteTime := manifest.get(key)
			if remoteTime.IsZero() || info.ModTime().After(remoteTime) {
				result, uploadThere = localData, true
				conflict = true
				if err := writeConflictCopy(lf.abs, remoteData); err != nil {
					return err
				}
			} else {
				result, writeHere = remoteData, true
				conflict = true
				if err := writeConflictCopy(lf.abs, localData); err != nil {
					return err
				}
			}
		}

		if !writeHere && !uploadThere {
			// Same content in a different order: already equivalent.
			s.state.SetReconciled(npath, info, sha256Hex(localData), etag)
			return nil
		}

		newETag := etag
		if uploadThere {
			newETag, err = s.put(ctx, npath, result, etag)
			if errors.Is(err, storage.ErrPreconditionFailed) {
				continue // remote moved again: re-merge against the newer version
			}
			if err != nil {
				return err
			}
			if newETag == "" {
				newETag = etag
			}
			s.recordMtime(manifest, npath, info.ModTime())
		}
		if writeHere {
			if err := writeLocal(lf.abs, result, info, s.claudeDir, lf.root.dir); err != nil {
				return err
			}
		}
		final, err := os.Stat(lf.abs)
		if err != nil {
			return err
		}
		s.state.SetReconciled(npath, final, sha256Hex(result), newETag)
		if conflict {
			rm.add(&rm.r.Conflicts, npath)
		}
		rm.add(&rm.r.Merged, npath)
		return nil
	}
	return fmt.Errorf("gave up after %d attempts: other devices keep changing it; will retry next sync", maxRaceRetries)
}

// --- local file helpers -------------------------------------------------------

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// errLocalChanged aborts a write because the local file changed after it was
// scanned; the next sync picks the change up.
var errLocalChanged = errors.New("local file changed during sync; will retry")

// writeLocal replaces abs with data without ever exposing a half-written
// file. When expect is set and the file no longer matches it, the write is
// abandoned. When data merely extends the current file (the common case for a
// transcript another device continued) only the new tail is appended, which
// is safe for readers tailing the file.
func writeLocal(abs string, data []byte, expect os.FileInfo, roots ...string) error {
	clean := filepath.Clean(abs)
	inside := false
	for _, r := range roots {
		if r != "" && strings.HasPrefix(clean, filepath.Clean(r)+string(filepath.Separator)) {
			inside = true
		}
	}
	if !inside {
		return fmt.Errorf("refusing to write outside the sync roots: %s", abs)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	if cur, err := os.Stat(abs); err == nil {
		if expect != nil && (cur.Size() != expect.Size() || !cur.ModTime().Equal(expect.ModTime())) {
			return errLocalChanged
		}
		if cur.Size() > 0 && cur.Size() < int64(len(data)) {
			if existing, err := os.ReadFile(abs); err == nil && bytes.HasPrefix(data, existing) {
				f, err := os.OpenFile(abs, os.O_WRONLY|os.O_APPEND, 0600)
				if err == nil {
					_, werr := f.Write(data[len(existing):])
					cerr := f.Close()
					if werr == nil && cerr == nil {
						return nil
					}
				}
			}
		}
	}

	tmp, err := os.CreateTemp(filepath.Dir(abs), ".stradl-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, abs)
}

// writeConflictCopy keeps the losing side of a non-mergeable conflict next to
// the file. Conflict copies are never synced.
func writeConflictCopy(abs string, data []byte) error {
	p := abs + ".conflict." + time.Now().Format("20060102-150405")
	return os.WriteFile(p, data, 0600)
}

// --- mtime manifest ------------------------------------------------------------

// mtimeBook is the shared mtime manifest, guarded for worker goroutines.
type mtimeBook struct {
	mu gosync.Mutex
	m  map[string]time.Time
}

func (b *mtimeBook) get(key string) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.m[key]
}

func (b *mtimeBook) snapshot() map[string]time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]time.Time, len(b.m))
	for k, v := range b.m {
		out[k] = v
	}
	return out
}

func (s *Syncer) recordMtime(manifest *mtimeBook, npath string, t time.Time) {
	manifest.mu.Lock()
	manifest.m[s.engineRemoteKey(npath)] = t
	manifest.mu.Unlock()
}

func (s *Syncer) loadMtimes(ctx context.Context) (map[string]time.Time, string) {
	m := make(map[string]time.Time)
	var (
		enc  []byte
		etag string
		err  error
	)
	if c, ok := s.conditional(); ok {
		enc, etag, err = c.DownloadWithETag(ctx, MtimeManifestKey)
	} else {
		enc, err = s.storage.Download(ctx, MtimeManifestKey)
	}
	if err != nil {
		return m, ""
	}
	data, err := s.encryptor.Decrypt(enc)
	if err != nil {
		return m, ""
	}
	if isGzipped(data) {
		if data, err = gzipDecompress(data); err != nil {
			return m, ""
		}
	}
	_ = json.Unmarshal(data, &m)
	return m, etag
}

// saveMtimes merges this sync's entries into the shared manifest with
// compare-and-swap, so concurrent devices add to it instead of replacing it.
func (s *Syncer) saveMtimes(ctx context.Context, mine map[string]time.Time, etag string) error {
	for attempt := 0; attempt < maxRaceRetries; attempt++ {
		if attempt > 0 {
			theirs, newETag := s.loadMtimes(ctx)
			for k, v := range theirs {
				if cur, ok := mine[k]; !ok || v.After(cur) {
					mine[k] = v
				}
			}
			etag = newETag
		}
		data, err := json.Marshal(mine)
		if err != nil {
			return err
		}
		compressed, err := gzipCompress(data)
		if err != nil {
			return err
		}
		enc, err := s.encryptor.Encrypt(compressed)
		if err != nil {
			return err
		}
		c, ok := s.conditional()
		if !ok {
			return s.storage.Upload(ctx, MtimeManifestKey, enc)
		}
		if _, err = c.UploadIf(ctx, MtimeManifestKey, enc, etag); errors.Is(err, storage.ErrPreconditionFailed) {
			continue
		}
		return err
	}
	return fmt.Errorf("manifest kept changing; will retry next sync")
}
