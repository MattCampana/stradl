package dashboard

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	desktopID = "aaaaaaaa-0000-4000-8000-000000000001"
	cliID     = "bbbbbbbb-0000-4000-8000-000000000002"
	testHome  = "/Users/test"
)

func newYork(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	return loc
}

func parseFixture(t *testing.T, name string, loc *time.Location, timeline bool) *transcriptParser {
	t.Helper()
	p, _, err := parseFile(filepath.Join("testdata", name), loc, timeline)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseDesktopSession(t *testing.T) {
	loc := newYork(t)
	s := parseFixture(t, "desktop-session.jsonl", loc, false).summary(testHome)

	if s.UserTurns != 3 {
		t.Errorf("UserTurns = %d, want 3 (meta, injected, tool-result, sidechain, notification and compact rows are not prompts)", s.UserTurns)
	}
	if s.Assistant != 6 {
		t.Errorf("AssistantTurns = %d, want 6 (one per message id, sidechain excluded)", s.Assistant)
	}
	if s.FirstPrompt != "Add a login page with a form" {
		t.Errorf("FirstPrompt = %q", s.FirstPrompt)
	}
	if s.LastPrompt != "Ship it" {
		t.Errorf("LastPrompt = %q", s.LastPrompt)
	}
	if s.CustomTitle != "Login page" {
		t.Errorf("CustomTitle = %q, want the last custom-title row", s.CustomTitle)
	}
	if s.Surface != "desktop" || s.Branch != "feature/login" || s.ProjectPath != "/Users/test/code/app" {
		t.Errorf("surface/branch/project = %q/%q/%q", s.Surface, s.Branch, s.ProjectPath)
	}
	if !s.StartedAt.Equal(ts("2026-10-24T03:30:00Z")) || !s.LastAt.Equal(ts("2026-10-25T13:00:31Z")) {
		t.Errorf("started/last = %v/%v", s.StartedAt, s.LastAt)
	}
	wantFiles := []string{"main.go", "~/notes/todo.md", "nb.ipynb", "main_test.go", "login.html"}
	if !reflect.DeepEqual(s.Files, wantFiles) {
		t.Errorf("Files = %q, want %q", s.Files, wantFiles)
	}
	if s.BadRows != 1 {
		t.Errorf("BadRows = %d, want 1", s.BadRows)
	}
}

func TestDayGroupingFollowsLocalMidnight(t *testing.T) {
	loc := newYork(t)
	s := parseFixture(t, "desktop-session.jsonl", loc, false).summary(testHome)

	// 03:30 UTC on Oct 24 is 23:30 on Oct 23 in New York.
	d23, ok := s.Days["2026-10-23"]
	if !ok {
		t.Fatalf("no Oct 23 activity; days = %v", keys(s.Days))
	}
	if d23.Turns != 1 || !reflect.DeepEqual(d23.Prompts, []string{"Add a login page with a form"}) || !reflect.DeepEqual(d23.Files, []string{"login.html"}) {
		t.Errorf("Oct 23 = %+v", d23)
	}
	if !d23.First.Equal(ts("2026-10-24T03:30:00Z")) || !d23.Last.Equal(ts("2026-10-24T03:31:05Z")) {
		t.Errorf("Oct 23 span = %v..%v", d23.First, d23.Last)
	}
	d24 := s.Days["2026-10-24"]
	if d24.Turns != 1 || !reflect.DeepEqual(d24.Prompts, []string{"Now fix the tests"}) {
		t.Errorf("Oct 24 = %+v", d24)
	}
	if want := []string{"main.go", "~/notes/todo.md", "nb.ipynb", "main_test.go"}; !reflect.DeepEqual(d24.Files, want) {
		t.Errorf("Oct 24 files = %q, want %q", d24.Files, want)
	}
	if !d24.First.Equal(ts("2026-10-24T04:10:00Z")) || !d24.Last.Equal(ts("2026-10-24T15:01:00Z")) {
		t.Errorf("Oct 24 span = %v..%v", d24.First, d24.Last)
	}
	if d25 := s.Days["2026-10-25"]; d25.Turns != 1 || len(d25.Files) != 0 {
		t.Errorf("Oct 25 = %+v", d25)
	}

	// In UTC the first evening belongs to Oct 24.
	u := parseFixture(t, "desktop-session.jsonl", time.UTC, false).summary(testHome)
	if _, ok := u.Days["2026-10-23"]; ok {
		t.Errorf("UTC days = %v, want no Oct 23", keys(u.Days))
	}
	if u.Days["2026-10-24"].Turns != 2 {
		t.Errorf("UTC Oct 24 turns = %d, want 2", u.Days["2026-10-24"].Turns)
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestParseCLISessionRelocated(t *testing.T) {
	s := parseFixture(t, "cli-session.jsonl", time.UTC, false).summary(testHome)
	if s.Surface != "cli" || s.ProjectPath != "/Users/test/code/api" || s.Branch != "main" {
		t.Errorf("surface/project/branch = %q/%q/%q (a relocated session lives in its new folder)", s.Surface, s.ProjectPath, s.Branch)
	}
	if s.Assistant != 2 {
		t.Errorf("AssistantTurns = %d, want 2 (rows without a message id count by uuid)", s.Assistant)
	}
	if want := []string{"health.go", "~/scratch/server.go"}; !reflect.DeepEqual(s.Files, want) {
		t.Errorf("Files = %q, want %q", s.Files, want)
	}
}

func TestTimeline(t *testing.T) {
	tl, err := sessionTimeline(filepath.Join("testdata", "desktop-session.jsonl"), time.UTC, testHome)
	if err != nil {
		t.Fatal(err)
	}
	type e struct{ kind, text string }
	var got []e
	for _, x := range tl {
		got = append(got, e{x.Kind, x.Text})
	}
	want := []e{
		{"prompt", "Add a login page with a form"},
		{"reply", "I'll create the page."},
		{"write", "login.html"},
		{"edit", "main.go"},
		{"prompt", "Now fix the tests"},
		{"reply", "Running the tests."},
		{"command", "go test ./..."},
		{"edit", "main_test.go"},
		{"edit", "nb.ipynb"},
		{"edit", "~/notes/todo.md"},
		{"edit", "main.go"},
		{"compact", "Conversation compacted"},
		{"prompt", "Ship it"},
		{"reply", "Shipped. All green."},
		{"compact", "Built and shipped the login page"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("timeline =\n%v\nwant\n%v", got, want)
	}
	if tl[2].Path != "login.html" {
		t.Errorf("write path = %q", tl[2].Path)
	}
	for i := 1; i < len(tl); i++ {
		if tl[i].At.Before(tl[i-1].At) {
			t.Errorf("timeline not oldest first at %d", i)
		}
	}
}

func TestTimelineKeepsNewest500(t *testing.T) {
	var b bytes.Buffer
	base := ts("2026-10-20T10:00:00Z")
	for i := 0; i < 700; i++ {
		b.WriteString(userRow(cliID, "/w", base.Add(time.Duration(i)*time.Minute), "prompt "+strconv.Itoa(i)))
	}
	p := filepath.Join(t.TempDir(), "s.jsonl")
	writeFile(t, p, b.String())
	tl, err := sessionTimeline(p, time.UTC, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl) != maxTimeline || tl[0].Text != "prompt 200" || tl[len(tl)-1].Text != "prompt 699" {
		t.Errorf("timeline: %d entries, %q..%q", len(tl), tl[0].Text, tl[len(tl)-1].Text)
	}
}

func TestPromptRules(t *testing.T) {
	cases := []struct {
		name string
		row  string
		want string // "" = not a prompt
	}{
		{"plain string", `{"type":"user","message":{"content":"  hello \n world "}}`, "hello world"},
		{"text parts", `{"type":"user","message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}`, "a b"},
		{"tool result only", `{"type":"user","message":{"content":[{"type":"tool_result","content":"x"}]}}`, ""},
		{"meta", `{"type":"user","isMeta":true,"message":{"content":"x"}}`, ""},
		{"sidechain", `{"type":"user","isSidechain":true,"message":{"content":"x"}}`, ""},
		{"compact summary", `{"type":"user","isCompactSummary":true,"message":{"content":"x"}}`, ""},
		{"command", `{"type":"user","message":{"content":"<command-name>/review</command-name>"}}`, ""},
		{"local command", `{"type":"user","message":{"content":"<local-command-stdout>ok</local-command-stdout>"}}`, ""},
		{"reminder", `{"type":"user","message":{"content":"<system-reminder>x</system-reminder>"}}`, ""},
		{"hook", `{"type":"user","message":{"content":"<user-prompt-submit-hook>x</user-prompt-submit-hook>"}}`, ""},
		{"caveat", `{"type":"user","message":{"content":"Caveat: generated"}}`, ""},
		{"notification origin", `{"type":"user","origin":{"kind":"task-notification"},"message":{"content":"done"}}`, ""},
		{"human origin", `{"type":"user","origin":{"kind":"human"},"message":{"content":"go"}}`, "go"},
		{"embedded reminder", `{"type":"user","message":{"content":"fix it<system-reminder>noise</system-reminder> now"}}`, "fix it now"},
		{"odd origin type", `{"type":"user","origin":"weird","message":{"content":"still a prompt"}}`, "still a prompt"},
		{"image only", `{"type":"user","message":{"content":[{"type":"image"}]}}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newTranscriptParser(time.UTC, false)
			p.feed([]byte(c.row))
			got := ""
			if p.userTurns == 1 {
				got = p.firstPrompt
			} else if p.userTurns > 1 {
				t.Fatalf("userTurns = %d", p.userTurns)
			}
			if got != c.want {
				t.Errorf("prompt = %q, want %q", got, c.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	long := strings.Repeat("é", 300)
	got := truncate(long, maxPromptChars)
	if n := len([]rune(got)); n != maxPromptChars || !strings.HasSuffix(got, "…") {
		t.Errorf("truncate: %d runes, suffix %q", n, got[len(got)-3:])
	}
	if truncate("short", 10) != "short" {
		t.Error("short strings are kept")
	}
}

func TestHugeRowsAndUnterminatedTail(t *testing.T) {
	dir := t.TempDir()
	base := ts("2026-10-24T10:00:00Z")
	var b bytes.Buffer
	b.WriteString(userRow(cliID, "/w", base, "first"))
	// A multi-megabyte API dump is skipped without being decoded...
	b.WriteString(`{"type":"api-request-blob","blob":"` + strings.Repeat("z", 3<<20) + "\"}\n")
	// ...while a multi-megabyte prompt is still read.
	b.WriteString(userRow(cliID, "/w", base.Add(time.Minute), strings.Repeat("x", 2<<20)))
	full := b.Len()
	// A final row without a newline counts when complete...
	complete := userRow(cliID, "/w", base.Add(2*time.Minute), "last")
	b.WriteString(strings.TrimSuffix(complete, "\n"))
	p := filepath.Join(dir, "s.jsonl")
	writeFile(t, p, b.String())

	parser, n, err := parseFile(p, time.UTC, false)
	if err != nil {
		t.Fatal(err)
	}
	if parser.userTurns != 3 || parser.lastPrompt != "last" || n != int64(b.Len()) {
		t.Errorf("turns=%d last=%q consumed=%d/%d", parser.userTurns, parser.lastPrompt, n, b.Len())
	}
	if got := len([]rune(parser.summary("").Days["2026-10-24"].Prompts[1])); got != maxDayPrompt {
		t.Errorf("long prompt kept at %d runes, want %d", got, maxDayPrompt)
	}

	// ...but a row still being written waits for the next read.
	partial := b.String()[:full] + `{"type":"user","message":{"content":"half`
	writeFile(t, p, partial)
	parser, n, err = parseFile(p, time.UTC, false)
	if err != nil {
		t.Fatal(err)
	}
	if parser.userTurns != 2 || n != int64(full) {
		t.Errorf("partial tail: turns=%d consumed=%d, want 2 and %d", parser.userTurns, n, full)
	}
}

// --- fixture helpers --------------------------------------------------------------

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, p, content string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func row(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

// userRow is a prompt typed in a CLI session.
func userRow(id, cwd string, at time.Time, text string) string {
	return row(map[string]any{
		"type": "user", "uuid": "u-" + at.Format("150405.000000000"), "sessionId": id, "cwd": cwd,
		"entrypoint": "cli", "gitBranch": "main", "timestamp": at.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": text},
	})
}

// editRow is an assistant turn that edits path.
func editRow(id, cwd string, at time.Time, path string) string {
	return row(map[string]any{
		"type": "assistant", "uuid": "a-" + at.Format("150405.000000000"), "sessionId": id, "cwd": cwd,
		"entrypoint": "cli", "timestamp": at.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{"id": "msg-" + at.Format("150405.000000000"), "role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "name": "Edit", "input": map[string]any{"file_path": path}},
		}},
	})
}
