package sync

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// foldMovedSessions keeps each session in exactly one project folder.
//
// Claude Code moves a session's transcript to another projects/ folder when
// the session's working folder changes (the desktop app does this when a
// session is moved out of its scratch folder), recording the move with
// {"type":"relocated","relocatedCwd":...} rows. Union sync never deletes
// remotely, so a Mac that synced the old copy keeps it and later receives the
// new one too, leaving one session in two folders: the CLI picker shows it
// twice and the desktop app may open the stale copy.
//
// For every session ID found in more than one folder, the copy that lives
// where its own latest relocated row points (or, failing that, the copy with
// the newest entry) is kept. The other copies are merged into it when they
// hold entries it lacks, then deleted on this device only; the sync pass
// records those deletes as local, so the remote copies stay untouched.
// Sessions owned by a running Claude process are left alone. It returns the
// paths (relative to the Claude dir) that were folded away.
func (s *Syncer) foldMovedSessions(live map[string]bool) []string {
	projects := filepath.Join(s.claudeDir, "projects")
	dirs, err := os.ReadDir(projects)
	if err != nil {
		return nil
	}
	byID := make(map[string][]string)
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(projects, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if !f.Type().IsRegular() || !strings.HasSuffix(name, ".jsonl") || isDeviceLocalFile(name) {
				continue
			}
			id := strings.TrimSuffix(name, ".jsonl")
			byID[id] = append(byID[id], filepath.Join(projects, d.Name(), name))
		}
	}

	var folded []string
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		copies := byID[id]
		if len(copies) < 2 || live[id] {
			continue
		}
		type copyInfo struct {
			abs      string
			info     os.FileInfo
			data     []byte
			home     bool // lives where its own latest relocated row points
			newest   int64
			uuidSeen map[string]bool
		}
		var all []*copyInfo
		for _, abs := range copies {
			info, err := os.Stat(abs)
			if err != nil {
				continue
			}
			data, err := os.ReadFile(abs)
			if err != nil {
				continue
			}
			c := &copyInfo{abs: abs, info: info, data: data, uuidSeen: map[string]bool{}}
			lastRelocated := ""
			for _, l := range parseJSONL(data) {
				if strings.HasPrefix(l.id, "u:") {
					c.uuidSeen[l.id] = true
				}
				if t := l.ts.UnixMilli(); t > c.newest {
					c.newest = t
				}
				if bytes.Contains(l.raw, []byte(`"relocated`)) {
					var r struct {
						Type         string `json:"type"`
						RelocatedCwd string `json:"relocatedCwd"`
					}
					if json.Unmarshal(l.raw, &r) == nil && r.Type == "relocated" && r.RelocatedCwd != "" {
						lastRelocated = r.RelocatedCwd
					}
				}
			}
			c.home = lastRelocated != "" && EncodeClaudePath(lastRelocated) == filepath.Base(filepath.Dir(abs))
			all = append(all, c)
		}
		if len(all) < 2 {
			continue
		}
		sort.SliceStable(all, func(i, j int) bool {
			if all[i].home != all[j].home {
				return all[i].home
			}
			if all[i].newest != all[j].newest {
				return all[i].newest > all[j].newest
			}
			return all[i].info.Size() > all[j].info.Size()
		})
		keep := all[0]
		merged := keep.data
		for _, other := range all[1:] {
			missing := false
			for u := range other.uuidSeen {
				if !keep.uuidSeen[u] {
					missing = true
					break
				}
			}
			if missing {
				merged, _ = MergeJSONL(merged, other.data)
			}
		}
		if !bytes.Equal(merged, keep.data) {
			if err := writeLocal(keep.abs, merged, keep.info, s.claudeDir); err != nil {
				continue // changed underneath us: try again next pass
			}
		}
		for _, other := range all[1:] {
			if err := os.Remove(other.abs); err == nil {
				if rel, err := filepath.Rel(s.claudeDir, other.abs); err == nil {
					folded = append(folded, filepath.ToSlash(rel))
				}
			}
		}
	}
	return folded
}
