// Package profiles keeps one Claude desktop app profile per Claude account.
//
// The Claude app keeps its sign-in, windows and settings in one data folder
// (~/Library/Application Support/Claude) and is signed in to one account at a
// time. Started with --user-data-dir it uses another folder instead, as an
// independent instance with its own sign-in. Stradl gives each account such a
// folder (~/Library/Application Support/Stradl/Profiles/<account>), so every
// account stays signed in and Stradl opens the one asked for. The app's own
// folder is the main profile, signed in to whichever account it is.
//
// Every profile shares the main folder's sidebar records: its
// claude-code-sessions is a link to the main one. The app shows only the
// signed-in account's part of it, and the records sync once, whichever
// profile wrote them. Nothing here deletes: a folder moved out of the way is
// renamed, never removed.
package profiles

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
)

const (
	// MainID names the Claude app's own data folder.
	MainID = "main"

	sessionsDir       = "claude-code-sessions"
	appConfigName     = "config.json"
	desktopConfigName = "claude_desktop_config.json"
	listFile          = "profiles.json"
)

var (
	accountIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	profileIDRe = regexp.MustCompile(`^[0-9a-z][0-9a-z-]{2,63}$`)
)

// Profile is one Claude app data folder.
type Profile struct {
	ID   string `json:"id"`   // MainID, or the folder's name
	Dir  string `json:"dir"`  // the data folder
	Main bool   `json:"main"` // the app's own folder (opened without --user-data-dir)
	// Account is the account it was signed in to last (the app's
	// config.json), else the one it was made for; "" for a new profile no
	// one has signed in to yet.
	Account  string `json:"account,omitempty"`
	For      string `json:"for,omitempty"` // the account Stradl made it for
	SignedIn bool   `json:"signedIn"`      // it holds a sign-in
	Running  bool   `json:"running"`       // a Claude window is open for it
	PID      int    `json:"pid,omitempty"`
}

// Env says where the profiles are. Instances lists the Claude app's running
// instances (process ID to data folder); nil reads the real processes.
type Env struct {
	MainDir     string
	ProfilesDir string
	StateDir    string
	Instances   func() map[int]string
}

// FromConfig returns the Env for cfg.
func FromConfig(cfg *config.Config) Env {
	state := cfg.StateDirOverride
	if state == "" {
		state = config.ConfigDirPath()
	}
	return Env{MainDir: cfg.ClaudeAppDir(), ProfilesDir: cfg.EffectiveProfilesDir(), StateDir: state}
}

// list is profiles.json: what Stradl made each profile for, and when it
// first turned this Mac's accounts into profiles.
type list struct {
	Profiles  map[string]listEntry `json:"profiles,omitempty"`
	Converted time.Time            `json:"converted,omitzero"`
}

type listEntry struct {
	For     string    `json:"for,omitempty"`
	Created time.Time `json:"created"`
}

func (e Env) readList() list {
	var l list
	if data, err := os.ReadFile(filepath.Join(e.StateDir, listFile)); err == nil {
		_ = json.Unmarshal(data, &l)
	}
	if l.Profiles == nil {
		l.Profiles = make(map[string]listEntry)
	}
	return l
}

func (e Env) writeList(l list) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(e.StateDir, 0700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(e.StateDir, listFile), data, 0600)
}

// readApp reads a data folder's config.json: the account it was signed in
// to last and whether it holds a sign-in now.
func readApp(dir string) (account string, signedIn bool) {
	data, err := os.ReadFile(filepath.Join(dir, appConfigName))
	if err != nil {
		return "", false
	}
	var c map[string]json.RawMessage
	if json.Unmarshal(data, &c) != nil {
		return "", false
	}
	var id string
	if raw, ok := c["lastKnownAccountUuid"]; ok && json.Unmarshal(raw, &id) == nil && accountIDRe.MatchString(strings.ToLower(id)) {
		account = strings.ToLower(id)
	}
	for _, k := range []string{"oauth:tokenCache", "oauth:tokenCacheV2"} {
		var v string
		if raw, ok := c[k]; ok && json.Unmarshal(raw, &v) == nil && v != "" {
			signedIn = true
		}
	}
	return account, signedIn
}

// List returns the main profile and every profile Stradl made, main first.
func (e Env) List() []Profile {
	l := e.readList()
	running := e.running()
	main := e.profile(MainID, e.MainDir, "", running)
	main.Main = true
	out := []Profile{main}
	entries, _ := os.ReadDir(e.ProfilesDir)
	for _, d := range entries {
		if !d.IsDir() || !profileIDRe.MatchString(d.Name()) {
			continue
		}
		out = append(out, e.profile(d.Name(), filepath.Join(e.ProfilesDir, d.Name()), l.Profiles[d.Name()].For, running))
	}
	sort.SliceStable(out[1:], func(i, j int) bool { return out[1+i].ID < out[1+j].ID })
	return out
}

func (e Env) profile(id, dir, made string, running map[string]int) Profile {
	if made == "" && accountIDRe.MatchString(id) {
		made = id // a folder named for its account
	}
	p := Profile{ID: id, Dir: dir, For: made}
	p.Account, p.SignedIn = readApp(dir)
	if p.Account == "" {
		p.Account = made
	}
	if pid, ok := running[filepath.Clean(dir)]; ok {
		p.Running, p.PID = true, pid
	}
	return p
}

// running maps each running instance's data folder to its process.
func (e Env) running() map[string]int {
	inst := e.Instances
	if inst == nil {
		inst = func() map[int]string { return Instances(e.MainDir) }
	}
	out := make(map[string]int)
	for pid, dir := range inst() {
		dir = filepath.Clean(dir)
		if old, ok := out[dir]; !ok || pid < old {
			out[dir] = pid
		}
	}
	return out
}

// Get returns the profile with id.
func (e Env) Get(id string) (Profile, bool) {
	for _, p := range e.List() {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

// ForAccount returns the profile to open for acct: one signed in to it (its
// own profile before the main one), else the one made for it, which needs a
// sign-in. ok is false when there is none yet (see Ensure).
func ForAccount(list []Profile, acct string) (Profile, bool) {
	acct = strings.ToLower(acct)
	rank := func(p Profile) int {
		switch {
		case p.Account == acct && p.SignedIn && p.For == acct && !p.Main:
			return 4
		case p.Account == acct && p.Main: // signed in to it last
			return 3
		case p.Account == acct && p.SignedIn:
			return 2
		case p.For == acct && (p.Account == acct || !p.SignedIn):
			return 1
		}
		return 0
	}
	best, bestRank := Profile{}, 0
	for _, p := range list {
		if r := rank(p); r > bestRank {
			best, bestRank = p, r
		}
	}
	return best, bestRank > 0
}

// Ensure returns acct's profile, making one when it has none.
func (e Env) Ensure(acct string) (Profile, error) {
	acct = strings.ToLower(acct)
	if !accountIDRe.MatchString(acct) {
		return Profile{}, fmt.Errorf("%q is not a Claude account ID", acct)
	}
	if p, ok := ForAccount(e.List(), acct); ok {
		return p, nil
	}
	return e.createFor(acct)
}

// createFor makes a profile for acct: the folder named after it, or, when
// that folder is someone else's now (signed in to another account), a new
// one.
func (e Env) createFor(acct string) (Profile, error) {
	id := acct
	if _, err := os.Lstat(filepath.Join(e.ProfilesDir, id)); err == nil {
		var err error
		if id, err = newID(); err != nil {
			return Profile{}, err
		}
	}
	return e.create(id, acct)
}

// New makes a profile for an account Stradl hasn't seen yet: whoever signs
// in to it decides whose it is.
func (e Env) New() (Profile, error) {
	id, err := newID()
	if err != nil {
		return Profile{}, err
	}
	return e.create(id, "")
}

func newID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "new-" + hex.EncodeToString(b), nil
}

// SidebarStateFiles names the files Stradl keeps for a profile's sidebar
// layout (package sync merges it): the last layout it agreed on with
// storage, and one waiting for the profile to quit. The main profile's are
// sidebar-base.json and sidebar-pending.json; another's carry its ID.
func SidebarStateFiles(id string) (base, pending string) {
	if id == MainID {
		return "sidebar-base.json", "sidebar-pending.json"
	}
	return "sidebar-base." + id + ".json", "sidebar-pending." + id + ".json"
}

// create makes the profile folder id, which must not exist yet. Callers
// that run beside a sync hold the sync lock: the preferences file and its
// sidebar base are copied as one pair.
func (e Env) create(id, acct string) (Profile, error) {
	if e.ProfilesDir == "" || e.MainDir == "" {
		return Profile{}, errors.New("no home folder")
	}
	if err := os.MkdirAll(e.ProfilesDir, 0700); err != nil {
		return Profile{}, err
	}
	dir := filepath.Join(e.ProfilesDir, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		return Profile{}, fmt.Errorf("profile %s: %w", id, err) // never someone else's folder
	}
	if err := e.Link(dir); err != nil {
		return Profile{}, err
	}
	// The app's preferences (MCP servers, the sidebar layout): the new
	// profile starts with the main one's, then keeps its own. Its sidebar
	// base starts as the main one's too, the layout that file agreed on
	// with storage: without it, the first merge would read whatever the
	// main file hasn't caught up on yet (changes from other Macs waiting
	// for Claude to quit) as this profile's own and send it back.
	err := copyFile(filepath.Join(e.MainDir, desktopConfigName), filepath.Join(dir, desktopConfigName))
	switch {
	case err == nil:
		mainBase, _ := SidebarStateFiles(MainID)
		base, _ := SidebarStateFiles(id)
		if err := copyFile(filepath.Join(e.StateDir, mainBase), filepath.Join(e.StateDir, base)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Profile{}, err
		}
	case !errors.Is(err, os.ErrNotExist):
		return Profile{}, err
	}
	l := e.readList()
	l.Profiles[id] = listEntry{For: acct, Created: time.Now().UTC()}
	if err := e.writeList(l); err != nil {
		return Profile{}, err
	}
	p, _ := e.Get(id)
	return p, nil
}

// Link makes dir's sidebar records the main profile's: claude-code-sessions
// becomes a link to the main folder's. A real folder there (the app made its
// own) has its records added to the main folder first, the newer copy of a
// record winning, and is then renamed aside. Call it only while dir's
// instance isn't running.
func (e Env) Link(dir string) error {
	target := filepath.Join(e.MainDir, sessionsDir)
	if err := os.MkdirAll(target, 0700); err != nil {
		return err
	}
	p := filepath.Join(dir, sessionsDir)
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return os.Symlink(target, p)
	case err != nil:
		return err
	case fi.Mode()&os.ModeSymlink != 0:
		if dest, err := os.Readlink(p); err == nil && filepath.Clean(dest) == filepath.Clean(target) {
			return nil
		}
		if err := os.Remove(p); err != nil { // a link elsewhere: only the link goes
			return err
		}
		return os.Symlink(target, p)
	case !fi.IsDir():
		return fmt.Errorf("%s is not a folder", p)
	}
	if err := mergeRecords(p, target); err != nil {
		return err
	}
	aside := fmt.Sprintf("%s.stradl-%d", p, time.Now().Unix())
	if err := os.Rename(p, aside); err != nil {
		return err
	}
	return os.Symlink(target, p)
}

// mergeRecords copies the sidebar records in from into to: each one to has
// no copy of, or an older one (by lastActivityAt).
func mergeRecords(from, to string) error {
	matches, _ := filepath.Glob(filepath.Join(from, "*", "*", "local_*.json"))
	for _, m := range matches {
		rel, err := filepath.Rel(from, m)
		if err != nil {
			continue
		}
		dst := filepath.Join(to, rel)
		if theirs, err := os.ReadFile(dst); err == nil {
			mine, err := os.ReadFile(m)
			if err != nil || lastActivity(mine) <= lastActivity(theirs) {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return err
		}
		if err := copyFile(m, dst); err != nil {
			return err
		}
	}
	return nil
}

func lastActivity(data []byte) int64 {
	var r struct {
		LastActivityAt int64 `json:"lastActivityAt"`
	}
	_ = json.Unmarshal(data, &r)
	return r.LastActivityAt
}

// Convert turns this Mac's accounts into profiles, once and for every
// account met since: each account in accounts that no profile is signed in
// to or made for gets one (signed out until its first sign-in). The main
// profile stays as it is, the profile of the account it's signed in to.
// Profiles not running get their records link checked. It returns the
// profiles it made.
func (e Env) Convert(accounts []string) ([]Profile, error) {
	var made []Profile
	list := e.List()
	for _, p := range list {
		if !p.Main && !p.Running {
			if err := e.Link(p.Dir); err != nil {
				return made, fmt.Errorf("profile %s: %w", p.ID, err)
			}
		}
	}
	for _, acct := range accounts {
		acct = strings.ToLower(acct)
		if !accountIDRe.MatchString(acct) {
			continue
		}
		if _, ok := ForAccount(list, acct); ok {
			continue
		}
		p, err := e.createFor(acct)
		if err != nil {
			return made, err
		}
		made = append(made, p)
		list = append(list, p)
	}
	l := e.readList()
	if l.Converted.IsZero() {
		l.Converted = time.Now().UTC()
		if err := e.writeList(l); err != nil {
			return made, err
		}
	}
	return made, nil
}

// Converted reports whether this Mac's accounts were turned into profiles.
func (e Env) Converted() bool { return !e.readList().Converted.IsZero() }

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".stradl-tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".stradl-tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
