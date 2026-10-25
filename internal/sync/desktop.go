package sync

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	gosync "sync"
)

// DesktopPrefix namespaces the Claude desktop app's Code-tab session index in
// the sync namespace and in remote storage. Each sidebar entry is one record,
// <account>/<org>/local_<id>.json, that points at a CLI transcript
// (cliSessionId) and a working folder (cwd). The app reads these records at
// launch, so synced entries appear in the sidebar the next time it starts.
const DesktopPrefix = "_desktop/sessions/"

// desktopHintKey carries folder-resolution hints inside an uploaded record. It
// is added on upload and removed before the record is written on any device.
const desktopHintKey = "_stradl"

var desktopRecordRe = regexp.MustCompile(`^[0-9A-Fa-f-]{8,36}/[0-9A-Fa-f-]{8,36}/local_[A-Za-z0-9_-]+\.json$`)

// isDesktopRecord reports whether rel (relative to the desktop sessions dir)
// is a sidebar session record. Tombstones, indexes and app-private state are
// left alone.
func isDesktopRecord(rel string) bool {
	return desktopRecordRe.MatchString(rel)
}

type desktopHint struct {
	GitRemote string `json:"gitRemote,omitempty"`
	RepoSub   string `json:"repoSub,omitempty"`
}

// decodeRecord parses a record keeping numbers exact (timestamps are
// millisecond integers).
func decodeRecord(data []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// prepareDesktopUpload adds a hint naming the git remote of the session's
// folder, so another device can find its own clone of the same repository
// even when it lives somewhere else there.
func prepareDesktopUpload(data []byte) []byte {
	m, ok := decodeRecord(data)
	if !ok {
		return data
	}
	cwd, _ := m["cwd"].(string)
	if cwd == "" {
		return data
	}
	repo := findRepoRoot(cwd)
	if repo == "" {
		return data
	}
	remote := gitOriginURL(repo)
	if remote == "" {
		return data
	}
	sub, err := filepath.Rel(repo, cwd)
	if err != nil || sub == "." {
		sub = ""
	}
	m[desktopHintKey] = desktopHint{GitRemote: remote, RepoSub: filepath.ToSlash(sub)}
	out, err := json.Marshal(m)
	if err != nil {
		return data
	}
	return out
}

// finishDesktopDownload removes the hint from a record whose paths were
// already resolved for this device, and makes sure its folder exists here. A
// folder that is missing locally is replaced by this device's clone of the
// same git repository when there is one; a missing non-repository folder under
// home (such as an app scratch folder) is created empty so the session opens.
// A repository with no local clone is left as is, and the app reports the
// folder missing.
func finishDesktopDownload(data []byte, home string, repos *repoIndex) []byte {
	m, ok := decodeRecord(data)
	if !ok {
		return data
	}
	var hint desktopHint
	raw, hadHint := m[desktopHintKey]
	if hadHint {
		if b, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(b, &hint)
		}
		delete(m, desktopHintKey)
	}

	cwd, _ := m["cwd"].(string)
	if cwd != "" && !dirExists(cwd) {
		switch {
		case hint.GitRemote != "":
			if clone := repos.find(hint.GitRemote); clone != "" {
				target := clone
				if hint.RepoSub != "" {
					target = filepath.Join(clone, filepath.FromSlash(hint.RepoSub))
				}
				rewritePathFields(m, cwd, target)
			}
		case home != "" && isWithin(home, cwd) && filepath.Clean(cwd) != filepath.Clean(home):
			_ = os.MkdirAll(cwd, 0700)
		}
	}

	if !hadHint && m["cwd"] == cwd {
		return data
	}
	out, err := json.Marshal(m)
	if err != nil {
		return data
	}
	return out
}

// rewritePathFields replaces the from prefix with to in every top-level string
// field that is from itself or a path below it (cwd, originCwd, worktree and
// git anchor folders).
func rewritePathFields(m map[string]any, from, to string) {
	from = filepath.Clean(from)
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s == from {
			m[k] = to
		} else if strings.HasPrefix(s, from+string(filepath.Separator)) {
			m[k] = to + s[len(from):]
		}
	}
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// findRepoRoot returns the nearest ancestor of dir (or dir) holding .git.
func findRepoRoot(dir string) string {
	dir = filepath.Clean(dir)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

var gitURLRe = regexp.MustCompile(`^\s*url\s*=\s*(.+?)\s*$`)

// gitOriginURL reads remote "origin"'s URL from a repository's config without
// running git. Worktrees (.git is a file) are followed to their common dir.
func gitOriginURL(repo string) string {
	gitPath := filepath.Join(repo, ".git")
	if fi, err := os.Stat(gitPath); err == nil && !fi.IsDir() {
		data, err := os.ReadFile(gitPath)
		if err != nil {
			return ""
		}
		line := strings.TrimSpace(string(data))
		dir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(repo, dir)
		}
		if common, err := os.ReadFile(filepath.Join(dir, "commondir")); err == nil {
			c := strings.TrimSpace(string(common))
			if !filepath.IsAbs(c) {
				c = filepath.Join(dir, c)
			}
			dir = c
		}
		gitPath = dir
	}
	f, err := os.Open(filepath.Join(gitPath, "config"))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	inOrigin := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if inOrigin {
			if mm := gitURLRe.FindStringSubmatch(line); mm != nil {
				return mm[1]
			}
		}
	}
	return ""
}

// canonicalRemote reduces the spellings of one repository's URL (https, ssh,
// scp-like, with or without .git or credentials) to host/owner/repo.
func canonicalRemote(u string) string {
	u = strings.TrimSpace(strings.ToLower(u))
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	} else if at := strings.Index(u, "@"); at >= 0 && strings.Contains(u[at:], ":") {
		// scp-like: git@github.com:owner/repo
		u = strings.Replace(u[at+1:], ":", "/", 1)
	}
	if at := strings.LastIndex(u, "@"); at >= 0 {
		u = u[at+1:]
	}
	if host, rest, ok := strings.Cut(u, "/"); ok {
		host = strings.Split(host, ":")[0] // drop ssh port
		u = host + "/" + rest
	}
	return u
}

// repoIndex finds local clones by remote URL. It is built lazily, once per
// sync, by scanning the usual project folders under home a few levels deep.
type repoIndex struct {
	home  string
	extra []string
	once  gosync.Once
	byURL map[string]string
}

// repoScanRoots are home-relative folders searched for clones.
var repoScanRoots = []string{
	"code", "Code", "src", "dev", "Developer", "Projects", "projects", "repos",
	"git", "GitHub", "github", "work", "Work", "workspace", "Documents", "Desktop",
}

const repoScanDepth = 4

func newRepoIndex(home string, extra ...string) *repoIndex {
	return &repoIndex{home: home, extra: extra}
}

func (r *repoIndex) find(remote string) string {
	if r == nil || r.home == "" {
		return ""
	}
	r.once.Do(r.build)
	return r.byURL[canonicalRemote(remote)]
}

func (r *repoIndex) build() {
	r.byURL = make(map[string]string)
	seen := make(map[string]bool)
	roots := append([]string{}, r.extra...)
	for _, name := range repoScanRoots {
		roots = append(roots, filepath.Join(r.home, name))
	}
	for _, root := range roots {
		r.scan(root, 0, seen)
	}
}

func (r *repoIndex) scan(dir string, depth int, seen map[string]bool) {
	if depth > repoScanDepth || seen[dir] {
		return
	}
	seen[dir] = true
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if u := gitOriginURL(dir); u != "" {
			key := canonicalRemote(u)
			if _, dup := r.byURL[key]; !dup {
				r.byURL[key] = dir
			}
		}
		return // don't descend into a repository
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || name == "node_modules" || name == "Library" {
			continue
		}
		r.scan(filepath.Join(dir, name), depth+1, seen)
	}
}
