package sync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	gosync "sync"
	"time"
)

// macOS guards a few folders in home behind its privacy settings (TCC): the
// first time a program reads ~/Desktop, ~/Documents, ~/Downloads or the media
// folders, macOS asks the person whether to allow it, and the read waits for
// the answer. A project folder there must not stall the whole sync while that
// question sits unanswered overnight, so the first touch happens in the
// background, the folder sits out until macOS answers, and a denial is
// reported rather than retried in a loop.

// protectedDirs are the home folders macOS asks about.
var protectedDirs = map[string]string{
	"Desktop": "Desktop", "Documents": "Documents", "Downloads": "Downloads",
	"Pictures": "Pictures", "Movies": "Movies", "Music": "Music",
}

// tccWait is how long a pass waits for macOS to answer before going on
// without the folder.
var tccWait = 2 * time.Second

// tccRetry is how soon a folder macOS refused is tried again (the person may
// have allowed it in System Settings since).
const tccRetry = 10 * time.Minute

type tccProbe struct {
	done chan struct{}
	err  error
	at   time.Time
}

var (
	tccMu     gosync.Mutex
	tccProbes = map[string]*tccProbe{}
)

// errAwaitingPermission defers a folder while macOS's privacy question about
// it is unanswered.
var errAwaitingPermission = errors.New("waiting for macOS permission")

// tccError names the protected folder and why it can't be read yet.
type tccError struct {
	dir    string // e.g. Desktop
	denied bool
	err    error
}

func (e *tccError) Error() string {
	if e.denied {
		return fmt.Sprintf("macOS doesn't let Stradl read your %s folder (%v)", e.dir, e.err)
	}
	return fmt.Sprintf("macOS is asking whether Stradl may read your %s folder", e.dir)
}

func (e *tccError) Unwrap() error { return errAwaitingPermission }

// warning is the line shown in the menu bar, the log and the dashboard.
func (e *tccError) warning() string {
	if e.denied {
		return fmt.Sprintf("Project folders in your %s folder aren't syncing: macOS doesn't allow Stradl to read it. Allow it in System Settings → Privacy & Security → Files & Folders (stradl → %s Folder).", e.dir, e.dir)
	}
	return fmt.Sprintf("Project folders in your %s folder are waiting: macOS is asking whether Stradl may read it. Click Allow in that dialog.", e.dir)
}

// tccWarnings lists the protected folders this process has asked about that
// still can't be read.
func (s *Syncer) tccWarnings() []string {
	tccMu.Lock()
	dirs := make([]string, 0, len(tccProbes))
	for dir := range tccProbes {
		dirs = append(dirs, dir)
	}
	tccMu.Unlock()
	sort.Strings(dirs)
	var out []string
	for _, dir := range dirs {
		rel, ok := homeRel(s.home, dir)
		if !ok {
			continue
		}
		var te *tccError
		if errors.As(s.tccCheck(rel, 0), &te) {
			out = append(out, te.warning())
		}
	}
	return out
}

// protectedTop returns the protected home folder abs lies in ("Desktop"), or
// "" when abs isn't in one (or this isn't a Mac).
func (s *Syncer) protectedTop(abs string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	rel, ok := homeRel(s.home, abs)
	if !ok {
		return ""
	}
	first, _, _ := strings.Cut(rel, "/")
	return protectedDirs[first]
}

// tccRead is the first read of a protected folder, the one macOS asks about.
// Tests replace it.
var tccRead = func(dir string) error {
	_, err := os.ReadDir(dir)
	return err
}

// tccReady reports whether the protected folder top (under home) can be
// read. The first call starts a read in the background, which is what makes
// macOS ask, and waits at most tccWait for it; later calls return what it
// found, or a *tccError while macOS hasn't answered or refused.
func (s *Syncer) tccReady(top string) error { return s.tccCheck(top, tccWait) }

func (s *Syncer) tccCheck(top string, wait time.Duration) error {
	dir := filepath.Join(s.home, top)
	tccMu.Lock()
	p := tccProbes[dir]
	if p != nil {
		select {
		case <-p.done:
			if p.err != nil && time.Since(p.at) > tccRetry {
				p = nil // refused a while ago: ask again
			}
		default:
		}
	}
	if p == nil {
		p = &tccProbe{done: make(chan struct{})}
		tccProbes[dir] = p
		go func() {
			err := tccRead(dir)
			tccMu.Lock()
			p.err, p.at = err, time.Now()
			tccMu.Unlock()
			close(p.done)
		}()
	}
	tccMu.Unlock()

	if wait <= 0 {
		select {
		case <-p.done:
		default:
			return &tccError{dir: top}
		}
	} else {
		select {
		case <-p.done:
		case <-time.After(wait):
			return &tccError{dir: top}
		}
	}
	tccMu.Lock()
	err := p.err
	tccMu.Unlock()
	switch {
	case err == nil, os.IsNotExist(err):
		return nil
	case os.IsPermission(err):
		return &tccError{dir: top, denied: true, err: err}
	}
	return nil // another error surfaces when the folder itself is read
}

// folderGate checks a folder before anything reads it: nil when it can be
// read now, a *tccError when it lies in a protected folder macOS hasn't let
// Stradl read yet.
func (s *Syncer) folderGate(abs string) error {
	if top := s.protectedTop(abs); top != "" {
		return s.tccReady(top)
	}
	return nil
}
