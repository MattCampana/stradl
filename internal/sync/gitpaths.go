package sync

import (
	"regexp"
	"strings"
)

// gitDirEntries are the names at the top of a git directory: what follows a
// submodule's name under .git/modules/<name>/.
var gitDirEntries = map[string]bool{
	"HEAD": true, "config": true, "index": true, "packed-refs": true, "refs": true,
	"objects": true, "logs": true, "info": true, "hooks": true, "modules": true,
	"worktrees": true, "description": true, "branches": true, "remotes": true,
	"FETCH_HEAD": true, "ORIG_HEAD": true, "COMMIT_EDITMSG": true, "MERGE_HEAD": true,
	"MERGE_MSG": true, "MERGE_MODE": true, "MERGE_AUTOSTASH": true, "AUTO_MERGE": true,
	"CHERRY_PICK_HEAD": true, "REVERT_HEAD": true, "BISECT_LOG": true, "BISECT_START": true,
	"BISECT_TERMS": true, "BISECT_NAMES": true, "BISECT_EXPECTED_REV": true,
	"BISECT_ANCESTORS_OK": true, "shallow": true, "lfs": true, "rebase-merge": true,
	"rebase-apply": true, "sequencer": true, "rr-cache": true, "gitk.cache": true,
	"fsmonitor--daemon": true, reindexMarker: true,
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
	for {
		rest, ok := strings.CutPrefix(inside, "modules/")
		if !ok {
			break
		}
		name, after := splitModuleName(rest)
		if name == "" {
			break
		}
		dir, inside = dir+"/modules/"+name, after
	}
	return dir, inside, true
}

// splitModuleName splits what follows .git/modules/ at the end of a
// submodule's name, which may contain slashes: the shortest leading path
// whose next segment is a git directory entry.
func splitModuleName(rest string) (name, after string) {
	for j := 0; j < len(rest); j++ {
		if rest[j] != '/' {
			continue
		}
		first, _, _ := strings.Cut(rest[j+1:], "/")
		if gitDirEntries[first] {
			return rest[:j], rest[j+1:]
		}
	}
	return "", ""
}

// innermostModule returns, for rest (what follows a repository's
// .git/modules/), the submodule whose git data holds it, as a module path
// ("lib", or "a/modules/b" for b nested in a); "" when rest names no
// submodule's data.
func innermostModule(rest string) string {
	name, after := splitModuleName(rest)
	for name != "" {
		inner, ok := strings.CutPrefix(after, "modules/")
		if !ok {
			return name
		}
		n, a := splitModuleName(inner)
		if n == "" {
			return name
		}
		name, after = name+"/modules/"+n, a
	}
	return ""
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
