package dashboard

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	gosync "sync"
	"time"

	"github.com/tawanorg/claude-sync/internal/sync"
)

// conflictMarker separates a file's name from the rest of a conflict copy's
// name: <file>.conflict.<YYYYMMDD-HHMMSS>.<origin>.
const conflictMarker = ".conflict."

// scanner finds and parses the transcripts under ~/.claude/projects. Parsed
// sessions are cached by path, size and modification time; a transcript that
// only grew is parsed from where the last scan stopped.
type scanner struct {
	mu     gosync.Mutex
	home   string
	cache  map[string]*scanEntry
	titles map[string]titleEntry // custom-title.json by path
}

type scanEntry struct {
	info    os.FileInfo
	offset  int64
	tail    [32]byte
	parser  *transcriptParser
	summary *sessionSummary
}

type titleEntry struct {
	size  int64
	mtime time.Time
	title string
}

// scanResult is one pass over the projects folder.
type scanResult struct {
	Sessions   []*sessionSummary          // one per session ID
	ByID       map[string]*sessionSummary // the same sessions, by ID
	Conflicts  map[string]bool            // session IDs with a .conflict copy of their transcript
	Unreadable int                        // transcripts that could not be read
}

func newScanner() *scanner {
	home, _ := os.UserHomeDir()
	return &scanner{home: home, cache: make(map[string]*scanEntry), titles: make(map[string]titleEntry)}
}

type transcriptFile struct {
	abs, npath, id string
	info           os.FileInfo
}

// scan parses every top-level transcript (projects/<dir>/<id>.jsonl).
// Subagent transcripts under projects/<dir>/<id>/ belong to their session
// and are not sessions themselves.
func (sc *scanner) scan(claudeDir string, loc *time.Location) *scanResult {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	res := &scanResult{ByID: make(map[string]*sessionSummary), Conflicts: make(map[string]bool)}
	projects := filepath.Join(claudeDir, "projects")
	dirs, _ := os.ReadDir(projects)
	var files []transcriptFile
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(projects, d.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !e.Type().IsRegular() {
				continue
			}
			if i := strings.Index(name, ".jsonl"+conflictMarker); i > 0 {
				res.Conflicts[name[:i]] = true
				continue
			}
			if !strings.HasSuffix(name, ".jsonl") || sync.IsDeviceLocalFile(name) || strings.Contains(name, conflictMarker) {
				continue
			}
			abs := filepath.Join(projects, d.Name(), name)
			info, err := e.Info()
			if err != nil {
				res.Unreadable++
				continue
			}
			files = append(files, transcriptFile{
				abs:   abs,
				npath: path.Join("projects", d.Name(), name),
				id:    strings.TrimSuffix(name, ".jsonl"),
				info:  info,
			})
		}
	}

	summaries := make([]*sessionSummary, len(files))
	var (
		wg   gosync.WaitGroup
		bad  int
		bmu  gosync.Mutex
		work = make(chan int)
	)
	workers := min(4, max(1, runtime.GOMAXPROCS(0)))
	entries := make([]*scanEntry, len(files))
	for i := range files {
		entries[i] = sc.cache[files[i].abs]
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				e, err := sc.refresh(files[i], entries[i], loc)
				if err != nil {
					bmu.Lock()
					bad++
					bmu.Unlock()
					continue
				}
				entries[i] = e
				summaries[i] = e.summary
			}
		}()
	}
	for i := range files {
		work <- i
	}
	close(work)
	wg.Wait()

	seen := make(map[string]bool, len(files))
	for i, f := range files {
		seen[f.abs] = true
		if entries[i] != nil && summaries[i] != nil {
			sc.cache[f.abs] = entries[i]
		}
	}
	for p := range sc.cache {
		if !seen[p] {
			delete(sc.cache, p)
		}
	}
	res.Unreadable += bad

	for i, f := range files {
		s := summaries[i]
		if s == nil || s.StartedAt.IsZero() {
			continue // unreadable, or no activity at all (only metadata rows)
		}
		s = sc.withFileTitle(s, filepath.Join(filepath.Dir(f.abs), f.id, "custom-title.json"))
		// Claude Code moves a session's transcript when its folder changes;
		// until the old copy is folded away, keep the one with the latest
		// activity.
		if cur, ok := res.ByID[s.ID]; ok && (cur.LastAt.After(s.LastAt) || (cur.LastAt.Equal(s.LastAt) && cur.SizeBytes >= s.SizeBytes)) {
			continue
		}
		res.ByID[s.ID] = s
	}
	for _, s := range res.ByID {
		res.Sessions = append(res.Sessions, s)
	}
	return res
}

// refresh returns an up-to-date cache entry for f, reusing prev when the
// file is unchanged and resuming its parser when the file only grew.
func (sc *scanner) refresh(f transcriptFile, prev *scanEntry, loc *time.Location) (*scanEntry, error) {
	if prev != nil && prev.info.Size() == f.info.Size() && prev.info.ModTime().Equal(f.info.ModTime()) && os.SameFile(prev.info, f.info) {
		return prev, nil
	}
	fh, err := os.Open(f.abs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	info, err := fh.Stat()
	if err != nil {
		return nil, err
	}

	if prev != nil && os.SameFile(prev.info, info) && info.Size() > prev.offset && prev.offset > 0 {
		if sum, err := tailSum(fh, prev.offset); err == nil && sum == prev.tail {
			if _, err := fh.Seek(prev.offset, io.SeekStart); err == nil {
				// The parser is shared only with this entry; summaries handed
				// out earlier are snapshots and stay as they were.
				n, err := prev.parser.consume(fh)
				if err == nil {
					return sc.finish(f, info, fh, prev.parser, prev.offset+n)
				}
			}
		}
	}

	if _, err := fh.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	p := newTranscriptParser(loc, false)
	n, err := p.consume(fh)
	if err != nil {
		return nil, err
	}
	return sc.finish(f, info, fh, p, n)
}

func (sc *scanner) finish(f transcriptFile, info os.FileInfo, fh *os.File, p *transcriptParser, offset int64) (*scanEntry, error) {
	sum, err := tailSum(fh, offset)
	if err != nil {
		return nil, err
	}
	s := p.summary(sc.home)
	s.ID = f.id
	s.Path = f.abs
	s.NPath = f.npath
	s.SizeBytes = info.Size()
	return &scanEntry{info: info, offset: offset, tail: sum, parser: p, summary: s}, nil
}

// withFileTitle fills in the title the desktop app keeps beside a session
// (projects/<dir>/<id>/custom-title.json) when the transcript has none.
func (sc *scanner) withFileTitle(s *sessionSummary, p string) *sessionSummary {
	if s.CustomTitle != "" {
		return s
	}
	info, err := os.Stat(p)
	if err != nil || !info.Mode().IsRegular() {
		delete(sc.titles, p)
		return s
	}
	t, ok := sc.titles[p]
	if !ok || t.size != info.Size() || !t.mtime.Equal(info.ModTime()) {
		t = titleEntry{size: info.Size(), mtime: info.ModTime()}
		if data, err := os.ReadFile(p); err == nil && len(data) < 1<<20 {
			var v struct {
				CustomTitle string `json:"customTitle"`
			}
			if json.Unmarshal(data, &v) == nil {
				t.title = strings.TrimSpace(v.CustomTitle)
			}
		}
		sc.titles[p] = t
	}
	if t.title == "" {
		return s
	}
	c := *s
	c.CustomTitle = t.title
	return &c
}

// --- desktop sidebar records -------------------------------------------------------

// desktopRecord is the part of a Claude desktop sidebar record
// (claude-code-sessions/<acct>/<org>/local_*.json) the dashboard reads.
type desktopRecord struct {
	CLISessionID   string `json:"cliSessionId"`
	Title          string `json:"title"`
	IsArchived     bool   `json:"isArchived"`
	Cwd            string `json:"cwd"`
	LastActivityAt int64  `json:"lastActivityAt"`
}

type desktopIndex struct {
	mu    gosync.Mutex
	cache map[string]desktopEntry
}

type desktopEntry struct {
	size  int64
	mtime time.Time
	rec   desktopRecord
	ok    bool
}

func newDesktopIndex() *desktopIndex {
	return &desktopIndex{cache: make(map[string]desktopEntry)}
}

// load returns this Mac's sidebar records by CLI session ID. When several
// records point at one session, an active one beats an archived one, then
// the most recently active wins.
func (d *desktopIndex) load(dir string) map[string]desktopRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]desktopRecord)
	if dir == "" {
		return out
	}
	seen := make(map[string]bool)
	accts, _ := os.ReadDir(dir)
	for _, a := range accts {
		if !a.IsDir() {
			continue
		}
		orgs, _ := os.ReadDir(filepath.Join(dir, a.Name()))
		for _, o := range orgs {
			if !o.IsDir() {
				continue
			}
			recs, _ := os.ReadDir(filepath.Join(dir, a.Name(), o.Name()))
			for _, r := range recs {
				name := r.Name()
				if !r.Type().IsRegular() || !strings.HasPrefix(name, "local_") || !strings.HasSuffix(name, ".json") {
					continue
				}
				p := filepath.Join(dir, a.Name(), o.Name(), name)
				seen[p] = true
				rec, ok := d.read(p, r)
				if !ok || rec.CLISessionID == "" {
					continue
				}
				if cur, dup := out[rec.CLISessionID]; dup && !betterRecord(rec, cur) {
					continue
				}
				out[rec.CLISessionID] = rec
			}
		}
	}
	for p := range d.cache {
		if !seen[p] {
			delete(d.cache, p)
		}
	}
	return out
}

func betterRecord(a, b desktopRecord) bool {
	if a.IsArchived != b.IsArchived {
		return !a.IsArchived
	}
	return a.LastActivityAt > b.LastActivityAt
}

func (d *desktopIndex) read(p string, de os.DirEntry) (desktopRecord, bool) {
	info, err := de.Info()
	if err != nil {
		return desktopRecord{}, false
	}
	if e, ok := d.cache[p]; ok && e.size == info.Size() && e.mtime.Equal(info.ModTime()) {
		return e.rec, e.ok
	}
	e := desktopEntry{size: info.Size(), mtime: info.ModTime()}
	if data, err := os.ReadFile(p); err == nil {
		err := json.Unmarshal(data, &e.rec)
		var typeErr *json.UnmarshalTypeError
		e.ok = err == nil || errors.As(err, &typeErr)
	}
	d.cache[p] = e
	return e.rec, e.ok
}
