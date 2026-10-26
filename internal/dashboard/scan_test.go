package dashboard

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

const (
	idA = "11111111-1111-4111-8111-111111111111"
	idB = "22222222-2222-4222-8222-222222222222"
	idC = "33333333-3333-4333-8333-333333333333"
)

func sessionIDs(r *scanResult) []string {
	var ids []string
	for _, s := range r.Sessions {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	return ids
}

func TestScanSkipsDeviceLocalFilesAndSubagents(t *testing.T) {
	claude := t.TempDir()
	proj := filepath.Join(claude, "projects", "-w")
	at := ts("2026-10-24T10:00:00Z")
	writeFile(t, filepath.Join(proj, idA+".jsonl"), userRow(idA, "/w", at, "real session"))
	for _, name := range []string{
		idB + ".orphaned-1700000000.jsonl",
		idB + ".jsonl.superseded-1700000000",
		".stradl-123.tmp",
		idB + ".jsonl.tmp",
		idB + ".tmp.jsonl",
		"x.lock",
		idB + ".desktop-released.json",
		idA + ".jsonl.conflict.20261024-101010.other-mac",
	} {
		writeFile(t, filepath.Join(proj, name), userRow(idB, "/w", at, "not a session"))
	}
	// Subagent transcripts and tool results belong to their session.
	writeFile(t, filepath.Join(proj, idA, "subagents", "agent-1.jsonl"), userRow(idC, "/w", at, "subagent"))
	writeFile(t, filepath.Join(proj, idA, "tool-results", "r.txt"), "x")
	writeFile(t, filepath.Join(claude, "projects", "stray.jsonl"), userRow(idC, "/w", at, "stray"))
	// A transcript with only metadata rows has no activity to show.
	writeFile(t, filepath.Join(proj, idC+".jsonl"), `{"type":"custom-title","customTitle":"x"}`+"\n")

	r := newScanner().scan(claude, time.UTC)
	if got := sessionIDs(r); len(got) != 1 || got[0] != idA {
		t.Fatalf("sessions = %v, want only %s", got, idA)
	}
	if !r.Conflicts[idA] || len(r.Conflicts) != 1 {
		t.Errorf("conflicts = %v, want %s", r.Conflicts, idA)
	}
	if r.ByID[idA].NPath != "projects/-w/"+idA+".jsonl" {
		t.Errorf("npath = %q", r.ByID[idA].NPath)
	}
}

func TestScanCacheAndIncrementalParse(t *testing.T) {
	claude := t.TempDir()
	p := filepath.Join(claude, "projects", "-w", idA+".jsonl")
	at := ts("2026-10-24T10:00:00Z")
	writeFile(t, p, userRow(idA, "/w", at, "one")+editRow(idA, "/w", at.Add(time.Minute), "/w/a.go"))
	sc := newScanner()

	first := sc.scan(claude, time.UTC).ByID[idA]
	again := sc.scan(claude, time.UTC).ByID[idA]
	if first != again {
		t.Error("an unchanged transcript is not re-parsed")
	}
	parser := sc.cache[p].parser

	// Growth resumes the same parser from where it stopped.
	appendFile(t, p, userRow(idA, "/w", at.Add(2*time.Minute), "two"))
	grown := sc.scan(claude, time.UTC).ByID[idA]
	if sc.cache[p].parser != parser {
		t.Error("a grown transcript is parsed from the start instead of resumed")
	}
	if grown.UserTurns != 2 || grown.LastPrompt != "two" || first.UserTurns != 1 {
		t.Errorf("after append: turns %d (earlier summary %d), last %q", grown.UserTurns, first.UserTurns, grown.LastPrompt)
	}
	fresh := newScanner().scan(claude, time.UTC).ByID[idA]
	if fresh.UserTurns != grown.UserTurns || fresh.Assistant != grown.Assistant || fresh.LastPrompt != grown.LastPrompt || fresh.SizeBytes != grown.SizeBytes {
		t.Errorf("resumed parse %+v differs from a fresh one %+v", grown, fresh)
	}

	// A rewrite that keeps the size but changes the modification time is
	// parsed again from the start.
	writeFile(t, p, userRow(idA, "/w", at, "ONE")+editRow(idA, "/w", at.Add(time.Minute), "/w/a.go")+userRow(idA, "/w", at.Add(2*time.Minute), "TWO"))
	mt := time.Now().Add(time.Hour)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	rewritten := sc.scan(claude, time.UTC).ByID[idA]
	if rewritten.FirstPrompt != "ONE" || rewritten.LastPrompt != "TWO" {
		t.Errorf("after rewrite: %q..%q", rewritten.FirstPrompt, rewritten.LastPrompt)
	}

	// A rewrite whose start differs from what was parsed is not resumed,
	// even when the file grew.
	parser = sc.cache[p].parser
	writeFile(t, p, userRow(idA, "/w", at, "uno")+editRow(idA, "/w", at.Add(time.Minute), "/w/a.go")+userRow(idA, "/w", at.Add(2*time.Minute), "dos")+userRow(idA, "/w", at.Add(3*time.Minute), "tres"))
	changed := sc.scan(claude, time.UTC).ByID[idA]
	if sc.cache[p].parser == parser || changed.FirstPrompt != "uno" || changed.UserTurns != 3 {
		t.Errorf("rewritten start: resumed=%v first=%q turns=%d", sc.cache[p].parser == parser, changed.FirstPrompt, changed.UserTurns)
	}

	// A deleted transcript leaves the cache.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if r := sc.scan(claude, time.UTC); len(r.Sessions) != 0 || len(sc.cache) != 0 {
		t.Errorf("after delete: %d sessions, %d cached", len(r.Sessions), len(sc.cache))
	}
}

func TestScanKeepsNewestCopyOfMovedSession(t *testing.T) {
	claude := t.TempDir()
	at := ts("2026-10-24T10:00:00Z")
	writeFile(t, filepath.Join(claude, "projects", "-old", idA+".jsonl"), userRow(idA, "/old", at, "stale copy"))
	writeFile(t, filepath.Join(claude, "projects", "-new", idA+".jsonl"), userRow(idA, "/old", at, "stale copy")+userRow(idA, "/new", at.Add(time.Hour), "moved on"))
	r := newScanner().scan(claude, time.UTC)
	if len(r.Sessions) != 1 || r.ByID[idA].LastPrompt != "moved on" {
		t.Errorf("sessions = %d, kept %q", len(r.Sessions), r.ByID[idA].LastPrompt)
	}
}

func TestScanCustomTitleFile(t *testing.T) {
	claude := t.TempDir()
	proj := filepath.Join(claude, "projects", "-w")
	at := ts("2026-10-24T10:00:00Z")
	writeFile(t, filepath.Join(proj, idA+".jsonl"), userRow(idA, "/w", at, "prompt"))
	writeFile(t, filepath.Join(proj, idA, "custom-title.json"), `{"customTitle":"  From the app  "}`)
	writeFile(t, filepath.Join(proj, idB+".jsonl"), `{"type":"custom-title","customTitle":"In transcript"}`+"\n"+userRow(idB, "/w", at, "p"))
	writeFile(t, filepath.Join(proj, idB, "custom-title.json"), `{"customTitle":"Ignored"}`)
	sc := newScanner()
	r := sc.scan(claude, time.UTC)
	if got := r.ByID[idA].CustomTitle; got != "From the app" {
		t.Errorf("title from custom-title.json = %q", got)
	}
	if got := r.ByID[idB].CustomTitle; got != "In transcript" {
		t.Errorf("transcript title = %q, want it to win", got)
	}
	// The cached summary itself is never modified.
	if sc.cache[filepath.Join(proj, idA+".jsonl")].summary.CustomTitle != "" {
		t.Error("cached summary was changed in place")
	}
}

func TestDesktopIndex(t *testing.T) {
	dir := t.TempDir()
	org := filepath.Join(dir, "acct", "org")
	writeFile(t, filepath.Join(org, "local_1.json"), `{"cliSessionId":"`+idA+`","title":"Sidebar title","isArchived":true,"lastActivityAt":100}`)
	writeFile(t, filepath.Join(org, "local_2.json"), `{"cliSessionId":"`+idA+`","title":"Newer active","isArchived":false,"lastActivityAt":50}`)
	writeFile(t, filepath.Join(org, "local_3.json"), `{"cliSessionId":"`+idB+`","title":"Archived","isArchived":true,"lastActivityAt":"odd"}`)
	writeFile(t, filepath.Join(org, "other.json"), `{"cliSessionId":"`+idC+`"}`)
	writeFile(t, filepath.Join(org, "local_bad.json"), `not json`)

	d := newDesktopIndex()
	recs := d.load(dir)
	if len(recs) != 2 {
		t.Fatalf("records = %v", recs)
	}
	if recs[idA].Title != "Newer active" || recs[idA].IsArchived {
		t.Errorf("an active record beats an archived one: %+v", recs[idA])
	}
	if !recs[idB].IsArchived || recs[idB].Title != "Archived" {
		t.Errorf("a field of an odd type leaves the rest readable: %+v", recs[idB])
	}
	if got := d.load(""); len(got) != 0 {
		t.Errorf("no folder: %v", got)
	}
}
