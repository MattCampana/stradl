package sync

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	gosync "sync"
	"time"
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

// gitUnmodified reports whether git tracks the file at abs and it matches
// what was last committed or staged, i.e. it holds no work of its own: the
// copy git would check out again. Anything git can't vouch for (no git, not a
// repository, untracked, ignored, modified) is treated as real work.
func gitUnmodified(abs string) bool {
	out, err := runGit(filepath.Dir(abs), "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching", "--", filepath.Base(abs))
	if err != nil || len(out) != 0 {
		return false
	}
	// Empty output also covers a path git doesn't know at all on an old git
	// without --ignored=matching support; make sure it is tracked.
	out, err = runGit(filepath.Dir(abs), "ls-files", "-z", "--error-unmatch", "--", filepath.Base(abs))
	return err == nil && len(out) != 0
}

// loose branch refs: <folder>/.git/refs/heads/<branch>
var gitBranchRefRe = regexp.MustCompile(`(^|/)\.git/refs/heads/(.+)$`)

var gitSHARe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// keptBranchPrefix holds branches the sync creates for commits that would
// otherwise only be reachable from a branch another Mac moved elsewhere.
const keptBranchPrefix = "stradl/"

// keepLosingBranch makes sure the commit a branch pointed to on the losing
// side of a conflict stays reachable: unless git confirms it is already part
// of the winning commit's history, it is saved as branch
// stradl/<branch>-<short sha> beside the original. abs is the winning ref
// file; loser is the losing side's content.
func keepLosingBranch(npath, abs string, winner, loser []byte) {
	m := gitBranchRefRe.FindStringSubmatch(npath)
	if m == nil || strings.HasPrefix(m[2], keptBranchPrefix) {
		return
	}
	win, lose := strings.TrimSpace(string(winner)), strings.TrimSpace(string(loser))
	if !gitSHARe.MatchString(lose) || win == lose {
		return
	}
	gitDir := refGitDir(abs)
	if gitDir == "" {
		return
	}
	headsDir := filepath.Join(gitDir, "refs", "heads")
	if gitSHARe.MatchString(win) {
		// Exit 0: the losing commit is an ancestor, nothing is lost.
		if _, err := runGit(filepath.Dir(gitDir), "--git-dir="+gitDir, "merge-base", "--is-ancestor", lose, win); err == nil {
			return
		}
	}
	name := keptBranchPrefix + m[2] + "-" + lose[:7]
	p := filepath.Join(headsDir, filepath.FromSlash(name))
	if existing, err := os.ReadFile(p); err == nil && bytes.Equal(bytes.TrimSpace(existing), []byte(lose)) {
		return
	}
	_ = writeFileAtomic(freeName(p), []byte(lose+"\n"), 0644)
}

// gitMetadata reports whether npath is git's own bookkeeping (refs, HEAD,
// index, config, logs) rather than an object. It syncs after everything else
// in a pass, so a ref never reaches a Mac before the objects it points to,
// and conflicts in a working tree are judged against this Mac's own index.
func gitMetadata(npath string) bool {
	i := strings.Index(npath, "/.git/")
	switch {
	case i >= 0:
		npath = npath[i+len("/.git/"):]
	case strings.HasPrefix(npath, ".git/"):
		npath = strings.TrimPrefix(npath, ".git/")
	default:
		return false
	}
	return !strings.HasPrefix(npath, "objects/") && !strings.Contains(npath, "/objects/")
}

// sideChoice says which copy of a non-mergeable file that differs on both
// sides keeps its name.
type sideChoice struct {
	local bool // this Mac's copy wins
	keep  bool // the losing copy is kept as a conflict copy
	aside bool // ...in ~/.stradl/conflicts even where it would go beside the file
}

// chooseSide picks the winner of a non-mergeable conflict. The newest copy
// wins, except in project folders:
//   - a branch ref moves to whichever commit contains the other, keeping
//     nothing since nothing is lost; refs that diverged keep one side and
//     save the other commit as a stradl/ branch (keepLosingBranch);
//   - the first time this Mac meets a file another Mac already synced (the
//     folder existed on both before), git's own files follow whichever Mac's
//     checkout leads (repoVerdict), so a ref, the index and HEAD always come
//     from the same Mac; in the working tree this Mac's copy stays, unless
//     the other Mac leads and git says this copy is an untouched checkout
//     file, in which case the other Mac's edit wins and this copy (which git
//     still holds) goes to the conflicts folder.
//
// localData and remoteData may be nil for files too large to hold in memory.
func (s *Syncer) chooseSide(ctx context.Context, npath string, lf *localFile, info os.FileInfo, localData, remoteData []byte, remoteTime time.Time) sideChoice {
	newest := sideChoice{local: remoteTime.IsZero() || info.ModTime().After(remoteTime), keep: true}
	if lf.root == nil || lf.root.kind != rootFolder {
		return newest
	}
	first := s.state.GetFile(npath) == nil
	if gitBranchRefRe.MatchString(npath) && localData != nil && remoteData != nil {
		if local, ok := gitFastForward(lf.abs, localData, remoteData); ok {
			return sideChoice{local: local}
		}
	}
	verdict := repoUnknown
	if first {
		verdict = s.repoVerdict(ctx, lf)
	}
	if hasGitSegment(npath) {
		switch verdict {
		case repoLocalLeads:
			return sideChoice{local: true, keep: true}
		case repoRemoteLeads:
			return sideChoice{local: false, keep: true}
		}
		return newest
	}
	if !first {
		return newest
	}
	if verdict == repoRemoteLeads && gitUnmodified(lf.abs) {
		return sideChoice{local: false, keep: true, aside: true}
	}
	return sideChoice{local: true, keep: true}
}

// repoVerdict says, for a git repository that exists on this Mac and on
// another Mac, whose checkout leads.
type repoVerdict int

const (
	repoUnknown     repoVerdict = iota // not a repository, or git can't tell
	repoLocalLeads                     // this Mac's HEAD contains the other Mac's
	repoRemoteLeads                    // the other Mac's HEAD contains this Mac's, or they match
)

// repoVerdicts caches repoVerdict per repository for one pass.
type repoVerdicts struct {
	mu gosync.Mutex
	m  map[string]*repoVerdictEntry
}

type repoVerdictEntry struct {
	once gosync.Once
	v    repoVerdict
}

// repoVerdict compares this Mac's HEAD with the HEAD the other Mac uploaded
// for the repository holding lf. It runs after the pass downloaded the other
// Mac's objects and before it touches any ref, so both commits are on disk
// and this Mac's refs are still its own. Diverged histories go to the side
// with the newer commit.
func (s *Syncer) repoVerdict(ctx context.Context, lf *localFile) repoVerdict {
	repoDir, prefix := repoOf(lf)
	if repoDir == "" || s.repos == nil {
		return repoUnknown
	}
	s.repos.mu.Lock()
	e := s.repos.m[repoDir]
	if e == nil {
		e = &repoVerdictEntry{}
		s.repos.m[repoDir] = e
	}
	s.repos.mu.Unlock()
	e.once.Do(func() { e.v = s.computeRepoVerdict(ctx, repoDir, prefix) })
	return e.v
}

func (s *Syncer) computeRepoVerdict(ctx context.Context, repoDir, prefix string) repoVerdict {
	remote := s.remoteHead(ctx, prefix)
	if remote == "" {
		return repoUnknown
	}
	out, err := runGit(repoDir, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	local := strings.TrimSpace(string(out))
	if err != nil || local == "" {
		// No commits here yet: the other Mac's history leads.
		if _, err := runGit(repoDir, "rev-parse", "--git-dir"); err == nil {
			return repoRemoteLeads
		}
		return repoUnknown
	}
	if local == remote {
		return repoRemoteLeads
	}
	if _, err := runGit(repoDir, "cat-file", "-e", remote+"^{commit}"); err != nil {
		return repoUnknown // the other Mac's commit isn't here: can't compare
	}
	isAncestor := func(a, b string) bool {
		_, err := runGit(repoDir, "merge-base", "--is-ancestor", a, b)
		return err == nil
	}
	switch {
	case isAncestor(local, remote):
		return repoRemoteLeads
	case isAncestor(remote, local):
		return repoLocalLeads
	}
	commitTime := func(sha string) int64 {
		out, err := runGit(repoDir, "show", "-s", "--format=%ct", sha)
		if err != nil {
			return 0
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		return n
	}
	if commitTime(remote) > commitTime(local) {
		return repoRemoteLeads
	}
	return repoLocalLeads
}

// remoteHead returns the commit the other Macs' copy of the repository at
// npath prefix has checked out, read from its uploaded HEAD and refs, or "".
func (s *Syncer) remoteHead(ctx context.Context, prefix string) string {
	read := func(rel string) string {
		np := prefix + rel
		data, _, err := s.fetch(ctx, np, s.engineRemoteKey(np), nil, s.home)
		if err != nil {
			return ""
		}
		return string(data)
	}
	head := strings.TrimSpace(read(".git/HEAD"))
	if gitSHARe.MatchString(head) {
		return head // detached
	}
	ref, ok := strings.CutPrefix(head, "ref: ")
	if !ok || !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") {
		return ""
	}
	if sha := strings.TrimSpace(read(".git/" + ref)); gitSHARe.MatchString(sha) {
		return sha
	}
	for _, line := range strings.Split(read(".git/packed-refs"), "\n") {
		if sha, name, ok := strings.Cut(strings.TrimSpace(line), " "); ok && name == ref && gitSHARe.MatchString(sha) {
			return sha
		}
	}
	return ""
}

// repoOf finds the git repository holding lf: the folder that contains .git
// on the way up from the file, staying inside the sync root. It returns the
// repository's local path and its npath prefix.
func repoOf(lf *localFile) (dir, prefix string) {
	rel := lf.rel
	if i := strings.Index("/"+rel, "/.git/"); i >= 0 {
		rel = strings.TrimSuffix(rel[:i], "/")
		return filepath.Join(lf.root.dir, filepath.FromSlash(rel)), lf.root.prefix + relPrefix(rel)
	}
	for rel = path.Dir(rel); ; rel = path.Dir(rel) {
		if rel == "." {
			rel = ""
		}
		dir := filepath.Join(lf.root.dir, filepath.FromSlash(rel))
		if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
			return dir, lf.root.prefix + relPrefix(rel)
		}
		if rel == "" {
			return "", ""
		}
	}
}

func relPrefix(rel string) string {
	if rel == "" {
		return ""
	}
	return rel + "/"
}

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
	slash := filepath.ToSlash(abs)
	i := strings.LastIndex(slash, "/.git/refs/")
	if i < 0 {
		return ""
	}
	return filepath.FromSlash(slash[:i+len("/.git")])
}
