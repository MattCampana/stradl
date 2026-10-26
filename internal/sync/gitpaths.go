package sync

import (
	"regexp"
	"strings"
)

// gitDirFiles and gitDirDirs are the files and folders at the top of a git
// directory: what follows a submodule's name under .git/modules/<name>/.
var gitDirFiles = map[string]bool{
	"HEAD": true, "config": true, "index": true, "packed-refs": true, "description": true,
	"FETCH_HEAD": true, "ORIG_HEAD": true, "COMMIT_EDITMSG": true, "MERGE_HEAD": true,
	"MERGE_MSG": true, "MERGE_MODE": true, "MERGE_AUTOSTASH": true, "AUTO_MERGE": true,
	"CHERRY_PICK_HEAD": true, "REVERT_HEAD": true, "BISECT_LOG": true, "BISECT_START": true,
	"BISECT_TERMS": true, "BISECT_NAMES": true, "BISECT_EXPECTED_REV": true,
	"BISECT_ANCESTORS_OK": true, "shallow": true, "gitk.cache": true, reindexMarker: true,
}

var gitDirDirs = map[string]bool{
	"refs": true, "objects": true, "logs": true, "info": true, "hooks": true, "modules": true,
	"worktrees": true, "branches": true, "remotes": true, "lfs": true, "rebase-merge": true,
	"rebase-apply": true, "sequencer": true, "rr-cache": true, "fsmonitor--daemon": true,
}

// gitDirSplit splits a slash path inside a git directory into the innermost
// git directory holding it (a .git folder, or a submodule's
// .git/modules/<name> inside one, nested submodules included) and the path
// inside that. ok is false outside any .git folder.
func gitDirSplit(p string) (dir, inside string, ok bool) {
	i := strings.LastIndex("/"+p, "/.git/")
	if i < 0 {
		return "", "", false
	}
	dir, inside = p[:i+len(".git")], p[i+len(".git/"):]
	if rest, ok := strings.CutPrefix(inside, "modules/"); ok {
		if chain, in, ok := parseModules(rest); ok {
			dir, inside = dir+"/modules/"+strings.Join(chain, "/modules/"), in
		}
	}
	return dir, inside, true
}

// parseModules splits what follows .git/modules/ into the chain of
// submodule names (which may contain slashes) and the path inside the
// innermost one's git directory. It takes the shortest names for which the
// rest reads as a git directory path: infra/modules/HEAD is the HEAD of
// submodule infra/modules, lib/logs/refs/heads/main a reflog of lib.
func parseModules(rest string) (chain []string, inside string, ok bool) {
	for j := 0; j < len(rest); j++ {
		if rest[j] != '/' {
			continue
		}
		name, after := rest[:j], rest[j+1:]
		first, more, deeper := strings.Cut(after, "/")
		switch {
		case gitDirFiles[first] && !deeper:
			return []string{name}, after, true
		case first == "modules" && deeper:
			if sub, in, ok := parseModules(more); ok {
				return append([]string{name}, sub...), in, true
			}
		case gitDirDirs[first] && first != "modules" && deeper && gitSubpath(first, more):
			return []string{name}, after, true
		}
	}
	return nil, "", false
}

// gitSubpath reports whether more can lie in the git directory folder dir.
func gitSubpath(dir, more string) bool {
	first, _, deeper := strings.Cut(more, "/")
	switch dir {
	case "logs":
		return more == "HEAD" || first == "refs"
	case "hooks", "info":
		return !deeper && !gitDirFiles[first]
	}
	return true
}

// innermostModule returns, for rest (what follows a repository's
// .git/modules/), the submodule whose git data holds it, as a module path
// ("lib", or "a/modules/b" for b nested in a); "" when rest names none.
func innermostModule(rest string) string {
	chain, _, ok := parseModules(rest)
	if !ok {
		return ""
	}
	return strings.Join(chain, "/modules/")
}

// gitBranchRef returns the branch a loose ref file names:
// <folder>/.git/refs/heads/<branch>, a submodule's included.
func gitBranchRef(npath string) (string, bool) {
	_, inside, ok := gitDirSplit(npath)
	if !ok {
		return "", false
	}
	branch, ok := strings.CutPrefix(inside, "refs/heads/")
	return branch, ok && branch != ""
}

func isGitBranchRef(npath string) bool {
	_, ok := gitBranchRef(npath)
	return ok
}

// loose objects: <git dir>/objects/<2 hex>/<38 or 62 hex>
var gitLooseObjectRe = regexp.MustCompile(`^objects/([0-9a-f]{2})/([0-9a-f]{38}|[0-9a-f]{62})$`)

// gitLooseObjectID returns the object id of a loose object file.
func gitLooseObjectID(npath string) (string, bool) {
	_, inside, ok := gitDirSplit(npath)
	if !ok {
		return "", false
	}
	m := gitLooseObjectRe.FindStringSubmatch(inside)
	if m == nil {
		return "", false
	}
	return m[1] + m[2], true
}

func gitLooseObject(npath string) bool {
	_, ok := gitLooseObjectID(npath)
	return ok
}
