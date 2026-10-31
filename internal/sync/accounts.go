package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/storage"
)

// Claude accounts. A Mac can be signed in to several Claude accounts over
// time: the desktop app keeps each account's sidebar under
// claude-code-sessions/<account>/<org>/, while ~/.claude is shared by all of
// them. The account registry, kept in storage for the whole Circle, names
// the accounts, records which ones sync, and which account each session and
// project folder belongs to. Sessions of an account that doesn't sync stay on
// the Mac they were made on; nothing already in storage is deleted.

// AccountsKey is where the account registry lives in storage. The engine
// ignores everything under _metadata/.
const AccountsKey = "_metadata/accounts.json.age"

// accountsCacheFile is this Mac's copy of the registry, for the agent's
// probes and the app, which don't read storage.
const accountsCacheFile = "accounts.json"

// accountIDRe matches account and organization IDs (UUIDs).
var accountIDRe = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

// AccountInfo is one Claude account in the registry.
type AccountInfo struct {
	Email string            `json:"email,omitempty"`
	Label string            `json:"label,omitempty"` // the user's name for it
	Orgs  map[string]string `json:"orgs,omitempty"`  // organization ID -> name ("" unknown)
	// Sync is the user's choice; nil until they make one (the account syncs
	// meanwhile, as before accounts were known). SyncAt orders choices made
	// on different Macs: the latest wins.
	Sync    *bool     `json:"sync,omitempty"`
	SyncAt  time.Time `json:"syncAt,omitempty"`
	LabelAt time.Time `json:"labelAt,omitempty"`
}

// AccountRegistry is the Circle's record of Claude accounts.
type AccountRegistry struct {
	Accounts map[string]*AccountInfo `json:"accounts"`
	// Sessions maps a session (transcript) ID to its account.
	Sessions map[string]string `json:"sessions,omitempty"`
	// Folders maps a project folder (home-relative) to the accounts whose
	// sessions work in it.
	Folders map[string][]string `json:"folders,omitempty"`
}

func newAccountRegistry() *AccountRegistry {
	return &AccountRegistry{Accounts: map[string]*AccountInfo{}, Sessions: map[string]string{}, Folders: map[string][]string{}}
}

func (r *AccountRegistry) normalize() *AccountRegistry {
	if r == nil {
		return newAccountRegistry()
	}
	if r.Accounts == nil {
		r.Accounts = map[string]*AccountInfo{}
	}
	if r.Sessions == nil {
		r.Sessions = map[string]string{}
	}
	if r.Folders == nil {
		r.Folders = map[string][]string{}
	}
	for id, a := range r.Accounts {
		if a == nil {
			r.Accounts[id] = &AccountInfo{}
		}
	}
	return r
}

// account returns the entry for id, creating it.
func (r *AccountRegistry) account(id string) *AccountInfo {
	a := r.Accounts[id]
	if a == nil {
		a = &AccountInfo{}
		r.Accounts[id] = a
	}
	return a
}

// off reports whether the user chose not to sync account id.
func (r *AccountRegistry) off(id string) bool {
	if r == nil || id == "" {
		return false
	}
	a := r.Accounts[id]
	return a != nil && a.Sync != nil && !*a.Sync
}

// anyOff reports whether the user chose not to sync some account.
func (r *AccountRegistry) anyOff() bool {
	if r == nil {
		return false
	}
	for id := range r.Accounts {
		if r.off(id) {
			return true
		}
	}
	return false
}

// sessionOff reports whether session id belongs to an account that doesn't
// sync.
func (r *AccountRegistry) sessionOff(id string) bool {
	return r != nil && r.off(r.Sessions[id])
}

// merge folds other into r: accounts, organizations and folders are united,
// a session keeps the account it was first given, and the latest choice and
// label win. It reports whether r changed.
func (r *AccountRegistry) merge(other *AccountRegistry) bool {
	if other == nil {
		return false
	}
	changed := false
	for id, o := range other.Accounts {
		if o == nil {
			continue
		}
		a, ok := r.Accounts[id]
		if !ok {
			a = &AccountInfo{}
			r.Accounts[id] = a
			changed = true
		}
		if a.Email == "" && o.Email != "" {
			a.Email, changed = o.Email, true
		}
		for org, name := range o.Orgs {
			if cur, ok := a.Orgs[org]; !ok || (cur == "" && name != "") {
				if a.Orgs == nil {
					a.Orgs = map[string]string{}
				}
				a.Orgs[org], changed = name, true
			}
		}
		if o.Sync != nil && (a.Sync == nil || o.SyncAt.After(a.SyncAt)) {
			v := *o.Sync
			a.Sync, a.SyncAt, changed = &v, o.SyncAt, true
		}
		if o.LabelAt.After(a.LabelAt) {
			a.Label, a.LabelAt, changed = o.Label, o.LabelAt, true
		}
	}
	for sid, acct := range other.Sessions {
		if _, ok := r.Sessions[sid]; !ok && acct != "" {
			r.Sessions[sid], changed = acct, true
		}
	}
	for folder, accts := range other.Folders {
		for _, acct := range accts {
			if r.addFolderAccount(folder, acct) {
				changed = true
			}
		}
	}
	return changed
}

func (r *AccountRegistry) addFolderAccount(folder, acct string) bool {
	for _, a := range r.Folders[folder] {
		if a == acct {
			return false
		}
	}
	r.Folders[folder] = append(r.Folders[folder], acct)
	sort.Strings(r.Folders[folder])
	return true
}

// folderOff reports a project folder whose sessions all belong to accounts
// that don't sync. A folder no session is known for (extra_folders, a
// marker from before accounts were known) isn't.
func (r *AccountRegistry) folderOff(rel string) bool {
	if r == nil {
		return false
	}
	accts := r.Folders[rel]
	if len(accts) == 0 {
		return false
	}
	for _, a := range accts {
		if !r.off(a) {
			return false
		}
	}
	return true
}

func (r *AccountRegistry) clone() *AccountRegistry {
	data, _ := json.Marshal(r)
	out := newAccountRegistry()
	_ = json.Unmarshal(data, out)
	return out.normalize()
}

// --- this Mac's accounts --------------------------------------------------------

// activeAccount is the account Claude Code on this Mac is signed in to, from
// ~/.claude.json (no secrets: account and organization IDs, email, name).
type activeAccount struct {
	ID, Email, OrgID, OrgName string
}

func (s *Syncer) activeAccount() activeAccount {
	a, _ := s.readActiveAccount()
	return a
}

// readActiveAccount is activeAccount that also reports whether
// ~/.claude.json could be read: a file that reads cleanly without a
// claude.ai account (signed out, or an API key) is no account, a file that
// can't be read says nothing.
func (s *Syncer) readActiveAccount() (activeAccount, bool) {
	data, err := os.ReadFile(s.claudeJSONPath())
	if errors.Is(err, fs.ErrNotExist) {
		return activeAccount{}, true
	}
	if err != nil {
		return activeAccount{}, false
	}
	var f struct {
		OAuthAccount *struct {
			AccountUUID      string `json:"accountUuid"`
			EmailAddress     string `json:"emailAddress"`
			OrganizationUUID string `json:"organizationUuid"`
			OrganizationName string `json:"organizationName"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(data, &f) != nil {
		return activeAccount{}, false // being written: ask again later
	}
	if f.OAuthAccount == nil || !accountIDRe.MatchString(f.OAuthAccount.AccountUUID) {
		return activeAccount{}, true
	}
	a := f.OAuthAccount
	return activeAccount{ID: strings.ToLower(a.AccountUUID), Email: a.EmailAddress, OrgID: strings.ToLower(a.OrganizationUUID), OrgName: a.OrganizationName}, true
}

// localRecords lists this Mac's sidebar records by account: the account and
// organization each lives under and the transcript (session) it shows.
type localRecord struct {
	Account, Org, Record, Session, Cwd, OriginCwd string
}

func (s *Syncer) localRecords() []localRecord {
	// Read whether or not the sidebar syncs: records are how a desktop
	// session's account is known.
	dir := s.cfg.DesktopDirOverride
	if dir == "" {
		dir = config.DesktopSessionsDir()
	}
	if dir == "" {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*", "*", "local_*.json"))
	var out []localRecord
	for _, m := range matches {
		rel, err := filepath.Rel(dir, m)
		if err != nil || !isDesktopRecord(filepath.ToSlash(rel)) {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var rec struct {
			CliSessionID string `json:"cliSessionId"`
			Cwd          string `json:"cwd"`
			OriginCwd    string `json:"originCwd"`
		}
		if json.Unmarshal(data, &rec) != nil {
			continue
		}
		out = append(out, localRecord{Account: strings.ToLower(parts[0]), Org: strings.ToLower(parts[1]),
			Record: strings.TrimSuffix(parts[2], ".json"), Session: rec.CliSessionID, Cwd: rec.Cwd, OriginCwd: rec.OriginCwd})
	}
	return out
}

// noteLocalAccounts adds what this Mac knows to the registry: the accounts
// and organizations it has sidebar records for and the one it's signed in
// to, and which session each record shows. With attribute, also which
// account each terminal session written or continued here without one
// belongs to: the account this Mac was signed in to when it was written
// (its sign-in history), never a guess. A desktop session waits for its
// sidebar record. It reports whether the registry changed.
func (s *Syncer) noteLocalAccounts(reg *AccountRegistry, objects []storage.ObjectInfo, attribute bool) bool {
	changed := false
	active := s.activeAccount()
	if active.ID != "" {
		a := reg.account(active.ID)
		if a.Email == "" && active.Email != "" {
			a.Email, changed = active.Email, true
		}
		if active.OrgID != "" {
			if cur, ok := a.Orgs[active.OrgID]; !ok || (cur == "" && active.OrgName != "") {
				if a.Orgs == nil {
					a.Orgs = map[string]string{}
				}
				a.Orgs[active.OrgID], changed = active.OrgName, true
			}
		}
	}
	for _, rec := range s.localRecords() {
		a := reg.account(rec.Account)
		if _, ok := a.Orgs[rec.Org]; !ok {
			if a.Orgs == nil {
				a.Orgs = map[string]string{}
			}
			a.Orgs[rec.Org], changed = "", true
		}
		// A record names its session's account for certain.
		if rec.Session != "" && reg.Sessions[rec.Session] != rec.Account {
			reg.Sessions[rec.Session], changed = rec.Account, true
		}
	}
	for _, obj := range objects {
		if rest, ok := strings.CutPrefix(obj.Key, DesktopPrefix); ok {
			acct, _, _ := strings.Cut(rest, "/")
			if accountIDRe.MatchString(acct) {
				if _, known := reg.Accounts[strings.ToLower(acct)]; !known {
					reg.account(strings.ToLower(acct))
					changed = true
				}
			}
		}
	}
	if !attribute {
		return changed
	}
	stored := s.storedSessions(objects)
	history := s.readSignIns()
	for np, sid := range s.localTranscripts() {
		if _, known := reg.Sessions[sid]; known {
			continue
		}
		t, ok := s.writtenHere(np, stored[sid])
		if !ok || transcriptEntrypoint(s.transcriptPath(np)) == "claude-desktop" {
			continue // synced as it is, or a desktop session: its record says
		}
		if acct, known := accountAt(history, t); known && acct != "" {
			reg.Sessions[sid], changed = acct, true
		}
	}
	return changed
}

// storedSessions lists the sessions whose transcripts storage holds.
func (s *Syncer) storedSessions(objects []storage.ObjectInfo) map[string]bool {
	stored := make(map[string]bool)
	for _, obj := range objects {
		if np, ok, _ := s.npathForKey(obj.Key); ok {
			if sid, ok := transcriptSessionID(np); ok {
				stored[sid] = true
			}
		}
	}
	return stored
}

func (s *Syncer) transcriptPath(np string) string {
	return filepath.Join(s.claudeDir, filepath.FromSlash(np))
}

// writtenHere reports whether the transcript at np was written on this Mac
// since it last synced (new here, or continued here), and when: its birth
// for a new one, its last write for one continued.
func (s *Syncer) writtenHere(np string, stored bool) (time.Time, bool) {
	info, err := os.Stat(s.transcriptPath(np))
	if err != nil {
		return time.Time{}, false
	}
	var st *FileState
	if s.state != nil {
		st = s.state.GetFile(np)
	}
	switch {
	case st == nil && !stored:
		return fileBirth(info), true
	case st != nil && (st.LocalDeleted || st.Size != info.Size() || !st.ModTime.Equal(info.ModTime())):
		return info.ModTime(), true
	}
	return time.Time{}, false
}

var entrypointRe = regexp.MustCompile(`"entrypoint"\s*:\s*"([^"]+)"`)

// transcriptEntrypoint returns how a transcript's session was started
// ("claude-desktop" for the desktop app, "cli" for the terminal), from its
// first lines.
func transcriptEntrypoint(abs string) string {
	f, err := os.Open(abs)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, buf)
	if m := entrypointRe.FindSubmatch(buf[:n]); m != nil {
		return string(m[1])
	}
	return ""
}

// heldSessions lists sessions that sit a pass out: while some account
// doesn't sync, a session written or continued here that has no account
// yet waits until it has one (its sidebar record, or this Mac's sign-in
// history), so an account the user keeps on each Mac never leaks through
// the gap.
func (s *Syncer) heldSessions(stored map[string]bool) map[string]bool {
	held := make(map[string]bool)
	reg := s.registry()
	if !reg.anyOff() {
		return held
	}
	history := s.readSignIns()
	for np, sid := range s.localTranscripts() {
		if reg.Sessions[sid] != "" {
			continue
		}
		t, ok := s.writtenHere(np, stored[sid])
		if !ok {
			continue
		}
		if transcriptEntrypoint(s.transcriptPath(np)) != "claude-desktop" {
			if acct, known := accountAt(history, t); known && acct == "" {
				continue // written with no claude.ai account: none to keep it for
			}
		}
		held[sid] = true
	}
	return held
}

// HeldSession is a session sitting syncs out because its account isn't
// known (see heldSessions).
type HeldSession struct {
	ID      string    `json:"id"`
	Cwd     string    `json:"cwd,omitempty"`
	Written time.Time `json:"written"`
}

const heldFile = "held.json"

// saveHeld records the sessions this pass held, for the app and the CLI.
func (s *Syncer) saveHeld(stored map[string]bool) {
	var out []HeldSession
	for np, sid := range s.localTranscripts() {
		if !s.held[sid] {
			continue
		}
		h := HeldSession{ID: sid}
		if t, ok := s.writtenHere(np, stored[sid]); ok {
			h.Written = t.UTC()
		}
		if s.state != nil {
			if e, ok := s.state.cachedCwd(np); ok && len(e.Cwds) > 0 {
				h.Cwd = e.Cwds[0]
			}
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Written.After(out[j].Written) })
	data, err := json.Marshal(out)
	if err == nil {
		_ = writeFileAtomic(filepath.Join(s.stateDir(), heldFile), data, 0600)
	}
}

// HeldSessions returns the sessions the last sync held because their
// account isn't known.
func HeldSessions(cfg *config.Config) []HeldSession {
	data, err := os.ReadFile(filepath.Join(StateDir(cfg), heldFile))
	if err != nil {
		return nil
	}
	var out []HeldSession
	_ = json.Unmarshal(data, &out)
	return out
}

// AssignSessions gives sessions an account, for the whole Circle; they sync
// (or not) by that account's choice from the next sync.
func (s *Syncer) AssignSessions(ctx context.Context, account string, sessions []string) error {
	_, err := s.updateAccounts(ctx, func(r *AccountRegistry) bool {
		changed := false
		for _, sid := range sessions {
			if sid != "" && r.Sessions[sid] != account {
				r.Sessions[sid], changed = account, true
			}
		}
		return changed
	})
	return err
}

// --- sign-ins ---------------------------------------------------------------------

// signInsFile is this Mac's sign-in history: which account Claude Code was
// signed in to, as noticed at each sync and by the agent's local check every
// few seconds. A terminal session belongs to the account signed in when it
// was written. A switch is only known to have happened between the last
// time the old account was seen (From) and when the new one was (Since,
// or earlier: ~/.claude.json changed then); a session written in between
// can't be placed.
const signInsFile = "signins.json"

type signIn struct {
	From    time.Time `json:"from,omitempty"` // the switch happened after this
	Since   time.Time `json:"since"`          // ... and by this
	Seen    time.Time `json:"seen"`           // last confirmed
	Account string    `json:"account"`        // "" no claude.ai account
}

func (s *Syncer) readSignIns() []signIn {
	data, err := os.ReadFile(filepath.Join(s.stateDir(), signInsFile))
	if err != nil {
		return nil
	}
	var h []signIn
	if json.Unmarshal(data, &h) != nil {
		return nil
	}
	return h
}

// noteSignIn records the account signed in now: a new entry when it changed,
// else (about once a minute) when it was last seen.
func (s *Syncer) noteSignIn(now time.Time) {
	a, ok := s.readActiveAccount()
	if !ok {
		return
	}
	now = now.UTC()
	h := s.readSignIns()
	if n := len(h); n > 0 && h[n-1].Account == a.ID {
		if now.Sub(h[n-1].Seen) < time.Minute {
			return
		}
		h[n-1].Seen = now
	} else {
		e := signIn{Since: now, Seen: now, Account: a.ID}
		if n > 0 {
			e.From = h[n-1].Seen
			// The switch wrote ~/.claude.json: from its change on, it's
			// certain.
			if fi, err := os.Stat(s.claudeJSONPath()); err == nil && fi.ModTime().After(e.From) && fi.ModTime().Before(now) {
				e.Since = fi.ModTime().UTC()
			}
		} else {
			e.From = e.Since // nothing known before the first entry
		}
		h = append(h, e)
		if len(h) > 500 {
			h = h[len(h)-500:]
		}
	}
	data, err := json.Marshal(h)
	if err != nil {
		return
	}
	if err := os.MkdirAll(s.stateDir(), 0700); err == nil {
		_ = writeFileAtomic(filepath.Join(s.stateDir(), signInsFile), data, 0600)
	}
}

// accountAt returns the account signed in at t ("" none) and whether the
// history can tell: not before it begins, nor between a switch's From and
// Since.
func accountAt(h []signIn, t time.Time) (string, bool) {
	acct, known := "", false
	for _, e := range h {
		switch {
		case !t.Before(e.Since):
			acct, known = e.Account, true
		case t.After(e.From):
			return "", false // during a switch
		default:
			return acct, known
		}
	}
	return acct, known
}

// localTranscripts lists this Mac's top-level transcripts: npath -> session.
func (s *Syncer) localTranscripts() map[string]string {
	out := make(map[string]string)
	projects := filepath.Join(s.claudeDir, "projects")
	dirs, err := os.ReadDir(projects)
	if err != nil {
		return out
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
			if f.Type().IsRegular() && strings.HasSuffix(f.Name(), ".jsonl") && !isDeviceLocalFile(f.Name()) {
				np := "projects/" + d.Name() + "/" + f.Name()
				if sid, ok := transcriptSessionID(np); ok {
					out[np] = sid
				}
			}
		}
	}
	return out
}

// claudeSession returns the session a ~/.claude path belongs to: its
// transcript and everything kept beside it (subagents, tool results), its
// file-history snapshots and its todo lists.
func claudeSession(rel string) string {
	if sid, ok := transcriptSessionID(rel); ok {
		return sid
	}
	parts := strings.Split(rel, "/")
	switch {
	case len(parts) >= 2 && parts[0] == "file-history":
		return parts[1]
	case len(parts) == 2 && parts[0] == "todos":
		sid, _, _ := strings.Cut(parts[1], "-agent-")
		return strings.TrimSuffix(sid, ".json")
	case len(parts) >= 2 && parts[0] == "tasks":
		return parts[1]
	}
	return ""
}

// claudeOff reports a ~/.claude path holding work of accounts that don't
// sync: a session's own files, a project's memory when every known session
// of that project belongs to such an account, a plan only such sessions
// wrote, and a paste only their prompts hold.
func (s *Syncer) claudeOff(rel string) bool {
	reg := s.registry()
	if !reg.anyOff() {
		return false
	}
	parts := strings.Split(rel, "/")
	switch {
	case len(parts) >= 3 && parts[0] == "projects" && parts[2] == "memory":
		return s.allOff(s.projectSessions()[parts[1]])
	case len(parts) == 2 && parts[0] == "plans":
		return s.allOff(s.planIndex()[parts[1]])
	case len(parts) == 2 && parts[0] == "paste-cache":
		return s.allOff(s.historyRefs().pastes[strings.TrimSuffix(parts[1], path.Ext(parts[1]))])
	}
	sid := claudeSession(rel)
	return sid != "" && reg.sessionOff(sid)
}

// planIndex maps each plan to the sessions that wrote or read it: what
// folder discovery noted this pass, else read from the transcripts (with
// folder sync off, discovery doesn't run).
func (s *Syncer) planIndex() map[string]map[string]bool {
	if s.planSessions == nil {
		s.planSessions = make(map[string]map[string]bool)
		s.transcriptCwds(s.state != nil)
	}
	return s.planSessions
}

// allOff reports a non-empty set of sessions that all belong to accounts
// that don't sync.
func (s *Syncer) allOff(sessions map[string]bool) bool {
	reg := s.registry()
	for sid := range sessions {
		if !reg.sessionOff(sid) && !s.held[sid] {
			return false
		}
	}
	return len(sessions) > 0
}

// projectSessions maps each ~/.claude/projects folder to its sessions, here
// and in storage.
func (s *Syncer) projectSessions() map[string]map[string]bool {
	if s.projects != nil {
		return s.projects
	}
	out := make(map[string]map[string]bool)
	addTo := func(np string) {
		parts := strings.Split(np, "/")
		if sid, ok := transcriptSessionID(np); ok && len(parts) >= 3 && sid != "memory" {
			if out[parts[1]] == nil {
				out[parts[1]] = make(map[string]bool)
			}
			out[parts[1]][sid] = true
		}
	}
	for np := range s.localTranscripts() {
		addTo(np)
	}
	for _, obj := range s.listing {
		if np, ok, _ := s.npathForKey(obj.Key); ok {
			addTo(np)
		}
	}
	s.projects = out
	return out
}

// historyIndex is what ~/.claude/history.jsonl says about pastes: which
// sessions' prompts hold each pasted text.
type historyIndex struct {
	pastes map[string]map[string]bool // content hash -> sessions
}

var contentHashRe = regexp.MustCompile(`"contentHash"\s*:\s*"([0-9A-Za-z]+)"`)

func (s *Syncer) historyRefs() historyIndex {
	if s.history != nil {
		return *s.history
	}
	idx := historyIndex{pastes: make(map[string]map[string]bool)}
	data, err := os.ReadFile(filepath.Join(s.claudeDir, "history.jsonl"))
	if err == nil {
		for _, line := range bytes.Split(data, []byte("\n")) {
			sid := historyLineSession(line)
			for _, m := range contentHashRe.FindAllSubmatch(line, -1) {
				h := string(m[1])
				if idx.pastes[h] == nil {
					idx.pastes[h] = make(map[string]bool)
				}
				// A paste in a line with no session can't be placed: it
				// counts as a session that syncs.
				if sid == "" {
					sid = "-"
				}
				idx.pastes[h][sid] = true
			}
		}
	}
	s.history = &idx
	return idx
}

func historyLineSession(line []byte) string {
	var l struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(line, &l) != nil {
		return ""
	}
	return l.SessionID
}

// sharedJSONL is what this Mac sends of a mergeable file: the prompt
// history without the lines that stay here, anything else as it is.
func (s *Syncer) sharedJSONL(npath string, data []byte) []byte {
	if npath == "history.jsonl" {
		return s.withoutOffSessions(data)
	}
	return data
}

// withoutOffSessions returns history.jsonl content without the prompts of
// sessions whose account doesn't sync (they stay in this Mac's copy).
func (s *Syncer) withoutOffSessions(data []byte) []byte {
	reg := s.registry()
	if !reg.anyOff() && len(s.held) == 0 {
		return data
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	out := make([]byte, 0, len(data))
	for _, line := range lines {
		if sid := historyLineSession(bytes.TrimSpace(line)); sid != "" && (reg.sessionOff(sid) || s.held[sid]) {
			continue
		}
		out = append(out, line...)
	}
	return out
}

// noteFolderAccounts adds the folders this pass's discovery saw sessions in
// to the registry, when any is new to it.
func (s *Syncer) noteFolderAccounts(ctx context.Context) {
	reg := s.registry()
	fresh := false
	for folder, accts := range s.folderAccounts {
		for acct := range accts {
			fresh = fresh || !slices.Contains(reg.Folders[folder], acct)
		}
	}
	if !fresh {
		return
	}
	updated, err := s.updateAccounts(ctx, func(r *AccountRegistry) bool {
		changed := false
		for folder, accts := range s.folderAccounts {
			for acct := range accts {
				changed = r.addFolderAccount(folder, acct) || changed
			}
		}
		return changed
	})
	if err != nil {
		s.log("Warning: failed to record which accounts use which folders: %v", err)
		return
	}
	s.accounts = updated
}

// --- storage ------------------------------------------------------------------------

func (s *Syncer) encodeAccounts(reg *AccountRegistry) ([]byte, error) {
	data, err := json.Marshal(reg)
	if err != nil {
		return nil, err
	}
	compressed, err := gzipCompress(bindKey(data, AccountsKey))
	if err != nil {
		return nil, err
	}
	return s.encryptor.Encrypt(compressed)
}

func (s *Syncer) decodeAccounts(enc []byte) (*AccountRegistry, error) {
	data, err := s.encryptor.Decrypt(enc)
	if err != nil {
		return nil, err
	}
	if isGzipped(data) {
		if data, err = gzipDecompress(data); err != nil {
			return nil, err
		}
	}
	if data, err = unbindKey(data, AccountsKey); err != nil {
		return nil, err
	}
	reg := newAccountRegistry()
	if err := json.Unmarshal(data, reg); err != nil {
		return nil, err
	}
	return reg.normalize(), nil
}

// loadAccounts reads the registry from storage, with the ETag it had ("" on
// backends without one); missing is true when storage has none.
func (s *Syncer) loadAccounts(ctx context.Context) (reg *AccountRegistry, etag string, missing bool, err error) {
	cond, ok := s.storage.(storage.Conditional)
	var enc []byte
	if ok {
		enc, etag, err = cond.DownloadWithETag(ctx, AccountsKey)
		if errors.Is(err, storage.ErrNotFound) {
			return newAccountRegistry(), "", true, nil
		}
	} else {
		// Backends word "not found" differently: ask the listing (of the
		// folder: some treat a prefix as a folder name).
		objs, lerr := s.storage.List(ctx, path.Dir(AccountsKey)+"/")
		if lerr != nil {
			return nil, "", false, fmt.Errorf("failed to read the account registry: %w", lerr)
		}
		found := false
		for _, o := range objs {
			found = found || o.Key == AccountsKey
		}
		if !found {
			return newAccountRegistry(), "", true, nil
		}
		enc, err = s.storage.Download(ctx, AccountsKey)
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("failed to read the account registry: %w", err)
	}
	if reg, err = s.decodeAccounts(enc); err != nil {
		return nil, "", false, fmt.Errorf("failed to read the account registry: %w", err)
	}
	return reg, etag, false, nil
}

// updateAccounts applies change to the registry in storage, with
// compare-and-swap: when another Mac wrote it meanwhile, its version is
// merged in and change applied again. It returns the registry as written
// and keeps this Mac's copy current.
func (s *Syncer) updateAccounts(ctx context.Context, change func(*AccountRegistry) bool) (*AccountRegistry, error) {
	for attempt := 0; attempt < 5; attempt++ {
		reg, etag, _, err := s.loadAccounts(ctx)
		if err != nil {
			return nil, err
		}
		// This Mac's copy brings back what storage lost (a new container, a
		// passphrase change), so an account turned off doesn't start syncing
		// again; merging keeps the latest choice.
		seeded := reg.merge(s.cachedAccounts())
		if !change(reg) && !seeded {
			s.saveAccountsCache(reg)
			return reg, nil
		}
		enc, err := s.encodeAccounts(reg)
		if err != nil {
			return nil, err
		}
		if cond, ok := s.storage.(storage.Conditional); ok {
			_, err = cond.UploadIf(ctx, AccountsKey, enc, etag)
			if errors.Is(err, storage.ErrPreconditionFailed) {
				continue
			}
		} else if err = s.storage.Upload(ctx, AccountsKey, enc); err == nil {
			// No compare-and-swap here: read it back, and apply the change
			// again if another Mac's write landed over this one.
			if back, _, _, rerr := s.loadAccounts(ctx); rerr == nil && change(back.clone()) {
				continue
			}
		}
		if err != nil {
			return nil, fmt.Errorf("failed to save the account registry: %w", err)
		}
		s.saveAccountsCache(reg)
		s.accounts = reg // this syncer goes by what it just wrote
		return reg, nil
	}
	return nil, errors.New("the account registry kept changing on another Mac; try again")
}

func (s *Syncer) accountsCachePath() string {
	return filepath.Join(s.stateDir(), accountsCacheFile)
}

func (s *Syncer) saveAccountsCache(reg *AccountRegistry) {
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(s.stateDir(), 0700); err == nil {
		_ = writeFileAtomic(s.accountsCachePath(), data, 0600)
	}
}

// cachedAccounts returns this Mac's copy of the registry (empty when there
// is none).
func (s *Syncer) cachedAccounts() *AccountRegistry {
	data, err := os.ReadFile(s.accountsCachePath())
	if err != nil {
		return newAccountRegistry()
	}
	reg := newAccountRegistry()
	if json.Unmarshal(data, reg) != nil {
		return newAccountRegistry()
	}
	return reg.normalize()
}

// registry returns the registry this pass goes by: the one reconcile loaded,
// else this Mac's copy (probes, the app).
func (s *Syncer) registry() *AccountRegistry {
	if s.accounts != nil {
		return s.accounts
	}
	s.accounts = s.cachedAccounts()
	return s.accounts
}

// prepareAccounts runs at the start of a pass: it reads the registry, adds
// what this Mac knows (with attribute, the account of each transcript first
// written here; objects is the pass's listing), and saves it when that
// changed (not in a preview).
func (s *Syncer) prepareAccounts(ctx context.Context, objects []storage.ObjectInfo, dryRun, attribute bool) error {
	s.listing, s.projects, s.history, s.planSessions = objects, nil, nil, nil
	s.noteSignIn(time.Now())
	reg, _, _, err := s.loadAccounts(ctx)
	if err != nil {
		return err
	}
	local := reg.clone()
	// This Mac's copy brings back choices storage lost (a new container, a
	// passphrase change); merging keeps the latest choice, so a stale copy
	// never undoes a newer one.
	seeded := local.merge(s.cachedAccounts())
	if (s.noteLocalAccounts(local, objects, attribute) || seeded) && !dryRun {
		// Applied to the registry as stored (not merged), so what a sidebar
		// record says about its session overrides an earlier guess.
		if reg, err = s.updateAccounts(ctx, func(r *AccountRegistry) bool { return s.noteLocalAccounts(r, objects, attribute) }); err != nil {
			return err
		}
	} else {
		reg = local
		if !dryRun {
			s.saveAccountsCache(reg)
		}
	}
	s.accounts = reg
	return nil
}

// --- the app and the CLI --------------------------------------------------------

// Account is one Claude account as the app and CLI show it.
type Account struct {
	ID       string       `json:"id"`
	Email    string       `json:"email,omitempty"`
	Label    string       `json:"label,omitempty"`
	Name     string       `json:"name"` // label, else email, else a short ID
	Orgs     []AccountOrg `json:"orgs,omitempty"`
	Sync     string       `json:"sync"`     // "on", "off" or "ask" (not chosen yet; syncs)
	Active   bool         `json:"active"`   // Claude on this Mac is signed in to it
	Here     bool         `json:"here"`     // this Mac has sidebar sessions of it
	Sessions int          `json:"sessions"` // sessions known to belong to it
}

// AccountOrg is an organization an account has sessions in.
type AccountOrg struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// ListAccounts returns the Claude accounts the Circle knows, from this Mac's
// copy of the registry and what this Mac has; no network.
func ListAccounts(cfg *config.Config) []Account {
	s := newScopeSyncer(cfg)
	reg := s.cachedAccounts()
	s.noteLocalAccounts(reg, nil, false)
	return accountList(reg, s.activeAccount().ID, s.localRecords())
}

func accountList(reg *AccountRegistry, active string, records []localRecord) []Account {
	here := make(map[string]bool)
	for _, r := range records {
		here[r.Account] = true
	}
	count := make(map[string]int)
	for _, acct := range reg.Sessions {
		count[acct]++
	}
	var out []Account
	for id, a := range reg.Accounts {
		acc := Account{ID: id, Email: a.Email, Label: a.Label, Active: id == active, Here: here[id], Sessions: count[id], Sync: "ask"}
		if a.Sync != nil {
			acc.Sync = map[bool]string{true: "on", false: "off"}[*a.Sync]
		}
		switch {
		case a.Label != "":
			acc.Name = a.Label
		case a.Email != "":
			acc.Name = a.Email
		default:
			acc.Name = "Account " + id[:min(8, len(id))]
		}
		for org, name := range a.Orgs {
			acc.Orgs = append(acc.Orgs, AccountOrg{ID: org, Name: name})
		}
		sort.Slice(acc.Orgs, func(i, j int) bool { return acc.Orgs[i].ID < acc.Orgs[j].ID })
		out = append(out, acc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Active != out[j].Active {
			return out[i].Active
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// FindAccount resolves who (an account ID or its first characters, an email
// or a label, ignoring case) among accounts.
func FindAccount(accounts []Account, who string) (Account, error) {
	who = strings.ToLower(strings.TrimSpace(who))
	var hits []Account
	for _, a := range accounts {
		if who != "" && (a.ID == who || strings.EqualFold(a.Email, who) || strings.EqualFold(a.Label, who) || (len(who) >= 4 && strings.HasPrefix(a.ID, who))) {
			hits = append(hits, a)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return Account{}, fmt.Errorf("no Claude account matches %q (see 'stradl accounts')", who)
	}
	return Account{}, fmt.Errorf("%q matches more than one account; use its ID", who)
}

// SetAccountSync records the choice to sync account id or not, for the whole
// Circle.
func (s *Syncer) SetAccountSync(ctx context.Context, id string, on bool, now time.Time) error {
	_, err := s.updateAccounts(ctx, func(r *AccountRegistry) bool {
		a := r.account(id)
		if a.Sync != nil && *a.Sync == on {
			return false
		}
		a.Sync, a.SyncAt = &on, now.UTC()
		return true
	})
	return err
}

// SetAccountLabel names account id for the whole Circle ("" clears it).
func (s *Syncer) SetAccountLabel(ctx context.Context, id, label string, now time.Time) error {
	label = strings.TrimSpace(label)
	if len(label) > 80 || strings.ContainsAny(label, "\n\r") {
		return errors.New("a label is one line of at most 80 characters")
	}
	_, err := s.updateAccounts(ctx, func(r *AccountRegistry) bool {
		a := r.account(id)
		if a.Label == label {
			return false
		}
		a.Label, a.LabelAt = label, now.UTC()
		return true
	})
	return err
}

// RefreshAccounts reads the registry from storage into this Mac's copy,
// adding what this Mac knows.
func (s *Syncer) RefreshAccounts(ctx context.Context) ([]Account, error) {
	if err := s.prepareAccounts(ctx, nil, false, false); err != nil {
		return nil, err
	}
	return accountList(s.registry(), s.activeAccount().ID, s.localRecords()), nil
}
