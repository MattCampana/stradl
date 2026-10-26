package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/storage"
)

// Synthetic ids: one account scope, three folder groups, a pool of sessions.
const (
	sbScope = testAcct + "/" + testOrg
	gWeb    = "cg-aaaaaaaa-0000-4000-8000-000000000001"
	gOps    = "cg-aaaaaaaa-0000-4000-8000-000000000002"
	gLab    = "cg-aaaaaaaa-0000-4000-8000-000000000003"
)

func sk(n int) string { return fmt.Sprintf("code:local_bbbbbbbb-0000-4000-8000-%012d", n) }

func fixtureSection(id, kind, name string, order int) string {
	members := ""
	if kind == "manual" {
		members = "\n              \"members\": [],"
	}
	return fmt.Sprintf(`            {
              "id": %q,
              "kind": %q,%s
              "v": 2,
              "name": %q,
              "order": %d,
              "sortBy": "recent",
              "ascending": false,
              "show": {
                "metadata": true,
                "emptyGroups": true,
                "sideChats": false
              },
              "collapsed": false,
              "shown": true
            }`, id, kind, members, name, order)
}

// sidebarFixture is a synthetic claude_desktop_config.json written the way the
// desktop app writes it (JSON.stringify with a two-space indent, no final
// newline), with keys Stradl must leave alone around the sidebar layout.
var sidebarFixture = strings.NewReplacer("SCOPE", sbScope, "GWEB", gWeb, "GOPS", gOps, "GLAB", gLab, "S1", sk(1), "S2", sk(2),
	"SECTIONS", strings.Join([]string{
		fixtureSection("pinned", "pinned", "Pinned", 0),
		fixtureSection(gWeb, "manual", "Websites", 1),
		fixtureSection(gOps, "manual", "Ops – café", 2),
		fixtureSection(gLab, "manual", "Lab", 3),
		fixtureSection("sessions", "sessions", "Sessions", 4),
	}, ",\n")).Replace(`{
  "mcpServers": {
    "files": {
      "command": "npx",
      "args": [
        "-y",
        "@modelcontextprotocol/server-filesystem",
        "/Users/someone/Documents"
      ],
      "env": {
        "API_TOKEN": "synthetic-token-not-real"
      }
    }
  },
  "coworkUserFilesPath": "/Users/someone/Claude Files",
  "preferences": {
    "sidebarMode": "code",
    "quickEntryNote": "<b>bold</b> & \"quoted\" \u0007 tab\there",
    "zoomFactor": 1.25,
    "firstLaunchAt": 1730000000000,
    "epitaxyPrefs": {
      "desktop-frame.paneStore.v1": {
        "width": 320,
        "panes": []
      },
      "starred-local-code-sessions": [],
      "starred-cowork-spaces": [],
      "starred-session-groups": [],
      "dframe-group-scopes": {
        "SCOPE": {
          "groups": [
            {
              "id": "GWEB",
              "name": "Websites"
            },
            {
              "id": "GOPS",
              "name": "Ops – café"
            },
            {
              "id": "GLAB",
              "name": "Lab"
            }
          ],
          "assignments": {
            "S1": "GWEB",
            "S2": "GOPS"
          },
          "order": {
            "GWEB": [
              "S1"
            ],
            "GOPS": [
              "S2"
            ]
          }
        }
      },
      "dframe-local-slice": {
        "pinnedOrder": [],
        "homeProjectsPinnedOrder": []
      },
      "ccd-sessions-filter": {
        "status": "all"
      },
      "dframe-code-sections": {
        "SCOPE": {
          "sections": [
SECTIONS
          ],
          "migratedFrom": 1,
          "prefsSeeded": true
        }
      }
    }
  }
}`)

// appFile is a device's desktop preferences file, edited the way the app
// edits it: read everything, change the sidebar, write everything back.
type appFile struct {
	t    *testing.T
	path string
}

func sidebarFile(d *device) appFile {
	return appFile{d.t, filepath.Join(filepath.Dir(d.desktop), sidebarConfigName)}
}

func (f appFile) write(content string) {
	f.t.Helper()
	if err := os.WriteFile(f.path, []byte(content), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f appFile) bytes() []byte {
	f.t.Helper()
	b, err := os.ReadFile(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func (f appFile) load() map[string]any {
	f.t.Helper()
	dec := json.NewDecoder(bytes.NewReader(f.bytes()))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		f.t.Fatalf("%s: %v", f.path, err)
	}
	return m
}

func (f appFile) save(m map[string]any) {
	f.t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		f.t.Fatal(err)
	}
	f.write(strings.TrimRight(buf.String(), "\n"))
}

func (f appFile) edit(fn func(ep map[string]any)) {
	f.t.Helper()
	m := f.load()
	fn(m["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any))
	f.save(m)
}

func (f appFile) prefs() map[string]any {
	return f.load()["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any)
}

func (f appFile) state() SidebarState {
	f.t.Helper()
	st, err := readSidebar(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

func (f appFile) groupOf(session string) string {
	g, _ := groupScopeOf(f.prefs())["assignments"].(map[string]any)[session].(string)
	return g
}

func (f appFile) groupName(id string) (string, bool) {
	for _, g := range groupScopeOf(f.prefs())["groups"].([]any) {
		if g.(map[string]any)["id"] == id {
			return g.(map[string]any)["name"].(string), true
		}
	}
	return "", false
}

func (f appFile) order(id string) []string {
	var out []string
	l, _ := groupScopeOf(f.prefs())["order"].(map[string]any)[id].([]any)
	for _, x := range l {
		out = append(out, x.(string))
	}
	return out
}

func (f appFile) hasSection(id string) bool {
	for _, s := range sectionsOf(f.prefs()) {
		if s.(map[string]any)["id"] == id {
			return true
		}
	}
	return false
}

func groupScopeOf(ep map[string]any) map[string]any {
	return ep["dframe-group-scopes"].(map[string]any)[sbScope].(map[string]any)
}

func sectionsOf(ep map[string]any) []any {
	return ep["dframe-code-sections"].(map[string]any)[sbScope].(map[string]any)["sections"].([]any)
}

func setSections(ep map[string]any, l []any) {
	ep["dframe-code-sections"].(map[string]any)[sbScope].(map[string]any)["sections"] = l
}

func without(l []any, x any) []any {
	out := []any{}
	for _, e := range l {
		if e != x {
			out = append(out, e)
		}
	}
	return out
}

// moveSession puts a session into group g ("" ungroups it), as the app does.
func moveSession(ep map[string]any, session, g string) {
	gs := groupScopeOf(ep)
	asg := gs["assignments"].(map[string]any)
	order, _ := gs["order"].(map[string]any)
	if order == nil {
		order = map[string]any{}
		gs["order"] = order
	}
	for id, l := range order {
		order[id] = without(l.([]any), session)
	}
	if g == "" {
		delete(asg, session)
		return
	}
	asg[session] = g
	l, _ := order[g].([]any)
	order[g] = append(l, session)
}

func renameGroup(ep map[string]any, id, name string) {
	for _, g := range groupScopeOf(ep)["groups"].([]any) {
		if g.(map[string]any)["id"] == id {
			g.(map[string]any)["name"] = name
		}
	}
	for _, s := range sectionsOf(ep) {
		if s.(map[string]any)["id"] == id {
			s.(map[string]any)["name"] = name
		}
	}
}

func addGroup(ep map[string]any, id, name string) {
	gs := groupScopeOf(ep)
	gs["groups"] = append(gs["groups"].([]any), map[string]any{"id": id, "name": name})
	setSections(ep, append(sectionsOf(ep), map[string]any{
		"id": id, "kind": "manual", "v": 2, "name": name, "order": len(sectionsOf(ep)), "members": []any{},
	}))
}

func deleteGroup(ep map[string]any, id string) {
	gs := groupScopeOf(ep)
	var groups []any
	for _, g := range gs["groups"].([]any) {
		if g.(map[string]any)["id"] != id {
			groups = append(groups, g)
		}
	}
	gs["groups"] = groups
	for k, v := range gs["assignments"].(map[string]any) {
		if v == id {
			delete(gs["assignments"].(map[string]any), k)
		}
	}
	delete(gs["order"].(map[string]any), id)
	var sections []any
	for _, s := range sectionsOf(ep) {
		if s.(map[string]any)["id"] != id {
			sections = append(sections, s)
		}
	}
	setSections(ep, sections)
}

func (d *device) syncSidebar(appRunning bool) SidebarResult {
	d.t.Helper()
	res, err := d.s.SyncSidebar(context.Background(), func() bool { return appRunning })
	if err != nil {
		d.t.Fatalf("%s: sidebar sync: %v", d.name, err)
	}
	return res
}

// newSidebarPair sets up two Macs sharing a container: alice has the layout
// with sessions in groups, bob has the same groups (their names came through
// the Claude account) but none of the membership, plus his own window width
// and MCP servers. Neither has synced the sidebar yet.
func newSidebarPair(t *testing.T, store storage.Storage) (*device, *device) {
	t.Helper()
	root := t.TempDir()
	a := newDevice(t, root, "alice", store)
	sidebarFile(a).write(sidebarFixture)
	return a, newEmptyGroupsMac(t, root, "bob", store)
}

// newEmptyGroupsMac adds a Mac whose groups have names but no sessions.
func newEmptyGroupsMac(t *testing.T, root, name string, store storage.Storage) *device {
	t.Helper()
	d := newDevice(t, root, name, store)
	f := sidebarFile(d)
	f.write(sidebarFixture)
	m := f.load()
	m["mcpServers"] = map[string]any{"notes": map[string]any{"command": "uvx", "args": []any{"notes-mcp"}}}
	ep := m["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any)
	ep["desktop-frame.paneStore.v1"].(map[string]any)["width"] = json.Number("280")
	gs := groupScopeOf(ep)
	gs["assignments"] = map[string]any{}
	gs["order"] = map[string]any{}
	f.save(m)
	return d
}

func stubDesktopApp(t *testing.T, running bool) {
	old := DesktopAppRunning
	DesktopAppRunning = func() bool { return running }
	t.Cleanup(func() { DesktopAppRunning = old })
}

// --- file handling ---------------------------------------------------------------

func TestSidebarEncoderMatchesTheApp(t *testing.T) {
	v, err := decodeOrdered([]byte(sidebarFixture))
	if err != nil {
		t.Fatal(err)
	}
	out, err := encodeJSON(v, "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != sidebarFixture {
		t.Fatalf("re-encoding the app's file changed it:\n%s", out)
	}
}

func TestSidebarWriteChangesOnlyTheLayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), sidebarConfigName)
	if err := os.WriteFile(path, []byte(sidebarFixture), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := readSidebar(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Scopes[sbScope].Assignments[sk(1)]; got != gWeb {
		t.Fatalf("read assignment %q", got)
	}
	if n := len(st.Sections[sbScope].Sections); n != 5 {
		t.Fatalf("read %d sections", n)
	}
	if written, err := writeSidebar(path, st); err != nil || written {
		t.Fatalf("writing back an unchanged layout must leave the file alone (written=%v, err=%v)", written, err)
	}

	// A rename changes that one line and nothing else.
	gs := st.Scopes[sbScope]
	gs.Groups[0] = json.RawMessage(`{"id":"` + gWeb + `","name":"Sites"}`)
	if _, err := writeSidebar(path, st); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(sidebarFixture, `"name": "Websites"`, `"name": "Sites"`, 1)
	if got, _ := os.ReadFile(path); string(got) != want {
		t.Fatalf("rename rewrote more than the name:\n%s", got)
	}

	// A move changes the assignment and both order lists in place.
	gs.Assignments[sk(1)] = gOps
	gs.Order[gWeb] = []string{}
	gs.Order[gOps] = []string{sk(2), sk(1)}
	if _, err := writeSidebar(path, st); err != nil {
		t.Fatal(err)
	}
	want = strings.NewReplacer(
		`"`+sk(1)+`": "`+gWeb+`"`, `"`+sk(1)+`": "`+gOps+`"`,
		`"`+gWeb+`": [
              "`+sk(1)+`"
            ]`, `"`+gWeb+`": []`,
		`"`+gOps+`": [
              "`+sk(2)+`"
            ]`, `"`+gOps+`": [
              "`+sk(2)+`",
              "`+sk(1)+`"
            ]`).Replace(want)
	got, _ := os.ReadFile(path)
	if string(got) != want {
		t.Fatalf("move rewrote more than the membership:\n%s", got)
	}

	// Everything Stradl doesn't own is untouched, values and all.
	strip := func(data []byte) map[string]any {
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		ep := m["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any)
		for _, k := range []string{prefGroupScopes, prefCodeSections, prefStarredCode, prefStarredGroups, prefLocalSlice} {
			delete(ep, k)
		}
		return m
	}
	if !reflect.DeepEqual(strip(got), strip([]byte(sidebarFixture))) {
		t.Fatal("keys outside the sidebar layout changed")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("file mode %v, want 0600", info.Mode().Perm())
	}
	if backup, err := os.ReadFile(path + ".stradl-backup"); err != nil || len(backup) == 0 {
		t.Fatalf("no backup of the previous file: %v", err)
	}
}

// TestSidebarRealFileRoundTrip checks, read-only and in memory, that a real
// claude_desktop_config.json reads and re-encodes byte for byte. Set
// STRADL_REAL_DESKTOP_CONFIG to its path to run it.
func TestSidebarRealFileRoundTrip(t *testing.T) {
	path := os.Getenv("STRADL_REAL_DESKTOP_CONFIG")
	if path == "" {
		t.Skip("set STRADL_REAL_DESKTOP_CONFIG to a claude_desktop_config.json to run")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sidebarFromBytes(data)
	if err != nil {
		t.Fatalf("reading the sidebar layout: %v", err)
	}
	out, err := applySidebar(data, st)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, data) {
		i := 0
		for i < len(out) && i < len(data) && out[i] == data[i] {
			i++
		}
		t.Fatalf("applying the unchanged layout would change the file from byte %d of %d", i, len(data))
	}
	t.Logf("%d bytes round-trip unchanged; %d group scopes, %d section scopes", len(data), len(st.Scopes), len(st.Sections))
}

func TestSidebarMissingOrEmptyFile(t *testing.T) {
	store := newCASStore()
	a, b := newSidebarPair(t, store)
	fa := sidebarFile(a)

	if err := os.Remove(fa.path); err != nil {
		t.Fatal(err)
	}
	if st, err := readSidebar(fa.path); err != nil || !st.isEmpty() {
		t.Fatalf("missing file: %+v, %v", st, err)
	}
	if res := a.syncSidebar(false); res != (SidebarResult{}) {
		t.Fatalf("nothing anywhere, yet %+v", res)
	}
	if _, err := os.Stat(fa.path); !os.IsNotExist(err) || len(store.keys()) > 1 {
		t.Fatalf("an empty layout created a file or uploaded something: %v %v", err, store.keys())
	}

	fa.write("")
	a.syncSidebar(false)
	if len(fa.bytes()) != 0 {
		t.Fatal("an empty file was rewritten with nothing to put in it")
	}

	// Once another Mac has a layout, it lands here in a new file.
	b.syncSidebar(false)
	if err := os.Remove(fa.path); err != nil {
		t.Fatal(err)
	}
	if res := a.syncSidebar(false); !res.AppliedLocally {
		t.Fatalf("layout not applied to a missing file: %+v", res)
	}
	if name, ok := fa.groupName(gOps); !ok || name != "Ops – café" {
		t.Fatalf("group in new file: %q %v", name, ok)
	}
	if info, _ := os.Stat(fa.path); info.Mode().Perm() != 0600 {
		t.Fatalf("new file mode %v", info.Mode().Perm())
	}
}

func TestSidebarSkippedWhenDesktopSyncIsOff(t *testing.T) {
	store := newCASStore()
	a, _ := newSidebarPair(t, store)
	off := false
	a.s.cfg.DesktopSync = &off
	if res := a.syncSidebar(false); res != (SidebarResult{}) || len(store.keys()) != 0 {
		t.Fatalf("desktop sync off, yet %+v %v", res, store.keys())
	}
}

// --- syncing between Macs ---------------------------------------------------------

func TestSidebarMembershipReachesOtherMac(t *testing.T) {
	store := newCASStore()
	a, b := newSidebarPair(t, store)
	fa, fb := sidebarFile(a), sidebarFile(b)

	if res := a.syncSidebar(false); !res.Uploaded || res.AppliedLocally {
		t.Fatalf("alice: %+v", res)
	}
	res := b.syncSidebar(false)
	if !res.AppliedLocally || res.AssignmentsChanged != 2 || res.GroupsChanged != 0 {
		t.Fatalf("bob: %+v", res)
	}
	if fb.groupOf(sk(1)) != gWeb || fb.groupOf(sk(2)) != gOps || !slices.Equal(fb.order(gWeb), []string{sk(1)}) {
		t.Fatalf("bob's groups are still empty: %v %v", fb.prefs()[prefGroupScopes], fb.order(gWeb))
	}
	// Bob's own window width and MCP servers stay his.
	m := fb.load()
	if w := m["preferences"].(map[string]any)["epitaxyPrefs"].(map[string]any)["desktop-frame.paneStore.v1"].(map[string]any)["width"]; w != json.Number("280") {
		t.Fatalf("bob's pane width became %v", w)
	}
	if _, ok := m["mcpServers"].(map[string]any)["notes"]; !ok {
		t.Fatal("bob's MCP servers were replaced")
	}

	// A later move on alice follows.
	fa.edit(func(ep map[string]any) { moveSession(ep, sk(1), gOps) })
	a.syncSidebar(false)
	b.syncSidebar(false)
	if fb.groupOf(sk(1)) != gOps || !slices.Equal(fb.order(gOps), []string{sk(2), sk(1)}) || len(fb.order(gWeb)) != 0 {
		t.Fatalf("move didn't follow: %s %v %v", fb.groupOf(sk(1)), fb.order(gOps), fb.order(gWeb))
	}
	if res := a.syncSidebar(false); res.Uploaded || res.AppliedLocally {
		t.Fatalf("settled layout still moving: %+v", res)
	}
}

func TestSidebarRenameAndMoveBothSurvive(t *testing.T) {
	store := newCASStore()
	a, b := newSidebarPair(t, store)
	fa, fb := sidebarFile(a), sidebarFile(b)
	a.syncSidebar(false)
	b.syncSidebar(false)

	fa.edit(func(ep map[string]any) { renameGroup(ep, gWeb, "Sites") })
	fb.edit(func(ep map[string]any) { moveSession(ep, sk(2), gLab) })
	a.syncSidebar(false)
	b.syncSidebar(false)
	a.syncSidebar(false)
	for _, f := range []appFile{fa, fb} {
		if name, _ := f.groupName(gWeb); name != "Sites" || f.groupOf(sk(2)) != gLab || f.groupOf(sk(1)) != gWeb {
			t.Fatalf("%s: name %q, s2 in %s", f.path, name, f.groupOf(sk(2)))
		}
	}
}

func TestSidebarSameSessionMovedOnBothMacs(t *testing.T) {
	for _, bobFirst := range []bool{false, true} {
		store := newCASStore()
		a, b := newSidebarPair(t, store)
		fa, fb := sidebarFile(a), sidebarFile(b)
		a.syncSidebar(false)
		b.syncSidebar(false)

		fa.edit(func(ep map[string]any) { moveSession(ep, sk(1), gOps) })
		fb.edit(func(ep map[string]any) { moveSession(ep, sk(1), gLab) })
		first, second, want := a, b, gOps
		if bobFirst {
			first, second, want = b, a, gLab
		}
		first.syncSidebar(false)
		second.syncSidebar(false)
		first.syncSidebar(false)
		// The move that reached storage first wins, on both Macs.
		for _, f := range []appFile{fa, fb} {
			if f.groupOf(sk(1)) != want || !slices.Contains(f.order(want), sk(1)) {
				t.Fatalf("bobFirst=%v %s: s1 in %s (order %v)", bobFirst, f.path, f.groupOf(sk(1)), f.order(want))
			}
			for _, g := range []string{gWeb, gOps, gLab} {
				if g != want && slices.Contains(f.order(g), sk(1)) {
					t.Fatalf("bobFirst=%v %s: s1 still listed in %s", bobFirst, f.path, g)
				}
			}
		}
	}
}

func TestSidebarDeletedGroupKeptWhenOtherMacMovesIntoIt(t *testing.T) {
	store := newCASStore()
	a, b := newSidebarPair(t, store)
	fa, fb := sidebarFile(a), sidebarFile(b)
	a.syncSidebar(false)
	b.syncSidebar(false)

	fa.edit(func(ep map[string]any) { deleteGroup(ep, gWeb) })
	fb.edit(func(ep map[string]any) { moveSession(ep, sk(3), gWeb) })
	a.syncSidebar(false)
	b.syncSidebar(false)
	a.syncSidebar(false)
	for _, f := range []appFile{fa, fb} {
		if _, ok := f.groupName(gWeb); !ok || !f.hasSection(gWeb) {
			t.Fatalf("%s: group bob moved a session into was deleted", f.path)
		}
		if f.groupOf(sk(3)) != gWeb || !slices.Equal(f.order(gWeb), []string{sk(3)}) || f.groupOf(sk(1)) != "" {
			t.Fatalf("%s: s3 in %q, order %v, s1 in %q", f.path, f.groupOf(sk(3)), f.order(gWeb), f.groupOf(sk(1)))
		}
	}

	// A plain delete, with nothing changed on the other Mac, goes through.
	fa.edit(func(ep map[string]any) { deleteGroup(ep, gOps) })
	a.syncSidebar(false)
	if res := b.syncSidebar(false); res.GroupsChanged != 1 || res.AssignmentsChanged != 1 {
		t.Fatalf("bob: %+v", res)
	}
	if _, ok := fb.groupName(gOps); ok || fb.hasSection(gOps) || fb.groupOf(sk(2)) != "" {
		t.Fatal("deleted group survived on bob")
	}
}

func TestSidebarPinsSync(t *testing.T) {
	store := newCASStore()
	a, b := newSidebarPair(t, store)
	fa, fb := sidebarFile(a), sidebarFile(b)
	a.syncSidebar(false)
	b.syncSidebar(false)

	siteA := filepath.Join(a.home, "code", "site")
	fa.edit(func(ep map[string]any) {
		ep[prefStarredCode] = []any{"local_one"}
		ep[prefLocalSlice].(map[string]any)[slicePinnedOrder] = []any{"local_one"}
		ep[prefLocalSlice].(map[string]any)[sliceHomeProjects] = []any{siteA}
	})
	fb.edit(func(ep map[string]any) {
		ep[prefStarredCode] = []any{"local_two"}
		ep[prefStarredGroups] = []any{map[string]any{"id": gLab}}
	})
	a.syncSidebar(false)
	b.syncSidebar(false)
	a.syncSidebar(false)
	sa, sb := fa.state(), fb.state()
	sa.HomeProjectsPinnedOrder, sb.HomeProjectsPinnedOrder = nil, nil // each Mac's own home; checked below
	if !sidebarEqual(sa, sb) {
		t.Fatalf("pins differ:\n%s\n%s", sa.canonical(), sb.canonical())
	}
	if got := canonicalJSON(mustJSON(t, sa.StarredLocalCodeSessions)); got != `["local_two","local_one"]` && got != `["local_one","local_two"]` {
		t.Fatalf("starred sessions %s", got)
	}
	if len(sb.StarredSessionGroups) != 1 || len(sb.PinnedOrder) != 1 {
		t.Fatalf("bob's pins: %+v", sb)
	}
	// A pinned project folder points at the same folder under each Mac's home.
	home := fb.prefs()[prefLocalSlice].(map[string]any)[sliceHomeProjects].([]any)
	if len(home) != 1 || home[0] != filepath.Join(b.home, "code", "site") {
		t.Fatalf("pinned project on bob: %v", home)
	}

	fa.edit(func(ep map[string]any) { ep[prefStarredCode] = without(ep[prefStarredCode].([]any), "local_one") })
	a.syncSidebar(false)
	b.syncSidebar(false)
	if got := canonicalJSON(mustJSON(t, fb.state().StarredLocalCodeSessions)); got != `["local_two"]` {
		t.Fatalf("unpin didn't follow: %s", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSidebarWaitsWhileAppRuns(t *testing.T) {
	store := newCASStore()
	a, b := newSidebarPair(t, store)
	fa, fb := sidebarFile(a), sidebarFile(b)
	a.syncSidebar(false)
	b.syncSidebar(false)
	basePath := filepath.Join(b.s.cfg.StateDirOverride, sidebarBaseFile)
	pendingPath := filepath.Join(b.s.cfg.StateDirOverride, sidebarPendingFile)

	fa.edit(func(ep map[string]any) { moveSession(ep, sk(1), gOps) })
	a.syncSidebar(false)

	before, baseBefore := fb.bytes(), mustRead(t, basePath)
	res := b.syncSidebar(true)
	if !res.PendingRestart || res.AppliedLocally || res.AssignmentsChanged != 1 {
		t.Fatalf("app running: %+v", res)
	}
	if !bytes.Equal(fb.bytes(), before) {
		t.Fatal("wrote the file underneath the running app")
	}
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatal("no pending layout saved")
	}
	if !bytes.Equal(mustRead(t, basePath), baseBefore) {
		t.Fatal("base moved although the file still holds the old layout")
	}
	// Syncing again while the app runs must not read the old file as bob
	// undoing alice's move.
	if res := b.syncSidebar(true); res.Uploaded || !res.PendingRestart {
		t.Fatalf("second pass while running: %+v", res)
	}
	a.syncSidebar(false)
	if fa.groupOf(sk(1)) != gOps {
		t.Fatal("alice's move was reverted by bob's stale file")
	}

	// Bob renames a group in the running app, then quits it.
	fb.edit(func(ep map[string]any) { renameGroup(ep, gLab, "Experiments") })
	stubDesktopApp(t, true)
	if applied, err := ApplyPendingSidebar(b.s.cfg, fb.path); err != nil || applied {
		t.Fatalf("applied while the app runs: %v %v", applied, err)
	}
	stubDesktopApp(t, false)
	if applied, err := ApplyPendingSidebar(b.s.cfg, fb.path); err != nil || !applied {
		t.Fatalf("pending layout not applied: %v %v", applied, err)
	}
	if name, _ := fb.groupName(gLab); name != "Experiments" || fb.groupOf(sk(1)) != gOps {
		t.Fatalf("after quit: name %q, s1 in %s", name, fb.groupOf(sk(1)))
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatal("pending layout left behind")
	}
	if applied, err := ApplyPendingSidebar(b.s.cfg, fb.path); err != nil || applied {
		t.Fatalf("applied twice: %v %v", applied, err)
	}

	// The rename made while the app ran reaches alice.
	if res := b.syncSidebar(false); !res.Uploaded || res.AppliedLocally {
		t.Fatalf("bob after quit: %+v", res)
	}
	a.syncSidebar(false)
	if name, _ := fa.groupName(gLab); name != "Experiments" || fa.groupOf(sk(1)) != gOps {
		t.Fatalf("alice: name %q, s1 in %s", name, fa.groupOf(sk(1)))
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSidebarCompareAndSwapRetry(t *testing.T) {
	store := newCASStore()
	a, b := newSidebarPair(t, store)
	fa, fb := sidebarFile(a), sidebarFile(b)
	a.syncSidebar(false)
	b.syncSidebar(false)

	fa.edit(func(ep map[string]any) { renameGroup(ep, gOps, "Operations") })
	fb.edit(func(ep map[string]any) { moveSession(ep, sk(3), gWeb) })
	raced := false
	store.beforeUploadIf = func(key string) {
		if key == SidebarRemoteKey && !raced {
			raced = true
			a.syncSidebar(false) // alice uploads between bob's read and bob's write
		}
	}
	res := b.syncSidebar(false)
	store.beforeUploadIf = nil
	if !raced || !res.Uploaded || !res.AppliedLocally {
		t.Fatalf("raced=%v %+v", raced, res)
	}
	a.syncSidebar(false)
	for _, f := range []appFile{fa, fb} {
		if name, _ := f.groupName(gOps); name != "Operations" || f.groupOf(sk(3)) != gWeb {
			t.Fatalf("%s: name %q, s3 in %q", f.path, name, f.groupOf(sk(3)))
		}
	}
}

func TestSidebarWithoutCompareAndSwap(t *testing.T) {
	store := newMockStorage()
	a, b := newSidebarPair(t, store)
	a.syncSidebar(false)
	if res := b.syncSidebar(false); !res.AppliedLocally || sidebarFile(b).groupOf(sk(1)) != gWeb {
		t.Fatalf("bob: %+v", res)
	}
	if res := b.syncSidebar(false); res.Uploaded || res.AppliedLocally {
		t.Fatalf("settled layout still moving: %+v", res)
	}
}

// sidebarSeeds is how many random histories each convergence test plays.
const sidebarSeeds = 4

func TestSidebarConvergesOverManyRounds(t *testing.T) {
	for seed := uint64(1); seed <= sidebarSeeds; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) { playSidebarRounds(t, seed, false) })
	}
}

// TestSidebarConvergesWithAppRunning edits only while each Mac's app runs and
// applies pending layouts when it quits, as the agent does.
func TestSidebarConvergesWithAppRunning(t *testing.T) {
	stubDesktopApp(t, false)
	for seed := uint64(1); seed <= sidebarSeeds; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) { playSidebarRounds(t, seed, true) })
	}
}

// playSidebarRounds plays random sidebar edits on three Macs over many sync
// rounds, then checks they settle on one layout the app could have written.
func playSidebarRounds(t *testing.T, seed uint64, withApp bool) {
	store := newCASStore()
	a, b := newSidebarPair(t, store)
	c := newEmptyGroupsMac(t, t.TempDir(), "carol", store)
	devs := []*device{a, b, c}
	running := map[*device]bool{a: withApp, b: withApp, c: withApp}
	quit := func(d *device) {
		if _, err := ApplyPendingSidebar(d.s.cfg, sidebarFile(d).path); err != nil {
			t.Fatalf("%s: apply pending: %v", d.name, err)
		}
		running[d] = false
	}
	rng := rand.New(rand.NewPCG(seed, 11))
	next := 100
	for round := 0; round < 40; round++ {
		for _, d := range devs {
			if withApp {
				switch {
				case running[d] && rng.IntN(4) == 0:
					quit(d)
				case !running[d] && rng.IntN(2) == 0:
					running[d] = true
				}
			}
			if rng.IntN(3) == 0 || (withApp && !running[d]) {
				continue
			}
			sidebarFile(d).edit(func(ep map[string]any) {
				var groups []string
				for _, g := range groupScopeOf(ep)["groups"].([]any) {
					groups = append(groups, g.(map[string]any)["id"].(string))
				}
				switch op := rng.IntN(7); {
				case op <= 1 && len(groups) > 0:
					g := ""
					if i := rng.IntN(len(groups) + 1); i < len(groups) {
						g = groups[i]
					}
					moveSession(ep, sk(1+rng.IntN(8)), g)
				case op == 2 && len(groups) > 0:
					renameGroup(ep, groups[rng.IntN(len(groups))], fmt.Sprintf("%s %d", d.name, round))
				case op == 3:
					next++
					addGroup(ep, fmt.Sprintf("cg-cccccccc-0000-4000-8000-%012d", next), fmt.Sprintf("new %d", next))
				case op == 4 && len(groups) > 1:
					deleteGroup(ep, groups[rng.IntN(len(groups))])
				case op == 5:
					starred, _ := ep[prefStarredCode].([]any)
					id := fmt.Sprintf("local_%d", rng.IntN(4))
					if slices.Contains(starred, any(id)) {
						ep[prefStarredCode] = without(starred, id)
					} else {
						ep[prefStarredCode] = append(starred, id)
					}
				case op == 6 && len(groups) > 0:
					order := groupScopeOf(ep)["order"].(map[string]any)
					if l, ok := order[groups[rng.IntN(len(groups))]].([]any); ok {
						slices.Reverse(l)
					}
				}
			})
		}
		order := slices.Clone(devs)
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		for _, d := range order {
			d.syncSidebar(running[d])
		}
	}
	for _, d := range devs {
		if running[d] {
			quit(d)
		}
	}
	for i := 0; i < 2; i++ {
		for _, d := range devs {
			d.syncSidebar(false)
		}
	}

	sa := sidebarFile(a).state()
	for _, d := range devs[1:] {
		if sd := sidebarFile(d).state(); !sidebarEqual(sa, sd) {
			t.Fatalf("alice and %s differ after settling:\n%s\n%s", d.name, sa.canonical(), sd.canonical())
		}
	}
	for _, d := range devs {
		if res := d.syncSidebar(false); res.Uploaded || res.AppliedLocally {
			t.Fatalf("%s: settled layout still moving: %+v", d.name, res)
		}
	}
	// The merged layout is one the app could have written itself.
	gs, secs := sa.Scopes[sbScope], indexRaw(sa.Sections[sbScope].Sections, objectID)
	groups := indexRaw(gs.Groups, objectID)
	for _, id := range groups.ids {
		if _, ok := secs.byID[id]; !ok {
			t.Errorf("group %s has no section", id)
		}
	}
	for _, id := range secs.ids {
		if strings.HasPrefix(id, "cg-") {
			if _, ok := groups.byID[id]; !ok {
				t.Errorf("section %s has no group", id)
			}
		}
	}
	for s, g := range gs.Assignments {
		if _, ok := groups.byID[g]; !ok {
			t.Errorf("%s assigned to missing group %s", s, g)
		}
		if !slices.Contains(gs.Order[g], s) {
			t.Errorf("%s assigned to %s but missing from its order %v", s, g, gs.Order[g])
		}
	}
	for g, l := range gs.Order {
		for _, s := range l {
			if gs.Assignments[s] != g {
				t.Errorf("%s listed in %s but assigned to %q", s, g, gs.Assignments[s])
			}
		}
	}
}

// TestSidebarOnAzurite runs the sidebar sync against the real Azure adapter
// on Azurite (azurite-blob --loose), with two Macs syncing at the same moment.
func TestSidebarOnAzurite(t *testing.T) {
	if testing.Short() {
		t.Skip("Azurite tests skipped in -short mode")
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:10000", 500*time.Millisecond)
	if err != nil {
		t.Skip("Azurite not running on 127.0.0.1:10000 (start it with: azurite-blob --loose)")
	}
	_ = conn.Close()
	const (
		account = "devstoreaccount1"
		key     = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==" // Azurite's public dev key
		ep      = "http://127.0.0.1:10000/" + account
	)
	name := fmt.Sprintf("sidebar%d", time.Now().UnixNano())
	cred, err := container.NewSharedKeyCredential(account, key)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := container.NewClientWithSharedKeyCredential(ep+"/"+name, cred, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Create(context.Background(), nil); err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Delete(context.Background(), nil) })
	newStore := func() storage.Storage {
		s, err := storage.New(&storage.StorageConfig{Provider: storage.ProviderAzure, Bucket: name, Endpoint: ep, AzureAccount: account, AzureAccountKey: key})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	a, b := newSidebarPair(t, newStore())
	b.s.storage = newStore()
	fa, fb := sidebarFile(a), sidebarFile(b)
	a.syncSidebar(false)
	if res := b.syncSidebar(false); !res.AppliedLocally || fb.groupOf(sk(1)) != gWeb {
		t.Fatalf("bob: %+v", res)
	}
	for i := 0; i < 4; i++ {
		fa.edit(func(ep map[string]any) { renameGroup(ep, gWeb, fmt.Sprintf("Sites %d", i)) })
		fb.edit(func(ep map[string]any) { moveSession(ep, sk(3+i), gLab) })
		var wg gosync.WaitGroup
		errs := make([]error, 2)
		for j, d := range []*device{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[j] = d.s.SyncSidebar(context.Background(), func() bool { return false })
			}()
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	a.syncSidebar(false)
	b.syncSidebar(false)
	a.syncSidebar(false)
	for _, f := range []appFile{fa, fb} {
		if name, _ := f.groupName(gWeb); name != "Sites 3" {
			t.Fatalf("%s: name %q", f.path, name)
		}
		for i := 0; i < 4; i++ {
			if f.groupOf(sk(3+i)) != gLab {
				t.Fatalf("%s: s%d in %q", f.path, 3+i, f.groupOf(sk(3+i)))
			}
		}
	}
}

// --- safety -------------------------------------------------------------------

func TestSidebarRestoresVanishedRemote(t *testing.T) {
	store := newCASStore()
	a, _ := newSidebarPair(t, store)
	a.syncSidebar(false)
	if err := store.Delete(context.Background(), SidebarRemoteKey); err != nil {
		t.Fatal(err)
	}
	before := sidebarFile(a).bytes()
	if res := a.syncSidebar(false); !res.Uploaded || res.AppliedLocally {
		t.Fatalf("vanished remote: %+v", res)
	}
	if !bytes.Equal(sidebarFile(a).bytes(), before) {
		t.Fatal("a vanished remote layout was applied as deletions")
	}
}

func TestSidebarRejectsBlobMovedOntoIt(t *testing.T) {
	store := newCASStore()
	a, _ := newSidebarPair(t, store)
	a.writeSession("proj", "s1", a.line("proj", "u1", "", time.Now(), "hello"))
	a.sync()
	store.copyBlob(sessionKey("proj", "s1"), SidebarRemoteKey)
	before := sidebarFile(a).bytes()
	if _, err := a.s.SyncSidebar(context.Background(), func() bool { return false }); !errors.Is(err, ErrBlobMoved) {
		t.Fatalf("moved blob accepted: %v", err)
	}
	if !bytes.Equal(sidebarFile(a).bytes(), before) {
		t.Fatal("file changed after a rejected blob")
	}
}

func TestSidebarObjectStaysOutOfFileSync(t *testing.T) {
	store := newCASStore()
	a, b := newSidebarPair(t, store)
	a.syncSidebar(false)
	r := b.sync()
	for _, l := range [][]string{r.Downloaded, r.Skipped, r.NewDesktopSessions} {
		for _, p := range l {
			if strings.Contains(p, "sidebar") {
				t.Fatalf("file sync picked up the sidebar object: %v", l)
			}
		}
	}
}

func TestSidebarResetFileDoesNotWipeOtherMacs(t *testing.T) {
	store := newCASStore()
	a, b := newSidebarPair(t, store)
	a.syncSidebar(false)
	b.syncSidebar(false)

	// The app on bob starts over with fresh preferences.
	fb := sidebarFile(b)
	fb.write(`{
  "preferences": {
    "sidebarMode": "code"
  }
}`)
	if res := b.syncSidebar(false); res.Uploaded || !res.AppliedLocally {
		t.Fatalf("bob after reset: %+v", res)
	}
	a.syncSidebar(false)
	for _, f := range []appFile{sidebarFile(a), fb} {
		if f.groupOf(sk(1)) != gWeb || !f.hasSection(gLab) {
			t.Fatalf("%s lost the layout after another Mac's reset", f.path)
		}
	}
	if fb.load()["preferences"].(map[string]any)["sidebarMode"] != "code" {
		t.Fatal("bob's other preferences were replaced")
	}
}

func TestSidebarAppWritingOldLayoutIsNotAnEdit(t *testing.T) {
	store := newCASStore()
	a, b := newSidebarPair(t, store)
	fa, fb := sidebarFile(a), sidebarFile(b)
	a.syncSidebar(false)
	b.syncSidebar(false)
	fa.edit(func(ep map[string]any) { moveSession(ep, sk(2), gLab) })
	a.syncSidebar(false)

	old := fb.bytes()
	if res := b.syncSidebar(false); !res.AppliedLocally {
		t.Fatalf("bob: %+v", res)
	}
	// The app writes back the layout it had in memory before reading ours.
	fb.write(string(old))
	if res := b.syncSidebar(false); res.Uploaded || !res.AppliedLocally {
		t.Fatalf("the app's stale write was taken as an edit: %+v", res)
	}
	a.syncSidebar(false)
	if fa.groupOf(sk(2)) != gLab || fb.groupOf(sk(2)) != gLab {
		t.Fatalf("move reverted: alice %s, bob %s", fa.groupOf(sk(2)), fb.groupOf(sk(2)))
	}
}

func TestSidebarSnapshotBelongsToOneStorage(t *testing.T) {
	p := filepath.Join(t.TempDir(), sidebarBaseFile)
	st := SidebarState{StarredSessionGroups: []json.RawMessage{json.RawMessage(`"x"`)}}
	if err := saveSidebarSnapshot(p, "azure|a", st, ""); err != nil {
		t.Fatal(err)
	}
	if snap, ok := loadSidebarSnapshot(p, "azure|a"); !ok || !sidebarEqual(snap.State, st) {
		t.Fatal("snapshot didn't round-trip")
	}
	if _, ok := loadSidebarSnapshot(p, "azure|b"); ok {
		t.Fatal("a base from another container must not be used")
	}
	cfg := &config.Config{StateDirOverride: filepath.Dir(p)}
	if err := saveSidebarSnapshot(filepath.Join(filepath.Dir(p), sidebarPendingFile), "azure|old", st, ""); err != nil {
		t.Fatal(err)
	}
	stubDesktopApp(t, false)
	if applied, err := ApplyPendingSidebar(cfg, filepath.Join(t.TempDir(), sidebarConfigName)); err != nil || applied {
		t.Fatalf("applied a pending layout from another container: %v %v", applied, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(p), sidebarPendingFile)); !os.IsNotExist(err) {
		t.Fatal("stale pending layout kept")
	}
}

// --- merge rules ----------------------------------------------------------------

func TestMergeSidebarIsDeterministic(t *testing.T) {
	scope := func(groups string, assign map[string]string, order map[string][]string) map[string]*GroupScope {
		var g []json.RawMessage
		if err := json.Unmarshal([]byte(groups), &g); err != nil {
			t.Fatal(err)
		}
		return map[string]*GroupScope{sbScope: {Groups: g, Assignments: assign, Order: order}}
	}
	base := SidebarState{Scopes: scope(`[{"id":"g1","name":"One"},{"id":"g2","name":"Two"}]`,
		map[string]string{"s1": "g1"}, map[string][]string{"g1": {"s1"}})}
	local := SidebarState{Scopes: scope(`[{"id":"g1","name":"Uno"},{"id":"g2","name":"Two"},{"id":"g3","name":"Three"}]`,
		map[string]string{"s1": "g2", "s2": "g3"}, map[string][]string{"g1": {}, "g2": {"s1"}, "g3": {"s2"}})}
	remote := SidebarState{Scopes: scope(`[{"id":"g1","name":"Ein"},{"id":"g2","name":"Two"}]`,
		map[string]string{"s1": "g1", "s3": "g2"}, map[string][]string{"g1": {"s1"}, "g2": {"s3"}})}

	first, cl, cr := MergeSidebar(base, local, remote)
	if !cl || !cr {
		t.Fatalf("changed local %v remote %v", cl, cr)
	}
	for i := 0; i < 20; i++ {
		again, _, _ := MergeSidebar(base, local, remote)
		if again.canonical() != first.canonical() {
			t.Fatal("same inputs merged differently")
		}
	}
	gs := first.Scopes[sbScope]
	if name := canonicalJSON(gs.Groups[0]); name != `{"id":"g1","name":"Ein"}` {
		t.Fatalf("both renamed: remote must win, got %s", name)
	}
	if len(gs.Groups) != 3 || gs.Assignments["s1"] != "g2" || gs.Assignments["s2"] != "g3" || gs.Assignments["s3"] != "g2" {
		t.Fatalf("merged %s", first.canonical())
	}
	if !slices.Equal(gs.Order["g2"], []string{"s3", "s1"}) && !slices.Equal(gs.Order["g2"], []string{"s1", "s3"}) {
		t.Fatalf("g2 order %v", gs.Order["g2"])
	}
	if len(gs.Order["g1"]) != 0 {
		t.Fatalf("s1 moved out of g1 but still listed: %v", gs.Order["g1"])
	}

	// Identical sides change nothing.
	if _, cl, cr := MergeSidebar(base, local, local); cl || cr {
		t.Fatal("identical sides reported changes")
	}
}

func TestOrderItems(t *testing.T) {
	all := func(string) bool { return true }
	for _, tc := range []struct {
		name                string
		base, local, remote []string
		keep                func(string) bool
		want                []string
	}{
		{"additions keep their place", []string{"a", "b", "c"}, []string{"a", "b", "c", "x"}, []string{"y", "a", "b", "c"}, all, []string{"y", "a", "b", "c", "x"}},
		{"remote reorder wins", []string{"a", "b", "c"}, []string{"a", "b", "c"}, []string{"c", "b", "a"}, all, []string{"c", "b", "a"}},
		{"local reorder kept", []string{"a", "b", "c"}, []string{"b", "a", "c"}, []string{"a", "b", "c", "d"}, all, []string{"b", "a", "c", "d"}},
		{"both reorder: remote", []string{"a", "b"}, []string{"b", "a"}, []string{"b", "a", "c"}, all, []string{"b", "a", "c"}},
		{"no base: remote order", nil, []string{"x", "a"}, []string{"a", "b"}, all, []string{"x", "a", "b"}},
		{"keep filters", []string{"a", "b"}, []string{"a", "b"}, []string{"a", "b"}, func(s string) bool { return s != "a" }, []string{"b"}},
	} {
		if got := orderItems(tc.base, tc.local, tc.remote, tc.keep); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPick3(t *testing.T) {
	eq := func(a, b string) bool { return a == b }
	for _, tc := range []struct {
		name          string
		b, l, r       string
		bok, lok, rok bool
		want          string
		wantOK        bool
	}{
		{"unchanged", "x", "x", "x", true, true, true, "x", true},
		{"local edit", "x", "y", "x", true, true, true, "y", true},
		{"remote edit", "x", "x", "z", true, true, true, "z", true},
		{"both edit: remote", "x", "y", "z", true, true, true, "z", true},
		{"local delete", "x", "", "x", true, false, true, "", false},
		{"deleted vs edited: edit", "x", "", "z", true, false, true, "z", true},
		{"edited vs deleted: edit", "x", "y", "", true, true, false, "y", true},
		{"both added: remote", "", "y", "z", false, true, true, "z", true},
		{"added here", "", "y", "", false, true, false, "y", true},
	} {
		got, ok := pick3(tc.b, tc.l, tc.r, tc.bok, tc.lok, tc.rok, eq)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("%s: got %q %v", tc.name, got, ok)
		}
	}
}
