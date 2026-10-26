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
	"path"
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
	Moved      []string // stale copies of sessions Claude Code moved to another project folder, folded into the current copy

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
			return !strings.Contains(rel, ".conflict.") && !isDeviceLocalFile(rel) && !s.isExcluded(rel)
		},
	}}
	if dir := s.cfg.EffectiveDesktopDir(); dir != "" && dirExists(dir) {
		roots = append(roots, &syncRoot{dir: dir, prefix: DesktopPrefix, paths: []string{"."}, include: isDesktopRecord})
	}
	return roots
}

// desktopReleasedSuffix marks a transcript the desktop app released after the
// user deleted or archived its session there. Claude Code's retention sweep
// treats it as "this device may now delete the transcript", so it must stay
// on the device that wrote it: synced, a delete on one Mac would expire the
// session on every Mac.
const desktopReleasedSuffix = ".desktop-released.json"

// isDeviceLocalFile reports files under ~/.claude that belong to this device
// alone and must never be synced: desktop release markers, Claude Code's
// temp, lock and set-aside files (<id>.orphaned-*.jsonl,
// *.jsonl.superseded-*), and Stradl's own in-flight temp files.
func isDeviceLocalFile(rel string) bool {
	base := path.Base(rel)
	switch {
	case strings.HasSuffix(base, desktopReleasedSuffix),
		strings.HasPrefix(base, ".stradl-"),
		strings.HasSuffix(base, ".tmp"), strings.Contains(base, ".tmp."),
		strings.HasSuffix(base, ".lock"),
		strings.Contains(base, ".orphaned-"), strings.Contains(base, ".superseded-"):
		return true
	}
	return false
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
	if !isCanonicalRel(k) {
		return "", false, false // crafted key: "..", ".", empty segment or backslash
	}
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
	if !isCanonicalRel(lp) {
		return "", false, false
	}
	return lp, true, false
}

// isCanonicalRel reports whether p is a clean relative slash path that cannot
// step outside the directory it is joined to. Remote keys are untrusted: the
// storage token alone (no passphrase) is enough to rename blobs, and age does
// not bind a ciphertext to its name.
func isCanonicalRel(p string) bool {
	if p == "" || strings.ContainsRune(p, '\\') || strings.HasPrefix(p, "/") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
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
	return s.reconcile(ctx, false)
}

// Preview reports what Sync would do right now without changing anything
// locally or remotely: Uploaded, Downloaded, Merged, Restored, Deferred and
// NewDesktopSessions list the files each action would touch.
func (s *Syncer) Preview(ctx context.Context) (*SyncReport, error) {
	return s.reconcile(ctx, true)
}

func (s *Syncer) reconcile(ctx context.Context, dryRun bool) (*SyncReport, error) {
	report := &SyncReport{}
	rm := &reportMu{r: report}
	roots := s.roots()
	if !dryRun {
		report.Moved = s.foldMovedSessions(liveSessionIDs(s.claudeDir))
	}

	objects, err := s.storage.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("failed to list remote objects: %w", err)
	}
	if err := s.checkKey(ctx, objects, dryRun); err != nil {
		return nil, err
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
		kind  string // upload, restore, download, new-desktop, merge
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

		if lf == nil && hasRemote {
			// On a case-insensitive volume another Mac's spelling of a folder
			// (~/Code vs ~/code) names a file the scan listed under this Mac's
			// spelling. Treat it as present so it merges instead of being
			// overwritten by a blind download.
			if root, rel := s.rootFor(roots, np); root != nil {
				abs := filepath.Join(root.dir, filepath.FromSlash(rel))
				if info, err := os.Stat(abs); err == nil && info.Mode().IsRegular() {
					lf = &localFile{root: root, rel: rel, abs: abs, info: info}
				}
			}
		}

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
		if id, ok := transcriptSessionID(np); ok {
			// A transcript written in the last few seconds may belong to a
			// Claude process that hasn't recorded itself yet.
			isLive = live[id] || (lf != nil && s.recentWrite > 0 && time.Since(lf.info.ModTime()) < s.recentWrite)
		}

		switch {
		case lf != nil && !hasRemote:
			// New here, or the remote copy vanished: upload create-only.
			restored := hasState && st.RemoteETag != "" && !localChanged
			kind := "upload"
			if restored {
				kind = "restore"
			}
			jobs = append(jobs, job{np, kind, func() error {
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
				if !st.LocalDeleted && !dryRun {
					s.state.MarkLocalDeleted(np, robj.ETag)
				}
				continue
			}
			if isLive {
				rm.add(&report.Deferred, np)
				continue
			}
			isNewDesktop := strings.HasPrefix(np, DesktopPrefix)
			kind := "download"
			if isNewDesktop {
				kind = "new-desktop"
			}
			jobs = append(jobs, job{np, kind, func() error {
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
				jobs = append(jobs, job{np, "upload", func() error {
					return s.reconcileUpload(ctx, np, lf, ifMatch, rm, repos, manifest, homeDir)
				}})
			case hasState && !localChanged && remoteChanged:
				if isLive {
					rm.add(&report.Deferred, np)
					continue
				}
				expect := lf.info
				jobs = append(jobs, job{np, "download", func() error {
					return s.reconcileDownload(ctx, np, lf, robj, expect, rm, repos, manifest, homeDir)
				}})
			default:
				// Changed on both sides, or present on both before this device
				// ever synced it.
				if isLive {
					rm.add(&report.Deferred, np)
					continue
				}
				jobs = append(jobs, job{np, "merge", func() error {
					return s.reconcileBoth(ctx, np, lf, rm, repos, manifest, homeDir)
				}})
			}

		case lf == nil && !hasRemote:
			if hasState && !dryRun {
				s.state.RemoveFile(np)
			}
		}
	}

	if dryRun {
		for _, j := range jobs {
			switch j.kind {
			case "upload":
				report.Uploaded = append(report.Uploaded, j.npath)
			case "restore":
				report.Restored = append(report.Restored, j.npath)
			case "download":
				report.Downloaded = append(report.Downloaded, j.npath)
			case "new-desktop":
				report.Downloaded = append(report.Downloaded, j.npath)
				report.NewDesktopSessions = append(report.NewDesktopSessions, j.npath)
			case "merge":
				report.Merged = append(report.Merged, j.npath)
			}
		}
		return report, nil
	}

	// Run with a bounded worker pool: transcripts first, then sidebar
	// records, so a record never lands on a device before its transcript.
	var transcripts, records []job
	for _, j := range jobs {
		if strings.HasPrefix(j.npath, DesktopPrefix) {
			records = append(records, j)
		} else {
			transcripts = append(transcripts, j)
		}
	}
	// Jobs that touch the same physical file (two remote spellings of one
	// path on a case-insensitive volume) run one at a time: the second then
	// sees the first's write and merges or defers instead of renaming over it.
	locks := &keyedLocks{m: make(map[string]*gosync.Mutex)}
	for _, batch := range [][]job{transcripts, records} {
		sem := make(chan struct{}, defaultWorkers)
		var wg gosync.WaitGroup
		for _, j := range batch {
			wg.Add(1)
			go func(j job) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				defer locks.lock(s.physicalKey(roots, j.npath))()
				if err := j.run(); errors.Is(err, errLocalChanged) {
					rm.add(&report.Deferred, j.npath)
				} else if err != nil {
					rm.fail(j.npath, err)
				}
			}(j)
		}
		wg.Wait()
	}

	// Downloads in this pass may have delivered the new copy of a moved session.
	report.Moved = append(report.Moved, s.foldMovedSessions(liveSessionIDs(s.claudeDir))...)

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

// keyedLocks serializes work per key.
type keyedLocks struct {
	mu gosync.Mutex
	m  map[string]*gosync.Mutex
}

func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	l, ok := k.m[key]
	if !ok {
		l = &gosync.Mutex{}
		k.m[key] = l
	}
	k.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// physicalKey identifies the file an npath lands on, folding case because
// macOS volumes are case-insensitive by default.
func (s *Syncer) physicalKey(roots []*syncRoot, npath string) string {
	if root, rel := s.rootFor(roots, npath); root != nil {
		return strings.ToLower(filepath.Join(root.dir, filepath.FromSlash(rel)))
	}
	return strings.ToLower(npath)
}

// recentWriteWindow treats a transcript written this recently as live.
const recentWriteWindow = 5 * time.Second

// transcriptLiveNow re-checks, right before a local write, whether a Claude
// process has claimed the transcript since the pass was planned.
func (s *Syncer) transcriptLiveNow(npath string) bool {
	id, ok := transcriptSessionID(npath)
	return ok && liveSessionIDs(s.claudeDir)[id]
}

// clampMtime keeps a downloaded transcript's restored modification time inside
// this device's retention window. Claude Code's cleanup deletes transcripts
// whose mtime is older than cleanupPeriodDays, so restoring a months-old mtime
// would let the sweep delete a session minutes after it arrived.
func (s *Syncer) clampMtime(npath string, mt time.Time) time.Time {
	if mt.IsZero() || !strings.HasPrefix(npath, "projects/") || path.Ext(npath) != ".jsonl" {
		return mt
	}
	days := claudeCleanupDays(s.claudeDir) - 2
	if days < 1 {
		days = 1
	}
	if floor := time.Now().Add(-time.Duration(days) * 24 * time.Hour); mt.Before(floor) {
		return floor
	}
	return mt
}

// claudeCleanupDays reads cleanupPeriodDays from ~/.claude/settings.json,
// defaulting to Claude Code's 30.
func claudeCleanupDays(claudeDir string) int {
	data, err := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if err != nil {
		return 30
	}
	var st struct {
		CleanupPeriodDays int `json:"cleanupPeriodDays"`
	}
	if json.Unmarshal(data, &st) != nil || st.CleanupPeriodDays <= 0 {
		return 30
	}
	return st.CleanupPeriodDays
}

// remoteDiffers reports whether the remote object changed since st was
// reconciled. ETags are authoritative; state written before ETags were
// tracked falls back to the legacy timestamp comparison.
func remoteDiffers(st FileState, obj storage.ObjectInfo) bool {
	if st.RemoteETag != "" && obj.ETag != "" {
		return normETag(st.RemoteETag) != normETag(obj.ETag)
	}
	return obj.LastModified.After(st.Uploaded)
}

// normETag strips quoting and weak markers so ETags from listings and from
// response headers compare equal whatever the backend's spelling.
func normETag(e string) string {
	return strings.Trim(strings.TrimPrefix(e, "W/"), `"`)
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
	if data, err = unbindKey(data, key); err != nil {
		return nil, "", err
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
	key := s.engineRemoteKey(npath)
	compressed, err := gzipCompress(bindKey(data, key))
	if err != nil {
		return "", fmt.Errorf("failed to compress: %w", err)
	}
	enc, err := s.encryptor.Encrypt(compressed)
	if err != nil {
		return "", fmt.Errorf("failed to encrypt: %w", err)
	}
	if int64(len(enc)) > storage.MaxDownloadSize {
		return "", fmt.Errorf("too large to sync (%d MB compressed; other devices accept up to %d MB)", len(enc)>>20, storage.MaxDownloadSize>>20)
	}
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
	if ifMatch != "" && strings.HasPrefix(npath, DesktopPrefix) {
		// The desktop app loads records only at launch and re-saves its
		// in-memory copy (lastFocusedAt on a click, say), so a record another
		// Mac updated, and this Mac downloaded, can come back older. A lower
		// activity stamp never replaces the remote record; a rename, archive
		// or pin made here keeps the stamp equal and still uploads.
		remoteData, etag, ferr := s.fetch(ctx, npath, s.engineRemoteKey(npath), repos, home)
		if ferr == nil && desktopMetaStamp(remoteData) > desktopMetaStamp(data) {
			if etag == "" {
				etag = ifMatch
			}
			if err := writeLocal(lf.abs, remoteData, info, s.claudeDir, lf.root.dir); err != nil {
				return err
			}
			final, err := os.Stat(lf.abs)
			if err != nil {
				return err
			}
			s.state.SetReconciled(npath, final, sha256Hex(remoteData), etag)
			rm.add(&rm.r.Downloaded, npath)
			return nil
		}
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
	if s.transcriptLiveNow(npath) {
		return errLocalChanged
	}
	if expect != nil && IsMergeableJSONL(npath) {
		// Transcripts only grow. A remote copy missing lines this Mac has (an
		// older version restored or replayed, or a truncated upload) must never
		// replace them: merge instead, which keeps the union on both sides.
		if local, err := os.ReadFile(lf.abs); err == nil {
			if _, localHasMore := MergeJSONL(data, local); localHasMore {
				return s.reconcileBoth(ctx, npath, lf, rm, repos, manifest, home)
			}
		}
	}
	if err := writeLocal(lf.abs, data, expect, s.claudeDir, lf.root.dir); err != nil {
		return err
	}
	if mt := s.clampMtime(npath, manifest.get(s.engineRemoteKey(npath))); !mt.IsZero() {
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
				if err := writeConflictCopy(lf.abs, remoteData, ConflictFromOtherMac); err != nil {
					return err
				}
			} else {
				result, writeHere = remoteData, true
				conflict = true
				if err := writeConflictCopy(lf.abs, localData, ConflictFromThisMac); err != nil {
					return err
				}
			}
		}

		if writeHere && s.transcriptLiveNow(npath) {
			// A Claude process picked the session up meanwhile; touch nothing
			// and merge once it exits.
			return errLocalChanged
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
// Conflict copy origins, the last segment of <file>.conflict.<time>.<origin>:
// which device wrote the version that lost.
const (
	ConflictFromThisMac  = "this-mac"
	ConflictFromOtherMac = "other-mac"
)

func writeConflictCopy(abs string, data []byte, origin string) error {
	p := abs + ".conflict." + time.Now().Format("20060102-150405") + "." + origin
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
