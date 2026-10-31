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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tawanorg/claude-sync/internal/storage"
)

// MtimeManifestKey is the remote key of the shared modification-time
// manifest, keyed by portable remote key so devices with different home
// directories agree on entries. Updated with compare-and-swap.
const MtimeManifestKey = "_metadata/mtimes.json.age"

// ModesManifestKey is the remote key of the shared file-mode manifest: the
// permission bits of synced files whose mode differs from the default for
// their path (see defaultPerm), such as an executable script or git hook,
// keyed like the mtime manifest. Updated with compare-and-swap.
const ModesManifestKey = "_metadata/modes.json.age"

// maxRaceRetries bounds how often one file is re-merged after losing a
// compare-and-swap race to another device within a single sync.
const maxRaceRetries = 4

// massDeleteFloor is the smallest number of deletions the mass-delete guard
// considers; below it, a folder can always lose more than half its files.
const massDeleteFloor = 10

type rootKind int

const (
	rootClaude  rootKind = iota // ~/.claude
	rootDesktop                 // the desktop app's sidebar records
	rootFolder                  // a project folder
)

// syncRoot is a local directory whose files join the sync namespace under
// prefix: ~/.claude with no prefix, the desktop session index under
// DesktopPrefix, and each project folder under FoldersPrefix.
type syncRoot struct {
	dir     string
	prefix  string
	paths   []string
	include func(rel string) bool
	skipDir func(rel string) bool // directories left out of the scan entirely
	kind    rootKind
	label   string      // how the user knows the root: ~/.claude, ~/code/app
	perm    os.FileMode // mode of files written here unless the modes manifest says otherwise

	// deferred keeps the root out of this pass (git is working in it, or it
	// couldn't be scanned completely).
	deferred bool

	// Project folders only. Every git repository syncs as its own unit, and
	// only when a session needs it: children are folder roots inside this
	// one (a repository a session works in), which this root leaves to them;
	// nested are other repositories inside it, which no session needs and
	// which don't sync at all. Both are slash paths relative to dir.
	children map[string]bool
	nested   map[string]bool
	// modules are where (under this root's .git/modules) git keeps the data
	// of child repositories that are submodules; only those sync. moduleOf
	// maps each child to its entry.
	modules  map[string]bool
	moduleOf map[string]string
	// inherited marks a repository inside another folder syncing on its own
	// for the first time with state from when it synced as part of that
	// folder.
	inherited bool
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
	Deferred   []string // remote changes held back while a live session owns the file, or project folders git is working in
	Conflicts  []string // non-mergeable files changed on both sides; loser kept as .conflict copy
	Skipped    []string // remote keys using a path token this device can't map
	Moved      []string // stale copies of sessions Claude Code moved to another project folder, folded into the current copy

	// Deleted lists files deleted on this device whose remote copy this pass
	// deleted too, so every Mac drops them (everything except transcripts,
	// prompt history and sidebar records, which keep every device's copy).
	Deleted []string

	// Trashed lists files another Mac deleted, moved from here into
	// ~/.stradl/trash rather than deleted.
	Trashed []string

	// SidebarPending is set when sidebar layout changes from other devices
	// (groups, membership, pins) are waiting for the Claude app to quit
	// before they can be applied; SidebarChanged counts what changed.
	SidebarPending bool
	SidebarChanged int

	// RemoteControlled lists sidebar records left off this Mac because another
	// Mac serves the session over Remote Control right now: the Claude app
	// already lists it there, so a copy here would show twice. They arrive
	// once Remote Control is off.
	RemoteControlled []string

	// InICloud lists files skipped because their contents are only in iCloud
	// (Optimize Mac Storage); their download was requested and a later pass
	// syncs them once they have arrived.
	InICloud []string

	// NewDesktopSessions lists desktop sidebar records that did not exist on
	// this device before. The desktop app loads its sidebar at launch, so they
	// show up after it restarts.
	NewDesktopSessions []string

	// Warnings are problems that held part of the sync back without failing
	// it, such as deletions the mass-delete guard stopped.
	Warnings []string

	Errors []error
}

// Changed reports whether the pass moved any data.
func (r *SyncReport) Changed() bool {
	return len(r.Uploaded)+len(r.Downloaded)+len(r.Merged)+len(r.Restored)+len(r.Deleted)+len(r.Trashed) > 0
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

// roots returns the ~/.claude root and, when present, the desktop session
// index. Project folders join them per pass (see syncRoots).
func (s *Syncer) roots() []*syncRoot {
	everything := s.cfg.IsEverything()
	roots := []*syncRoot{{
		dir:   s.claudeDir,
		paths: s.syncPaths(),
		include: func(rel string) bool {
			if strings.Contains(rel, ".conflict.") || isDeviceLocalFile(rel) || claudeDenied(rel) || s.isExcluded(rel) {
				return false
			}
			if s.claudeOff(rel) {
				return false // work of an account that doesn't sync
			}
			return !everything || !s.cfg.IsBuildArtifact(rel)
		},
		skipDir: func(rel string) bool {
			return claudeDenied(rel) || s.isExcluded(rel) || (everything && s.cfg.IsBuildArtifact(rel))
		},
		kind:  rootClaude,
		label: "~/.claude",
		perm:  0600,
	}}
	if dir := s.cfg.EffectiveDesktopDir(); dir != "" && dirExists(dir) {
		include := func(rel string) bool {
			acct, _, _ := strings.Cut(rel, "/")
			return isDesktopRecord(rel) && !s.registry().off(strings.ToLower(acct))
		}
		roots = append(roots, &syncRoot{dir: dir, prefix: DesktopPrefix, paths: []string{"."}, include: include, kind: rootDesktop, label: "Claude sidebar", perm: 0600})
	}
	return roots
}

// syncRoots returns every root for one pass: roots() plus, when folder sync
// is on, each project folder (see discoverFolders).
func (s *Syncer) syncRoots(objects []storage.ObjectInfo, useCache bool) []*syncRoot {
	roots := s.roots()
	if s.cfg.IsFolderSyncEnabled() {
		for _, rel := range s.discoverFolders(objects, useCache) {
			roots = append(roots, s.folderRoot(rel))
		}
		s.linkFolderRoots(roots, objects)
	}
	return roots
}

// claudeDeniedDirs are top-level ~/.claude folders that belong to one Mac:
// running-process records, per-process environments and shell snapshots,
// caches, telemetry, logs, IDE connection locks and backups.
var claudeDeniedDirs = map[string]bool{
	"sessions": true, "session-env": true, "shell-snapshots": true, "cache": true,
	"statsig": true, "telemetry": true, "debug": true, "logs": true, "ide": true,
	"backups": true,
}

// claudeDenied reports ~/.claude paths that never sync whatever the scope:
// the per-Mac folders above, login credentials, which stay on each Mac, and
// the managed settings and policy limits of the organization this Mac is
// signed in to (another Mac may be signed in to another account).
func claudeDenied(rel string) bool {
	first, _, _ := strings.Cut(rel, "/")
	return claudeDeniedDirs[first] || path.Base(rel) == ".credentials.json" ||
		strings.HasPrefix(rel, "policy-limits.json") || strings.HasPrefix(rel, "remote-settings.json")
}

// desktopReleasedSuffix marks a transcript the desktop app released after the
// user deleted or archived its session there. Claude Code's retention sweep
// treats it as "this device may now delete the transcript", so it must stay
// on the device that wrote it: synced, a delete on one Mac would expire the
// session on every Mac.
const desktopReleasedSuffix = ".desktop-released.json"

// isDeviceLocalFile reports files under ~/.claude that belong to this device
// alone and must never be synced: login credentials, desktop release markers,
// Claude Code's temp, lock and set-aside files (<id>.orphaned-*.jsonl,
// *.jsonl.superseded-*), and Stradl's own in-flight temp files.
func isDeviceLocalFile(rel string) bool {
	base := path.Base(rel)
	switch {
	case base == ".credentials.json",
		strings.HasSuffix(base, desktopReleasedSuffix),
		strings.HasPrefix(base, ".stradl-"),
		strings.HasSuffix(base, ".tmp"), strings.Contains(base, ".tmp."),
		strings.HasSuffix(base, ".lock"),
		strings.Contains(base, ".orphaned-"), strings.Contains(base, ".superseded-"):
		return true
	}
	return false
}

// unionPath reports paths that keep union semantics: transcripts and
// everything else under projects/, the prompt history, desktop sidebar
// records, and git's objects and refs. A local delete stays local and a
// vanished remote copy is restored. Every other path mirrors deletes between
// Macs.
//
// git deletes objects and loose refs when it packs them (gc, pack-refs) or
// prunes what it no longer needs itself; mirrored, that would remove commits
// another Mac still builds on, or empty .git/refs. So a Mac keeps every
// object and ref another Mac had; each Mac's own gc tidies its copy.
func unionPath(npath string) bool {
	return strings.HasPrefix(npath, "projects/") || npath == "history.jsonl" || strings.HasPrefix(npath, DesktopPrefix) ||
		gitUnionPath(npath)
}

func (s *Syncer) rootFor(roots []*syncRoot, npath string) (*syncRoot, string) {
	var best *syncRoot
	for _, r := range roots {
		if r.prefix != "" && strings.HasPrefix(npath, r.prefix) && (best == nil || len(r.prefix) > len(best.prefix)) {
			best = r
		}
	}
	if best != nil {
		return best, strings.TrimPrefix(npath, best.prefix)
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
	if r.kind == rootFolder && !r.owns(rel) {
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
	if strings.HasPrefix(npath, DesktopPrefix) || strings.HasPrefix(npath, FoldersPrefix) {
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
	if strings.HasPrefix(k, FoldersPrefix) {
		if path.Base(k) == folderMarker {
			return "", false, false
		}
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

// scanRoot lists a root's local files in scope. A project folder that git is
// working in (errGitBusy) or that can't be read completely returns an error
// the caller turns into a deferral.
func (s *Syncer) scanRoot(root *syncRoot) (map[string]os.FileInfo, error) {
	if root.kind == rootFolder {
		return s.scanFolder(root)
	}
	files, err := GetLocalFiles(root.dir, root.paths, root.skipDir)
	if err != nil {
		return nil, err
	}
	out := make(map[string]os.FileInfo, len(files))
	for rel, info := range files {
		rel = strings.TrimPrefix(rel, "./")
		if root.inScope(rel) {
			out[rel] = info
		}
	}
	return out, nil
}

// Sync reconciles this device with remote storage in both directions:
// every device ends up with every file any device created.
//
//   - New on either side: copied to the other.
//   - Changed on one side: that side's version wins.
//   - Changed on both: transcripts and history are merged line by line,
//     desktop session records keep the most recently active version, and any
//     other file keeps the newer copy and saves the other as .conflict.
//   - Deleted locally: transcripts, history and sidebar records stay deleted
//     here only (the remote copy is kept for the other devices and comes back
//     here only if another device changes it); every other file is deleted
//     remotely too, with compare-and-swap, so a change made meanwhile on
//     another Mac wins over the delete.
//   - Vanished remotely: transcripts, history and sidebar records are
//     re-uploaded from this device; any other file moves to ~/.stradl/trash,
//     or, if it changed here since the last sync, is uploaded again.
//
// A pass that would delete or trash more than half of a folder's synced
// files stops short of that and warns instead (see massDeleteFloor).
//
// Remote changes are detected by ETag, not timestamps, so clock skew between
// devices can't cause lost or phantom updates. Uploads use compare-and-swap
// when the backend supports it; losing a race to another device triggers a
// fresh merge instead of an overwrite.
func (s *Syncer) Sync(ctx context.Context) (*SyncReport, error) {
	return s.reconcile(ctx, false)
}

// Preview reports what Sync would do right now without changing anything
// locally or remotely: Uploaded, Downloaded, Merged, Restored, Deleted,
// Trashed, Deferred and NewDesktopSessions list the files each action would
// touch, and Warnings what it would hold back.
func (s *Syncer) Preview(ctx context.Context) (*SyncReport, error) {
	return s.reconcile(ctx, true)
}

// job is one file's planned action.
type job struct {
	npath string
	kind  string // upload, restore, download, new-desktop, merge, delete, trash
	root  *syncRoot
	run   func() error
}

// ownsState reports whether this Mac's state holds files the folder root
// itself owns (not those of a repository inside it).
func (s *Syncer) ownsState(root *syncRoot) bool {
	for np := range s.state.Snapshot() {
		if rel, ok := strings.CutPrefix(np, root.prefix); ok && root.owns(rel) {
			return true
		}
	}
	return false
}

// insideFolder reports whether root lies inside another project folder.
func insideFolder(roots []*syncRoot, root *syncRoot) bool {
	for _, r := range roots {
		if r != root && r.kind == rootFolder && strings.HasPrefix(root.prefix, r.prefix) {
			return true
		}
	}
	return false
}

// insideDeferred reports whether root is inside a project folder sitting
// this pass out.
func insideDeferred(roots []*syncRoot, root *syncRoot) bool {
	for _, r := range roots {
		if r != root && r.kind == rootFolder && r.deferred && strings.HasPrefix(root.prefix, r.prefix) {
			return true
		}
	}
	return false
}

func (s *Syncer) reconcile(ctx context.Context, dryRun bool) (*SyncReport, error) {
	report := &SyncReport{}
	rm := &reportMu{r: report}
	if !dryRun {
		report.Moved = s.foldMovedSessions(liveSessionIDs(s.claudeDir))
		s.purgeTrash(time.Now())
	}

	objects, err := s.storage.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("failed to list remote objects: %w", err)
	}
	if err := s.checkKey(ctx, objects, dryRun); err != nil {
		return nil, err
	}
	if err := s.prepareAccounts(ctx, objects, dryRun, true); err != nil {
		return nil, err
	}
	stored := s.storedSessions(objects)
	s.held = s.heldSessions(stored)
	roots := s.syncRoots(objects, true)
	if !dryRun {
		s.noteFolderAccounts(ctx)
	}

	// Scan first: a project folder git is working in sits the pass out, and
	// so does every repository inside it (folders before the ones they hold).
	local := make(map[string]*localFile)
	var lockWarnings []string
	var fresh []string // see errFolderMissing below
	wasRoot := make(map[string]bool)
	for _, rel := range s.knownFolderList() {
		wasRoot[FoldersPrefix+rel+"/"] = true
	}
	order := append([]*syncRoot(nil), roots...)
	sort.SliceStable(order, func(i, j int) bool { return order[i].prefix < order[j].prefix })
	for _, root := range order {
		if root.kind == rootFolder && insideDeferred(order, root) {
			root.deferred = true
			continue
		}
		files, err := s.scanRoot(root)
		var busy *gitBusyError
		switch {
		case errors.Is(err, errAwaitingPermission):
			root.deferred = true // reported once per protected folder below
			continue
		case root.kind == rootFolder && errors.Is(err, syscall.EDEADLK):
			// Part of it is only in iCloud and downloading: sit this pass out.
			root.deferred = true
			report.InICloud = append(report.InICloud, root.prefix)
			continue
		case errors.Is(err, errFolderMissing) && s.state.hasPrefix(root.prefix) && !wasRoot[root.prefix] && insideFolder(roots, root):
			// A repository inside another folder that synced only as part of
			// it (an older Stradl, before each repository synced on its own):
			// its state is the folder's, not this repository's. It's
			// downloaded fresh, and nothing is deleted for it.
			fresh = append(fresh, root.prefix)
		case errors.Is(err, errFolderMissing) && claudeWorktree(strings.TrimPrefix(root.prefix, FoldersPrefix)) && s.state.hasPrefix(root.prefix):
			// A Claude worktree that synced from here and is gone now (its
			// session ended and the app removed it) sits out quietly; other
			// Macs keep their copy.
			root.deferred = true
			continue
		case errors.Is(err, errFolderMissing):
			if s.state.hasPrefix(root.prefix) && !s.cfg.AllowMassDelete {
				// It synced from here before: a rename, a re-clone in
				// progress or a deleted folder. Deleting it on every Mac
				// takes a confirmation.
				root.deferred = true
				lockWarnings = append(lockWarnings, fmt.Sprintf("%s synced from this Mac before but is gone now, so it wasn't deleted on your other Macs. If you deleted it on purpose and want it gone everywhere, run 'stradl sync --allow-mass-delete'.", root.label))
				continue
			}
		case errors.As(err, &busy):
			root.deferred = true
			report.Deferred = append(report.Deferred, root.prefix)
			if !busy.since.IsZero() && time.Since(busy.since) > staleGitLock {
				lockWarnings = append(lockWarnings, fmt.Sprintf("%s: git left %s behind %s ago, and the folder won't sync while it exists. If no git command is running there, delete that file.", root.label, busy.lock, time.Since(busy.since).Round(time.Minute)))
			}
			continue
		case err != nil && root.kind == rootFolder:
			root.deferred = true
			rm.fail(root.label, fmt.Errorf("couldn't read the whole folder, so it was left as is: %w", err))
			continue
		case err != nil:
			return nil, fmt.Errorf("failed to scan %s: %w", root.dir, err)
		}
		if root.kind == rootFolder && err == nil && !wasRoot[root.prefix] && insideFolder(roots, root) && s.ownsState(root) {
			root.inherited = true
		}
		for rel, info := range files {
			local[root.prefix+rel] = &localFile{root: root, rel: rel, abs: filepath.Join(root.dir, filepath.FromSlash(rel)), info: info}
		}
	}
	// A submodule sitting the pass out holds back its git data in the
	// repository holding it too, so its HEAD never moves without its files.
	for _, outer := range roots {
		for child, m := range outer.moduleOf {
			inner, _ := s.rootFor(roots, outer.prefix+child+"/")
			if inner == nil || inner.prefix != outer.prefix+child+"/" || !inner.deferred || outer.deferred {
				continue
			}
			delete(outer.modules, m)
			for np := range local {
				if strings.HasPrefix(np, outer.prefix+".git/modules/"+m+"/") {
					delete(local, np)
				}
			}
		}
	}
	active := func(np string) (*syncRoot, string, bool) {
		root, rel := s.rootFor(roots, np)
		return root, rel, root != nil && !root.deferred && root.inScope(rel)
	}

	remote := make(map[string]storage.ObjectInfo, len(objects))
	hasModes := false
	for _, obj := range objects {
		if obj.Key == ModesManifestKey {
			hasModes = true
		}
		np, ok, unknown := s.npathForKey(obj.Key)
		if unknown {
			report.Skipped = append(report.Skipped, obj.Key)
			continue
		}
		if !ok {
			continue
		}
		if _, _, in := active(np); !in {
			continue
		}
		// Prefer the canonical (normalized) key over a legacy duplicate.
		if existing, dup := remote[np]; dup && existing.Key == s.engineRemoteKey(np) {
			continue
		}
		remote[np] = obj
	}

	known := s.state.Snapshot()
	for np := range known {
		forget := false
		for _, p := range fresh {
			forget = forget || strings.HasPrefix(np, p)
		}
		if root, _, in := active(np); in && root.kind == rootFolder && root.inherited && !unionPath(np) {
			// Its state is from when it synced inside the folder holding
			// it; a file storage no longer has was deleted while this Mac
			// didn't follow the repository (an older Stradl on another
			// Mac). It uploads as new rather than going to the trash here.
			_, stored := remote[np]
			forget = forget || !stored
		}
		if forget {
			delete(known, np)
			if !dryRun {
				s.state.RemoveFile(np)
			}
		}
	}
	s.known = known
	defer func() { s.known = nil }()
	paths := make(map[string]bool, len(local)+len(remote))
	for p := range local {
		paths[p] = true
	}
	for p := range remote {
		paths[p] = true
	}
	for p := range known {
		if _, _, in := active(p); in {
			paths[p] = true
		}
	}
	// Sessions without an account yet while some account doesn't sync sit
	// the pass out (see heldSessions), also one that appeared during it.
	for sid := range s.heldSessions(stored) {
		s.held[sid] = true
	}
	for p := range paths {
		if sid := claudeSession(p); sid != "" && s.held[sid] {
			delete(paths, p)
		}
	}

	live := liveSessionIDs(s.claudeDir)
	loaded, manifestETag := s.loadMtimes(ctx)
	manifest := &mtimeBook{m: loaded}
	s.modes = s.loadModes(ctx, hasModes)
	defer func() { s.modes = nil }()
	homeDir := s.home
	repos := newRepoIndex(homeDir)
	repos.ready = func(dir string) bool { return s.folderGate(dir) == nil }

	var jobs []job
	var objectChecks []objectCheck
	symlinked := make(map[string]bool)

	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	progress := progressFrom(ctx)
	for i, np := range sorted {
		np := np
		if i%25 == 0 {
			progress(PhaseCompare, i, len(sorted))
		}
		lf := local[np]
		robj, hasRemote := remote[np]
		st, hasState := known[np]
		root, rel, _ := active(np)

		if lf == nil && hasRemote && root.kind != rootFolder && s.underSymlink(root, rel, symlinked) {
			// A symbolic link here (a dotfile kept elsewhere): never write
			// over it or through it.
			continue
		}
		if lf == nil && hasRemote {
			// On a case-insensitive volume another Mac's spelling of a folder
			// (~/Code vs ~/code) names a file the scan listed under this Mac's
			// spelling. Treat it as present so it merges instead of being
			// overwritten by a blind download.
			probe := &localFile{root: root, rel: rel, abs: filepath.Join(root.dir, filepath.FromSlash(rel))}
			if info, err := statLocal(probe); err == nil && (info.Mode().IsRegular() || (isSymlink(info) && root.kind == rootFolder)) {
				probe.info = info
				lf = probe
			}
		}

		var lhash string
		if lf != nil {
			if hasState && !st.LocalDeleted && st.Size == lf.info.Size() && st.ModTime.Equal(lf.info.ModTime()) {
				lhash = st.Hash // unchanged size and mtime: skip re-hashing
			} else if h, err := hashLocal(lf.abs, lf.info); err == nil {
				lhash = h
			} else if _, cloud := inICloud(err); cloud {
				// Its download was requested; decide once it is here.
				rm.add(&report.InICloud, np)
				continue
			} else {
				rm.fail(np, err)
				continue
			}
		}

		localChanged := lf != nil && (!hasState || st.LocalDeleted || lhash != st.Hash)
		remoteChanged := hasRemote && (!hasState || remoteDiffers(st, robj))
		mirror := !unionPath(np)
		isLive := false
		if id, ok := transcriptSessionID(np); ok {
			// A transcript written in the last few seconds may belong to a
			// Claude process that hasn't recorded itself yet.
			isLive = live[id] || (lf != nil && s.recentWrite > 0 && time.Since(lf.info.ModTime()) < s.recentWrite)
		}

		switch {
		case lf != nil && !hasRemote:
			synced := hasState && st.RemoteETag != "" && !st.LocalDeleted
			if mirror && synced && !localChanged {
				// Another Mac deleted it: keep this copy in the trash.
				lf := lf
				jobs = append(jobs, job{np, "trash", root, func() error {
					return s.reconcileTrash(np, lf, rm)
				}})
				continue
			}
			// New here, the remote copy vanished (union), or it changed here
			// after another Mac deleted it (an edit beats a delete): upload
			// create-only.
			restored := !mirror && hasState && st.RemoteETag != "" && !localChanged
			kind := "upload"
			if restored {
				kind = "restore"
			}
			looseRef := !hasState && root.kind == rootFolder && isGitBranchRef(np) && !s.repoSyncedBefore(np)
			jobs = append(jobs, job{np, kind, root, func() error {
				if looseRef {
					if settled, err := s.firstContactLooseRef(ctx, np, lf); settled || err != nil {
						if settled {
							rm.add(&report.Conflicts, np)
						}
						return err
					}
				}
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
				if mirror {
					// Deleted on this device: delete it everywhere, unless
					// another Mac changed it since (then its change wins).
					jobs = append(jobs, job{np, "delete", root, func() error {
						return s.reconcileRemoteDelete(ctx, np, robj, root, rel, rm, repos, manifest, homeDir)
					}})
					continue
				}
				switch {
				case root.kind == rootFolder && isGitBranchRef(np):
					// A branch deleted here (or packed by gc) comes back
					// while storage has it: another Mac's HEAD may name it,
					// and a packed copy of it is merged on the way in.
				case root.kind == rootFolder && gitLooseObject(np):
					// gc packed it, or pruned it: fetch it again only if
					// git can't find it here (a commit from another Mac may
					// build on it).
					objectChecks = append(objectChecks, objectCheck{np: np, rel: rel, root: root, obj: robj})
					continue
				default:
					// Deleted on this device; keep it deleted here only.
					if !st.LocalDeleted && !dryRun {
						s.state.MarkLocalDeleted(np, robj.ETag)
					}
					continue
				}
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
			jobs = append(jobs, job{np, kind, root, func() error {
				target := &localFile{root: root, rel: rel, abs: filepath.Join(root.dir, filepath.FromSlash(rel))}
				if root.kind == rootFolder {
					if packed, ok := shadowsPackedRef(np, target.abs); ok {
						// This Mac keeps the branch in packed-refs: write
						// that value as a loose ref and merge it like any
						// ref the two Macs both moved.
						if err := writeFolderContent(root.dir, target.abs, []byte(packed+"\n"), nil, 0644); err != nil {
							return err
						}
						info, err := os.Lstat(target.abs)
						if err != nil {
							return err
						}
						target.info = info
						return s.reconcileBoth(ctx, np, target, rm, repos, manifest, homeDir)
					}
				}
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
				// In sync; only a mode change may be waiting.
				if !dryRun {
					s.reconcileMode(np, lf, st)
				}
			case hasState && localChanged && !remoteChanged:
				ifMatch := st.RemoteETag
				if ifMatch == "" {
					ifMatch = robj.ETag // legacy state without ETags
				}
				jobs = append(jobs, job{np, "upload", root, func() error {
					return s.reconcileUpload(ctx, np, lf, ifMatch, rm, repos, manifest, homeDir)
				}})
			case hasState && !localChanged && remoteChanged:
				if isLive {
					rm.add(&report.Deferred, np)
					continue
				}
				expect := lf.info
				jobs = append(jobs, job{np, "download", root, func() error {
					return s.reconcileDownload(ctx, np, lf, robj, expect, rm, repos, manifest, homeDir)
				}})
			default:
				// Changed on both sides, or present on both before this device
				// ever synced it.
				if isLive {
					rm.add(&report.Deferred, np)
					continue
				}
				jobs = append(jobs, job{np, "merge", root, func() error {
					return s.reconcileBoth(ctx, np, lf, rm, repos, manifest, homeDir)
				}})
			}

		case lf == nil && !hasRemote:
			if hasState && !dryRun {
				s.state.RemoveFile(np)
			}
		}
	}

	for _, c := range s.missingObjects(objectChecks, dryRun) {
		c := c
		jobs = append(jobs, job{c.np, "download", c.root, func() error {
			target := &localFile{root: c.root, rel: c.rel, abs: filepath.Join(c.root.dir, filepath.FromSlash(c.rel))}
			return s.reconcileDownload(ctx, c.np, target, c.obj, nil, rm, repos, manifest, homeDir)
		}})
	}

	jobs, report.Warnings = s.guardMassDeletes(jobs, roots, known)
	report.Warnings = append(report.Warnings, lockWarnings...)
	report.Warnings = append(report.Warnings, s.tccWarnings()...)

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
			case "delete":
				report.Deleted = append(report.Deleted, j.npath)
			case "trash":
				report.Trashed = append(report.Trashed, j.npath)
			}
		}
		return report, nil
	}

	s.markFolders(ctx, roots, local, objects)

	// Run with a bounded worker pool: transcripts, settings and project
	// folders first, then git's refs and index, then sidebar records, so a
	// ref never lands on a device before the objects it points to and a
	// record never before its transcript and its folder.
	// Conflicts in a working tree run before git's files change: deciding
	// them asks git whether this Mac's copy is committed (chooseSide).
	var files, worktree, gitMeta, records []job
	for _, j := range jobs {
		switch {
		case strings.HasPrefix(j.npath, DesktopPrefix):
			records = append(records, j)
		case gitMetadata(j.npath):
			gitMeta = append(gitMeta, j)
		case j.kind == "merge" && j.root != nil && j.root.kind == rootFolder:
			worktree = append(worktree, j)
		default:
			files = append(files, j)
		}
	}
	// A first sync of everything can outlast the agent's time limit, or the
	// Mac can sleep: save progress as it goes, so the next pass picks up where
	// this one stopped instead of comparing every file again.
	stopSaving := s.saveEvery(progressSaveInterval)
	// Jobs that touch the same physical file (two remote spellings of one
	// path on a case-insensitive volume) run one at a time: the second then
	// sees the first's write and merges or defers instead of renaming over it.
	locks := &keyedLocks{m: make(map[string]*gosync.Mutex)}
	total := len(files) + len(worktree) + len(gitMeta) + len(records)
	var done atomic.Int64
	progress(PhaseTransfer, 0, total)
	for _, batch := range [][]job{files, worktree, gitMeta, records} {
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
				} else if errors.Is(err, errRemoteControlled) {
					rm.add(&report.RemoteControlled, j.npath)
				} else if _, cloud := inICloud(err); cloud {
					rm.add(&report.InICloud, j.npath)
				} else if err != nil {
					rm.fail(j.npath, err)
				}
				progress(PhaseTransfer, int(done.Add(1)), total)
			}(j)
		}
		wg.Wait()
	}
	stopSaving()
	s.saveHeld(stored)
	s.reindexRepos(roots)

	// Downloads in this pass may have delivered the new copy of a moved session.
	report.Moved = append(report.Moved, s.foldMovedSessions(liveSessionIDs(s.claudeDir))...)

	// Record what this pass did even when it ran out of time.
	metaCtx, cancelMeta := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancelMeta()
	if len(report.Uploaded)+len(report.Merged)+len(report.Restored) > 0 {
		if err := s.saveMtimes(metaCtx, manifest.snapshot(), manifestETag); err != nil {
			s.log("Warning: failed to update mtime manifest: %v", err)
		}
	}
	if err := s.saveModes(metaCtx, s.modes); err != nil {
		// Forget the modes this pass recorded, so the next pass offers them
		// again rather than taking the old manifest entries for a remote change.
		for _, np := range s.modes.pendingPaths() {
			s.state.setMode(np, 0)
		}
		s.log("Warning: failed to update file modes: %v", err)
	}
	if s.cfg.IsFolderSyncEnabled() {
		var folders []string
		nested := make(map[string][]string)
		before := s.readFolderList().Nested
		for _, r := range roots {
			if r.kind != rootFolder {
				continue
			}
			if r.deferred && !wasRoot[r.prefix] {
				continue // never scanned as its own folder yet: still new next pass
			}
			rel := strings.TrimSuffix(strings.TrimPrefix(r.prefix, FoldersPrefix), "/")
			folders = append(folders, rel)
			set := make(map[string]bool)
			for d := range r.nested {
				set[d] = true
			}
			if r.deferred { // not scanned this pass: keep what the last one found
				for _, d := range before[rel] {
					set[d] = true
				}
			}
			for d := range set {
				nested[rel] = append(nested[rel], d)
			}
			sort.Strings(nested[rel])
		}
		if err := s.saveFolderList(folders, nested, s.offFolders); err != nil {
			s.log("Warning: failed to record project folders: %v", err)
		}
	}

	now := time.Now()
	s.state.LastSync = now
	s.state.LastPull = now
	if len(report.Uploaded)+len(report.Merged)+len(report.Restored)+len(report.Deleted) > 0 {
		s.state.LastPush = now
	}
	if err := s.state.Save(); err != nil {
		return report, fmt.Errorf("failed to save state: %w", err)
	}
	return report, nil
}

// progressSaveInterval is how often a running pass saves its sync state.
const progressSaveInterval = time.Minute

// saveEvery saves the sync state every interval until the returned function
// is called (which waits for a save in progress).
func (s *Syncer) saveEvery(interval time.Duration) func() {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := s.state.Save(); err != nil {
					s.log("Warning: failed to save sync progress: %v", err)
				}
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// guardMassDeletes holds back this Mac's deletions in a root when one pass
// would delete more than half of the root's synced files, and at least
// massDeleteFloor of them: that is more often a folder that vanished or a bad
// checkout than a decision, and every other Mac would follow. Deletions
// recorded in an earlier pass (before deletes were mirrored) don't count as
// sudden.
//
// Trash moves (files another Mac deleted) aren't limited per root: that Mac
// passed its own guard, and the trash keeps every copy. But when more than
// half of everything this Mac synced vanished from remote storage or would go
// at once, transcripts included, the container was most likely wiped or
// replaced: then nothing is deleted or trashed (transcripts and history are
// still restored).
//
// Files inside .git are left out of every count, as git replaces loose
// objects with packs by the thousand.
func (s *Syncer) guardMassDeletes(jobs []job, roots []*syncRoot, known map[string]FileState) ([]job, []string) {
	if s.cfg.AllowMassDelete {
		return jobs, nil
	}
	deletes := make(map[*syncRoot]int)
	gone := 0
	for _, j := range jobs {
		if hasGitSegment(j.npath) {
			continue
		}
		switch j.kind {
		case "delete":
			gone++
			if !known[j.npath].LocalDeleted {
				deletes[j.root]++
			}
		case "trash", "restore":
			gone++
		}
	}
	if gone < massDeleteFloor {
		return jobs, nil
	}
	tracked := make(map[*syncRoot]int)
	allTracked := 0
	for np := range known {
		if hasGitSegment(np) {
			continue
		}
		if root, rel := s.rootFor(roots, np); root != nil && !root.deferred && root.inScope(rel) {
			allTracked++
			if !unionPath(np) {
				tracked[root]++
			}
		}
	}
	var warnings []string
	blocked := make(map[*syncRoot]bool)
	everything := 2*gone > allTracked
	if everything {
		warnings = append(warnings, fmt.Sprintf("%d of the %d files this Mac synced are missing from remote storage or would be deleted at once, so nothing was deleted or moved to the trash. If remote storage was replaced or emptied on purpose, or you meant to delete them, run 'stradl sync --allow-mass-delete'.", gone, allTracked))
	}
	for _, r := range roots {
		if n := deletes[r]; !everything && n >= massDeleteFloor && 2*n > tracked[r] {
			blocked[r] = true
			warnings = append(warnings, fmt.Sprintf("%s: %d of %d synced files would be deleted at once, so none were. If that is what you want, run 'stradl sync --allow-mass-delete'.", r.label, n, tracked[r]))
		}
	}
	if !everything && len(blocked) == 0 {
		return jobs, nil
	}
	kept := jobs[:0]
	for _, j := range jobs {
		if everything && (j.kind == "delete" || j.kind == "trash") {
			continue
		}
		if j.kind == "delete" && blocked[j.root] {
			continue
		}
		kept = append(kept, j)
	}
	return kept, warnings
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
// the version read when the backend reports one. On a streaming backend it
// reads through the stream, up to mergeMemoryLimit.
func (s *Syncer) fetch(ctx context.Context, npath, key string, repos *repoIndex, home string) ([]byte, string, error) {
	var (
		data []byte
		etag string
		err  error
	)
	if _, ok := s.streamer(); ok {
		if data, etag, err = s.fetchStream(ctx, key); err != nil {
			return nil, "", err
		}
	} else {
		var enc []byte
		if c, ok := s.conditional(); ok {
			enc, etag, err = c.DownloadWithETag(ctx, key)
		} else {
			enc, err = s.storage.Download(ctx, key)
		}
		if err != nil {
			return nil, "", err
		}
		data, err = s.encryptor.Decrypt(enc)
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
	}
	if strings.HasPrefix(npath, DesktopPrefix) {
		held := remoteControlled(data)
		data = s.paths.ResolveContent("record.json", data)
		data = finishDesktopDownload(data, home, repos, s.folderTracked)
		if held {
			return data, etag, errRemoteControlled
		}
	} else if IsPortableContentPath(npath) {
		data = s.paths.ResolveContent(npath, data)
	}
	return data, etag, nil
}

// put encodes plaintext for remote storage and uploads it, conditionally when
// the backend supports compare-and-swap. ifMatch "" means create-only.
func (s *Syncer) put(ctx context.Context, npath string, plain []byte, ifMatch string) (string, error) {
	if s.streams(npath, int64(len(plain))) {
		return s.putStream(ctx, npath, bytes.NewReader(plain), ifMatch)
	}
	data := plain
	if npath == "history.jsonl" {
		data = s.withoutOffSessions(data) // prompts of accounts that don't sync stay here
	}
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
		return "", fmt.Errorf("too large to sync with this storage provider (%d MB compressed; it accepts up to %d MB)", len(enc)>>20, storage.MaxDownloadSize>>20)
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

// deleteRemote deletes a remote object only if it is still at version etag.
// It returns an error wrapping storage.ErrPreconditionFailed when another
// device changed it since.
func (s *Syncer) deleteRemote(ctx context.Context, key, etag string) error {
	if c, ok := s.conditional(); ok && etag != "" {
		return c.DeleteIf(ctx, key, etag)
	}
	// No compare-and-swap: check the version, then delete (a narrow race).
	info, err := s.storage.Head(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if etag != "" && info != nil && info.ETag != "" && normETag(info.ETag) != normETag(etag) {
		return fmt.Errorf("%s: %w", key, storage.ErrPreconditionFailed)
	}
	return s.storage.Delete(ctx, key)
}

// defaultPerm is the mode a file at npath gets when the modes manifest has no
// entry for it: the root's default (owner-only under ~/.claude, whose files
// can hold secrets), or read-only for git objects, as git writes them (so a
// repository's thousands of objects need no entries).
func defaultPerm(npath string, root *syncRoot) os.FileMode {
	if root.kind == rootFolder && strings.Contains(npath, ".git/objects/") {
		return 0444
	}
	return root.perm
}

// permFor returns the mode a file at npath is written with: the modes
// manifest's entry, else the default.
func (s *Syncer) permFor(npath string, root *syncRoot) os.FileMode {
	if root == nil || root.kind == rootDesktop {
		return 0600
	}
	if m, ok := s.modes.get(s.engineRemoteKey(npath)); ok && m&0400 != 0 {
		return os.FileMode(m).Perm()
	}
	return defaultPerm(npath, root)
}

// noteMode records the mode of a file this pass uploads.
func (s *Syncer) noteMode(npath string, lf *localFile, info os.FileInfo) {
	if lf.root == nil || lf.root.kind == rootDesktop || isSymlink(info) {
		return
	}
	s.modes.note(s.engineRemoteKey(npath), npath, uint32(info.Mode().Perm()), uint32(defaultPerm(npath, lf.root)))
}

// reconcileMode settles the mode of a file whose content is in sync: a chmod
// here is recorded for the other Macs; one made on another Mac is applied.
// Before modes were tracked (st.Mode 0) the manifest wins where it has an
// entry, and this Mac's mode is recorded where it has none.
func (s *Syncer) reconcileMode(npath string, lf *localFile, st FileState) {
	if s.modes == nil || !s.modes.ok || lf.root.kind == rootDesktop || isSymlink(lf.info) {
		return // a chmod on a link would change its target
	}
	key := s.engineRemoteKey(npath)
	cur := uint32(lf.info.Mode().Perm())
	def := uint32(defaultPerm(npath, lf.root))
	m, has := s.modes.get(key)
	switch {
	case st.Mode != 0 && cur != st.Mode:
		s.modes.note(key, npath, cur, def)
		s.state.setMode(npath, cur)
	case has && m != cur && m&0400 != 0:
		if os.Chmod(lf.abs, os.FileMode(m).Perm()) == nil {
			s.state.setMode(npath, m)
		}
	case st.Mode == 0:
		s.modes.note(key, npath, cur, def)
		s.state.setMode(npath, cur)
	}
}

// writeContent writes downloaded content to lf's path with mode perm.
func (s *Syncer) writeContent(lf *localFile, data []byte, expect os.FileInfo, perm os.FileMode) error {
	if lf.root.kind == rootFolder {
		if !gitRefsReady(lf.abs, data) {
			return errLocalChanged // its commits haven't arrived yet: next pass
		}
		if err := writeFolderContent(lf.root.dir, lf.abs, data, expect, perm); err != nil {
			return err
		}
		s.wroteGitFile(lf.abs)
		return nil
	}
	return writeLocalPerm(lf.abs, data, expect, perm, s.claudeDir, lf.root.dir)
}

// restoreMtime gives a downloaded file the modification time it had on the
// device that wrote it (links keep their own: setting it would touch the
// target).
func (s *Syncer) restoreMtime(npath string, lf *localFile, manifest *mtimeBook) {
	if mt := s.clampMtime(npath, manifest.get(s.engineRemoteKey(npath))); !mt.IsZero() {
		if fi, err := os.Lstat(lf.abs); err == nil && !isSymlink(fi) {
			_ = os.Chtimes(lf.abs, mt, mt)
		}
	}
}

// --- reconcile actions --------------------------------------------------------

func (s *Syncer) reconcileUpload(ctx context.Context, npath string, lf *localFile, ifMatch string, rm *reportMu, repos *repoIndex, manifest *mtimeBook, home string) error {
	info, err := statLocal(lf) // stat before reading: a later append makes the next scan re-hash
	if err != nil {
		return err
	}
	if s.streams(npath, info.Size()) && !isSymlink(info) {
		etag, hash, err := s.uploadFileStream(ctx, npath, lf.abs, info, ifMatch)
		if errors.Is(err, storage.ErrPreconditionFailed) {
			return s.reconcileBoth(ctx, npath, lf, rm, repos, manifest, home)
		}
		if err != nil {
			return err
		}
		s.state.SetReconciled(npath, info, hash, etag)
		s.recordMtime(manifest, npath, info.ModTime())
		s.noteMode(npath, lf, info)
		rm.add(&rm.r.Uploaded, npath)
		return nil
	}
	data, err := readLocalContent(lf.abs, info)
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
		if errors.Is(ferr, errRemoteControlled) {
			if st := s.state.GetFile(npath); st != nil && st.LocalDeleted && !remoteControlled(data) {
				// Held back for Remote Control, and the app re-saved its
				// in-memory copy: keep it off the remote and the sidebar.
				if etag == "" {
					etag = ifMatch
				}
				return s.holdRemoteControlled(npath, lf, info, etag)
			}
			ferr = nil
		}
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
	s.noteMode(npath, lf, info)
	rm.add(&rm.r.Uploaded, npath)
	return nil
}

func (s *Syncer) reconcileDownload(ctx context.Context, npath string, lf *localFile, obj storage.ObjectInfo, expect os.FileInfo, rm *reportMu, repos *repoIndex, manifest *mtimeBook, home string) error {
	if s.streams(npath, obj.Size) {
		return s.reconcileDownloadStream(ctx, npath, lf, obj, expect, rm, repos, manifest, home)
	}
	data, etag, err := s.fetch(ctx, npath, obj.Key, repos, home)
	if errors.Is(err, errTooLargeForMemory) {
		return s.reconcileDownloadStream(ctx, npath, lf, obj, expect, rm, repos, manifest, home)
	}
	if etag == "" {
		etag = obj.ETag
	}
	if errors.Is(err, errRemoteControlled) {
		return s.holdRemoteControlled(npath, lf, expect, etag)
	}
	if err != nil {
		return err
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
	if err := s.writeContent(lf, data, expect, s.permFor(npath, lf.root)); err != nil {
		return err
	}
	s.restoreMtime(npath, lf, manifest)
	info, err := statLocal(lf)
	if err != nil {
		return err
	}
	s.state.SetReconciled(npath, info, sha256Hex(data), etag)
	rm.add(&rm.r.Downloaded, npath)
	return nil
}

// reconcileDownloadStream is reconcileDownload for a file too large to hold
// in memory: it lands in a temporary file beside the target first.
func (s *Syncer) reconcileDownloadStream(ctx context.Context, npath string, lf *localFile, obj storage.ObjectInfo, expect os.FileInfo, rm *reportMu, repos *repoIndex, manifest *mtimeBook, home string) error {
	if err := s.checkWritable(lf); err != nil {
		return err
	}
	perm := s.permFor(npath, lf.root)
	tmp, hash, size, etag, err := s.fetchToTemp(ctx, npath, obj.Key, filepath.Dir(lf.abs), perm)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if etag == "" {
		etag = obj.ETag
	}
	if s.transcriptLiveNow(npath) {
		return errLocalChanged
	}
	if expect != nil && IsMergeableJSONL(npath) {
		// Transcripts only grow (see reconcileDownload).
		if size < expect.Size() {
			return s.reconcileBoth(ctx, npath, lf, rm, repos, manifest, home)
		}
		if size <= mergeMemoryLimit && expect.Size() <= mergeMemoryLimit {
			remoteData, rerr := os.ReadFile(tmp)
			local, lerr := os.ReadFile(lf.abs)
			if rerr == nil && lerr == nil {
				if _, localHasMore := MergeJSONL(remoteData, local); localHasMore {
					return s.reconcileBoth(ctx, npath, lf, rm, repos, manifest, home)
				}
			}
		}
	}
	defer s.wroteGitFile(lf.abs)
	if err := commitTemp(tmp, lf.abs, expect, perm, s.claudeDir, lf.root.dir); err != nil {
		return err
	}
	s.restoreMtime(npath, lf, manifest)
	info, err := statLocal(lf)
	if err != nil {
		return err
	}
	s.state.SetReconciled(npath, info, hash, etag)
	rm.add(&rm.r.Downloaded, npath)
	return nil
}

// checkWritable refuses a write outside the sync roots or, in a project
// folder, through a symbolic link.
func (s *Syncer) checkWritable(lf *localFile) error {
	if err := checkInsideRoots(lf.abs, s.claudeDir, lf.root.dir); err != nil {
		return err
	}
	if lf.root.kind == rootFolder {
		return checkNoSymlinkParents(lf.root.dir, lf.abs)
	}
	return nil
}

// reconcileRemoteDelete deletes the remote copy of a file deleted on this
// device. When another Mac changed it since this one last synced it, the
// change wins and the file comes back here instead.
func (s *Syncer) reconcileRemoteDelete(ctx context.Context, npath string, obj storage.ObjectInfo, root *syncRoot, rel string, rm *reportMu, repos *repoIndex, manifest *mtimeBook, home string) error {
	err := s.deleteRemote(ctx, obj.Key, obj.ETag)
	if errors.Is(err, storage.ErrPreconditionFailed) {
		target := &localFile{root: root, rel: rel, abs: filepath.Join(root.dir, filepath.FromSlash(rel))}
		return s.reconcileDownload(ctx, npath, target, obj, nil, rm, repos, manifest, home)
	}
	if err != nil {
		return err
	}
	s.state.RemoveFile(npath)
	rm.add(&rm.r.Deleted, npath)
	return nil
}

// reconcileTrash moves a file another Mac deleted into the trash, unless it
// changed since this pass scanned it.
func (s *Syncer) reconcileTrash(npath string, lf *localFile, rm *reportMu) error {
	if _, err := s.moveToTrash(lf.abs, lf.info, lf.root.dir); err != nil {
		return err
	}
	s.state.RemoveFile(npath)
	rm.add(&rm.r.Trashed, npath)
	return nil
}

// reconcileBoth resolves a file that changed on both sides (or exists on both
// sides with no shared history), retrying when another device wins a race.
func (s *Syncer) reconcileBoth(ctx context.Context, npath string, lf *localFile, rm *reportMu, repos *repoIndex, manifest *mtimeBook, home string) error {
	if _, ok := s.streamer(); ok && !strings.HasPrefix(npath, DesktopPrefix) {
		if info, err := statLocal(lf); err == nil && !isSymlink(info) {
			limit := streamThreshold
			if IsMergeableJSONL(npath) {
				limit = mergeMemoryLimit
			}
			if info.Size() > limit {
				return s.reconcileBothStream(ctx, npath, lf, rm, manifest)
			}
		}
	}
	key := s.engineRemoteKey(npath)
	for attempt := 0; attempt < maxRaceRetries; attempt++ {
		info, err := statLocal(lf)
		if err != nil {
			return err
		}
		localData, err := readLocalContent(lf.abs, info)
		if err != nil {
			return err
		}
		remoteData, etag, err := s.fetch(ctx, npath, key, repos, home)
		if errors.Is(err, errRemoteControlled) {
			if !remoteControlled(localData) {
				// Another Mac serves it over Remote Control: never upload
				// over its live record, nor show it here twice.
				return s.holdRemoteControlled(npath, lf, info, etag)
			}
			err = nil // both serve it: merge as usual
		}
		if errors.Is(err, errTooLargeForMemory) {
			return s.reconcileBothStream(ctx, npath, lf, rm, manifest)
		}
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
			s.noteMode(npath, lf, info)
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
			// Storage needs only what this Mac would send (the prompts of
			// accounts that don't sync stay here).
			_, gotLocal := MergeJSONL(remoteData, s.sharedJSONL(npath, localData))
			result, writeHere, uploadThere = merged, gotRemote, gotLocal
		case strings.HasPrefix(npath, DesktopPrefix):
			if PreferRemoteDesktopMeta(localData, remoteData) {
				result, writeHere = remoteData, true
			} else {
				result, uploadThere = localData, true
			}
		default:
			// Non-mergeable: one copy keeps the name, the other is kept.
			if isGitBranchRef(npath) && remoteData != nil && !gitRefsReady(lf.abs, remoteData) {
				// The other Mac's commit hasn't arrived yet: settle the
				// branch once it has, so whichever side loses stays
				// reachable on a stradl/ branch.
				return errLocalChanged
			}
			side := s.chooseSide(npath, lf, info, localData, remoteData, manifest.get(key))
			loser, origin := remoteData, ConflictFromOtherMac
			if side.local {
				result, uploadThere = localData, true
			} else {
				result, writeHere = remoteData, true
				loser, origin = localData, ConflictFromThisMac
				if _, inside, _ := gitDirSplit(npath); inside == "packed-refs" && s.state.GetFile(npath) == nil {
					// First contact: keep the refs only this Mac has.
					result = mergePackedRefs(remoteData, localData)
					uploadThere = !bytes.Equal(result, remoteData)
				}
			}
			if side.keep {
				conflict = true
				var err error
				if side.share {
					err = s.shareCopy(lf, info, "", loser)
				} else {
					err = s.keepLoser(npath, lf.abs, loser, origin, side.aside)
				}
				if err != nil {
					return err
				}
				s.keepLosingBranch(ctx, npath, lf.abs, result, loser, !side.local)
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
			s.noteMode(npath, lf, info)
		}
		if writeHere {
			if err := s.writeContent(lf, result, info, s.permFor(npath, lf.root)); err != nil {
				return err
			}
		}
		final, err := statLocal(lf)
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

// reconcileBothStream resolves a file changed on both sides that is too large
// to hold in memory: the newest copy wins and the other is kept as a conflict
// copy, transcripts included (above mergeMemoryLimit they aren't merged).
func (s *Syncer) reconcileBothStream(ctx context.Context, npath string, lf *localFile, rm *reportMu, manifest *mtimeBook) error {
	if err := s.checkWritable(lf); err != nil {
		return err
	}
	key := s.engineRemoteKey(npath)
	perm := s.permFor(npath, lf.root)
	for attempt := 0; attempt < maxRaceRetries; attempt++ {
		info, err := statLocal(lf)
		if err != nil {
			return err
		}
		tmp, rhash, _, etag, err := s.fetchToTemp(ctx, npath, key, filepath.Dir(lf.abs), perm)
		if errors.Is(err, storage.ErrNotFound) {
			etag, hash, err := s.uploadLocal(ctx, npath, lf.abs, info, "")
			if errors.Is(err, storage.ErrPreconditionFailed) {
				continue
			}
			if err != nil {
				return err
			}
			s.state.SetReconciled(npath, info, hash, etag)
			s.noteMode(npath, lf, info)
			rm.add(&rm.r.Uploaded, npath)
			return nil
		}
		if err != nil {
			return err
		}
		done, err := s.settleStreamed(ctx, npath, lf, info, tmp, rhash, etag, perm, rm, manifest)
		_ = os.Remove(tmp)
		if err != nil || done {
			return err
		}
	}
	return fmt.Errorf("gave up after %d attempts: other devices keep changing it; will retry next sync", maxRaceRetries)
}

// settleStreamed finishes reconcileBothStream for one downloaded remote copy.
// done is false when the upload lost a race and the caller should try again.
func (s *Syncer) settleStreamed(ctx context.Context, npath string, lf *localFile, info os.FileInfo, tmp, rhash, etag string, perm os.FileMode, rm *reportMu, manifest *mtimeBook) (done bool, err error) {
	lhash, err := hashLocal(lf.abs, info)
	if err != nil {
		return true, err
	}
	if lhash == rhash {
		s.state.SetReconciled(npath, info, lhash, etag)
		return true, nil
	}
	key := s.engineRemoteKey(npath)
	side := s.chooseSide(npath, lf, info, nil, nil, manifest.get(key))
	if side.local {
		// Keep the other Mac's copy before replacing it in storage.
		if err := s.keepConflictFile(npath, lf.abs, tmp, ConflictFromOtherMac, true, false); err != nil {
			return true, err
		}
		newETag, hash, err := s.uploadLocal(ctx, npath, lf.abs, info, etag)
		if errors.Is(err, storage.ErrPreconditionFailed) {
			return false, nil
		}
		if err != nil {
			return true, err
		}
		s.recordMtime(manifest, npath, info.ModTime())
		s.noteMode(npath, lf, info)
		s.state.SetReconciled(npath, info, hash, newETag)
	} else {
		if s.transcriptLiveNow(npath) {
			return true, errLocalChanged
		}
		switch {
		case isSymlink(info):
			data, err := readLocalContent(lf.abs, info)
			if err == nil {
				err = s.keepLoser(npath, lf.abs, data, ConflictFromThisMac, side.aside)
			}
			if err != nil {
				return true, err
			}
		case side.share:
			if err := s.shareCopy(lf, info, lf.abs, nil); err != nil {
				return true, err
			}
		default:
			if err := s.keepConflictFile(npath, lf.abs, lf.abs, ConflictFromThisMac, false, side.aside); err != nil {
				return true, err
			}
		}
		if err := commitTemp(tmp, lf.abs, info, perm, s.claudeDir, lf.root.dir); err != nil {
			return true, err
		}
		final, err := statLocal(lf)
		if err != nil {
			return true, err
		}
		s.state.SetReconciled(npath, final, rhash, etag)
	}
	rm.add(&rm.r.Conflicts, npath)
	rm.add(&rm.r.Merged, npath)
	return true, nil
}

// uploadLocal uploads a local file of any size (streaming it) or a symbolic
// link, returning the new ETag and the hash of what was sent.
func (s *Syncer) uploadLocal(ctx context.Context, npath, abs string, info os.FileInfo, ifMatch string) (etag, hash string, err error) {
	if !isSymlink(info) {
		return s.uploadFileStream(ctx, npath, abs, info, ifMatch)
	}
	data, err := readLocalContent(abs, info)
	if err != nil {
		return "", "", err
	}
	etag, err = s.put(ctx, npath, data, ifMatch)
	return etag, sha256Hex(data), err
}

// markFolders uploads the marker of each project folder that has files here
// and none in remote storage yet, so the other Macs learn the folder.
func (s *Syncer) markFolders(ctx context.Context, roots []*syncRoot, local map[string]*localFile, objects []storage.ObjectInfo) {
	present := make(map[string]bool)
	for _, rel := range remoteFolderMarkers(objects) {
		present[rel] = true
	}
	hasFiles := make(map[*syncRoot]bool)
	for _, lf := range local {
		hasFiles[lf.root] = true
	}
	for _, r := range roots {
		if r.kind != rootFolder || r.deferred || !hasFiles[r] {
			continue
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(r.prefix, FoldersPrefix), "/")
		if present[rel] {
			continue
		}
		if _, err := s.put(ctx, FoldersPrefix+rel+"/"+folderMarker, []byte("{}\n"), ""); err != nil && !errors.Is(err, storage.ErrPreconditionFailed) {
			s.log("Warning: failed to mark folder %s: %v", r.label, err)
		}
	}
}

// --- local file helpers -------------------------------------------------------

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// errLocalChanged aborts a write because the local file changed after it was
// scanned; the next sync picks the change up.
var errLocalChanged = errors.New("local file changed during sync; will retry")

// checkInsideRoots refuses a path outside every sync root.
func checkInsideRoots(abs string, roots ...string) error {
	clean := filepath.Clean(abs)
	for _, r := range roots {
		if r != "" && strings.HasPrefix(clean, filepath.Clean(r)+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("refusing to write outside the sync roots: %s", abs)
}

// dirPermFor is the mode of folders created for a file with mode perm:
// readable by others when the file is.
func dirPermFor(perm os.FileMode) os.FileMode {
	if perm&0044 != 0 {
		return 0755
	}
	return 0700
}

// writeLocal replaces abs with data without ever exposing a half-written
// file. When expect is set and the file no longer matches it, the write is
// abandoned. When data merely extends the current file (the common case for a
// transcript another device continued) only the new tail is appended, which
// is safe for readers tailing the file.
func writeLocal(abs string, data []byte, expect os.FileInfo, roots ...string) error {
	return writeLocalPerm(abs, data, expect, 0600, roots...)
}

// writeLocalPerm is writeLocal leaving the file with mode perm.
// overwrite, passed as expect to writeLocalPerm, replaces whatever is at the
// path. A nil expect means the file must not exist yet: a download of a file
// new to this Mac never replaces one created here while the pass ran.
var overwrite os.FileInfo = overwriteInfo{}

type overwriteInfo struct{ os.FileInfo }

func writeLocalPerm(abs string, data []byte, expect os.FileInfo, perm os.FileMode, roots ...string) error {
	if err := checkInsideRoots(abs, roots...); err != nil {
		return err
	}
	mustBeNew := expect == nil
	if expect == overwrite {
		expect = nil
	}
	if _, err := os.Lstat(abs); err == nil && mustBeNew {
		return errLocalChanged
	}
	if err := os.MkdirAll(filepath.Dir(abs), dirPermFor(perm)); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Appending through a symbolic link would write to its target; a link is
	// replaced instead.
	lcur, lerr := os.Lstat(abs)
	link := lerr == nil && isSymlink(lcur)
	if cur, err := os.Stat(abs); err == nil {
		if expect != nil && (cur.Size() != expect.Size() || !cur.ModTime().Equal(expect.ModTime())) {
			return errLocalChanged
		}
		if !link && cur.Size() > 0 && cur.Size() < int64(len(data)) {
			if existing, err := os.ReadFile(abs); err == nil && bytes.HasPrefix(data, existing) {
				f, err := os.OpenFile(abs, os.O_WRONLY|os.O_APPEND, 0600)
				if err == nil {
					_, werr := f.Write(data[len(existing):])
					cerr := f.Close()
					if werr == nil && cerr == nil {
						if cur.Mode().Perm() != perm {
							_ = os.Chmod(abs, perm)
						}
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
	if err := tmp.Chmod(perm); err != nil {
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
	if mustBeNew {
		// Link fails if a file appeared at abs since the check above.
		if err := os.Link(tmpPath, abs); err != nil {
			if os.IsExist(err) {
				return errLocalChanged
			}
			return err
		}
		return nil
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
	return os.WriteFile(freeName(p), data, 0600)
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

// recordMtime notes a file's modification time in the shared manifest. Files
// inside .git are left out: git doesn't care about their times, objects never
// conflict (their names are their content), and a repository holds thousands,
// which every sync would download again. A .git file changed on two Macs
// keeps this Mac's copy (the other goes to ~/.stradl/conflicts).
func (s *Syncer) recordMtime(manifest *mtimeBook, npath string, t time.Time) {
	if hasGitSegment(npath) {
		return
	}
	manifest.mu.Lock()
	manifest.m[s.engineRemoteKey(npath)] = t
	manifest.mu.Unlock()
}

func (s *Syncer) loadMtimes(ctx context.Context) (map[string]time.Time, string) {
	m := make(map[string]time.Time)
	data, etag, err := s.downloadMeta(ctx, MtimeManifestKey)
	if err != nil {
		return m, ""
	}
	_ = json.Unmarshal(data, &m)
	return m, etag
}

// downloadMeta reads and decrypts one of the shared manifests.
func (s *Syncer) downloadMeta(ctx context.Context, key string) ([]byte, string, error) {
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
		return nil, "", err
	}
	if isGzipped(data) {
		if data, err = gzipDecompress(data); err != nil {
			return nil, "", err
		}
	}
	return data, etag, nil
}

// uploadMeta encrypts and writes one of the shared manifests, conditionally
// on etag when the backend supports compare-and-swap.
func (s *Syncer) uploadMeta(ctx context.Context, key string, v any, etag string) error {
	data, err := json.Marshal(v)
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
		return s.storage.Upload(ctx, key, enc)
	}
	_, err = c.UploadIf(ctx, key, enc, etag)
	return err
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
		if err := s.uploadMeta(ctx, MtimeManifestKey, mine, etag); errors.Is(err, storage.ErrPreconditionFailed) {
			continue
		} else {
			return err
		}
	}
	return fmt.Errorf("manifest kept changing; will retry next sync")
}

// --- modes manifest ------------------------------------------------------------

// modeBook is the shared modes manifest for one pass. Entries are written
// only for modes that differ from what the manifest (or, lacking an entry,
// the root's default) already says, and once written stay explicit, so a
// chmod back to the default reaches the other Macs too.
type modeBook struct {
	mu      gosync.Mutex
	ok      bool              // loaded, or absent; false (unreadable) leaves modes alone this pass
	m       map[string]uint32 // remote key -> permission bits, as loaded
	etag    string
	pending map[string]uint32 // this pass's changes
	npaths  map[string]string // remote key -> sync path, for pending entries
}

func (b *modeBook) get(key string) (uint32, bool) {
	if b == nil {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if v, ok := b.pending[key]; ok {
		return v, true
	}
	v, ok := b.m[key]
	return v, ok
}

// note records perm for key unless the manifest already implies it.
func (b *modeBook) note(key, npath string, perm, def uint32) {
	if b == nil || !b.ok {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cur, has := b.pending[key]
	if !has {
		cur, has = b.m[key]
	}
	if (has && cur == perm) || (!has && perm == def) {
		return
	}
	b.pending[key] = perm
	b.npaths[key] = npath
}

func (b *modeBook) pendingPaths() []string {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.npaths))
	for _, np := range b.npaths {
		out = append(out, np)
	}
	return out
}

// loadModes reads the modes manifest when the listing shows one.
func (s *Syncer) loadModes(ctx context.Context, present bool) *modeBook {
	b := &modeBook{m: make(map[string]uint32), pending: make(map[string]uint32), npaths: make(map[string]string)}
	if !present {
		b.ok = true
		return b
	}
	data, etag, err := s.downloadMeta(ctx, ModesManifestKey)
	if err != nil {
		b.ok = errors.Is(err, storage.ErrNotFound)
		return b
	}
	if json.Unmarshal(data, &b.m) != nil {
		b.m = make(map[string]uint32)
		return b
	}
	b.ok, b.etag = true, etag
	return b
}

// saveModes merges this pass's mode changes into the shared manifest with
// compare-and-swap.
func (s *Syncer) saveModes(ctx context.Context, b *modeBook) error {
	if b == nil || !b.ok || len(b.pending) == 0 {
		return nil
	}
	theirs, etag := b.m, b.etag
	for attempt := 0; attempt < maxRaceRetries; attempt++ {
		if attempt > 0 {
			data, newETag, err := s.downloadMeta(ctx, ModesManifestKey)
			theirs, etag = make(map[string]uint32), newETag
			switch {
			case errors.Is(err, storage.ErrNotFound):
				etag = ""
			case err != nil:
				return err
			case json.Unmarshal(data, &theirs) != nil:
				return fmt.Errorf("modes manifest is unreadable")
			}
		}
		merged := make(map[string]uint32, len(theirs)+len(b.pending))
		for k, v := range theirs {
			merged[k] = v
		}
		for k, v := range b.pending {
			merged[k] = v
		}
		if err := s.uploadMeta(ctx, ModesManifestKey, merged, etag); errors.Is(err, storage.ErrPreconditionFailed) {
			continue
		} else {
			return err
		}
	}
	return fmt.Errorf("modes manifest kept changing; will retry next sync")
}

// underSymlink reports whether rel inside root is a symbolic link, or lies
// below one, on this Mac. cache remembers directories already checked.
func (s *Syncer) underSymlink(root *syncRoot, rel string, cache map[string]bool) bool {
	parts := strings.Split(rel, "/")
	cur := root.dir
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		last := i == len(parts)-1
		if v, ok := cache[cur]; ok && !last {
			if v {
				return true
			}
			continue
		}
		fi, err := os.Lstat(cur)
		link := err == nil && isSymlink(fi)
		if !last {
			cache[cur] = link
		}
		if link {
			return true
		}
		if err != nil {
			return false // missing from here down
		}
	}
	return false
}
