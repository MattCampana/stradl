package dashboard

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits from the contract in model.go.
const (
	maxPromptChars   = 280
	maxDayPrompt     = 200
	maxTitleChars    = 80
	maxTimelineText  = 400
	maxCommandChars  = 200
	maxFilesTouched  = 20
	maxDayFiles      = 8
	maxDayPrompts    = 3
	maxTimeline      = 500
	maxLineBytes     = 64 << 20 // longer rows (giant API request dumps) are skipped
	readerBufferSize = 64 << 10
	tailCheckBytes   = 4 << 10
)

// injectedPrefixes start user rows that Claude Code (or a hook) wrote, not
// the person: slash-command echoes, local command output, reminders, hook
// output and background task notices.
var injectedPrefixes = []string{
	"<command-", "<local-command", "<system-reminder>", "<user-prompt-submit-hook>",
	"<task-notification", "<bash-input", "<bash-stdout", "<bash-stderr", "Caveat:",
}

// skippedRowPrefixes are row types the dashboard never needs. Recognizing
// them from the first bytes lets the reader skip multi-megabyte API request
// dumps without buffering or decoding them. Rows that put another key first
// are simply decoded and ignored.
var skippedRowPrefixes = [][]byte{
	[]byte(`{"type":"api-request`), []byte(`{"type":"queue-operation"`), []byte(`{"type":"file-history-`),
	[]byte(`{"type":"attachment"`), []byte(`{"type":"atis-latch"`), []byte(`{"type":"bridge-session"`),
	[]byte(`{"type":"agent-name"`), []byte(`{"type":"progress"`), []byte(`{"type":"last-prompt"`),
}

// rawLine is the subset of a transcript row the dashboard reads.
type rawLine struct {
	Type             string      `json:"type"`
	Subtype          string      `json:"subtype"`
	UUID             string      `json:"uuid"`
	Cwd              string      `json:"cwd"`
	GitBranch        string      `json:"gitBranch"`
	Entrypoint       string      `json:"entrypoint"`
	Timestamp        string      `json:"timestamp"`
	IsMeta           bool        `json:"isMeta"`
	IsSidechain      bool        `json:"isSidechain"`
	IsCompactSummary bool        `json:"isCompactSummary"`
	Origin           *rawOrigin  `json:"origin"`
	CustomTitle      string      `json:"customTitle"`
	RelocatedCwd     string      `json:"relocatedCwd"`
	Summary          string      `json:"summary"`
	Message          *rawMessage `json:"message"`
}

type rawOrigin struct {
	Kind string `json:"kind"`
}

type rawMessage struct {
	ID      string          `json:"id"`
	Content json.RawMessage `json:"content"`
}

type rawPart struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type rawInput struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Command      string `json:"command"`
}

// dayAgg is one session's activity on one local calendar day.
type dayAgg struct {
	turns   int
	prompts []string       // the day's latest prompts, latest last
	files   map[string]int // absolute path → sequence number of its latest edit
	first   time.Time
	last    time.Time
}

// transcriptParser accumulates one transcript row by row. Transcripts only
// grow, so a parser can be fed the new tail of a file instead of starting
// over.
type transcriptParser struct {
	loc *time.Location

	cwd, relocated, branch, entrypoint string
	started, last, lastAt              time.Time

	userTurns   int
	assistant   map[string]struct{}
	firstPrompt string
	lastPrompt  string
	customTitle string
	files       map[string]int
	seq         int
	days        map[string]*dayAgg
	badRows     int

	// timeline is collected only for a session's detail view.
	timeline     []TimelineEntry
	withTimeline bool
	lastReplyID  string
}

func newTranscriptParser(loc *time.Location, withTimeline bool) *transcriptParser {
	return &transcriptParser{
		loc:          loc,
		assistant:    make(map[string]struct{}),
		files:        make(map[string]int),
		days:         make(map[string]*dayAgg),
		withTimeline: withTimeline,
	}
}

// consume feeds every complete row from r and returns how many bytes it used.
// A final row without a newline counts only when it is valid JSON; otherwise
// it is still being written and is left for the next read.
func (p *transcriptParser) consume(r io.Reader) (int64, error) {
	br := bufio.NewReaderSize(r, readerBufferSize)
	var (
		buf      []byte
		consumed int64
	)
	for {
		line, size, terminated, skipped, err := readRow(br, &buf)
		if size == 0 {
			if err == nil || errors.Is(err, io.EOF) {
				return consumed, nil
			}
			return consumed, err
		}
		if !terminated {
			if !skipped && json.Valid(line) {
				p.feed(line)
				consumed += size
			}
			if errors.Is(err, io.EOF) {
				return consumed, nil
			}
			return consumed, err
		}
		consumed += size
		if !skipped {
			p.feed(line)
		}
	}
}

// readRow reads one row. Rows recognized as unneeded from their first bytes,
// and rows longer than maxLineBytes, are read through without buffering and
// reported as skipped.
func readRow(br *bufio.Reader, buf *[]byte) (line []byte, size int64, terminated, skipped bool, err error) {
	*buf = (*buf)[:0]
	first := true
	for {
		chunk, e := br.ReadSlice('\n')
		size += int64(len(chunk))
		if first {
			first = false
			skipped = skipRow(chunk)
		}
		if !skipped {
			if len(*buf)+len(chunk) > maxLineBytes {
				skipped = true
				*buf = (*buf)[:0]
			} else {
				*buf = append(*buf, chunk...)
			}
		}
		switch {
		case e == nil:
			if skipped {
				return nil, size, true, true, nil
			}
			return bytes.TrimRight(*buf, "\r\n"), size, true, false, nil
		case errors.Is(e, bufio.ErrBufferFull):
			continue
		default:
			if skipped {
				return nil, size, false, true, e
			}
			return *buf, size, false, false, e
		}
	}
}

func skipRow(start []byte) bool {
	for _, p := range skippedRowPrefixes {
		if bytes.HasPrefix(start, p) {
			return true
		}
	}
	return false
}

func parseTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// feed applies one row.
func (p *transcriptParser) feed(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var l rawLine
	if err := json.Unmarshal(line, &l); err != nil {
		// A field of an unexpected type leaves the rest decoded; only
		// malformed JSON loses the row.
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			p.badRows++
			return
		}
	}
	ts := parseTimestamp(l.Timestamp)
	if !ts.IsZero() {
		if p.started.IsZero() || ts.Before(p.started) {
			p.started = ts
		}
		if ts.After(p.last) {
			p.last = ts
		}
		p.lastAt = ts
	}
	switch l.Type {
	case "custom-title":
		if t := strings.TrimSpace(l.CustomTitle); t != "" {
			p.customTitle = t
		}
	case "relocated":
		if l.RelocatedCwd != "" {
			p.relocated = l.RelocatedCwd
		}
	case "summary":
		// Summary rows carry no timestamp; date them by the row before.
		if s := cleanText(l.Summary); s != "" && !p.lastAt.IsZero() {
			p.addEntry(TimelineEntry{At: p.lastAt, Kind: "compact", Text: truncate(s, maxTimelineText)})
		}
	case "system":
		if l.Subtype == "compact_boundary" && !ts.IsZero() {
			p.addEntry(TimelineEntry{At: ts, Kind: "compact", Text: "Conversation compacted"})
		}
		p.touchDay(ts, l.IsSidechain)
	case "user", "assistant":
		p.message(&l, ts)
	}
}

func (p *transcriptParser) day(ts time.Time) *dayAgg {
	if ts.IsZero() {
		return nil
	}
	key := ts.In(p.loc).Format("2006-01-02")
	d := p.days[key]
	if d == nil {
		d = &dayAgg{files: make(map[string]int)}
		p.days[key] = d
	}
	return d
}

func (p *transcriptParser) touchDay(ts time.Time, sidechain bool) *dayAgg {
	if sidechain {
		return nil
	}
	d := p.day(ts)
	if d == nil {
		return nil
	}
	if d.first.IsZero() || ts.Before(d.first) {
		d.first = ts
	}
	if ts.After(d.last) {
		d.last = ts
	}
	return d
}

func (p *transcriptParser) message(l *rawLine, ts time.Time) {
	if p.cwd == "" && l.Cwd != "" {
		p.cwd = l.Cwd
	}
	if l.GitBranch != "" {
		p.branch = l.GitBranch
	}
	if p.entrypoint == "" && l.Entrypoint != "" {
		p.entrypoint = l.Entrypoint
	}
	if l.IsSidechain || l.Message == nil {
		return
	}
	day := p.touchDay(ts, false)

	if l.Type == "user" {
		if l.IsCompactSummary {
			if !ts.IsZero() && !p.recentCompact(ts) {
				p.addEntry(TimelineEntry{At: ts, Kind: "compact", Text: "Conversation compacted"})
			}
			return
		}
		text, ok := promptText(l)
		if !ok {
			return
		}
		p.userTurns++
		if p.firstPrompt == "" {
			p.firstPrompt = truncate(text, maxPromptChars)
		}
		p.lastPrompt = truncate(text, maxPromptChars)
		if day != nil {
			day.turns++
			day.prompts = append(day.prompts, truncate(text, maxDayPrompt))
			if len(day.prompts) > maxDayPrompts {
				day.prompts = append([]string(nil), day.prompts[len(day.prompts)-maxDayPrompts:]...)
			}
		}
		p.addEntry(TimelineEntry{At: ts, Kind: "prompt", Text: truncate(text, maxTimelineText)})
		p.lastReplyID = ""
		return
	}

	id := l.Message.ID
	if id == "" {
		id = l.UUID
	}
	if id != "" {
		p.assistant[id] = struct{}{}
	}
	for _, part := range decodeParts(l.Message.Content) {
		switch part.Type {
		case "text":
			p.reply(id, ts, part.Text)
		case "tool_use":
			p.toolUse(part, ts, day)
		}
	}
}

func (p *transcriptParser) reply(id string, ts time.Time, text string) {
	if !p.withTimeline {
		return
	}
	text = cleanText(text)
	if text == "" {
		return
	}
	if n := len(p.timeline); n > 0 && id != "" && id == p.lastReplyID && p.timeline[n-1].Kind == "reply" {
		p.timeline[n-1].Text = truncate(p.timeline[n-1].Text+" "+text, maxTimelineText)
		return
	}
	p.addEntry(TimelineEntry{At: ts, Kind: "reply", Text: truncate(text, maxTimelineText)})
	p.lastReplyID = id
}

func (p *transcriptParser) toolUse(part rawPart, ts time.Time, day *dayAgg) {
	switch part.Name {
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		var in rawInput
		if json.Unmarshal(part.Input, &in) != nil {
			return
		}
		path := in.FilePath
		if part.Name == "NotebookEdit" && in.NotebookPath != "" {
			path = in.NotebookPath
		}
		if path == "" {
			return
		}
		p.seq++
		p.files[path] = p.seq
		if day != nil {
			day.files[path] = p.seq
		}
		kind := "edit"
		if part.Name == "Write" {
			kind = "write"
		}
		p.addEntry(TimelineEntry{At: ts, Kind: kind, Text: path, Path: path})
		p.lastReplyID = ""
	case "Bash":
		var in rawInput
		if json.Unmarshal(part.Input, &in) != nil {
			return
		}
		if cmd := strings.TrimSpace(in.Command); cmd != "" {
			p.addEntry(TimelineEntry{At: ts, Kind: "command", Text: truncate(collapseSpace(cmd), maxCommandChars)})
			p.lastReplyID = ""
		}
	}
}

// recentCompact reports whether the timeline already marks a compaction
// within a minute of ts (a compact boundary row followed by its summary).
func (p *transcriptParser) recentCompact(ts time.Time) bool {
	n := len(p.timeline)
	if n == 0 {
		return false
	}
	e := p.timeline[n-1]
	d := ts.Sub(e.At)
	return e.Kind == "compact" && d < time.Minute && d > -time.Minute
}

func (p *transcriptParser) addEntry(e TimelineEntry) {
	if !p.withTimeline {
		return
	}
	p.timeline = append(p.timeline, e)
	if len(p.timeline) > 2*maxTimeline {
		p.timeline = append([]TimelineEntry(nil), p.timeline[len(p.timeline)-maxTimeline:]...)
	}
}

// decodeParts returns a message's content blocks. String content is one
// text block.
func decodeParts(raw json.RawMessage) []rawPart {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		return []rawPart{{Type: "text", Text: s}}
	case '[':
		var parts []rawPart
		if json.Unmarshal(raw, &parts) == nil {
			return parts
		}
		// One odd block must not hide the others.
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return nil
		}
		for _, it := range items {
			var part rawPart
			if json.Unmarshal(it, &part) == nil {
				parts = append(parts, part)
			}
		}
		return parts
	}
	return nil
}

// promptText returns what the person typed in a user row: its text blocks
// minus anything Claude Code injected. ok is false for rows that are not a
// prompt (tool results, meta rows, sidechains, notices).
func promptText(l *rawLine) (string, bool) {
	if l.IsMeta || l.IsSidechain || l.IsCompactSummary || l.Message == nil {
		return "", false
	}
	if l.Origin != nil && l.Origin.Kind != "" && l.Origin.Kind != "human" {
		return "", false
	}
	var texts []string
	for _, part := range decodeParts(l.Message.Content) {
		if part.Type != "text" || isInjected(part.Text) {
			continue
		}
		if t := cleanText(part.Text); t != "" {
			texts = append(texts, t)
		}
	}
	if len(texts) == 0 {
		return "", false
	}
	return strings.Join(texts, " "), true
}

func isInjected(s string) bool {
	s = strings.TrimLeft(s, " \t\r\n")
	for _, p := range injectedPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// cleanText removes embedded reminder blocks and collapses whitespace.
func cleanText(s string) string {
	for {
		i := strings.Index(s, "<system-reminder>")
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], "</system-reminder>")
		if j < 0 {
			s = s[:i]
			break
		}
		s = s[:i] + " " + s[i+j+len("</system-reminder>"):]
	}
	return collapseSpace(s)
}

func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncate shortens s to at most n characters, ending with "…" when cut.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}

// --- results ---------------------------------------------------------------------

// sessionSummary is one parsed transcript. It never changes once built, so
// it can be shared between overviews while the parser moves on.
type sessionSummary struct {
	ID          string
	Path        string // absolute path of the transcript
	NPath       string // sync path: projects/<dir>/<id>.jsonl
	ProjectPath string
	Branch      string
	Surface     string
	StartedAt   time.Time
	LastAt      time.Time
	UserTurns   int
	Assistant   int
	FirstPrompt string
	LastPrompt  string
	Files       []string // newest first, at most maxFilesTouched
	CustomTitle string
	SizeBytes   int64
	BadRows     int
	Days        map[string]daySummary // keyed by local date, YYYY-MM-DD
}

// daySummary is a session's activity on one day.
type daySummary struct {
	Turns   int
	Prompts []string
	Files   []string
	First   time.Time
	Last    time.Time
}

func surfaceOf(entrypoint string) string {
	switch entrypoint {
	case "claude-desktop":
		return "desktop"
	case "cli":
		return "cli"
	case "":
		return "unknown"
	}
	return entrypoint
}

// projectPath is where the session lives: the folder it was last moved to,
// else the folder it started in.
func (p *transcriptParser) projectPath() string {
	if p.relocated != "" {
		return p.relocated
	}
	return p.cwd
}

// summary snapshots the parser's state.
func (p *transcriptParser) summary(home string) *sessionSummary {
	project := p.projectPath()
	s := &sessionSummary{
		ProjectPath: project,
		Branch:      p.branch,
		Surface:     surfaceOf(p.entrypoint),
		StartedAt:   p.started,
		LastAt:      p.last,
		UserTurns:   p.userTurns,
		Assistant:   len(p.assistant),
		FirstPrompt: p.firstPrompt,
		LastPrompt:  p.lastPrompt,
		Files:       newestFiles(p.files, maxFilesTouched, project, home),
		CustomTitle: p.customTitle,
		BadRows:     p.badRows,
		Days:        make(map[string]daySummary, len(p.days)),
	}
	for key, d := range p.days {
		s.Days[key] = daySummary{
			Turns:   d.turns,
			Prompts: append([]string{}, d.prompts...),
			Files:   newestFiles(d.files, maxDayFiles, project, home),
			First:   d.first,
			Last:    d.last,
		}
	}
	return s
}

// newestFiles orders paths by their latest edit, newest first, and shows
// each relative to the project folder when inside it.
func newestFiles(files map[string]int, limit int, project, home string) []string {
	type f struct {
		path string
		seq  int
	}
	list := make([]f, 0, len(files))
	for p, seq := range files {
		list = append(list, f{p, seq})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].seq > list[j].seq })
	out := make([]string, 0, min(limit, len(list)))
	seen := make(map[string]bool, len(list))
	for _, e := range list {
		d := displayPath(e.path, project, home)
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
		if len(out) == limit {
			break
		}
	}
	return out
}

// displayPath shows p relative to project when inside it, else with the
// home folder abbreviated to ~.
func displayPath(p, project, home string) string {
	if !filepath.IsAbs(p) {
		return p
	}
	if project != "" {
		if rel, ok := within(project, p); ok && rel != "." {
			return rel
		}
	}
	if home != "" {
		if rel, ok := within(home, p); ok {
			if rel == "." {
				return "~"
			}
			return "~/" + rel
		}
	}
	return p
}

func within(root, p string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// parseFile parses a whole transcript.
func parseFile(path string, loc *time.Location, withTimeline bool) (*transcriptParser, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	p := newTranscriptParser(loc, withTimeline)
	n, err := p.consume(f)
	return p, n, err
}

// sessionTimeline returns a transcript's timeline: at most maxTimeline
// entries, the newest ones, oldest first.
func sessionTimeline(path string, loc *time.Location, home string) ([]TimelineEntry, error) {
	p, _, err := parseFile(path, loc, true)
	if err != nil {
		return nil, err
	}
	tl := p.timeline
	if len(tl) > maxTimeline {
		tl = tl[len(tl)-maxTimeline:]
	}
	project := p.projectPath()
	out := make([]TimelineEntry, len(tl))
	for i, e := range tl {
		if e.Path != "" {
			e.Path = displayPath(e.Path, project, home)
			e.Text = e.Path
		}
		out[i] = e
	}
	return out, nil
}

// tailSum fingerprints the bytes just before offset, to confirm on the next
// scan that a grown file still starts with what was parsed.
func tailSum(f *os.File, offset int64) ([32]byte, error) {
	n := min(offset, tailCheckBytes)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, offset-n); err != nil && !errors.Is(err, io.EOF) {
		return [32]byte{}, err
	}
	return sha256.Sum256(buf), nil
}
