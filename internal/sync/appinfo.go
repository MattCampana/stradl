package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/storage"
)

// FolderStat describes one project folder this Mac syncs, for the Stradl app
// and `stradl folders`.
type FolderStat struct {
	Path       string    `json:"path"` // relative to home, e.g. code/app
	Abs        string    `json:"abs"`
	Exists     bool      `json:"exists"`
	Git        bool      `json:"git"`   // holds a git repository at its root
	Files      int       `json:"files"` // files synced from it
	Bytes      int64     `json:"bytes"`
	LastChange time.Time `json:"lastChange,omitzero"` // newest synced file's modification time
}

// FolderStats lists the project folders the last sync covered (including
// folders learned from other Macs) with what this Mac has synced from each,
// read from local state only.
func FolderStats(cfg *config.Config) ([]FolderStat, error) {
	s := &Syncer{cfg: cfg, home: cfg.HomeDir()}
	state, err := LoadStateFromDir(StateDir(cfg))
	if err != nil {
		return nil, err
	}
	snap := state.Snapshot()
	folders := s.knownFolderList()
	// A file belongs to the innermost listed folder holding it (a repository
	// inside another folder is listed, and counted, on its own).
	owner := func(np string) string {
		best := ""
		for _, rel := range folders {
			if strings.HasPrefix(np, FoldersPrefix+rel+"/") && len(rel) > len(best) {
				best = rel
			}
		}
		return best
	}
	var out []FolderStat
	for _, rel := range folders {
		abs := filepath.Join(s.home, filepath.FromSlash(rel))
		st := FolderStat{Path: rel, Abs: abs}
		if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
			st.Exists = true
		}
		if fi, err := os.Stat(filepath.Join(abs, ".git")); err == nil && fi.IsDir() {
			st.Git = true
		}
		prefix := FoldersPrefix + rel + "/"
		hasChild := false
		for _, other := range folders {
			hasChild = hasChild || strings.HasPrefix(other, rel+"/")
		}
		repoAt := make(map[string]bool)
		inNested := func(inner string) bool {
			for i := 0; i < len(inner); i++ {
				if inner[i] != '/' {
					continue
				}
				d := inner[:i]
				if d == ".git" || strings.HasSuffix(d, "/.git") {
					return false
				}
				is, ok := repoAt[d]
				if !ok {
					is = isRepoDir(filepath.Join(abs, filepath.FromSlash(d)))
					repoAt[d] = is
				}
				if is {
					return true // a repository no session needs (listed ones own their files)
				}
			}
			return false
		}
		for np, fs := range snap {
			if !strings.HasPrefix(np, prefix) || fs.LocalDeleted || owner(np) != rel {
				continue
			}
			// State can still hold files that no longer sync (build output
			// added to the excluded names later, or a repository inside the
			// folder that syncs only when needed): count what syncs now.
			inner := np[len(prefix):]
			if !s.folderInclude(inner) || cfg.IsBuildArtifact(inner) || inNested(inner) ||
				(strings.HasPrefix(inner, ".git/modules/") && !hasChild) {
				continue
			}
			st.Files++
			st.Bytes += fs.Size
			if fs.ModTime.After(st.LastChange) {
				st.LastChange = fs.ModTime
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// SetDeviceName sets the name this Mac shows in the Circle (the dashboard,
// the app and other Macs' lists); "" goes back to the Mac's computer name.
func SetDeviceName(cfg *config.Config, name string) error {
	p := filepath.Join(StateDir(cfg), deviceNameFile)
	name = strings.TrimSpace(name)
	if name == "" {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if len(name) > 80 || strings.ContainsAny(name, "\n\r") {
		return errors.New("a device name is one line of at most 80 characters")
	}
	return writeFileAtomic(p, []byte(name+"\n"), 0600)
}

// ForgetDevice removes a Mac's heartbeat from storage, so a Mac that no
// longer syncs drops out of the Circle. A Mac that still syncs reappears
// after its next sync. idOrName matches a device ID or, ignoring case, a
// device name; it must match exactly one Mac other than this one.
func (s *Syncer) ForgetDevice(ctx context.Context, idOrName string) (DeviceHeartbeat, error) {
	devices, err := s.Heartbeats(ctx)
	if err != nil {
		return DeviceHeartbeat{}, err
	}
	thisID, _ := DeviceID(s.cfg)
	var match []DeviceHeartbeat
	for _, d := range devices {
		if d.ID == idOrName || strings.EqualFold(d.Name, idOrName) {
			match = append(match, d)
		}
	}
	switch {
	case len(match) == 0:
		return DeviceHeartbeat{}, fmt.Errorf("no Mac named %q in the Circle", idOrName)
	case len(match) > 1:
		return DeviceHeartbeat{}, fmt.Errorf("%d Macs are named %q; use the device ID from 'stradl circle'", len(match), idOrName)
	case match[0].ID == thisID:
		return DeviceHeartbeat{}, errors.New("that is this Mac; it reappears after every sync")
	}
	key := DevicesPrefix + match[0].ID + ".json.age"
	if err := s.storage.Delete(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return DeviceHeartbeat{}, err
	}
	return match[0], nil
}

// FolderSyncable reports why a folder can't be a project folder, or nil when
// it can: it must lie inside home (not be home itself) and outside Stradl's,
// Claude's and the system's own folders.
func FolderSyncable(cfg *config.Config, abs string) error {
	claudeDir := config.ClaudeDir()
	if cfg.ClaudeDirOverride != "" {
		claudeDir = cfg.ClaudeDirOverride
	}
	s := &Syncer{cfg: cfg, home: cfg.HomeDir(), claudeDir: claudeDir}
	rel, ok := homeRel(s.home, filepath.Clean(abs))
	if !ok {
		return fmt.Errorf("%s isn't inside your home folder; Stradl syncs folders in your home folder", abs)
	}
	if !s.folderAllowed(rel) {
		return fmt.Errorf("%s can't sync: Stradl leaves out your home folder itself, ~/Library (except Claude's scratch folders), the Trash and Stradl's and Claude's own folders", abs)
	}
	return nil
}
