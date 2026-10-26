package sync

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Files another Mac deleted are never deleted here: they move to
// ~/.stradl/trash/<YYYY-MM-DD>/<path relative to home>, where they stay for
// trashRetention. Losing sides of conflicts inside a git repository or
// ~/.claude/plugins go to ~/.stradl/conflicts/ rather than beside the file,
// where git or Claude Code would pick them up.

// trashRetention is how long trashed files are kept.
const trashRetention = 30 * 24 * time.Hour

const trashDayLayout = "2006-01-02"

func (s *Syncer) trashDir() string     { return filepath.Join(s.stateDir(), "trash") }
func (s *Syncer) conflictsDir() string { return filepath.Join(s.stateDir(), "conflicts") }

// keepRel names a local file inside the trash and conflicts folders: its path
// relative to home, or its path below the volume root for a file outside home.
func (s *Syncer) keepRel(abs string) string {
	if rel, ok := homeRel(s.home, abs); ok {
		return filepath.FromSlash(rel)
	}
	return strings.TrimPrefix(filepath.Clean(abs), string(filepath.Separator))
}

// moveToTrash moves a local file into today's trash folder, unless it changed
// since it was scanned (expect), and removes the folders it leaves empty up
// to (not including) stopAt. It returns where the file went.
func (s *Syncer) moveToTrash(abs string, expect os.FileInfo, stopAt string) (string, error) {
	cur, err := os.Lstat(abs)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if expect != nil && (cur.Size() != expect.Size() || !cur.ModTime().Equal(expect.ModTime())) {
		return "", errLocalChanged
	}
	dst := filepath.Join(s.trashDir(), time.Now().Format(trashDayLayout), s.keepRel(abs))
	dst = freeName(dst)
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return "", fmt.Errorf("failed to create trash folder: %w", err)
	}
	if err := moveFile(abs, dst); err != nil {
		return "", err
	}
	pruneEmptyDirs(filepath.Dir(abs), stopAt)
	return dst, nil
}

// freeName returns p, or p with a numeric suffix when p is taken.
func freeName(p string) string {
	if _, err := os.Lstat(p); os.IsNotExist(err) {
		return p
	}
	for i := 1; ; i++ {
		c := fmt.Sprintf("%s.%d", p, i)
		if _, err := os.Lstat(c); os.IsNotExist(err) {
			return c
		}
	}
}

// moveFile renames src to dst, copying when they sit on different volumes.
func moveFile(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

// pruneEmptyDirs removes dir and its parents while they are empty, stopping
// at stopAt, which is kept.
func pruneEmptyDirs(dir, stopAt string) {
	stopAt = filepath.Clean(stopAt)
	for dir = filepath.Clean(dir); dir != stopAt && isWithin(stopAt, dir); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil { // fails unless empty
			return
		}
	}
}

// purgeTrash deletes trash days older than trashRetention.
func (s *Syncer) purgeTrash(now time.Time) {
	entries, err := os.ReadDir(s.trashDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		day, err := time.ParseInLocation(trashDayLayout, e.Name(), time.Local)
		if err != nil || !e.IsDir() {
			continue
		}
		if now.Sub(day) > trashRetention+24*time.Hour {
			_ = os.RemoveAll(filepath.Join(s.trashDir(), e.Name()))
		}
	}
}

// conflictsAside reports whether the losing side of a conflict on npath is
// kept in ~/.stradl/conflicts instead of beside the file: inside a git
// repository a stray file would show up as untracked (or worse, inside .git,
// confuse git itself), and Claude Code loads everything under plugins/.
func conflictsAside(npath string) bool {
	return strings.HasPrefix(npath, "plugins/") || hasGitSegment(npath)
}

// keepConflict saves the losing side of a non-mergeable conflict as
// <file>.conflict.<time>.<origin>, beside the file or in the conflicts folder
// (see conflictsAside). Conflict copies are never synced.
func (s *Syncer) keepConflict(npath, abs string, data []byte, origin string) error {
	if !conflictsAside(npath) {
		return writeConflictCopy(abs, data, origin)
	}
	p := filepath.Join(s.conflictsDir(), s.keepRel(abs)) + conflictSuffix(origin)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	return os.WriteFile(freeName(p), data, 0600)
}

// keepConflictFile is keepConflict for a losing side that is already a file
// (a streamed download, or a local file too large to hold in memory). It
// moves (move=true) or copies src.
func (s *Syncer) keepConflictFile(npath, abs, src, origin string, move bool) error {
	p := abs + conflictSuffix(origin)
	if conflictsAside(npath) {
		p = filepath.Join(s.conflictsDir(), s.keepRel(abs)) + conflictSuffix(origin)
	}
	p = freeName(p)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	if move {
		return moveFile(src, p)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func conflictSuffix(origin string) string {
	return ".conflict." + time.Now().Format("20060102-150405") + "." + origin
}
