package sync

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	gosync "sync"
	"syscall"
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

var cwdFieldRe = regexp.MustCompile(`"cwd"\s*:\s*("(?:[^"\\]|\\.)*")`)

// filePathFieldRe finds the files a session's tools read or edited.
var filePathFieldRe = regexp.MustCompile(`"(?:file_path|notebook_path)"\s*:\s*("(?:[^"\\]|\\.)*")`)

// maxTouched bounds the folders of touched files kept per transcript.
const maxTouched = 2000

// neverProject reports home-relative folders a touched file can't make a
// project folder of: caches, toolchains and package stores, credentials.
func neverProject(rel string) bool {
	for _, p := range []string{"Library", ".Trash", ".claude", ".stradl", ".cache", ".npm", ".cargo", ".rustup", "go/pkg", ".gradle", ".m2", ".nvm", ".pyenv", ".local/share", ".bun", ".deno"} {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return credentialRel(rel) || strings.Contains("/"+rel+"/", "/node_modules/")
}

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

// credentialDirs are home folders (and ~/.config folders) holding keys and
// logins, which stay on each Mac even when a session works in them.
var credentialDirs = map[string]bool{
	".ssh": true, ".gnupg": true, ".aws": true, ".azure": true, ".kube": true,
	".docker": true, ".password-store": true, ".config/gcloud": true, ".config/gh": true,
}

// credentialRel reports whether a home-relative path is, or lies in, a
// credential folder.
func credentialRel(rel string) bool {
	first, rest, _ := strings.Cut(rel, "/")
	second, _, _ := strings.Cut(rest, "/")
	return credentialDirs[first] || (rest != "" && credentialDirs[first+"/"+second])
}

// folderAllowed reports whether a home-relative folder may be a sync root.
// Home itself never is, and neither is anything inside Stradl's or Claude's
// own folders, credential folders (credentialDirs), the Trash, or ~/Library (where iCloud Drive and other cloud
// drives live), except the desktop app's scratch folders.
func (s *Syncer) folderAllowed(rel string) bool {
	if !isCanonicalRel(rel) {
		return false
	}
	if credentialRel(rel) {
		return false // credentials stay on each Mac
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
func (s *Syncer) canonicalFolder(abs string) string { return s.canonicalFolderIn(abs, nil) }

// canonicalFolderIn is canonicalFolder that also counts as a repository a
// folder other Macs sync as one (remoteRepos, home-relative): a repository
// inside another stays its own on a Mac that doesn't have it yet.
func (s *Syncer) canonicalFolderIn(abs string, remoteRepos map[string]bool) string {
	if s.home == "" || !filepath.IsAbs(abs) {
		return ""
	}
	abs = filepath.Clean(abs)
	rel, ok := homeRel(s.home, abs)
	if !ok || !s.folderAllowed(rel) {
		return ""
	}
	if s.folderGate(abs) != nil {
		return "" // macOS hasn't let Stradl look there yet
	}
	if fi, err := os.Lstat(abs); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return "" // a link itself is never a folder to sync (folders inside one sync as links)
	}
	// The nearest enclosing repository below home wins; a dotfiles repository
	// at home itself never swallows every folder.
	for dir := abs; dir != filepath.Clean(s.home) && isWithin(s.home, dir); dir = filepath.Dir(dir) {
		r, ok := homeRel(s.home, dir)
		if isRepoDir(dir) || (ok && remoteRepos[r]) {
			if ok && s.folderAllowed(r) {
				return r
			}
			break
		}
	}
	return rel
}

// enclosing returns the folder (absolute) among the home-relative rels that
// abs lies in, the innermost; "" when none.
func enclosing(home string, rels []string, abs string, fold bool) string {
	best := ""
	for _, c := range rels {
		if insideAny(home, []string{c}, abs, fold) && len(c) > len(best) {
			best = c
		}
	}
	if best == "" {
		return ""
	}
	return filepath.Join(home, filepath.FromSlash(best))
}

// viaSymlink reports whether a folder below base, or one on the way to it,
// is a symbolic link.
func viaSymlink(base, abs string) bool {
	rel, ok := homeRel(base, abs)
	if !ok {
		return false
	}
	p := filepath.Clean(base)
	for _, seg := range strings.Split(rel, "/") {
		p = filepath.Join(p, seg)
		fi, err := os.Lstat(p)
		if err != nil {
			return false // missing here: nothing to follow
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// discoverFolders returns the project folders to sync, relative to home: the
// working folders of desktop sidebar sessions and of transcripts, the
// configured extra folders, and folders other Macs sync (their remote
// markers). Each is canonicalized (see canonicalFolder); spellings of one
// folder that differ only in case collapse to one, preferring the spelling
// already in remote storage; and a folder inside another is covered by the
// outer one.
func (s *Syncer) discoverFolders(objects []storage.ObjectInfo, useCache bool) []string {
	remoteRepos := remoteRepoFolders(objects)
	for _, rel := range remoteFolderMarkers(objects) {
		// A Claude worktree another Mac syncs is its own folder here too,
		// though its .git pointer stays on that Mac.
		if claudeWorktree(path.Dir(rel)) && path.Base(path.Dir(rel)) == "worktrees" {
			remoteRepos[rel] = true
		}
	}
	var candidates []string
	seen := make(map[string]bool) // thousands of transcripts share a few folders
	add := func(abs string) {
		if abs == "" || seen[abs] {
			return
		}
		seen[abs] = true
		if rel := s.canonicalFolderIn(abs, remoteRepos); rel != "" {
			candidates = append(candidates, rel)
		}
	}
	fold := homeCaseInsensitive(s.home)
	for _, cwd := range s.desktopCwds() {
		add(cwd)
	}
	first, later := s.transcriptCwds(useCache)
	for _, cwd := range first {
		add(cwd)
	}
	for _, p := range s.cfg.ExtraFolders {
		add(p)
	}
	remote := make(map[string]bool)
	for _, rel := range remoteFolderMarkers(objects) {
		if c := s.canonicalFolderIn(filepath.Join(s.home, filepath.FromSlash(rel)), remoteRepos); c != "" {
			remote[c] = true
			candidates = append(candidates, c)
		}
	}
	// A folder a session moved into, or touched files in, after it started
	// counts inside a folder found above (a repository inside it, say); a
	// cd anywhere else (into ~/.ssh) doesn't make a project folder.
	primary := append([]string(nil), candidates...)
	for _, p := range later {
		if seen[p] || !filepath.IsAbs(p) {
			continue
		}
		p = filepath.Clean(p)
		// Only inside a folder found above (which passed the privacy gate),
		// and never through a link inside it (code/app/shared -> ../lib):
		// that's the link's folder's business.
		if top := enclosing(s.home, primary, p, fold); top != "" && !viaSymlink(top, p) {
			add(p)
		}
	}
	// The folders the last sync covered stay, so a folder whose sessions
	// are gone keeps syncing; a repository inside another folder stays only
	// while a session (or a marker) needs it, except for the local probe,
	// which watches exactly what the last sync covered.
	known := s.knownFolderList()
	sort.SliceStable(known, func(i, j int) bool { return strings.Count(known[i], "/") < strings.Count(known[j], "/") })
	probe := objects == nil && !useCache // the agent's local probe: no markers to go by
	for _, rel := range known {
		if probe || !insideAny(s.home, candidates, filepath.Join(s.home, filepath.FromSlash(rel)), fold) || containsFold(candidates, rel, fold) {
			add(filepath.Join(s.home, filepath.FromSlash(rel)))
		}
	}
	// A submodule a session needs brings its superproject, which holds its
	// git data.
	for i := 0; i < len(candidates); i++ {
		if super := s.superproject(candidates[i]); super != "" {
			add(super)
		}
		if main := s.mainWorktree(candidates[i]); main != "" {
			add(main)
		}
	}
	candidates = s.dropSkippedNested(candidates, fold)
	// Git repositories among them: one inside another folder still syncs as
	// its own (see linkFolderRoots).
	repo := make(map[string]bool)
	for _, c := range candidates {
		repo[c] = remoteRepos[c] || isRepoDir(filepath.Join(s.home, filepath.FromSlash(c)))
	}
	return chooseFolders(candidates, remote, fold, repo)
}

// insideAny reports whether abs lies in (or is) one of the home-relative
// folders rels.
func insideAny(home string, rels []string, abs string, fold bool) bool {
	r, ok := homeRel(home, abs)
	if !ok {
		return false
	}
	if fold {
		r = strings.ToLower(r)
	}
	for _, c := range rels {
		if fold {
			c = strings.ToLower(c)
		}
		if r == c || strings.HasPrefix(r, c+"/") {
			return true
		}
	}
	return false
}

func containsFold(rels []string, rel string, fold bool) bool {
	for _, c := range rels {
		if c == rel || (fold && strings.EqualFold(c, rel)) {
			return true
		}
	}
	return false
}

// dropSkippedNested drops a folder that lies inside another one at a path
// that folder leaves out (build output, folder_exclude, git's per-Mac
// folders): a checkout in node_modules or .build stays out even when a
// session works in it, and folder_exclude takes a repository out.
func (s *Syncer) dropSkippedNested(candidates []string, fold bool) []string {
	out := candidates[:0:0]
	for _, c := range candidates {
		skipped := false
		for _, o := range candidates {
			if len(c) <= len(o) || c[len(o)] != '/' || (c[:len(o)] != o && !(fold && strings.EqualFold(c[:len(o)], o))) {
				continue
			}
			rest := c[len(o)+1:]
			for i := 0; i <= len(rest) && !skipped; i++ {
				if i == len(rest) || rest[i] == '/' {
					// (Not claudeWorktree: a worktree a session works in is
					// a folder of its own.)
					r := rest[:i]
					skipped = s.cfg.IsBuildArtifact(r) || s.cfg.IsFolderExcluded(r) || gitPerMac(r) || hasGitSegment(r)
				}
			}
			if skipped {
				break
			}
		}
		if !skipped {
			out = append(out, c)
		}
	}
	return out
}

// mainWorktree returns, for a linked worktree (the Claude app's
// .claude/worktrees/<name>, or `git worktree add`), the folder of the
// repository holding its git data, when that lies under home.
func (s *Syncer) mainWorktree(rel string) string {
	gitdir, err := gitfileTarget(filepath.Join(s.home, filepath.FromSlash(rel)))
	if err != nil || gitdir == "" {
		return ""
	}
	i := strings.LastIndex(filepath.ToSlash(gitdir), "/.git/worktrees/")
	if i < 0 {
		return ""
	}
	main, ok := s.underHome(filepath.FromSlash(filepath.ToSlash(gitdir)[:i]))
	if !ok {
		return ""
	}
	return main
}

// underHome returns abs spelled under home when it lies there, also when
// it is spelled through home's real path (git records resolved paths).
func (s *Syncer) underHome(abs string) (string, bool) {
	if _, ok := homeRel(s.home, abs); ok {
		return abs, true
	}
	real, err := filepath.EvalSymlinks(s.home)
	if err != nil || real == filepath.Clean(s.home) {
		return "", false
	}
	r, ok := homeRel(real, abs)
	if !ok {
		return "", false
	}
	return filepath.Join(s.home, filepath.FromSlash(r)), true
}

// superproject returns, for a submodule (its .git file points into another
// repository's .git/modules), that repository's folder.
func (s *Syncer) superproject(rel string) string {
	dir := filepath.Join(s.home, filepath.FromSlash(rel))
	gitdir, err := gitfileTarget(dir)
	if err != nil || gitdir == "" {
		return ""
	}
	i := strings.LastIndex(filepath.ToSlash(gitdir), "/.git/modules/")
	if i < 0 {
		return ""
	}
	super := filepath.FromSlash(filepath.ToSlash(gitdir)[:i])
	if super == dir || !isWithin(super, dir) {
		return ""
	}
	return super
}

// chooseFolders collapses case variants (on a case-insensitive volume) and
// nested folders, except git repositories (repo), which stay separate even
// inside another folder. Among spellings of one folder, one already in remote
// storage wins, then the first in sort order, so every Mac picks the same
// spelling.
func chooseFolders(candidates []string, remote map[string]bool, foldCase bool, repo map[string]bool) []string {
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
		covered, at := false, 0
		for i := 0; i < len(k) && !covered; i++ {
			covered, at = k[i] == '/' && kept[k[:i]], i
		}
		if covered && !repo[best[k]] {
			continue
		}
		name := best[k]
		if covered && foldCase && len(best[k]) == len(k) {
			// Spell it under the folder holding it as that folder is spelled,
			// so its files keep the keys they synced under inside it.
			name = best[k[:at]] + best[k][at:]
		}
		kept[k] = true
		out = append(out, name)
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

// transcriptCwds returns, for every transcript, the folder its session
// started in (first) and every other folder it worked in: later working
// folders and the folders of files its tools read or edited (later),
// subagents' included. Results are cached in the sync state (useCache) and,
// for the background agent's probes, in memory, so each transcript is read
// once, and then only where it grew.
func (s *Syncer) transcriptCwds(useCache bool) (first, later []string) {
	projects := filepath.Join(s.claudeDir, "projects")
	dirs, err := os.ReadDir(projects)
	if err != nil {
		return nil, nil
	}
	present := make(map[string]bool)
	read := func(abs, np string, info os.FileInfo) cwdEntry {
		present[np] = true
		var e cwdEntry
		var ok bool
		if useCache && s.state != nil {
			e, ok = s.state.cachedCwd(np)
		} else {
			e, ok = memCwds.get(np)
		}
		if !ok || e.Size != info.Size() || (e.Scanned == 0 && info.Size() > 0) {
			e = updateCwds(abs, e, info.Size(), s.home)
			if useCache && s.state != nil {
				s.state.setCachedCwd(np, e)
			} else {
				memCwds.set(np, e)
			}
		}
		return e
	}
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
			if f.IsDir() {
				// <session>/subagents/*.jsonl: work a session delegated.
				sub := filepath.Join(projects, d.Name(), name, "subagents")
				agents, _ := os.ReadDir(sub)
				for _, a := range agents {
					if !a.Type().IsRegular() || !strings.HasSuffix(a.Name(), ".jsonl") {
						continue
					}
					if info, err := a.Info(); err == nil {
						e := read(filepath.Join(sub, a.Name()), "projects/"+d.Name()+"/"+name+"/subagents/"+a.Name(), info)
						later = append(later, e.Cwds...)
						later = append(later, e.touchedDirs()...)
					}
				}
				continue
			}
			if !f.Type().IsRegular() || !strings.HasSuffix(name, ".jsonl") || isDeviceLocalFile(name) {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			e := read(filepath.Join(projects, d.Name(), name), "projects/"+d.Name()+"/"+name, info)
			if len(e.Cwds) > 0 {
				first = append(first, e.Cwds[0])
				later = append(later, e.Cwds[1:]...)
			}
			later = append(later, e.touchedDirs()...)
		}
	}
	if useCache && s.state != nil {
		s.state.pruneCwds(present)
	}
	return first, later
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

// updateCwds brings a transcript's cached working folders up to date: a
// session can start in one folder and work in others (a repository inside
// it, say), and each one it worked in is needed. A transcript only grows, so
// reading resumes where the last read stopped, at the end of the last whole
// line; one that shrank (replaced) is read again from the start.
func updateCwds(p string, e cwdEntry, size int64, home string) cwdEntry {
	f, err := os.Open(p)
	if err != nil {
		return e
	}
	defer func() { _ = f.Close() }()
	from := e.Scanned
	cwds, touched := e.Cwds, e.Touched
	if from > size || from == 0 || e.Tail == 0 || tailSum(f, from) != e.Tail {
		// New, truncated, or rewritten before where the last read stopped
		// (MergeJSONL interleaves another Mac's lines by time).
		from, cwds, touched = 0, nil, nil
	}
	seen := make(map[string]bool, len(cwds)+len(touched))
	for _, c := range cwds {
		seen[c] = true
	}
	for _, c := range touched {
		seen["\x00"+c] = true
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return cwdEntry{Size: size, Cwds: cwds, Touched: touched, Scanned: from, Tail: e.Tail}
	}
	r := bufio.NewReaderSize(f, 256<<10)
	pos := from
	for {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 || line[len(line)-1] != '\n' {
			break // end of file, or a line still being written
		}
		pos += int64(len(line))
		for _, m := range cwdFieldRe.FindAllSubmatch(line, -1) {
			var cwd string
			if json.Unmarshal(m[1], &cwd) == nil && cwd != "" && !seen[cwd] {
				seen[cwd] = true
				cwds = append(cwds, cwd)
			}
		}
		if len(touched) < maxTouched {
			for _, m := range filePathFieldRe.FindAllSubmatch(line, -1) {
				var p string
				if json.Unmarshal(m[1], &p) != nil || !filepath.IsAbs(p) {
					continue
				}
				dir := filepath.Dir(filepath.Clean(p))
				if r, ok := homeRel(home, dir); !ok || neverProject(r) {
					continue
				}
				if len(cwds) > 0 {
					// Inside where the session started: kept short.
					if r, err := filepath.Rel(cwds[0], dir); err == nil && r != ".." && !strings.HasPrefix(r, "../") && !filepath.IsAbs(r) {
						dir = "./" + filepath.ToSlash(r)
					}
				}
				if !seen["\x00"+dir] && len(touched) < maxTouched {
					seen["\x00"+dir] = true
					touched = append(touched, dir)
				}
			}
		}
		if err != nil {
			break
		}
	}
	first := ""
	if len(cwds) > 0 {
		first = cwds[0]
	}
	return cwdEntry{Size: size, Cwd: first, Cwds: cwds, Touched: touched, Scanned: pos, Tail: tailSum(f, pos)}
}

// tailSum checksums up to 64 bytes of f ending at pos (0 when pos is 0 or
// unreadable).
func tailSum(f *os.File, pos int64) uint32 {
	n := min(pos, 64)
	if n <= 0 {
		return 0
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, pos-n); err != nil {
		return 0
	}
	return crc32.ChecksumIEEE(buf)
}

// folderRoot builds the sync root for a home-relative project folder.
func (s *Syncer) folderRoot(rel string) *syncRoot {
	return &syncRoot{
		dir:    filepath.Join(s.home, filepath.FromSlash(rel)),
		prefix: FoldersPrefix + rel + "/",
		paths:  []string{"."},
		// A credential folder inside it (~/.config/gh in ~/.config) stays
		// on each Mac.
		include:  func(r string) bool { return s.folderInclude(r) && !credentialRel(rel+"/"+r) },
		skipDir:  func(r string) bool { return s.folderSkipDir(r) || credentialRel(rel+"/"+r) },
		kind:     rootFolder,
		label:    "~/" + rel,
		perm:     0644,
		children: make(map[string]bool),
		nested:   make(map[string]bool),
		modules:  make(map[string]bool),
		moduleOf: make(map[string]string),
	}
}

// linkFolderRoots tells each project folder which git repositories inside it
// are not its own: folder roots inside it (children), and repositories that
// other Macs' files show inside it (an older Stradl synced them along with
// the folder) but that no session here needs (nested). The scan adds the
// nested repositories it finds on disk.
func (s *Syncer) linkFolderRoots(roots []*syncRoot, objects []storage.ObjectInfo) {
	var folders []*syncRoot
	for _, r := range roots {
		if r.kind == rootFolder {
			folders = append(folders, r)
		}
	}
	for _, outer := range folders {
		for _, inner := range folders {
			if inner != outer && strings.HasPrefix(inner.prefix, outer.prefix) {
				child := strings.TrimSuffix(strings.TrimPrefix(inner.prefix, outer.prefix), "/")
				m := moduleDir(outer.dir, inner.dir, child)
				outer.children[child] = true
				outer.modules[m] = true
				outer.moduleOf[child] = m
				if len(s.cfg.FolderExclude) > 0 {
					// folder_exclude patterns written for the outer folder
					// ("lib/.env") still apply inside it.
					inc, skip, c := inner.include, inner.skipDir, child
					inner.include = func(r string) bool { return inc(r) && !s.cfg.IsFolderExcluded(c+"/"+r) }
					inner.skipDir = func(r string) bool { return skip(r) || s.cfg.IsFolderExcluded(c+"/"+r) }
				}
			}
		}
	}
	markers := make(map[string]bool)
	for _, rel := range remoteFolderMarkers(objects) {
		markers[rel] = true
	}
	for _, r := range folders {
		// A registered submodule never belongs to the repository holding
		// it, whatever its folder holds here (git tracks nothing under it).
		for _, p := range submodulePaths(r.dir) {
			// Not checked out (missing or empty) or checked out: either way
			// the submodule's, unless it's a plain folder of files (a
			// submodule converted to files, a source tarball).
			if abs := filepath.Join(r.dir, filepath.FromSlash(p)); !r.children[p] && !(plainDir(abs) && hasEntries(abs)) {
				r.nested[p] = true
			}
		}
	}
	for _, obj := range objects {
		if !strings.HasPrefix(obj.Key, FoldersPrefix) {
			continue
		}
		np, ok, _ := s.npathForKey(obj.Key)
		if !ok {
			continue
		}
		root, rel := s.rootFor(roots, np)
		if root == nil || root.kind != rootFolder {
			continue
		}
		// <dir>/.git/HEAD or a <dir>/.git file (a submodule's) names a
		// repository at <dir> inside the root, unless <dir> is a plain folder
		// of files here (leftovers of a .git since removed); one other Macs
		// sync as its own folder is a child, not nested.
		dir := ""
		if d, ok := strings.CutSuffix(rel, "/.git/HEAD"); ok {
			dir = d
		} else if d, ok := strings.CutSuffix(rel, "/.git"); ok {
			dir = d
		}
		outerRel := strings.TrimSuffix(strings.TrimPrefix(root.prefix, FoldersPrefix), "/")
		if abs := filepath.Join(root.dir, filepath.FromSlash(dir)); dir != "" && !root.children[dir] && !markers[outerRel+"/"+dir] && !(plainDir(abs) && hasEntries(abs)) {
			root.nested[dir] = true
		}
	}
}

// remoteRepoFolders lists the home-relative folders other Macs sync as git
// repositories: those with a .git/HEAD, or a .git file (a submodule).
func remoteRepoFolders(objects []storage.ObjectInfo) map[string]bool {
	out := make(map[string]bool)
	for _, o := range objects {
		k, ok := strings.CutPrefix(o.Key, FoldersPrefix)
		if !ok {
			continue
		}
		if rel, ok := strings.CutSuffix(k, "/.git/HEAD.age"); ok {
			out[rel] = true
		} else if rel, ok := strings.CutSuffix(k, "/.git.age"); ok {
			out[rel] = true
		}
	}
	return out
}

// moduleDir returns where, under outer's .git/modules, git keeps the data of
// the submodule at child (relative to outer): what its .git file points to,
// else its path (git's default name).
func moduleDir(outerDir, childDir, child string) string {
	gitdir, err := gitfileTarget(childDir)
	if err != nil || gitdir == "" {
		return child
	}
	rel, err := filepath.Rel(filepath.Join(outerDir, ".git", "modules"), gitdir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return child
	}
	return filepath.ToSlash(rel)
}

// owns reports whether rel, inside this project folder, belongs to it rather
// than to a git repository inside it (see children and nested), counting
// that repository's data in this one's .git/modules (a submodule) as its.
func (r *syncRoot) owns(rel string) bool {
	if rest, ok := strings.CutPrefix(rel, ".git/modules/"); ok {
		// Submodule data syncs only for a submodule a session needs, each
		// nested submodule's on its own.
		return r.moduleFor(rest) != ""
	}
	if strings.Contains(rel, "/.git/") || strings.HasSuffix(rel, "/.git") {
		return false // another repository's git data, or a stray .git: never this folder's
	}
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' && (r.children[rel[:i]] || r.nested[rel[:i]]) {
			return false
		}
	}
	return true
}

// moduleFor returns the needed submodule whose git data rest (what follows
// .git/modules/) lies in: the longest entry of modules it falls under,
// unless it lies in a nested submodule's data (modules/...) that isn't an
// entry itself.
func (r *syncRoot) moduleFor(rest string) string {
	best := ""
	for c := range r.modules {
		if tail, ok := strings.CutPrefix(rest, c+"/"); ok && len(c) > len(best) && tail != "modules" && !strings.HasPrefix(tail, "modules/") {
			best = c
		}
	}
	return best
}

// wantsModuleDir reports whether the scan descends into .git/modules/<rest>:
// a needed submodule's data, or a folder on the way to it.
func (r *syncRoot) wantsModuleDir(rest string) bool {
	for c := range r.modules {
		if rest == c || strings.HasPrefix(c, rest+"/") {
			return true // the submodule's data, or a folder on the way to it
		}
		if tail, ok := strings.CutPrefix(rest, c+"/"); ok && tail != "modules" && !strings.HasPrefix(tail, "modules/") {
			return true // inside it, outside its own submodules' data
		}
	}
	return false
}

// isRepoDir reports whether the folder at abs is a git repository: a clone
// (.git folder), a submodule or a linked worktree (.git file).
//
// A .git folder counts when it holds HEAD and a .git file when it points
// somewhere ("gitdir: ..."); a stray .git doesn't hide a plain folder. One
// iCloud keeps only in the cloud counts too: reading into it would only stall.
func isRepoDir(abs string) bool {
	fi, err := os.Lstat(filepath.Join(abs, ".git"))
	switch {
	case err != nil:
		return errors.Is(err, syscall.EDEADLK)
	case fi.IsDir():
		_, err := os.Lstat(filepath.Join(abs, ".git", "HEAD"))
		return err == nil || errors.Is(err, syscall.EDEADLK)
	case fi.Mode().IsRegular():
		gitdir, err := gitfileTarget(abs)
		return gitdir != "" || errors.Is(err, syscall.EDEADLK)
	}
	return false
}

// plainDir reports a folder that exists here and is surely not a git
// repository (isRepoDir's uncertain cases are not plain).
func plainDir(abs string) bool {
	fi, err := os.Lstat(abs)
	if err != nil || !fi.IsDir() {
		return false
	}
	if _, err := os.Lstat(filepath.Join(abs, ".git")); err == nil {
		return !isRepoDir(abs)
	} else {
		return errors.Is(err, fs.ErrNotExist)
	}
}

// hasEntries reports whether the folder at abs holds anything.
func hasEntries(abs string) bool {
	f, err := os.Open(abs)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	names, _ := f.Readdirnames(1)
	return len(names) > 0
}

// submodulePaths returns the submodule paths a repository's .gitmodules
// registers, relative to dir.
func submodulePaths(dir string) []string {
	f, err := os.Open(filepath.Join(dir, ".gitmodules"))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.TrimSpace(k) != "path" {
			continue
		}
		p := path.Clean(strings.Trim(strings.TrimSpace(v), `"`))
		if p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") || hasGitSegment(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// folderTracked reports whether cwd is, or lies in, a project folder this
// Mac has synced files of.
func (s *Syncer) folderTracked(cwd string) bool {
	known := make(map[string]bool)
	for _, f := range s.knownFolderList() {
		known[f] = true
	}
	rel, ok := homeRel(s.home, cwd)
	for ok && rel != "." && rel != "" {
		if known[rel] && s.state != nil && s.state.hasPrefix(FoldersPrefix+rel+"/") {
			return true
		}
		rel = path.Dir(rel)
	}
	return false
}

// gitfileTarget returns where the .git file of the repository at dir points
// (a submodule's or linked worktree's git data), cleaned and absolute, or ""
// when dir has no such file.
func gitfileTarget(dir string) (string, error) {
	p := filepath.Join(dir, ".git")
	fi, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() || fi.Size() > 4096 {
		return "", nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if gitdir = strings.TrimSpace(gitdir); !ok || gitdir == "" {
		return "", nil
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	return filepath.Clean(gitdir), nil
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
	case gitPerMac(rel), claudeWorktree(rel):
		return false
	}
	return !s.cfg.IsBuildArtifact(rel) && !s.cfg.IsFolderExcluded(rel)
}

// folderSkipDir reports directories inside a project folder that are left out
// entirely.
func (s *Syncer) folderSkipDir(rel string) bool {
	return s.cfg.IsBuildArtifact(rel) || s.cfg.IsFolderExcluded(rel) || gitPerMac(rel) || claudeWorktree(rel)
}

// claudeWorktree reports the Claude app's worktrees folder inside a project
// (.claude/worktrees) and what's in it: linked git worktrees of one Mac,
// whose git data never syncs. A worktree a session works in syncs as its
// own folder (with the repository holding its git data, see mainWorktree).
func claudeWorktree(rel string) bool {
	return strings.Contains("/"+rel+"/", "/.claude/worktrees/")
}

// scanFolder lists a project folder's files. It returns errGitBusy when git
// holds a lock file anywhere under a .git directory, and any walk error
// (an unreadable subfolder): either way the folder sits this pass out, since
// files missing from a partial scan would look deleted.
func (s *Syncer) scanFolder(root *syncRoot) (map[string]os.FileInfo, error) {
	files := make(map[string]os.FileInfo)
	if err := s.folderGate(root.dir); err != nil {
		return nil, err
	}
	if _, err := os.Stat(root.dir); os.IsNotExist(err) {
		return files, errFolderMissing
	}
	if err := s.gitfileBusy(root.dir); err != nil {
		return nil, err
	}
	own := []string{filepath.Clean(s.claudeDir), filepath.Clean(s.stateDir())}
	err := filepath.WalkDir(root.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, syscall.EDEADLK) {
				// A folder iCloud keeps only in the cloud: listing it needs
				// it downloaded. The whole folder waits (skipping just this
				// part would make its files look deleted).
				RequestICloudDownload(p)
				return &ICloudError{Path: p}
			}
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
			if root.kind == rootFolder {
				switch {
				case root.children[rel]:
					return filepath.SkipDir // synced as its own folder
				case root.nested[rel], d.Name() == ".git" && rel != ".git":
					return filepath.SkipDir // a repository no session needs (known from storage), or a stray .git
				case strings.HasPrefix(rel, ".git/modules/"):
					if !root.wantsModuleDir(strings.TrimPrefix(rel, ".git/modules/")) {
						return filepath.SkipDir // a submodule no session needs
					}
				case !hasGitSegment(rel) && isRepoDir(p):
					// Another repository inside this folder that no session
					// works in: it doesn't sync.
					root.nested[rel] = true
					return filepath.SkipDir
				}
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

// gitfileBusy checks, for a repository whose .git is a file (a submodule or
// a linked worktree), the git data that file points to for lock files: git
// keeps them there, outside the folder the scan walks. It returns a
// *gitBusyError, or the folderGate's error when that data lies in a folder
// macOS hasn't let Stradl read yet.
func (s *Syncer) gitfileBusy(dir string) error {
	gitdir, err := gitfileTarget(dir)
	if err != nil || gitdir == "" {
		return nil
	}
	gitdir, ok := s.underHome(gitdir)
	if !ok {
		return nil // another volume (whose first read macOS may ask about) or system folder: not checked
	}
	if err := s.folderGate(gitdir); err != nil {
		return err
	}
	var busy *gitBusyError
	_ = filepath.WalkDir(gitdir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || busy != nil {
			return filepath.SkipDir
		}
		if d.IsDir() && d.Name() == "modules" && p != gitdir {
			return filepath.SkipDir // nested submodules check their own
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".lock") {
			busy = &gitBusyError{lock: p}
			if info, err := d.Info(); err == nil {
				busy.since = info.ModTime()
			}
			return filepath.SkipAll
		}
		return nil
	})
	if busy == nil {
		return nil // not a nil *gitBusyError in a non-nil error
	}
	return busy
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
	if err := checkInICloud(abs, info); err != nil {
		return nil, err
	}
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
	if err := checkInICloud(abs, info); err != nil {
		return "", err
	}
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

// folderList is what the last sync recorded about project folders: the
// folders it covered and, for each, the repositories inside it that didn't
// sync (no session needed them).
type folderList struct {
	Folders []string            `json:"folders"`
	Nested  map[string][]string `json:"nested,omitempty"`
}

func (s *Syncer) readFolderList() folderList {
	var list folderList
	f, err := os.Open(s.folderListFile())
	if err != nil {
		return list
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil || json.Unmarshal(data, &list) != nil {
		return folderList{}
	}
	return list
}

// knownFolderList returns the project folders the last sync covered.
func (s *Syncer) knownFolderList() []string { return s.readFolderList().Folders }

// saveFolderList records the folders this sync covered, for the agent's
// local probe and the app, and the repositories each left out.
func (s *Syncer) saveFolderList(folders []string, nested map[string][]string) error {
	data, err := json.MarshalIndent(folderList{folders, nested}, "", "  ")
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
