package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/profiles"
	"github.com/tawanorg/claude-sync/internal/storage"
)

// The Claude desktop app keeps its Code-tab sidebar layout (custom folder
// groups, which session sits in which group, section settings, pins) in its
// own preferences file, claude_desktop_config.json, under
// preferences.epitaxyPrefs. The app carries group names to other Macs through
// the Claude account but not membership, so a second Mac shows the groups
// empty. Stradl syncs those keys as one small remote object, merged three ways
// against the last layout this Mac and storage agreed on, and leaves every
// other key in the file as the app wrote it (MCP servers, window and pane
// state, filters, notices).
//
// The running app holds the layout in memory and rewrites the file on every
// sidebar change, so an edit made underneath it would be overwritten and then
// look like the user undoing the other Mac's change. Incoming changes are
// therefore written only while the app is not running; otherwise they wait in
// sidebar-pending.json until it quits (see ApplyPendingSidebar).

const (
	// SidebarRemoteKey is the remote object holding the sidebar layout. Its
	// "_" prefix keeps it out of the file reconcile pass.
	SidebarRemoteKey = "_desktop/sidebar.json.age"

	sidebarConfigName  = "claude_desktop_config.json"
	sidebarBaseFile    = "sidebar-base.json"
	sidebarPendingFile = "sidebar-pending.json"
	sidebarVersion     = 1

	// sidebarContentName picks the JSON flavor of path mapping for the payload
	// (pinned home projects may hold folder paths).
	sidebarContentName = "sidebar.json"
)

// The epitaxyPrefs keys that make up the sidebar layout.
const (
	prefGroupScopes   = "dframe-group-scopes"
	prefCodeSections  = "dframe-code-sections"
	prefStarredCode   = "starred-local-code-sessions"
	prefStarredGroups = "starred-session-groups"
	prefLocalSlice    = "dframe-local-slice"
	slicePinnedOrder  = "pinnedOrder"
	sliceHomeProjects = "homeProjectsPinnedOrder"
)

// SidebarState is the synced part of the desktop app's sidebar preferences.
// Scope maps are keyed by "<accountUuid>/<orgUuid>", as in the app. Objects the
// app owns (groups, sections, starred entries) stay raw JSON, so fields Stradl
// doesn't know about survive the trip to another Mac.
type SidebarState struct {
	Scopes                   map[string]*GroupScope   `json:"groupScopes,omitempty"`
	Sections                 map[string]*SectionScope `json:"codeSections,omitempty"`
	StarredLocalCodeSessions []json.RawMessage        `json:"starredLocalCodeSessions,omitempty"`
	StarredSessionGroups     []json.RawMessage        `json:"starredSessionGroups,omitempty"`
	PinnedOrder              []json.RawMessage        `json:"pinnedOrder,omitempty"`
	HomeProjectsPinnedOrder  []json.RawMessage        `json:"homeProjectsPinnedOrder,omitempty"`
}

// GroupScope is one account's folder groups, a "dframe-group-scopes" entry.
// Session keys ("code:local_<id>") are the same on every Mac because the
// session records sync under the same IDs.
type GroupScope struct {
	Groups      []json.RawMessage          // {"id":"cg-…","name":…}, in the app's order
	Assignments map[string]string          // session key -> group id
	Order       map[string][]string        // group id -> session keys, top to bottom
	Extra       map[string]json.RawMessage // any other keys the app keeps in the scope
}

// SectionScope is one account's sidebar sections, a "dframe-code-sections"
// entry: pinned, routines, sessions and one manual section per group (with
// the group's id), plus the app's migration markers in Extra.
type SectionScope struct {
	Sections []json.RawMessage
	Extra    map[string]json.RawMessage
}

// MarshalJSON writes the scope in the app's own shape.
func (g GroupScope) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, len(g.Extra)+3)
	for k, v := range g.Extra {
		m[k] = v
	}
	order := make(map[string][]string, len(g.Order))
	for k, v := range g.Order {
		if v == nil {
			v = []string{}
		}
		order[k] = v
	}
	assignments := g.Assignments
	if assignments == nil {
		assignments = map[string]string{}
	}
	m["groups"] = nonNilList(g.Groups)
	m["assignments"] = assignments
	m["order"] = order
	return json.Marshal(m)
}

// UnmarshalJSON reads the app's shape, keeping unknown keys in Extra.
func (g *GroupScope) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*g = GroupScope{}
	for k, v := range raw {
		var err error
		switch k {
		case "groups":
			err = json.Unmarshal(v, &g.Groups)
		case "assignments":
			err = json.Unmarshal(v, &g.Assignments)
		case "order":
			err = json.Unmarshal(v, &g.Order)
		default:
			if g.Extra == nil {
				g.Extra = make(map[string]json.RawMessage)
			}
			g.Extra[k] = v
		}
		if err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

// MarshalJSON writes the scope in the app's own shape.
func (s SectionScope) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, len(s.Extra)+1)
	for k, v := range s.Extra {
		m[k] = v
	}
	m["sections"] = nonNilList(s.Sections)
	return json.Marshal(m)
}

// UnmarshalJSON reads the app's shape, keeping unknown keys in Extra.
func (s *SectionScope) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = SectionScope{}
	for k, v := range raw {
		if k == "sections" {
			if err := json.Unmarshal(v, &s.Sections); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			continue
		}
		if s.Extra == nil {
			s.Extra = make(map[string]json.RawMessage)
		}
		s.Extra[k] = v
	}
	return nil
}

func nonNilList(l []json.RawMessage) []json.RawMessage {
	if l == nil {
		return []json.RawMessage{}
	}
	return l
}

func (st SidebarState) isEmpty() bool {
	return len(st.Scopes)+len(st.Sections)+len(st.StarredLocalCodeSessions)+len(st.StarredSessionGroups)+
		len(st.PinnedOrder)+len(st.HomeProjectsPinnedOrder) == 0
}

// canonical renders the state with sorted keys, for comparisons and hashes.
func (st SidebarState) canonical() string {
	data, err := json.Marshal(st)
	if err != nil {
		return ""
	}
	return canonicalJSON(data)
}

func sidebarEqual(a, b SidebarState) bool { return a.canonical() == b.canonical() }

func sidebarHash(st SidebarState) string {
	h := sha256.Sum256([]byte(st.canonical()))
	return hex.EncodeToString(h[:])
}

// --- paths and process detection ---------------------------------------------

// SidebarConfigPath returns the desktop app's preferences file
// (~/Library/Application Support/Claude/claude_desktop_config.json).
func SidebarConfigPath() string {
	dir := config.DesktopSessionsDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(dir), sidebarConfigName)
}

// sidebarPath is the preferences file this Syncer manages, next to the desktop
// session index it syncs, or "" when desktop sync is off.
func (s *Syncer) sidebarPath() string {
	dir := s.cfg.EffectiveDesktopDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(dir), sidebarConfigName)
}

func sidebarStateDir(cfg *config.Config) string {
	if cfg.StateDirOverride != "" {
		return cfg.StateDirOverride
	}
	return config.ConfigDirPath()
}

// DesktopAppRunning reports whether the Claude desktop app is running on this
// Mac on the data folder dir (see package profiles: each profile is its own
// instance). When the processes can't be read it reports true, so nothing
// gets written underneath a running app. Tests replace it.
var DesktopAppRunning = func(dir string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	inst, ok := profiles.InstancesChecked((&config.Config{}).ClaudeAppDir())
	if !ok {
		return true
	}
	for _, d := range inst {
		if filepath.Clean(d) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

// sidebarTarget is one profile's preferences file and the names of the
// state Stradl keeps for it: the main profile's are sidebar-base.json and
// sidebar-pending.json, another's carry its ID.
type sidebarTarget struct {
	path, base, pending string
}

func sidebarTargetFor(cfg *config.Config, path string) sidebarTarget {
	t := sidebarTarget{path: path, base: sidebarBaseFile, pending: sidebarPendingFile}
	if dir := filepath.Dir(path); filepath.Clean(filepath.Dir(dir)) == filepath.Clean(cfg.EffectiveProfilesDir()) {
		id := filepath.Base(dir)
		t.base = strings.TrimSuffix(sidebarBaseFile, ".json") + "." + id + ".json"
		t.pending = strings.TrimSuffix(sidebarPendingFile, ".json") + "." + id + ".json"
	}
	return t
}

// sidebarTargets returns the preferences file of every profile whose folder
// exists, the main one first; none while desktop sync is off.
func (s *Syncer) sidebarTargets() []sidebarTarget {
	main := s.sidebarPath()
	if main == "" {
		return nil
	}
	var out []sidebarTarget
	if dirExists(filepath.Dir(main)) {
		out = append(out, sidebarTargetFor(s.cfg, main))
	}
	env := profiles.FromConfig(s.cfg)
	env.Instances = func() map[int]string { return nil } // which are open doesn't matter here
	for _, p := range env.List() {
		if !p.Main && dirExists(p.Dir) {
			out = append(out, sidebarTargetFor(s.cfg, filepath.Join(p.Dir, sidebarConfigName)))
		}
	}
	return out
}

// --- sync ---------------------------------------------------------------------

// SidebarResult describes one sidebar sync.
type SidebarResult struct {
	Uploaded       bool // this Mac's layout changes went to storage
	AppliedLocally bool // other Macs' changes were written to the desktop app's file
	PendingRestart bool // other Macs' changes wait for the desktop app to quit

	// GroupsChanged counts groups added, removed or renamed on this Mac, and
	// AssignmentsChanged sessions moved into, out of or between groups, by
	// changes from other Macs (applied now or pending).
	GroupsChanged      int
	AssignmentsChanged int
}

// SyncSidebar reconciles the desktop app's sidebar layout with storage, for
// each profile in turn (see package profiles; each keeps its own layout
// file and merges like another Mac would): it merges the profile's layout,
// the one in storage and the last one both agreed on (see MergeSidebar),
// uploads the result when storage lacks something, and writes it to the
// profile's file when it lacks something. appRunning (nil means
// DesktopAppRunning on the profile's folder) is checked right before
// writing: while the profile is open the merged layout is kept in its
// sidebar-pending file instead and PendingRestart is set, for
// ApplyPendingSidebar to write once it quits. It does nothing when desktop
// sync is off or the app's folder doesn't exist. Callers hold the sync lock.
func (s *Syncer) SyncSidebar(ctx context.Context, appRunning func() bool) (SidebarResult, error) {
	var res SidebarResult
	for _, t := range s.sidebarTargets() {
		running := appRunning
		if running == nil {
			dir := filepath.Dir(t.path)
			running = func() bool { return DesktopAppRunning(dir) }
		}
		r, err := s.syncSidebarFile(ctx, t, running)
		res.Uploaded = res.Uploaded || r.Uploaded
		res.AppliedLocally = res.AppliedLocally || r.AppliedLocally
		res.PendingRestart = res.PendingRestart || r.PendingRestart
		res.GroupsChanged += r.GroupsChanged
		res.AssignmentsChanged += r.AssignmentsChanged
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

func (s *Syncer) syncSidebarFile(ctx context.Context, t sidebarTarget, appRunning func() bool) (SidebarResult, error) {
	var res SidebarResult
	path := t.path
	stateDir := sidebarStateDir(s.cfg)
	basePath := filepath.Join(stateDir, t.base)
	pendingPath := filepath.Join(stateDir, t.pending)
	storageID := StorageIdentity(s.cfg.GetStorageConfig())
	base, _ := loadSidebarSnapshot(basePath, storageID)
	// The base's part for accounts that don't sync is kept as it is, so the
	// layout merges correctly when such an account syncs again.
	var baseOff SidebarState

	for attempt := 0; attempt < maxRaceRetries; attempt++ {
		remote, etag, exists, err := s.fetchSidebar(ctx)
		if err != nil {
			return res, err
		}
		if !exists {
			// Missing from storage (first sync, or the object vanished): nothing
			// changed there, so this Mac's layout is restored, not erased.
			remote = base.State
		}
		local, err := readSidebar(path)
		if err != nil {
			return res, err
		}
		// The layout of accounts that don't sync stays out: only the rest
		// merges and uploads; storage keeps its copy of theirs as it was,
		// and this Mac keeps its own.
		fullLocal := local
		var localOff, remoteOff SidebarState
		if off := s.sidebarOff(); off != nil {
			local, localOff = splitSidebar(local, off)
			remote, remoteOff = splitSidebar(remote, off)
			var bOff SidebarState
			base.State, bOff = splitSidebar(base.State, off)
			baseOff = joinSidebar(baseOff, bOff)
		}
		mergeLocal := assumeUnchanged(local, base.State)
		if base.Replaced != "" && sidebarHash(fullLocal) == base.Replaced && !sidebarEqual(local, base.State) {
			// The file is back to exactly the layout Stradl last replaced: the
			// app wrote out what it had in memory before loading the new one.
			// That is not an edit, and merging it as one would undo the other
			// Macs' changes everywhere.
			mergeLocal = base.State
		}
		merged, _, toRemote := MergeSidebar(base.State, mergeLocal, remote)
		toLocal := !sidebarEqual(merged, local)
		if !exists {
			toRemote = !merged.isEmpty()
		}

		if toRemote {
			if !exists {
				etag = ""
			}
			if _, err := s.putSidebar(ctx, joinSidebar(merged, remoteOff), etag, exists); errors.Is(err, storage.ErrPreconditionFailed) {
				continue // another Mac wrote it meanwhile: merge against theirs
			} else if err != nil {
				return res, fmt.Errorf("failed to upload sidebar layout: %w", err)
			}
			res.Uploaded = true
		}

		if !toLocal {
			_ = os.Remove(pendingPath)
			return res, saveSidebarSnapshot(basePath, storageID, joinSidebar(merged, baseOff), "")
		}
		res.GroupsChanged, res.AssignmentsChanged = sidebarChanges(local, merged)
		if appRunning() {
			// The base stays as it is: the file still holds the old layout, and
			// comparing it with a newer base would read the difference as this
			// Mac undoing the other Macs' changes.
			// The whole layout, this Mac's own part of accounts that don't
			// sync included: applied when the app quits, a pending layout
			// without it would read as having removed it.
			if err := saveSidebarSnapshot(pendingPath, storageID, joinSidebar(merged, localOff), ""); err != nil {
				return res, err
			}
			res.PendingRestart = true
			return res, nil
		}
		written, err := writeSidebar(path, joinSidebar(merged, localOff))
		if err != nil {
			return res, err
		}
		replaced := ""
		if written {
			res.AppliedLocally = true
			replaced = sidebarHash(fullLocal)
		}
		_ = os.Remove(pendingPath)
		return res, saveSidebarSnapshot(basePath, storageID, joinSidebar(merged, baseOff), replaced)
	}
	return res, fmt.Errorf("sidebar layout: gave up after %d attempts: other Macs keep changing it; will retry next sync", maxRaceRetries)
}

// sidebarOff returns, while some account doesn't sync, which parts of the
// sidebar layout belong to such an account; nil otherwise.
func (s *Syncer) sidebarOff() *sidebarFilter {
	reg := s.registry()
	if !reg.anyOff() {
		return nil
	}
	f := &sidebarFilter{reg: reg, records: make(map[string]string)}
	for _, rec := range s.localRecords() {
		f.records[rec.Record] = rec.Account
	}
	return f
}

type sidebarFilter struct {
	reg     *AccountRegistry
	records map[string]string // "local_<id>" -> account
}

var sidebarSessionRe = regexp.MustCompile(`local_[A-Za-z0-9_-]+`)

func (f *sidebarFilter) scopeOff(scope string) bool {
	acct, _, _ := strings.Cut(scope, "/")
	return f.reg.off(strings.ToLower(acct))
}

// entryOff reports a starred or pinned entry naming a session of an
// account that doesn't sync.
func (f *sidebarFilter) entryOff(raw json.RawMessage) bool {
	for _, id := range sidebarSessionRe.FindAll(raw, -1) {
		if f.reg.off(f.records[string(id)]) {
			return true
		}
	}
	return false
}

// splitSidebar separates the parts of st that belong to accounts that don't
// sync (off) from the rest (on).
func splitSidebar(st SidebarState, f *sidebarFilter) (on, off SidebarState) {
	for k, v := range st.Scopes {
		dst := &on
		if f.scopeOff(k) {
			dst = &off
		}
		if dst.Scopes == nil {
			dst.Scopes = map[string]*GroupScope{}
		}
		dst.Scopes[k] = v
	}
	for k, v := range st.Sections {
		dst := &on
		if f.scopeOff(k) {
			dst = &off
		}
		if dst.Sections == nil {
			dst.Sections = map[string]*SectionScope{}
		}
		dst.Sections[k] = v
	}
	split := func(list []json.RawMessage, on, off *[]json.RawMessage) {
		for _, e := range list {
			if f.entryOff(e) {
				*off = append(*off, e)
			} else {
				*on = append(*on, e)
			}
		}
	}
	split(st.StarredLocalCodeSessions, &on.StarredLocalCodeSessions, &off.StarredLocalCodeSessions)
	on.StarredSessionGroups = st.StarredSessionGroups
	split(st.PinnedOrder, &on.PinnedOrder, &off.PinnedOrder)
	split(st.HomeProjectsPinnedOrder, &on.HomeProjectsPinnedOrder, &off.HomeProjectsPinnedOrder)
	return on, off
}

// joinSidebar puts the parts splitSidebar separated back together. on is
// left as it was.
func joinSidebar(on, off SidebarState) SidebarState {
	out := on
	out.Scopes, out.Sections = maps.Clone(on.Scopes), maps.Clone(on.Sections)
	for k, v := range off.Scopes {
		if out.Scopes == nil {
			out.Scopes = map[string]*GroupScope{}
		}
		if _, ok := out.Scopes[k]; !ok {
			out.Scopes[k] = v
		}
	}
	for k, v := range off.Sections {
		if out.Sections == nil {
			out.Sections = map[string]*SectionScope{}
		}
		if _, ok := out.Sections[k]; !ok {
			out.Sections[k] = v
		}
	}
	out.StarredLocalCodeSessions = append(append([]json.RawMessage(nil), on.StarredLocalCodeSessions...), off.StarredLocalCodeSessions...)
	out.PinnedOrder = append(append([]json.RawMessage(nil), on.PinnedOrder...), off.PinnedOrder...)
	out.HomeProjectsPinnedOrder = append(append([]json.RawMessage(nil), on.HomeProjectsPinnedOrder...), off.HomeProjectsPinnedOrder...)
	return out
}

// SidebarPendingExists reports whether a sidebar layout is waiting for the
// Claude app (any profile) to quit.
func SidebarPendingExists(cfg *config.Config) bool {
	m, _ := filepath.Glob(filepath.Join(sidebarStateDir(cfg), strings.TrimSuffix(sidebarPendingFile, ".json")+"*.json"))
	return len(m) > 0
}

// ApplyPendingSidebars runs ApplyPendingSidebar for every profile, and
// reports whether any layout was written. Callers hold the sync lock.
func ApplyPendingSidebars(cfg *config.Config) (applied bool, err error) {
	s := newScopeSyncer(cfg)
	for _, t := range s.sidebarTargets() {
		a, e := ApplyPendingSidebar(cfg, t.path)
		applied = applied || a
		if e != nil {
			err = errors.Join(err, e)
		}
	}
	return applied, err
}

// ApplyPendingSidebar writes a layout SyncSidebar held back while the desktop
// app was running. Call it right after the app quits (the Restart Claude
// flow) and when the agent starts; it does nothing while the app runs or when
// nothing is pending. The file is re-read and merged three ways with the
// pending layout, so sidebar edits made on this Mac in the meantime are kept
// (and go to storage on the next sync). Callers hold the sync lock.
func ApplyPendingSidebar(cfg *config.Config, sidebarPath string) (applied bool, err error) {
	t := sidebarTargetFor(cfg, sidebarPath)
	stateDir := sidebarStateDir(cfg)
	pendingPath := filepath.Join(stateDir, t.pending)
	basePath := filepath.Join(stateDir, t.base)
	storageID := StorageIdentity(cfg.GetStorageConfig())

	pending, ok := loadSidebarSnapshot(pendingPath, storageID)
	if !ok {
		if _, err := os.Stat(pendingPath); err == nil {
			_ = os.Remove(pendingPath) // unreadable, or recorded for another storage target
		}
		return false, nil
	}
	if sidebarPath == "" || !dirExists(filepath.Dir(sidebarPath)) || DesktopAppRunning(filepath.Dir(sidebarPath)) {
		return false, nil
	}
	base, _ := loadSidebarSnapshot(basePath, storageID)
	local, err := readSidebar(sidebarPath)
	if err != nil {
		return false, err
	}
	// The layout of accounts that don't sync stays as this Mac has it (the
	// pending copy of it is older), and the base keeps its part of it.
	fullLocal, newBase := local, pending.State
	var localOff SidebarState
	if off := newScopeSyncer(cfg).sidebarOff(); off != nil {
		var baseOff, pendingOn SidebarState
		base.State, baseOff = splitSidebar(base.State, off)
		local, localOff = splitSidebar(local, off)
		pendingOn, _ = splitSidebar(pending.State, off)
		pending.State = pendingOn
		newBase = joinSidebar(pendingOn, baseOff)
	}
	merged, _, _ := MergeSidebar(base.State, assumeUnchanged(local, base.State), pending.State)
	merged = joinSidebar(merged, localOff)
	local = fullLocal
	replaced := ""
	if !sidebarEqual(merged, local) {
		if applied, err = writeSidebar(sidebarPath, merged); err != nil {
			return false, err
		}
		if applied {
			replaced = sidebarHash(local)
		}
	}
	// Storage held the pending layout when it was recorded, so it is the last
	// layout both sides agreed on; edits made here since then upload next sync.
	if err := saveSidebarSnapshot(basePath, storageID, newBase, replaced); err != nil {
		return applied, err
	}
	_ = os.Remove(pendingPath)
	return applied, nil
}

// SidebarFingerprint summarizes the sidebar layout in the desktop app's file
// (and nothing else the file holds), for change polling. It is "" when the
// file is missing or unreadable.
func SidebarFingerprint(path string) string {
	st, err := readSidebar(path)
	if err != nil || st.isEmpty() {
		return ""
	}
	return sidebarHash(st)
}

// sidebarPayload is the remote object's plaintext.
type sidebarPayload struct {
	Version int `json:"version"`
	SidebarState
}

// fetchSidebar downloads the layout in storage. exists is false when there is
// none yet.
func (s *Syncer) fetchSidebar(ctx context.Context) (st SidebarState, etag string, exists bool, err error) {
	var enc []byte
	if c, ok := s.conditional(); ok {
		enc, etag, err = c.DownloadWithETag(ctx, SidebarRemoteKey)
		if errors.Is(err, storage.ErrNotFound) {
			return SidebarState{}, "", false, nil
		}
	} else {
		// Without compare-and-swap, backends don't promise a recognizable
		// not-found error; a listing tells absence from failure.
		objs, lerr := s.storage.List(ctx, SidebarRemoteKey)
		if lerr != nil {
			return SidebarState{}, "", false, fmt.Errorf("failed to list sidebar layout: %w", lerr)
		}
		found := false
		for _, o := range objs {
			if o.Key == SidebarRemoteKey {
				found, etag = true, o.ETag
			}
		}
		if !found {
			return SidebarState{}, "", false, nil
		}
		enc, err = s.storage.Download(ctx, SidebarRemoteKey)
	}
	if err != nil {
		return SidebarState{}, "", false, fmt.Errorf("failed to download sidebar layout: %w", err)
	}
	data, err := s.encryptor.Decrypt(enc)
	if err != nil {
		return SidebarState{}, "", false, fmt.Errorf("failed to decrypt sidebar layout: %w", err)
	}
	if isGzipped(data) {
		if data, err = gzipDecompress(data); err != nil {
			return SidebarState{}, "", false, fmt.Errorf("failed to decompress sidebar layout: %w", err)
		}
	}
	// Every sidebar object was written with the header, so one without it was
	// put there by something else.
	if !bytes.HasPrefix(data, []byte(blobHeader)) {
		return SidebarState{}, "", false, fmt.Errorf("%s: %w", SidebarRemoteKey, ErrBlobMoved)
	}
	if data, err = unbindKey(data, SidebarRemoteKey); err != nil {
		return SidebarState{}, "", false, err
	}
	data = s.paths.ResolveContent(sidebarContentName, data)
	var p sidebarPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return SidebarState{}, "", false, fmt.Errorf("failed to parse sidebar layout: %w", err)
	}
	if p.Version != sidebarVersion {
		return SidebarState{}, "", false, fmt.Errorf("the sidebar layout in storage has format version %d, which this Stradl can't read (run 'stradl update')", p.Version)
	}
	return p.SidebarState, etag, true, nil
}

// putSidebar encodes and uploads a layout, with compare-and-swap against
// ifMatch when the backend supports it ("" with exists=false: create-only).
func (s *Syncer) putSidebar(ctx context.Context, st SidebarState, ifMatch string, exists bool) (string, error) {
	data, err := json.Marshal(sidebarPayload{Version: sidebarVersion, SidebarState: st})
	if err != nil {
		return "", err
	}
	data = s.paths.NormalizeContent(sidebarContentName, data)
	compressed, err := gzipCompress(bindKey(data, SidebarRemoteKey))
	if err != nil {
		return "", fmt.Errorf("failed to compress: %w", err)
	}
	enc, err := s.encryptor.Encrypt(compressed)
	if err != nil {
		return "", fmt.Errorf("failed to encrypt: %w", err)
	}
	if c, ok := s.conditional(); ok && (ifMatch != "" || !exists) {
		return c.UploadIf(ctx, SidebarRemoteKey, enc, ifMatch)
	}
	return "", s.storage.Upload(ctx, SidebarRemoteKey, enc)
}

// assumeUnchanged fills parts of the layout missing from the file with base's
// version, so they merge as unchanged rather than deleted. A whole account's
// groups or sections (or the whole layout) go missing when the app resets its
// preferences or a fresh file is created, never by editing the sidebar, and
// merging that as a deletion would wipe the layout on every Mac.
func assumeUnchanged(local, base SidebarState) SidebarState {
	out := local
	out.Scopes = fillMissing(local.Scopes, base.Scopes)
	out.Sections = fillMissing(local.Sections, base.Sections)
	if len(local.Scopes)+len(local.Sections) == 0 {
		out.StarredLocalCodeSessions = base.StarredLocalCodeSessions
		out.StarredSessionGroups = base.StarredSessionGroups
		out.PinnedOrder = base.PinnedOrder
		out.HomeProjectsPinnedOrder = base.HomeProjectsPinnedOrder
	}
	return out
}

// fillMissing returns local plus base's entries for keys local lacks, without
// modifying either map.
func fillMissing[V any](local, base map[string]V) map[string]V {
	var out map[string]V
	for k, v := range base {
		if _, ok := local[k]; ok {
			continue
		}
		if out == nil {
			out = make(map[string]V, len(local)+1)
			maps.Copy(out, local)
		}
		out[k] = v
	}
	if out == nil {
		return local
	}
	return out
}

// sidebarChanges counts the groups and assignments that differ between two
// layouts.
func sidebarChanges(from, to SidebarState) (groups, assignments int) {
	for _, k := range unionKeys(from.Scopes, to.Scopes) {
		f, t := orEmptyScope(from.Scopes[k]), orEmptyScope(to.Scopes[k])
		fg, tg := indexRaw(f.Groups, objectID), indexRaw(t.Groups, objectID)
		for _, id := range unionKeys(fg.byID, tg.byID) {
			a, aok := fg.byID[id]
			b, bok := tg.byID[id]
			if aok != bok || (aok && !rawEqual(a, b)) {
				groups++
			}
		}
		for _, sk := range unionKeys(f.Assignments, t.Assignments) {
			a, aok := f.Assignments[sk]
			b, bok := t.Assignments[sk]
			if aok != bok || a != b {
				assignments++
			}
		}
	}
	return groups, assignments
}

// --- base and pending snapshots -----------------------------------------------

// sidebarSnapshot is a layout saved in the state directory: the base (the
// last layout this Mac and storage agreed on) or a pending apply. Both belong
// to one storage target; against another they mean nothing.
type sidebarSnapshot struct {
	Version   int    `json:"version"`
	StorageID string `json:"storageId"`
	// Replaced hashes the layout the file held before Stradl last wrote it.
	Replaced string       `json:"replaced,omitempty"`
	State    SidebarState `json:"state"`
}

// loadSidebarSnapshot reads a snapshot, reporting ok=false (and an empty
// layout) when it is missing, unreadable or for another storage target. Like
// state.json it is a cache: losing it only turns the next merge into a union.
func loadSidebarSnapshot(path, storageID string) (sidebarSnapshot, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return sidebarSnapshot{}, false
	}
	var snap sidebarSnapshot
	if json.Unmarshal(data, &snap) != nil || snap.Version != sidebarVersion || snap.StorageID != storageID {
		return sidebarSnapshot{}, false
	}
	return snap, true
}

func saveSidebarSnapshot(path, storageID string, st SidebarState, replaced string) error {
	data, err := json.MarshalIndent(sidebarSnapshot{Version: sidebarVersion, StorageID: storageID, Replaced: replaced, State: st}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("failed to save %s: %w", filepath.Base(path), err)
	}
	return nil
}

// writeFileAtomic replaces path with data through a temp file and a rename,
// so a reader (the desktop app at launch) never sees half a file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".stradl-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// --- reading and writing the app's file -----------------------------------------

// readSidebar extracts the sidebar layout from the desktop app's preferences
// file. A missing or empty file, or missing keys, is an empty layout.
func readSidebar(path string) (SidebarState, error) {
	data, err := readDesktopConfig(path)
	if err != nil {
		return SidebarState{}, err
	}
	st, err := sidebarFromBytes(data)
	if err != nil {
		return SidebarState{}, fmt.Errorf("%s: %w", path, err)
	}
	return st, nil
}

// readDesktopConfig returns the file's content, or nil when it doesn't exist.
func readDesktopConfig(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// sidebarFromBytes extracts the sidebar layout from the file's content.
func sidebarFromBytes(data []byte) (SidebarState, error) {
	var st SidebarState
	root, err := parseDesktopConfig(data)
	if err != nil || root == nil {
		return st, err
	}
	ep, err := root.objectAt(false, "preferences", "epitaxyPrefs")
	if err != nil || ep == nil {
		// An error means a shape this code doesn't know. Read as empty, it
		// would look like every group was deleted here.
		return st, err
	}
	for _, f := range []struct {
		key  string
		into any
	}{
		{prefGroupScopes, &st.Scopes},
		{prefCodeSections, &st.Sections},
		{prefStarredCode, &st.StarredLocalCodeSessions},
		{prefStarredGroups, &st.StarredSessionGroups},
	} {
		if err := decodeOwned(ep, f.key, f.into); err != nil {
			return SidebarState{}, err
		}
	}
	slice, err := ep.objectAt(false, prefLocalSlice)
	if err != nil {
		return SidebarState{}, err
	}
	if slice != nil {
		if err := decodeOwned(slice, slicePinnedOrder, &st.PinnedOrder); err != nil {
			return SidebarState{}, err
		}
		if err := decodeOwned(slice, sliceHomeProjects, &st.HomeProjectsPinnedOrder); err != nil {
			return SidebarState{}, err
		}
	}
	return st, nil
}

// parseDesktopConfig parses the app's file keeping key order. root is nil for
// an empty file.
func parseDesktopConfig(data []byte) (*jsonObject, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	v, err := decodeOrdered(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse desktop app preferences: %w", err)
	}
	obj, ok := v.(*jsonObject)
	if !ok {
		return nil, errors.New("desktop app preferences are not a JSON object")
	}
	return obj, nil
}

func decodeOwned(obj *jsonObject, key string, into any) error {
	v, ok := obj.vals[key]
	if !ok || v == nil {
		return nil
	}
	raw, err := encodeJSON(v, "")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

// writeSidebar sets the sidebar keys in the desktop app's preferences file to
// st and leaves everything else as it was: other keys keep their values and
// their order, and the file keeps the app's formatting (two-space indent, no
// final newline) and permissions. Nothing is written when the layout already
// matches. The previous file is kept as <file>.stradl-backup.
func writeSidebar(path string, st SidebarState) (written bool, err error) {
	orig, err := readDesktopConfig(path)
	if err != nil {
		return false, err
	}
	mode := os.FileMode(0600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	out, err := applySidebar(orig, st)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if bytes.Equal(out, orig) {
		return false, nil
	}
	if len(orig) > 0 {
		if err := writeFileAtomic(path+".stradl-backup", orig, 0600); err != nil {
			return false, fmt.Errorf("failed to back up %s: %w", filepath.Base(path), err)
		}
	}
	if err := writeFileAtomic(path, out, mode); err != nil {
		return false, fmt.Errorf("failed to write %s: %w", path, err)
	}
	return true, nil
}

// applySidebar returns the file content orig with the sidebar keys set to st
// (see writeSidebar). It returns orig itself when nothing needs to change.
func applySidebar(orig []byte, st SidebarState) ([]byte, error) {
	root, err := parseDesktopConfig(orig)
	if err != nil {
		return nil, err
	}
	if root == nil {
		if st.isEmpty() {
			return orig, nil
		}
		root = newJSONObject()
	}

	ep, err := root.objectAt(false, "preferences", "epitaxyPrefs")
	if err != nil {
		return nil, err
	}
	if ep == nil {
		if st.isEmpty() {
			return orig, nil
		}
		if ep, err = root.objectAt(true, "preferences", "epitaxyPrefs"); err != nil {
			return nil, err
		}
	}
	scopes, sections := st.Scopes, st.Sections // nil would encode as null
	if scopes == nil {
		scopes = map[string]*GroupScope{}
	}
	if sections == nil {
		sections = map[string]*SectionScope{}
	}
	for _, f := range []struct {
		key   string
		val   any
		empty bool
	}{
		{prefGroupScopes, scopes, len(scopes) == 0},
		{prefCodeSections, sections, len(sections) == 0},
		{prefStarredCode, nonNilList(st.StarredLocalCodeSessions), len(st.StarredLocalCodeSessions) == 0},
		{prefStarredGroups, nonNilList(st.StarredSessionGroups), len(st.StarredSessionGroups) == 0},
	} {
		if err := setOwned(ep, f.key, f.val, f.empty); err != nil {
			return nil, err
		}
	}
	slice, err := ep.objectAt(false, prefLocalSlice)
	if err != nil {
		return nil, err
	}
	if slice == nil && len(st.PinnedOrder)+len(st.HomeProjectsPinnedOrder) > 0 {
		if slice, err = ep.objectAt(true, prefLocalSlice); err != nil {
			return nil, err
		}
	}
	if slice != nil {
		if err := setOwned(slice, slicePinnedOrder, nonNilList(st.PinnedOrder), len(st.PinnedOrder) == 0); err != nil {
			return nil, err
		}
		if err := setOwned(slice, sliceHomeProjects, nonNilList(st.HomeProjectsPinnedOrder), len(st.HomeProjectsPinnedOrder) == 0); err != nil {
			return nil, err
		}
	}

	out, err := encodeJSON(root, "  ")
	if err != nil {
		return nil, err
	}
	out = append(out, orig[len(bytes.TrimRight(orig, " \t\r\n")):]...) // keep the app's ending
	if bytes.Equal(out, orig) {
		return orig, nil
	}
	return out, nil
}

// setOwned replaces obj[key] with val, keeping the key's position and the key
// order of objects inside it. An empty value is not added where the key was
// absent. For the per-account maps, accounts the file has and val lacks are
// kept: accounts are only ever added.
func setOwned(obj *jsonObject, key string, val any, empty bool) error {
	old, exists := obj.vals[key]
	if !exists && empty {
		return nil
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return err
	}
	nv, err := decodeOrdered(raw)
	if err != nil {
		return err
	}
	merged := reconcile(old, nv)
	if mo, ok := merged.(*jsonObject); ok && (key == prefGroupScopes || key == prefCodeSections) {
		if oo, ok := old.(*jsonObject); ok {
			for _, k := range oo.keys {
				if _, ok := mo.vals[k]; !ok {
					mo.set(k, oo.vals[k])
				}
			}
		}
	}
	obj.set(key, merged)
	return nil
}

// reconcile returns nv, reusing ov's key order wherever both hold an object,
// so rewriting a value the app wrote changes only what actually differs.
// Array elements are paired by their "id", else by position.
func reconcile(ov, nv any) any {
	switch n := nv.(type) {
	case *jsonObject:
		o, ok := ov.(*jsonObject)
		if !ok {
			return n
		}
		out := newJSONObject()
		for _, k := range o.keys {
			if v, ok := n.vals[k]; ok {
				out.set(k, reconcile(o.vals[k], v))
			}
		}
		for _, k := range n.keys {
			if _, ok := out.vals[k]; !ok {
				out.set(k, n.vals[k])
			}
		}
		return out
	case []any:
		o, ok := ov.([]any)
		if !ok {
			return n
		}
		byID := make(map[string]any)
		for _, e := range o {
			if id := elementID(e); id != "" {
				byID[id] = e
			}
		}
		out := make([]any, len(n))
		for i, e := range n {
			var match any
			if id := elementID(e); id != "" {
				match = byID[id]
			} else if i < len(o) {
				match = o[i]
			}
			out[i] = reconcile(match, e)
		}
		return out
	}
	return nv
}

func elementID(v any) string {
	if o, ok := v.(*jsonObject); ok {
		if id, ok := o.vals["id"].(string); ok {
			return id
		}
	}
	return ""
}

// --- merge -------------------------------------------------------------------

// MergeSidebar merges two versions of the sidebar layout against base, the
// last layout both sides agreed on. It is deterministic, so every Mac reaches
// the same result from the same inputs; where both sides changed the same
// thing, remote wins (it reached storage first).
//
//   - Groups are matched by id and unioned. Each field (the name) merges
//     three ways. A group deleted on one side stays deleted unless the other
//     side changed it or moved a session into it, in which case it is kept.
//   - Assignments (which group a session is in) merge per session.
//   - Each group's order merges as a list: removals and additions from both
//     sides apply, the order comes from whichever side reordered (remote if
//     both did), and a session now assigned to another group leaves it.
//   - Sections merge by id, field by field; a group's own section lives and
//     dies with the group. New sections are unioned.
//   - Starred sessions and groups and the pinned orders merge like a group's
//     order.
//
// changedLocal and changedRemote report whether the result differs from
// local and from remote.
func MergeSidebar(base, local, remote SidebarState) (merged SidebarState, changedLocal, changedRemote bool) {
	type groupSets struct{ known, final map[string]bool }
	sets := make(map[string]groupSets)
	for _, key := range unionKeys(local.Scopes, remote.Scopes) {
		if merged.Scopes == nil {
			merged.Scopes = make(map[string]*GroupScope)
		}
		gs, known, final := mergeGroupScope(base.Scopes[key], local.Scopes[key], remote.Scopes[key])
		merged.Scopes[key] = gs
		sets[key] = groupSets{known, final}
	}
	for _, key := range unionKeys(local.Sections, remote.Sections) {
		if merged.Sections == nil {
			merged.Sections = make(map[string]*SectionScope)
		}
		merged.Sections[key] = mergeSectionScope(base.Sections[key], local.Sections[key], remote.Sections[key], sets[key].known, sets[key].final)
	}
	merged.StarredLocalCodeSessions = mergeRawList(base.StarredLocalCodeSessions, local.StarredLocalCodeSessions, remote.StarredLocalCodeSessions)
	merged.StarredSessionGroups = mergeRawList(base.StarredSessionGroups, local.StarredSessionGroups, remote.StarredSessionGroups)
	merged.PinnedOrder = mergeRawList(base.PinnedOrder, local.PinnedOrder, remote.PinnedOrder)
	merged.HomeProjectsPinnedOrder = mergeRawList(base.HomeProjectsPinnedOrder, local.HomeProjectsPinnedOrder, remote.HomeProjectsPinnedOrder)
	mc := merged.canonical()
	return merged, mc != local.canonical(), mc != remote.canonical()
}

// mergeGroupScope merges one account's groups. known lists every group id
// any side has; final the ones that survive.
func mergeGroupScope(b, l, r *GroupScope) (merged *GroupScope, known, final map[string]bool) {
	b, l, r = orEmptyScope(b), orEmptyScope(l), orEmptyScope(r)
	bg, lg, rg := indexRaw(b.Groups, objectID), indexRaw(l.Groups, objectID), indexRaw(r.Groups, objectID)

	assign := make(map[string]string)
	for _, k := range unionKeys(b.Assignments, l.Assignments, r.Assignments) {
		bv, bok := b.Assignments[k]
		lv, lok := l.Assignments[k]
		rv, rok := r.Assignments[k]
		if v, ok := pick3(bv, lv, rv, bok, lok, rok, func(x, y string) bool { return x == y }); ok {
			assign[k] = v
		}
	}

	known = make(map[string]bool)
	final = make(map[string]bool)
	groups := make(map[string]json.RawMessage)
	for _, id := range unionKeys(bg.byID, lg.byID, rg.byID) {
		known[id] = true
		bv, bok := bg.byID[id]
		lv, lok := lg.byID[id]
		rv, rok := rg.byID[id]
		switch {
		case lok && rok:
			groups[id] = mergeObjectFields(bv, lv, rv, bok)
		case lok && (!bok || groupTouched(id, b, l, bv, lv)):
			groups[id] = lv // new here, or deleted there while changed here
		case rok && (!bok || groupTouched(id, b, r, bv, rv)):
			groups[id] = rv
		default:
			continue // deleted (on one side, unchanged on the other, or on both)
		}
		final[id] = true
	}
	// Sessions in a deleted group become ungrouped.
	for k, g := range assign {
		if known[g] && !final[g] {
			delete(assign, k)
		}
	}

	merged = &GroupScope{Assignments: assign, Order: make(map[string][]string)}
	for _, id := range orderItems(bg.ids, lg.ids, rg.ids, func(id string) bool { return final[id] }) {
		merged.Groups = append(merged.Groups, groups[id])
	}
	for _, g := range unionKeys(l.Order, r.Order) {
		if known[g] && !final[g] {
			continue
		}
		bo, lo, ro := b.Order[g], l.Order[g], r.Order[g]
		inB, inL, inR := setOf(bo), setOf(lo), setOf(ro)
		merged.Order[g] = orderItems(bo, lo, ro, func(k string) bool {
			if a, ok := assign[k]; ok && a != g {
				return false // moved to another group
			}
			return keep3(inB[k], inL[k], inR[k])
		})
	}
	merged.Extra = mergeRawMap(b.Extra, l.Extra, r.Extra)
	return merged, known, final
}

// groupTouched reports whether side changed group id since base: edited it,
// or assigned a session to it.
func groupTouched(id string, base, side *GroupScope, baseRaw, sideRaw json.RawMessage) bool {
	if !rawEqual(baseRaw, sideRaw) {
		return true
	}
	for k, g := range side.Assignments {
		if g == id && base.Assignments[k] != id {
			return true
		}
	}
	return false
}

// mergeSectionScope merges one account's sections. A section whose id is a
// group's (known) exists exactly when that group survives (final).
func mergeSectionScope(b, l, r *SectionScope, known, final map[string]bool) *SectionScope {
	b, l, r = orEmptySections(b), orEmptySections(l), orEmptySections(r)
	bs, ls, rs := indexRaw(b.Sections, objectID), indexRaw(l.Sections, objectID), indexRaw(r.Sections, objectID)
	objs := make(map[string]json.RawMessage)
	for _, id := range unionKeys(ls.byID, rs.byID) {
		bv, bok := bs.byID[id]
		lv, lok := ls.byID[id]
		rv, rok := rs.byID[id]
		switch {
		case known[id] && !final[id]:
			// The group is gone, and its section with it.
		case lok && rok:
			objs[id] = mergeObjectFields(bv, lv, rv, bok)
		case lok && (known[id] || !bok || !rawEqual(bv, lv)):
			objs[id] = lv
		case rok && (known[id] || !bok || !rawEqual(bv, rv)):
			objs[id] = rv
		}
	}
	out := &SectionScope{Extra: mergeRawMap(b.Extra, l.Extra, r.Extra)}
	for _, id := range orderItems(bs.ids, ls.ids, rs.ids, func(id string) bool { _, ok := objs[id]; return ok }) {
		out.Sections = append(out.Sections, objs[id])
	}
	return out
}

// mergeRawList merges a list of opaque entries (starred items, pinned order),
// identified by their canonical JSON.
func mergeRawList(b, l, r []json.RawMessage) []json.RawMessage {
	bi, li, ri := indexRaw(b, rawID), indexRaw(l, rawID), indexRaw(r, rawID)
	ids := orderItems(bi.ids, li.ids, ri.ids, func(id string) bool {
		_, inB := bi.byID[id]
		_, inL := li.byID[id]
		_, inR := ri.byID[id]
		return keep3(inB, inL, inR)
	})
	var out []json.RawMessage
	for _, id := range ids {
		if v, ok := li.byID[id]; ok {
			out = append(out, v)
		} else {
			out = append(out, ri.byID[id])
		}
	}
	return out
}

// mergeRawMap merges the opaque extra keys of a scope, key by key.
func mergeRawMap(b, l, r map[string]json.RawMessage) map[string]json.RawMessage {
	var out map[string]json.RawMessage
	for _, k := range unionKeys(b, l, r) {
		bv, bok := b[k]
		lv, lok := l[k]
		rv, rok := r[k]
		if v, ok := pick3(bv, lv, rv, bok, lok, rok, rawEqual); ok {
			if out == nil {
				out = make(map[string]json.RawMessage)
			}
			out[k] = v
		}
	}
	return out
}

// mergeObjectFields merges a group or section object present on both sides,
// field by field, keeping local's key order. Non-objects merge whole.
func mergeObjectFields(b, l, r json.RawMessage, bok bool) json.RawMessage {
	lo, lok := decodeObject(l)
	ro, rok := decodeObject(r)
	if !lok || !rok {
		v, _ := pick3(b, l, r, bok, true, true, rawEqual)
		return v
	}
	bo, _ := decodeObject(b)
	if !bok || bo == nil {
		bo = newJSONObject()
	}
	out := newJSONObject()
	keys := append(append([]string{}, lo.keys...), ro.keys...)
	for _, k := range keys {
		if _, done := out.vals[k]; done {
			continue
		}
		bv, bk := bo.vals[k]
		lv, lk := lo.vals[k]
		rv, rk := ro.vals[k]
		if v, ok := pick3(bv, lv, rv, bk, lk, rk, valueEqual); ok {
			out.set(k, v)
		}
	}
	raw, err := encodeJSON(out, "")
	if err != nil {
		return r
	}
	return raw
}

// pick3 resolves one value three ways. The ok flags say whether the value
// exists on that side. A side that didn't change takes the other side's
// version; when both changed, a deletion loses to an edit and otherwise
// remote wins.
func pick3[T any](b, l, r T, bok, lok, rok bool, eq func(x, y T) bool) (T, bool) {
	same := func(x T, xok bool, y T, yok bool) bool { return xok == yok && (!xok || eq(x, y)) }
	switch {
	case same(l, lok, r, rok):
		return l, lok
	case same(l, lok, b, bok):
		return r, rok
	case same(r, rok, b, bok):
		return l, lok
	case !rok:
		return l, lok
	default:
		return r, rok
	}
}

// keep3 decides an entry of a set three ways: kept when both sides have it,
// or when one side added it; dropped when one side removed it.
func keep3(inB, inL, inR bool) bool {
	return (inL && inR) || (inL && !inB) || (inR && !inB)
}

// orderItems orders the entries keep accepts. The order comes from the side
// that reordered entries since base (remote when both did, or when there is
// no base yet), and entries only the other side lists go right after their
// nearest predecessor in that side's list.
func orderItems(base, local, remote []string, keep func(string) bool) []string {
	primary, secondary := local, remote
	if reordered(remote, base) {
		primary, secondary = remote, local
	}
	out := make([]string, 0, len(primary)+len(secondary))
	placed := make(map[string]bool, len(primary)+len(secondary))
	for _, x := range primary {
		if !placed[x] && keep(x) {
			out = append(out, x)
			placed[x] = true
		}
	}
	after := -1 // position in out of the secondary entry seen last
	for _, x := range secondary {
		if placed[x] {
			after = slices.Index(out, x)
			continue
		}
		if !keep(x) {
			continue
		}
		out = slices.Insert(out, after+1, x)
		placed[x] = true
		after++
	}
	return out
}

// reordered reports whether seq orders the entries it shares with base
// differently from base. A list that exists where base had none counts as
// reordered.
func reordered(seq, base []string) bool {
	if len(base) == 0 {
		return len(seq) > 0
	}
	inSeq, inBase := setOf(seq), setOf(base)
	var a, b []string
	seen := make(map[string]bool)
	for _, x := range seq {
		if inBase[x] && !seen[x] {
			a = append(a, x)
			seen[x] = true
		}
	}
	seen = make(map[string]bool)
	for _, x := range base {
		if inSeq[x] && !seen[x] {
			b = append(b, x)
			seen[x] = true
		}
	}
	return !slices.Equal(a, b)
}

// rawIndex is a list of raw JSON entries indexed by identity (first wins).
type rawIndex struct {
	ids  []string
	byID map[string]json.RawMessage
}

func indexRaw(items []json.RawMessage, id func(json.RawMessage) string) rawIndex {
	idx := rawIndex{byID: make(map[string]json.RawMessage, len(items))}
	for _, it := range items {
		k := id(it)
		if _, dup := idx.byID[k]; dup {
			continue
		}
		idx.ids = append(idx.ids, k)
		idx.byID[k] = it
	}
	return idx
}

// objectID is an object's "id", or its canonical JSON (behind a NUL, so it
// can't collide with a real id) when it has none.
func objectID(raw json.RawMessage) string {
	var p struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(raw, &p) == nil {
		var s string
		if json.Unmarshal(p.ID, &s) == nil && s != "" {
			return s
		}
	}
	return "\x00" + canonicalJSON(raw)
}

func rawID(raw json.RawMessage) string { return canonicalJSON(raw) }

func orEmptyScope(g *GroupScope) *GroupScope {
	if g == nil {
		return &GroupScope{}
	}
	return g
}

func orEmptySections(s *SectionScope) *SectionScope {
	if s == nil {
		return &SectionScope{}
	}
	return s
}

func setOf(l []string) map[string]bool {
	m := make(map[string]bool, len(l))
	for _, x := range l {
		m[x] = true
	}
	return m
}

// unionKeys returns the keys of all maps, sorted.
func unionKeys[V any](maps ...map[string]V) []string {
	seen := make(map[string]bool)
	var keys []string
	for _, m := range maps {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// --- order-preserving JSON ------------------------------------------------------

// jsonObject is a decoded JSON object that remembers its key order. Values are
// *jsonObject, []any, string, json.Number (numbers stay exactly as written),
// bool or nil.
type jsonObject struct {
	keys []string
	vals map[string]any
}

func newJSONObject() *jsonObject { return &jsonObject{vals: make(map[string]any)} }

// set replaces k's value in place, or appends k.
func (o *jsonObject) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// objectAt walks a path of object keys. A missing step is created when create
// is set, else the result is nil; a step that holds something other than an
// object is an error, so nothing the app stores differently gets replaced.
func (o *jsonObject) objectAt(create bool, path ...string) (*jsonObject, error) {
	cur := o
	for _, k := range path {
		v, ok := cur.vals[k]
		if !ok {
			if !create {
				return nil, nil
			}
			child := newJSONObject()
			cur.set(k, child)
			cur = child
			continue
		}
		child, ok := v.(*jsonObject)
		if !ok {
			return nil, fmt.Errorf("desktop app preferences: %q is not an object", k)
		}
		cur = child
	}
	return cur, nil
}

// MarshalJSON writes the object compactly, in key order.
func (o *jsonObject) MarshalJSON() ([]byte, error) { return encodeJSON(o, "") }

func decodeObject(raw json.RawMessage) (*jsonObject, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	v, err := decodeOrdered(raw)
	if err != nil {
		return nil, false
	}
	o, ok := v.(*jsonObject)
	return o, ok
}

const maxJSONDepth = 512

// decodeOrdered parses one JSON value, keeping object key order and numbers
// as written.
func decodeOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after the JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder, depth int) (any, error) {
	if depth > maxJSONDepth {
		return nil, errors.New("JSON nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil // string, json.Number, bool or nil
	}
	switch d {
	case '{':
		obj := newJSONObject()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, _ := kt.(string)
			v, err := decodeValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			obj.set(k, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return obj, nil
	case '[':
		arr := []any{}
		for dec.More() {
			v, err := decodeValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return arr, nil
	}
	return nil, fmt.Errorf("unexpected %q", d)
}

// encodeJSON writes v compactly (indent "") or the way JavaScript's
// JSON.stringify(v, null, indent) does, which is how the desktop app writes
// its file: unchanged parts of a file it wrote come out byte for byte.
func encodeJSON(v any, indent string) ([]byte, error) {
	var b bytes.Buffer
	if err := writeJSONValue(&b, v, indent, 0); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeJSONValue(b *bytes.Buffer, v any, indent string, depth int) error {
	newline := func(d int) {
		if indent != "" {
			b.WriteByte('\n')
			for i := 0; i < d; i++ {
				b.WriteString(indent)
			}
		}
	}
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		b.WriteString(t.String())
	case string:
		writeJSONString(b, t)
	case *jsonObject:
		if len(t.keys) == 0 {
			b.WriteString("{}")
			return nil
		}
		b.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			newline(depth + 1)
			writeJSONString(b, k)
			b.WriteByte(':')
			if indent != "" {
				b.WriteByte(' ')
			}
			if err := writeJSONValue(b, t.vals[k], indent, depth+1); err != nil {
				return err
			}
		}
		newline(depth)
		b.WriteByte('}')
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return nil
		}
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			newline(depth + 1)
			if err := writeJSONValue(b, e, indent, depth+1); err != nil {
				return err
			}
		}
		newline(depth)
		b.WriteByte(']')
	default:
		return fmt.Errorf("cannot encode %T", v)
	}
	return nil
}

// writeJSONString escapes like JSON.stringify: quotes, backslashes and
// control characters only; everything else is written as is.
func writeJSONString(b *bytes.Buffer, s string) {
	const hexDigits = "0123456789abcdef"
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hexDigits[r>>4])
				b.WriteByte(hexDigits[r&0xF])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// canonicalJSON renders raw JSON with sorted keys and no spacing, so two
// spellings of the same value compare equal.
func canonicalJSON(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

func rawEqual(a, b json.RawMessage) bool { return canonicalJSON(a) == canonicalJSON(b) }

// valueEqual compares two decoded values canonically.
func valueEqual(a, b any) bool {
	ra, err1 := encodeJSON(a, "")
	rb, err2 := encodeJSON(b, "")
	return err1 == nil && err2 == nil && canonicalJSON(ra) == canonicalJSON(rb)
}
