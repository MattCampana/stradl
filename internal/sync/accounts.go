package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	data, err := os.ReadFile(s.claudeJSONPath())
	if err != nil {
		return activeAccount{}
	}
	var f struct {
		OAuthAccount struct {
			AccountUUID      string `json:"accountUuid"`
			EmailAddress     string `json:"emailAddress"`
			OrganizationUUID string `json:"organizationUuid"`
			OrganizationName string `json:"organizationName"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(data, &f) != nil || !accountIDRe.MatchString(f.OAuthAccount.AccountUUID) {
		return activeAccount{}
	}
	a := f.OAuthAccount
	return activeAccount{ID: strings.ToLower(a.AccountUUID), Email: a.EmailAddress, OrgID: strings.ToLower(a.OrganizationUUID), OrgName: a.OrganizationName}
}

// localRecords lists this Mac's sidebar records by account: the account and
// organization each lives under and the transcript (session) it shows.
type localRecord struct {
	Account, Org, Session, Cwd, OriginCwd string
}

func (s *Syncer) localRecords() []localRecord {
	dir := s.cfg.EffectiveDesktopDir()
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
		out = append(out, localRecord{Account: strings.ToLower(parts[0]), Org: strings.ToLower(parts[1]), Session: rec.CliSessionID, Cwd: rec.Cwd, OriginCwd: rec.OriginCwd})
	}
	return out
}

// noteLocalAccounts adds what this Mac knows to the registry: the accounts
// and organizations it has sidebar records for and the one it's signed in
// to, and which session each record shows. With attribute, also which
// account each transcript first written here belongs to (the one signed in
// when it was); transcripts that came from another Mac, or that synced
// before, are never guessed at. It reports whether the registry changed.
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
	if active.ID == "" || !attribute {
		return changed
	}
	stored := make(map[string]bool)
	for _, obj := range objects {
		if np, ok, _ := s.npathForKey(obj.Key); ok {
			if sid, ok := transcriptSessionID(np); ok {
				stored[sid] = true
			}
		}
	}
	for np, sid := range s.localTranscripts() {
		if _, known := reg.Sessions[sid]; known || stored[sid] || s.state.GetFile(np) != nil {
			continue
		}
		reg.Sessions[sid], changed = active.ID, true
	}
	return changed
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
	}
	return ""
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

// loadAccounts reads the registry from storage, with the ETag it had (""
// when there is none yet).
func (s *Syncer) loadAccounts(ctx context.Context) (*AccountRegistry, string, error) {
	cond, ok := s.storage.(storage.Conditional)
	var (
		enc  []byte
		etag string
		err  error
	)
	if ok {
		enc, etag, err = cond.DownloadWithETag(ctx, AccountsKey)
	} else {
		enc, err = s.storage.Download(ctx, AccountsKey)
	}
	if errors.Is(err, storage.ErrNotFound) || (err != nil && !ok && strings.Contains(err.Error(), "not found")) {
		return newAccountRegistry(), "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("failed to read the account registry: %w", err)
	}
	reg, err := s.decodeAccounts(enc)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read the account registry: %w", err)
	}
	return reg, etag, nil
}

// updateAccounts applies change to the registry in storage, with
// compare-and-swap: when another Mac wrote it meanwhile, its version is
// merged in and change applied again. It returns the registry as written
// and keeps this Mac's copy current.
func (s *Syncer) updateAccounts(ctx context.Context, change func(*AccountRegistry) bool) (*AccountRegistry, error) {
	for attempt := 0; attempt < 5; attempt++ {
		reg, etag, err := s.loadAccounts(ctx)
		if err != nil {
			return nil, err
		}
		if !change(reg) {
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
		} else {
			err = s.storage.Upload(ctx, AccountsKey, enc)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to save the account registry: %w", err)
		}
		s.saveAccountsCache(reg)
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
	reg, _, err := s.loadAccounts(ctx)
	if err != nil {
		return err
	}
	local := reg.clone()
	if s.noteLocalAccounts(local, objects, attribute) && !dryRun {
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
