package sync

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	gosync "sync"
	"time"

	"github.com/tawanorg/claude-sync/internal/storage"
)

// Project folders are often git clones, and a folder can exist on two Macs
// before either one synced it (cloned twice, or copied by hand). These helpers
// ask git which side holds real work, so a fresh clone never replaces edits
// another Mac made, and a branch two Macs moved differently stays reachable.

// gitTimeout bounds each git command the sync runs.
const gitTimeout = 10 * time.Second

var (
	gitPathOnce gosync.Once
	gitPathVal  string
)

// gitBinary returns a git executable that runs without side effects, or "".
// On a Mac /usr/bin/git is a stub that opens an "install developer tools"
// dialog when they're missing, so it is only used once xcode-select points at
// tools that include git.
func gitBinary() string {
	gitPathOnce.Do(func() {
		if runtime.GOOS != "darwin" {
			gitPathVal, _ = exec.LookPath("git")
			return
		}
		for _, p := range []string{"/opt/homebrew/bin/git", "/usr/local/bin/git"} {
			if isExecutable(p) {
				gitPathVal = p
				return
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, "/usr/bin/xcode-select", "-p").Output()
		if err != nil {
			return
		}
		if p := filepath.Join(strings.TrimSpace(string(out)), "usr", "bin", "git"); isExecutable(p) {
			gitPathVal = p
		}
	})
	return gitPathVal
}

func isExecutable(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0
}

// runGit runs git in dir without taking any of git's optional locks, so it
// never blocks a git command the user runs at the same moment.
func runGit(dir string, args ...string) ([]byte, error) {
	bin := gitBinary()
	if bin == "" {
		return nil, exec.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	return cmd.Output()
}

// gitFile is how git sees one file in a working tree.
type gitFile int

const (
	gitWork    gitFile = iota // real work: modified, untracked, or git can't tell
	gitClean                  // tracked and identical to what was committed or staged
	gitIgnored                // ignored: build output, caches, local settings
)

// gitFileState asks git about the file at abs. Anything git can't vouch for
// (no git, not a repository) counts as work.
func gitFileState(abs string) gitFile {
	out, err := runGit(filepath.Dir(abs), "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching", "--", filepath.Base(abs))
	switch {
	case err != nil:
		return gitWork
	case bytes.HasPrefix(out, []byte("!! ")):
		return gitIgnored
	case len(out) != 0:
		return gitWork
	}
	// Empty output also covers a path git doesn't know at all on an old git
	// without --ignored=matching; make sure it is tracked.
	out, err = runGit(filepath.Dir(abs), "ls-files", "-z", "--error-unmatch", "--", filepath.Base(abs))
	if err == nil && len(out) != 0 {
		return gitClean
	}
	return gitWork
}

var gitSHARe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// keptBranchPrefix holds branches the sync creates for commits that would
// otherwise only be reachable from a branch another Mac moved elsewhere.
const keptBranchPrefix = "stradl/"

// keepLosingBranch makes sure the commits a branch pointed to on the losing
// side of a conflict stay reachable. For a loose ref file the losing commit,
// and for packed-refs each branch in the losing copy, is saved as branch
// stradl/<branch>-<short sha> unless git confirms the winning side's branch
// of the same name already contains it. abs is the local file; winner and
// loser are the two sides' contents, and winnerIsRemote says which side won
// (a loose ref on that side outranks its packed-refs entry).
func (s *Syncer) keepLosingBranch(ctx context.Context, npath, abs string, winner, loser []byte, winnerIsRemote bool) {
	gitDir, gitNP := "", ""
	var lost, kept map[string]string // branch -> sha
	dir, inside, _ := gitDirSplit(filepath.ToSlash(abs))
	if branch, ok := gitBranchRef(npath); ok {
		gitDir = refGitDir(abs)
		lost = map[string]string{branch: strings.TrimSpace(string(loser))}
		kept = map[string]string{branch: strings.TrimSpace(string(winner))}
	} else if inside == "HEAD" || inside == "refs/stash" {
		// A detached HEAD, or the latest stash: commits no branch holds.
		name := filepath.Base(abs)
		gitDir = filepath.FromSlash(dir)
		lost = map[string]string{name: strings.TrimSpace(string(loser))}
		kept = map[string]string{name: strings.TrimSpace(string(winner))}
	} else if inside == "packed-refs" {
		gitDir, gitNP = filepath.FromSlash(dir), strings.TrimSuffix(npath, "packed-refs")
		lost, kept = packedBranches(loser), packedBranches(winner)
	}
	if gitDir == "" {
		return
	}
	headsDir := filepath.Join(gitDir, "refs", "heads")
	for branch, sha := range lost {
		if strings.HasPrefix(branch, keptBranchPrefix) || !gitSHARe.MatchString(sha) {
			continue
		}
		win := kept[branch]
		if gitNP != "" {
			if loose := s.looseRef(ctx, gitNP, headsDir, branch, winnerIsRemote); loose != "" {
				win = loose
			}
		}
		if win == sha {
			continue
		}
		if gitSHARe.MatchString(win) {
			// Exit 0: the losing commit is an ancestor, nothing is lost.
			if _, err := runGit(gitDir, "--git-dir="+gitDir, "merge-base", "--is-ancestor", sha, win); err == nil {
				continue
			}
		}
		p := filepath.Join(headsDir, filepath.FromSlash(keptBranchPrefix+branch+"-"+sha[:7]))
		if existing, err := os.ReadFile(p); err == nil && bytes.Equal(bytes.TrimSpace(existing), []byte(sha)) {
			continue
		}
		_ = writeFileAtomic(freeName(p), []byte(sha+"\n"), 0644)
	}
}

// looseRef reads a loose branch ref from storage (remote) or from this Mac,
// or "" when that side has none. gitNP is the repository's .git/ npath.
func (s *Syncer) looseRef(ctx context.Context, gitNP, headsDir, branch string, remote bool) string {
	if !remote {
		data, _ := os.ReadFile(filepath.Join(headsDir, filepath.FromSlash(branch)))
		return strings.TrimSpace(string(data))
	}
	np := gitNP + "refs/heads/" + branch
	data, _, err := s.fetch(ctx, np, s.engineRemoteKey(np), nil, s.home)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// firstContactLooseRef settles a loose branch ref this Mac has and storage
// lacks, in a repository this Mac never synced, when the reference copy keeps
// that branch in packed-refs: uploaded, the loose ref would override the
// reference's branch on every Mac. The reference's value wins instead; this
// Mac's ref goes to the conflicts folder and its commit stays reachable on a
// stradl/ branch. It reports whether it settled the ref.
func (s *Syncer) firstContactLooseRef(ctx context.Context, npath string, lf *localFile) (bool, error) {
	branch, ok := gitBranchRef(npath)
	dir, _, _ := gitDirSplit(npath)
	if !ok {
		return false, nil
	}
	packedNP := dir + "/packed-refs"
	packed, _, err := s.fetch(ctx, packedNP, s.engineRemoteKey(packedNP), nil, s.home)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ref := packedBranches(packed)[branch]
	local, err := os.ReadFile(lf.abs)
	if err != nil || ref == "" || strings.TrimSpace(string(local)) == ref {
		return false, err
	}
	s.keepLosingBranch(ctx, npath, lf.abs, []byte(ref), local, true)
	if err := s.keepLoser(npath, lf.abs, local, ConflictFromThisMac, true); err != nil {
		return false, err
	}
	return true, os.Remove(lf.abs)
}

// mergePackedRefs returns the reference copy of a packed-refs file with the
// refs only the other copy has added (remote-tracking branches, tags, local
// branches), so first contact never drops a ref; a ref in both keeps the
// reference's value. Lines stay sorted by ref name, as git expects.
func mergePackedRefs(ref, other []byte) []byte {
	type entry struct{ lines []string }
	parse := func(data []byte) (header string, refs map[string]*entry) {
		refs = make(map[string]*entry)
		var last *entry
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			switch {
			case line == "":
			case strings.HasPrefix(line, "#"):
				if header == "" {
					header = line
				}
			case strings.HasPrefix(line, "^"):
				if last != nil {
					last.lines = append(last.lines, line)
				}
			default:
				if _, name, ok := strings.Cut(line, " "); ok {
					last = &entry{lines: []string{line}}
					refs[name] = last
				}
			}
		}
		return header, refs
	}
	header, merged := parse(ref)
	_, theirs := parse(other)
	for name, e := range theirs {
		if _, ok := merged[name]; !ok {
			merged[name] = e
		}
	}
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	if header != "" {
		b.WriteString(header + "\n")
	}
	for _, name := range names {
		for _, line := range merged[name].lines {
			b.WriteString(line + "\n")
		}
	}
	return []byte(b.String())
}

// packedBranches lists the branches in a packed-refs file.
func packedBranches(data []byte) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), " ")
		if branch, isBranch := strings.CutPrefix(ref, "refs/heads/"); ok && isBranch && gitSHARe.MatchString(sha) {
			out[branch] = sha
		}
	}
	return out
}

// gitMetadata reports whether npath is git's own bookkeeping (refs, HEAD,
// index, config, logs) rather than an object. It syncs after everything else
// in a pass, so a ref never reaches a Mac before the objects it points to,
// and conflicts in a working tree are judged against this Mac's own index.
func gitMetadata(npath string) bool {
	_, inside, ok := gitDirSplit(npath)
	return ok && !strings.HasPrefix(inside, "objects/")
}

// sideChoice says which copy of a non-mergeable file that differs on both
// sides keeps its name.
type sideChoice struct {
	local bool // this Mac's copy wins
	keep  bool // the losing copy is kept as a conflict copy
	aside bool // ...in ~/.stradl/conflicts even where it would go beside the file
	share bool // ...as <file>.from-<this Mac> beside the file, which syncs
}

// chooseSide picks the winner of a non-mergeable conflict. The newest copy
// wins, except in project folders:
//   - a branch ref moves to whichever commit contains the other, keeping
//     nothing since nothing is lost; refs that diverged keep one side and
//     save the other commit as a stradl/ branch (keepLosingBranch);
//   - the first time this Mac meets a file another Mac already synced (the
//     folder existed on both before), the synced copy is the reference: it
//     stays, on every Mac, and this Mac's differing copy is kept. Work git
//     doesn't already hold (a modified, untracked or non-git file) is saved
//     as <file>.from-<this Mac>, which syncs so every Mac can see both;
//     an untouched or ignored file goes to ~/.stradl/conflicts on this Mac.
//     git's own files follow the reference too, so HEAD, the index and the
//     refs agree, and this Mac's commits stay reachable on stradl/ branches.
//
// localData and remoteData may be nil for files too large to hold in memory.
func (s *Syncer) chooseSide(npath string, lf *localFile, info os.FileInfo, localData, remoteData []byte, remoteTime time.Time) sideChoice {
	newest := sideChoice{local: remoteTime.IsZero() || info.ModTime().After(remoteTime), keep: true}
	if lf.root == nil || lf.root.kind != rootFolder {
		return newest
	}
	if hasGitSegment(npath) {
		if !s.repoSyncedBefore(npath) {
			return sideChoice{keep: true} // aside: conflictsAside covers .git
		}
		if isGitBranchRef(npath) && localData != nil && remoteData != nil {
			if local, ok := gitFastForward(lf.abs, localData, remoteData); ok {
				return sideChoice{local: local}
			}
		}
		return newest
	}
	if s.state.GetFile(npath) != nil {
		if lf.root.inherited {
			// Its state is from when it synced inside the folder holding
			// it: both sides changed since mostly means both pulled, so the
			// other copy goes aside instead of beside the file.
			newest.aside = true
		}
		return newest
	}
	if isSymlink(info) {
		return sideChoice{keep: true, aside: true}
	}
	if st := gitFileState(lf.abs); st == gitClean || st == gitIgnored {
		return sideChoice{keep: true, aside: true}
	}
	return sideChoice{keep: true, share: true}
}

// repoSyncedBefore reports whether this Mac had synced the git repository
// holding the .git path npath before this pass (it knows the repository's
// HEAD). A repository it never synced takes the reference copy's git files.
func (s *Syncer) repoSyncedBefore(npath string) bool {
	dir, _, ok := gitDirSplit(npath)
	if !ok {
		return true
	}
	_, ok = s.known[dir+"/HEAD"]
	return ok
}

// shareSuffix names the copy of a file this Mac keeps when another Mac's
// copy is the reference: <file>.from-<this Mac's name>.
func (s *Syncer) shareSuffix() string {
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(DeviceName(s.cfg)), "-"), "-")
	if slug == "" {
		slug = "other-mac"
	}
	return ".from-" + slug
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// gitFastForward compares two values of the branch ref at abs. When one
// commit is an ancestor of the other, the descendant wins (local reports
// whether it is this Mac's) and ok is true.
func gitFastForward(abs string, localData, remoteData []byte) (local, ok bool) {
	l, r := strings.TrimSpace(string(localData)), strings.TrimSpace(string(remoteData))
	if !gitSHARe.MatchString(l) || !gitSHARe.MatchString(r) {
		return false, false
	}
	gitDir := refGitDir(abs)
	if gitDir == "" {
		return false, false
	}
	isAncestor := func(a, b string) bool {
		_, err := runGit(filepath.Dir(gitDir), "--git-dir="+gitDir, "merge-base", "--is-ancestor", a, b)
		return err == nil
	}
	switch {
	case isAncestor(r, l):
		return true, true
	case isAncestor(l, r):
		return false, true
	}
	return false, false
}

// refGitDir returns the .git directory holding the loose ref file at abs.
func refGitDir(abs string) string {
	dir, inside, ok := gitDirSplit(filepath.ToSlash(abs))
	if !ok || !strings.HasPrefix(inside, "refs/") {
		return ""
	}
	return filepath.FromSlash(dir)
}

// gitUnionPath reports git objects and refs (loose refs and the reflogs that
// go with them), which keep union semantics (see unionPath).
func gitUnionPath(npath string) bool {
	_, inside, ok := gitDirSplit(npath)
	if !ok {
		return false
	}
	return strings.HasPrefix(inside, "objects/") || strings.HasPrefix(inside, "refs/") || strings.HasPrefix(inside, "logs/refs/")
}

// gitOperationState is what git keeps while a merge, cherry-pick, revert,
// rebase or bisect is under way in a checkout. It describes this Mac's
// checkout (whose index never syncs either); arriving on another Mac it
// would put that checkout mid-merge.
var gitOperationState = map[string]bool{
	"MERGE_HEAD": true, "MERGE_MSG": true, "MERGE_MODE": true, "MERGE_AUTOSTASH": true,
	"AUTO_MERGE": true, "CHERRY_PICK_HEAD": true, "REVERT_HEAD": true,
	"rebase-merge": true, "rebase-apply": true, "sequencer": true,
}

// gitPerMac reports the parts of .git that belong to one Mac and never sync:
// the index, a cache of this checkout's file stats that git rewrites on every
// status (it is rebuilt from HEAD when sync moves HEAD, see reindexRepos),
// an operation under way (gitOperationState), and linked worktrees' admin
// folders, which hold absolute paths into this
// Mac's home and which `git worktree prune` deletes on any Mac lacking them.
func gitPerMac(rel string) bool {
	_, inside, ok := gitDirSplit(rel)
	if !ok {
		return false
	}
	first, _, _ := strings.Cut(inside, "/")
	if gitOperationState[first] || strings.HasPrefix(first, "BISECT_") {
		return true
	}
	return inside == "index" || inside == "worktrees" || strings.HasPrefix(inside, "worktrees/")
}

// reindexMarker sits in a .git folder whose index needs rebuilding from HEAD
// because sync changed HEAD or a ref; Stradl's own files are never synced.
const reindexMarker = ".stradl-reindex"

// wroteGitFile runs after sync writes the file at abs. Inside a .git folder
// it makes sure the folders git needs exist (sync carries files, not empty
// folders, and git refuses a .git without refs/ and objects/), and when the
// file is HEAD, a ref or packed-refs it marks the index for rebuilding.
func (s *Syncer) wroteGitFile(abs string) {
	dir, inside, ok := gitDirSplit(filepath.ToSlash(abs))
	if !ok {
		return
	}
	gitDir := filepath.FromSlash(dir)
	for _, d := range []string{"refs/heads", "refs/tags", "objects/info", "objects/pack"} {
		_ = os.MkdirAll(filepath.Join(gitDir, filepath.FromSlash(d)), 0755)
	}
	if inside != "HEAD" && inside != "packed-refs" && !strings.HasPrefix(inside, "refs/heads/") {
		return
	}
	if f, err := os.OpenFile(filepath.Join(gitDir, reindexMarker), os.O_CREATE|os.O_WRONLY, 0600); err == nil {
		_ = f.Close()
	}
	s.reindexMu.Lock()
	if s.reindex == nil {
		s.reindex = make(map[string]bool)
	}
	s.reindex[gitDir] = true
	s.reindexMu.Unlock()
}

// reindexRepos rebuilds the index of every repository whose HEAD or refs
// sync changed, from the commit HEAD now names, leaving the files alone: the
// index never syncs, and one built for another commit would show changes
// nobody staged. It runs after a pass, for the folders the pass marked and
// any marker left by an earlier pass, and skips a repository while git holds
// a lock in it (the marker stays for next time). Staged-but-uncommitted
// changes in such a repository go back to unstaged; their content is kept.
func (s *Syncer) reindexRepos(roots []*syncRoot) {
	s.reindexMu.Lock()
	dirs := s.reindex
	s.reindex = nil
	s.reindexMu.Unlock()
	if dirs == nil {
		dirs = make(map[string]bool)
	}
	rootDirs := make(map[string]bool)
	for _, r := range roots {
		if r.kind == rootFolder {
			rootDirs[filepath.Clean(r.dir)] = true
		}
		if r.kind == rootFolder && !r.deferred {
			gd := filepath.Join(r.dir, ".git")
			if target, err := gitfileTarget(r.dir); err == nil && target != "" {
				// A submodule: its git data is in the superproject's
				// .git/modules (only inside home, past the privacy gate).
				t, ok := s.underHome(target)
				if !ok || s.folderGate(t) != nil {
					continue
				}
				gd = t
			}
			if _, err := os.Stat(filepath.Join(gd, reindexMarker)); err == nil {
				dirs[gd] = true
			}
		}
	}
	for gitDir := range dirs {
		if _, err := os.Stat(filepath.Join(gitDir, "index.lock")); err == nil {
			continue
		}
		work := gitWorkTree(gitDir)
		if work == "" {
			continue
		}
		if _, err := runGit(work, "--git-dir="+gitDir, "--work-tree="+work, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err != nil {
			continue // HEAD's commit hasn't arrived yet (or no commits): next pass
		}
		if _, err := runGit(work, "--git-dir="+gitDir, "--work-tree="+work, "read-tree", "HEAD"); err != nil {
			s.log("Warning: couldn't rebuild the git index in %s: %v", work, err)
			continue
		}
		_, _ = runGit(work, "--git-dir="+gitDir, "--work-tree="+work, "update-index", "-q", "--refresh")
		_ = os.Remove(filepath.Join(gitDir, reindexMarker))
		placeSubmodules(work, gitDir, rootDirs)
	}
}

// gitWorkTree returns the working tree of the git directory gitDir: the
// folder holding a .git folder, or for a submodule's .git/modules/<name>
// the core.worktree its config names ("" when unknown).
func gitWorkTree(gitDir string) string {
	if filepath.Base(gitDir) == ".git" {
		return filepath.Dir(gitDir)
	}
	out, err := runGit(gitDir, "--git-dir="+gitDir, "config", "--get", "core.worktree")
	wt := strings.TrimSpace(string(out))
	if err != nil || wt == "" {
		return ""
	}
	if !filepath.IsAbs(wt) {
		wt = filepath.Join(gitDir, wt)
	}
	return filepath.Clean(wt)
}

// placeSubmodules creates the empty folder of each submodule HEAD records
// that this Mac doesn't have, as a clone without its submodules has them:
// a submodule no session works in doesn't sync, and a missing folder makes
// git show it as deleted, one `git add -A` away from committing that. A
// submodule that syncs on its own (in roots) is left to its own sync: an
// empty folder there would make its files look deleted.
func placeSubmodules(work, gitDir string, roots map[string]bool) {
	out, err := runGit(work, "--git-dir="+gitDir, "--work-tree="+work, "ls-files", "-s", "-z")
	if err != nil {
		return
	}
	for _, entry := range strings.Split(string(out), "\x00") {
		meta, p, ok := strings.Cut(entry, "\t")
		if !ok || !strings.HasPrefix(meta, "160000 ") {
			continue
		}
		if p = filepath.ToSlash(filepath.Clean(filepath.FromSlash(p))); p == "." || strings.HasPrefix(p, "../") || filepath.IsAbs(p) || hasGitSegment(p) {
			continue
		}
		abs := filepath.Join(work, filepath.FromSlash(p))
		if roots[filepath.Clean(abs)] {
			continue
		}
		if _, err := os.Lstat(abs); errors.Is(err, fs.ErrNotExist) {
			_ = os.MkdirAll(abs, 0755)
		}
	}
}

// packedValue returns the commit a branch has in this Mac's packed-refs, or
// "". gitDir is the .git folder.
func packedValue(gitDir, branch string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		return ""
	}
	return packedBranches(data)[branch]
}

// shadowsPackedRef reports a loose branch ref arriving from storage for a
// branch this Mac keeps, with another commit, only in packed-refs: written
// as is it would override this Mac's branch. It returns the local value so
// the caller can merge the two like any ref conflict.
func shadowsPackedRef(npath, abs string) (string, bool) {
	branch, ok := gitBranchRef(npath)
	if !ok {
		return "", false
	}
	if _, err := os.Lstat(abs); err == nil {
		return "", false
	}
	gitDir := refGitDir(abs)
	if gitDir == "" {
		return "", false
	}
	v := packedValue(gitDir, branch)
	return v, v != ""
}

// objectCheck is a loose object this Mac deleted that storage still has.
type objectCheck struct {
	np, rel string
	root    *syncRoot
	obj     storage.ObjectInfo
}

// missingObjects returns the checks whose object git can't find in this
// Mac's repository at all, loose or packed: gc pruned it here, yet another
// Mac's commits may still build on it, so it is fetched again. Objects gc
// merely packed stay deleted. One git call per repository.
func (s *Syncer) missingObjects(checks []objectCheck, dryRun bool) []objectCheck {
	byRepo := make(map[string][]objectCheck)
	for _, c := range checks {
		dir, _, _ := gitDirSplit(filepath.ToSlash(filepath.Join(c.root.dir, filepath.FromSlash(c.rel))))
		gitDir := filepath.FromSlash(dir)
		byRepo[gitDir] = append(byRepo[gitDir], c)
	}
	var missing []objectCheck
	for gitDir, cs := range byRepo {
		var in strings.Builder
		for _, c := range cs {
			id, _ := gitLooseObjectID(c.np)
			in.WriteString(id + "\n")
		}
		out, err := runGitInput(gitDir, in.String(), "--git-dir="+gitDir, "cat-file", "--batch-check=%(objectname)")
		if err != nil {
			continue // can't tell: leave them as they are
		}
		lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		for i, c := range cs {
			if i < len(lines) && strings.HasSuffix(lines[i], " missing") {
				missing = append(missing, c)
			} else if !dryRun {
				s.state.MarkLocalDeleted(c.np, c.obj.ETag)
			}
		}
	}
	return missing
}

// runGitInput is runGit with stdin.
func runGitInput(dir, input string, args ...string) ([]byte, error) {
	bin := gitBinary()
	if bin == "" {
		return nil, exec.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	return cmd.Output()
}

// gitRefsReady reports whether every commit a ref file about to be written
// at abs names is already in this Mac's repository. A ref read from storage
// can be newer than the objects this pass listed (another Mac pushed
// meanwhile), or its objects can have failed to download; written anyway it
// would leave "bad object HEAD". The write waits for a later pass instead.
// Files that aren't refs, and Macs without git, always count as ready.
func gitRefsReady(abs string, data []byte) bool {
	dir, inside, ok := gitDirSplit(filepath.ToSlash(abs))
	if !ok {
		return true
	}
	gitDir := filepath.FromSlash(dir)
	var shas []string
	switch {
	case inside == "packed-refs":
		for _, sha := range packedBranches(data) {
			shas = append(shas, sha)
		}
	case inside == "HEAD", strings.HasPrefix(inside, "refs/heads/"), inside == "refs/stash":
		if sha := strings.TrimSpace(string(data)); gitSHARe.MatchString(sha) {
			shas = append(shas, sha)
		}
	}
	if len(shas) == 0 || gitBinary() == "" {
		return true
	}
	var in strings.Builder
	for _, sha := range shas {
		in.WriteString(sha + "^{commit}\n")
	}
	out, err := runGitInput(gitDir, in.String(), "--git-dir="+gitDir, "cat-file", "--batch-check=%(objectname)")
	if err != nil {
		return true // git couldn't answer (not a repository yet): don't block
	}
	return !strings.Contains(string(out), " missing")
}
