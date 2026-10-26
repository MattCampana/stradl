package sync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	gosync "sync"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/storage"
)

// FoldersPrefix namespaces project folders in the sync namespace and in remote
// storage: _folders/<folder, relative to home>/<path inside the folder>. A key
// is simply the file's path relative to home, whichever folder it was synced
// as part of, so two Macs that draw folder boundaries differently (one syncs
// ~/code, another only ~/code/app) still agree on every file. Folder files are
// never rewritten: what lands on another Mac is byte-identical.
const FoldersPrefix = "_folders/"

// folderMarker names the empty blob each project folder carries in remote
// storage (_folders/<folder>/.stradl-folder.age), so a Mac that has never seen
// the folder learns where it starts and creates it. Local files whose names
// start with ".stradl-" are never synced, so it can't collide with a real file.
const folderMarker = ".stradl-folder"

// scratchWorkspaces is the one place under ~/Library project folders may live:
// the desktop app's scratch folders for sessions started without a folder.
const scratchWorkspaces = "Library/Application Support/Claude/scratch-workspaces"

// errGitBusy defers a project folder while git holds a lock file in it: a
// half-written index or ref must never reach another Mac, and a checkout or
// commit in progress here must not have files replaced under it.
var errGitBusy = errors.New("git is working in this folder")

// staleGitLock is how old a git lock file gets before the sync warns that git
// probably crashed and left it behind.
const staleGitLock = 15 * time.Minute

// gitBusyError names the lock file that defers a folder.
type gitBusyError struct {
	lock  string // path inside the folder
	since time.Time
}

func (e *gitBusyError) Error() string { return errGitBusy.Error() + " (" + e.lock + ")" }
func (e *gitBusyError) Unwrap() error { return errGitBusy }

// folderSourceLimit bounds how much of a transcript is read looking for its
// first working folder.
const folderSourceLimit = 4 << 20

var cwdFieldRe = regexp.MustCompile(`"cwd"\s*:\s*("(?:[^"\\]|\\.)*")`)

// homeRel returns abs relative to home as a slash path, when abs lies strictly
// inside home.
func homeRel(home, abs string) (string, bool) {
	if home == "" || !filepath.IsAbs(abs) {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(abs))
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// hasGitSegment reports whether a slash path lies inside a .git directory.
func hasGitSegment(p string) bool {
	return p == ".git" || strings.HasPrefix(p, ".git/") || strings.Contains(p, "/.git/") || strings.HasSuffix(p, "/.git")
}

// stateDir is where this device keeps state, trash and conflict copies.
func (s *Syncer) stateDir() string {
	if s.cfg.StateDirOverride != "" {
		return s.cfg.StateDirOverride
	}
	return config.ConfigDirPath()
}

// folderAllowed reports whether a home-relative folder may be a sync root.
// Home itself never is, and neither is anything inside Stradl's or Claude's
// own folders, the Trash, or ~/Library (where iCloud Drive and other cloud
// drives live), except the desktop app's scratch folders.
func (s *Syncer) folderAllowed(rel string) bool {
	if !isCanonicalRel(rel) {
		return false
	}
	first, _, _ := strings.Cut(rel, "/")
	switch first {
	case ".claude", ".stradl", ".claude-sync", ".Trash":
		return false
	case "Library":
		if rel != scratchWorkspaces && !strings.HasPrefix(rel, scratchWorkspaces+"/") {
			return false
		}
	}
	abs := filepath.Join(s.home, filepath.FromSlash(rel))
	for _, own := range []string{s.claudeDir, s.stateDir(), s.cfg.EffectiveDesktopDir()} {
		if own != "" && (isWithin(own, abs) || isWithin(abs, own)) {
			return false
		}
	}
	return true
}

// canonicalFolder turns a working folder into the project folder that syncs
// it, relative to home: the root of the git repository it lies in, else the
// folder itself. It returns "" when the folder can't be synced.
func (s *Syncer) canonicalFolder(abs string) string {
	if s.home == "" || !filepath.IsAbs(abs) {
		return ""
	}
	abs = filepath.Clean(abs)
	rel, ok := homeRel(s.home, abs)
	if !ok || !s.folderAllowed(rel) {
		return ""
	}
	// The nearest enclosing repository below home wins; a dotfiles repository
	// at home itself never swallows every folder.
	for dir := abs; dir != filepath.Clean(s.home) && isWithin(s.home, dir); dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			if r, ok := homeRel(s.home, dir); ok && s.folderAllowed(r) {
				return r
			}
			break
		}
	}
	return rel
}

// discoverFolders returns the project folders to sync, relative to home: the
// working folders of desktop sidebar sessions and of transcripts, the
// configured extra folders, and folders other Macs sync (their remote
// markers). Each is canonicalized (see canonicalFolder); spellings of one
// folder that differ only in case collapse to one, preferring the spelling
// already in remote storage; and a folder inside another is covered by the
// outer one.
func (s *Syncer) discoverFolders(objects []storage.ObjectInfo, useCache bool) []string {
	var candidates []string
	seen := make(map[string]bool) // thousands of transcripts share a few folders
	add := func(abs string) {
		if abs == "" || seen[abs] {
			return
		}
		seen[abs] = true
		if rel := s.canonicalFolder(abs); rel != "" {
			candidates = append(candidates, rel)
		}
	}
	for _, cwd := range s.desktopCwds() {
		add(cwd)
	}
	for _, cwd := range s.transcriptCwds(useCache) {
		add(cwd)
	}
	for _, p := range s.cfg.ExtraFolders {
		add(p)
	}
	for _, rel := range s.knownFolderList() {
		add(filepath.Join(s.home, filepath.FromSlash(rel)))
	}
	remote := make(map[string]bool)
	for _, rel := range remoteFolderMarkers(objects) {
		if c := s.canonicalFolder(filepath.Join(s.home, filepath.FromSlash(rel))); c != "" {
			remote[c] = true
			candidates = append(candidates, c)
		}
	}
	return chooseFolders(candidates, remote, homeCaseInsensitive(s.home))
}

// chooseFolders collapses case variants (on a case-insensitive volume) and
// nested folders. Among spellings of one folder, one already in remote storage
// wins, then the first in sort order, so every Mac picks the same spelling.
func chooseFolders(candidates []string, remote map[string]bool, foldCase bool) []string {
	fold := func(p string) string {
		if foldCase {
			return strings.ToLower(p)
		}
		return p
	}
	best := make(map[string]string)
	for _, c := range candidates {
		k := fold(c)
		cur, ok := best[k]
		switch {
		case !ok:
			best[k] = c
		case remote[c] != remote[cur]:
			if remote[c] {
				best[k] = c
			}
		case c < cur:
			best[k] = c
		}
	}
	keys := make([]string, 0, len(best))
	for k := range best {
		keys = append(keys, k)
	}
	// Shallowest first, so an outer folder is kept before anything inside it.
	sort.Slice(keys, func(i, j int) bool {
		di, dj := strings.Count(keys[i], "/"), strings.Count(keys[j], "/")
		if di != dj {
			return di < dj
		}
		return keys[i] < keys[j]
	})
	kept := make(map[string]bool)
	var out []string
	for _, k := range keys {
		covered := false
		for i := 0; i < len(k) && !covered; i++ {
			covered = k[i] == '/' && kept[k[:i]]
		}
		if covered {
			continue
		}
		kept[k] = true
		out = append(out, best[k])
	}
	sort.Strings(out)
	return out
}

// remoteFolderMarkers lists the folders other Macs sync, from their markers.
func remoteFolderMarkers(objects []storage.ObjectInfo) []string {
	suffix := "/" + folderMarker + ".age"
	var out []string
	for _, o := range objects {
		if !strings.HasPrefix(o.Key, FoldersPrefix) || !strings.HasSuffix(o.Key, suffix) {
			continue
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(o.Key, FoldersPrefix), suffix)
		if isCanonicalRel(rel) {
			out = append(out, rel)
		}
	}
	return out
}

// desktopCwds returns the working folders of this Mac's desktop sidebar
// sessions (cwd and originCwd of every record).
func (s *Syncer) desktopCwds() []string {
	dir := s.cfg.EffectiveDesktopDir()
	if dir == "" {
		return nil
	}
	var out []string
	matches, _ := filepath.Glob(filepath.Join(dir, "*", "*", "local_*.json"))
	for _, m := range matches {
		rel, err := filepath.Rel(dir, m)
		if err != nil || !isDesktopRecord(filepath.ToSlash(rel)) {
			continue
		}
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var rec struct {
			Cwd       string `json:"cwd"`
			OriginCwd string `json:"originCwd"`
		}
		if json.Unmarshal(data, &rec) != nil {
			continue
		}
		out = append(out, rec.Cwd, rec.OriginCwd)
	}
	return out
}

// transcriptCwds returns the first working folder of every top-level
// transcript. Results are cached in the sync state (useCache) and, for the
// background agent's probes, in memory, so each transcript is read once.
func (s *Syncer) transcriptCwds(useCache bool) []string {
	projects := filepath.Join(s.claudeDir, "projects")
	dirs, err := os.ReadDir(projects)
	if err != nil {
		return nil
	}
	present := make(map[string]bool)
	var out []string
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(projects, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if !f.Type().IsRegular() || !strings.HasSuffix(name, ".jsonl") || isDeviceLocalFile(name) {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			np := "projects/" + d.Name() + "/" + name
			present[np] = true
			var e cwdEntry
			var ok bool
			if useCache && s.state != nil {
				e, ok = s.state.cachedCwd(np)
			} else {
				e, ok = memCwds.get(np)
			}
			if !ok || (e.Cwd == "" && e.Size != info.Size()) {
				e = cwdEntry{Size: info.Size(), Cwd: firstCwd(filepath.Join(projects, d.Name(), name))}
				if useCache && s.state != nil {
					s.state.setCachedCwd(np, e)
				} else {
					memCwds.set(np, e)
				}
			}
			if e.Cwd != "" {
				out = append(out, e.Cwd)
			}
		}
	}
	if useCache && s.state != nil {
		s.state.pruneCwds(present)
	}
	return out
}

// memCwds is transcriptCwds' cache for syncers without state (probes).
var memCwds = &cwdCache{m: make(map[string]cwdEntry)}

type cwdCache struct {
	mu gosync.Mutex
	m  map[string]cwdEntry
}

func (c *cwdCache) get(np string) (cwdEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[np]
	return e, ok
}

func (c *cwdCache) set(np string, e cwdEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[np] = e
}

// firstCwd returns the first "cwd" recorded in a transcript, reading at most
// folderSourceLimit bytes.
func firstCwd(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 256<<10)
	var window []byte
	read := 0
	for read < folderSourceLimit {
		n, err := f.Read(buf)
		read += n
		window = append(window, buf[:n]...)
		if m := cwdFieldRe.FindSubmatch(window); m != nil {
			var cwd string
			if json.Unmarshal(m[1], &cwd) == nil {
				return cwd
			}
			return ""
		}
		if err != nil {
			return ""
		}
		// Keep a tail in case the field straddles two reads.
		if len(window) > 64<<10 {
			window = append([]byte(nil), window[len(window)-64<<10:]...)
		}
	}
	return ""
}

// folderRoot builds the sync root for a home-relative project folder.
func (s *Syncer) folderRoot(rel string) *syncRoot {
	return &syncRoot{
		dir:     filepath.Join(s.home, filepath.FromSlash(rel)),
		prefix:  FoldersPrefix + rel + "/",
		paths:   []string{"."},
		include: s.folderInclude,
		skipDir: s.folderSkipDir,
		kind:    rootFolder,
		label:   "~/" + rel,
		perm:    0644,
	}
}

// folderInclude reports whether a file inside a project folder syncs.
// Everything does, .git included, except build artifacts (unless
// sync_build_artifacts is on), folder_exclude matches, git's lock files,
// conflict copies and Stradl's own temporary files.
func (s *Syncer) folderInclude(rel string) bool {
	base := path.Base(rel)
	switch {
	case strings.HasPrefix(base, ".stradl-"), strings.Contains(base, ".conflict."):
		return false
	case strings.HasSuffix(base, ".lock") && hasGitSegment(path.Dir(rel)):
		return false
	case gitPerMac(rel):
		return false
	}
	return !s.cfg.IsBuildArtifact(rel) && !s.cfg.IsFolderExcluded(rel)
}

// folderSkipDir reports directories inside a project folder that are left out
// entirely.
func (s *Syncer) folderSkipDir(rel string) bool {
	return s.cfg.IsBuildArtifact(rel) || s.cfg.IsFolderExcluded(rel) || gitPerMac(rel)
}

// scanFolder lists a project folder's files. It returns errGitBusy when git
// holds a lock file anywhere under a .git directory, and any walk error
// (an unreadable subfolder): either way the folder sits this pass out, since
// files missing from a partial scan would look deleted.
func (s *Syncer) scanFolder(root *syncRoot) (map[string]os.FileInfo, error) {
	files := make(map[string]os.FileInfo)
	if _, err := os.Stat(root.dir); os.IsNotExist(err) {
		return files, errFolderMissing
	}
	own := []string{filepath.Clean(s.claudeDir), filepath.Clean(s.stateDir())}
	err := filepath.WalkDir(root.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root.dir, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			for _, o := range own {
				if filepath.Clean(p) == o {
					return filepath.SkipDir
				}
			}
			if root.skipDir != nil && root.skipDir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".lock") && hasGitSegment(path.Dir(rel)) {
			busy := &gitBusyError{lock: rel}
			if info, err := d.Info(); err == nil {
				busy.since = info.ModTime()
			}
			return busy
		}
		if (!d.Type().IsRegular() && d.Type()&fs.ModeSymlink == 0) || !root.inScope(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.Name() == ".git" && worktreeLink(p) {
			return nil
		}
		files[rel] = info
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// errFolderMissing reports a project folder that doesn't exist on this Mac.
var errFolderMissing = errors.New("folder is missing on this Mac")

// worktreeLink reports a .git file (a linked worktree or submodule checkout)
// that points at its repository by an absolute path: that path names this
// Mac's home, so on another Mac it would point nowhere, or into a folder of
// the same name there. A submodule's relative pointer syncs.
func worktreeLink(p string) bool {
	data, err := os.ReadFile(p)
	if err != nil || len(data) > 4096 {
		return false
	}
	dir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	return ok && filepath.IsAbs(strings.TrimSpace(dir))
}

// symlinkPrefix starts the stored content of a symbolic link inside a project
// folder; the link's target follows. Git repositories track links like files,
// so a missing one would show as deleted on the other Mac. Only project
// folders store links (the other roots skip them), and only clients that sync
// project folders read these keys.
const symlinkPrefix = "stradl-symlink v1\n"

func isSymlink(info os.FileInfo) bool {
	return info != nil && info.Mode()&os.ModeSymlink != 0
}

// readLocalContent returns what syncs for a local file: its bytes, or for a
// symbolic link its target behind symlinkPrefix.
func readLocalContent(abs string, info os.FileInfo) ([]byte, error) {
	if isSymlink(info) {
		target, err := os.Readlink(abs)
		if err != nil {
			return nil, err
		}
		return []byte(symlinkPrefix + target), nil
	}
	return os.ReadFile(abs)
}

// hashLocal hashes what syncs for a local file.
func hashLocal(abs string, info os.FileInfo) (string, error) {
	if isSymlink(info) {
		data, err := readLocalContent(abs, info)
		if err != nil {
			return "", err
		}
		return sha256Hex(data), nil
	}
	return HashFile(abs)
}

// statLocal describes a local file the way its root syncs it: a symbolic
// link itself in a project folder, else what the path points at.
func statLocal(lf *localFile) (os.FileInfo, error) {
	if lf.root != nil && lf.root.kind == rootFolder {
		return os.Lstat(lf.abs)
	}
	return os.Stat(lf.abs)
}

// checkNoSymlinkParents refuses a write into a project folder when a folder
// between the root and the file is a symbolic link: following it would write
// outside the folder.
func checkNoSymlinkParents(rootDir, abs string) error {
	rel, err := filepath.Rel(rootDir, filepath.Dir(abs))
	if err != nil || rel == "." {
		return err
	}
	cur := rootDir
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil // created below as a real folder
		}
		if err != nil {
			return err
		}
		if isSymlink(fi) {
			return fmt.Errorf("refusing to write through the symbolic link %s", cur)
		}
	}
	return nil
}

// writeFolderContent writes downloaded content into a project folder: a
// symbolic link for link content, else a file with mode perm, abandoning the
// write when the local copy changed since it was scanned (expect).
func writeFolderContent(rootDir, abs string, data []byte, expect os.FileInfo, perm os.FileMode) error {
	if err := checkInsideRoots(abs, rootDir); err != nil {
		return err
	}
	if err := checkNoSymlinkParents(rootDir, abs); err != nil {
		return err
	}
	cur, err := os.Lstat(abs)
	link := bytes.HasPrefix(data, []byte(symlinkPrefix))
	if !link && !isSymlink(cur) {
		return writeLocalPerm(abs, data, expect, perm, rootDir)
	}
	if expect != nil && (err != nil || cur.Size() != expect.Size() || !cur.ModTime().Equal(expect.ModTime())) {
		return errLocalChanged
	}
	if !link {
		return writeLocalPerm(abs, data, overwrite, perm, rootDir) // a file replacing a link
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".stradl-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(name)
	if err := os.Symlink(string(data[len(symlinkPrefix):]), name); err != nil {
		return err
	}
	if err := os.Rename(name, abs); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// folderListFile is where the last sync recorded its project folders.
func (s *Syncer) folderListFile() string {
	return filepath.Join(s.stateDir(), config.FoldersFile)
}

// knownFolderList returns the project folders the last sync covered.
func (s *Syncer) knownFolderList() []string {
	f, err := os.Open(s.folderListFile())
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var list struct {
		Folders []string `json:"folders"`
	}
	data, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil || json.Unmarshal(data, &list) != nil {
		return nil
	}
	return list.Folders
}

// saveFolderList records the folders this sync covered for the agent's
// local probe.
func (s *Syncer) saveFolderList(folders []string) error {
	data, err := json.MarshalIndent(struct {
		Folders []string `json:"folders"`
	}{folders}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.stateDir(), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.stateDir(), ".folders-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.folderListFile())
}

// homeCaseInsensitive reports whether the volume holding home ignores case,
// as macOS volumes do by default. It is checked once per home.
func homeCaseInsensitive(home string) bool {
	caseMu.Lock()
	defer caseMu.Unlock()
	if v, ok := caseCache[home]; ok {
		return v
	}
	v := false
	if swapped := swapCase(home); swapped != home {
		a, errA := os.Stat(home)
		b, errB := os.Stat(swapped)
		v = errA == nil && errB == nil && os.SameFile(a, b)
	}
	caseCache[home] = v
	return v
}

var (
	caseMu    gosync.Mutex
	caseCache = make(map[string]bool)
)

func swapCase(p string) string {
	b := []byte(p)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z':
			b[i] = c - 32
		case c >= 'A' && c <= 'Z':
			b[i] = c + 32
		}
	}
	return string(b)
}
