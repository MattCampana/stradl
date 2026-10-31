package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/storage"
)

// Fingerprints let the background agent notice that a sync is worth running
// without running one. They cover exactly what Sync reconciles (same roots,
// scope and excludes), so a fingerprint that hasn't changed means Sync would
// find nothing new on that side.

// LocalFingerprint summarizes every local file Sync would consider by path,
// size and modification time. It reads no file contents and needs no network
// or encryption key, so it is cheap enough to poll every few seconds.
func LocalFingerprint(cfg *config.Config) (string, error) {
	return newScopeSyncer(cfg).LocalFingerprint()
}

// LocalFingerprint is the method form of the package-level LocalFingerprint.
//
// The set of live sessions is folded in too: Sync defers remote changes to a
// transcript while its process runs, so a session ending must count as a
// local change for those deferred updates to land promptly. So is each
// project folder git is busy in: the fingerprint moves when git finishes.
//
// Project folders come from the same local sources Sync discovers them from,
// plus the list the last sync recorded (which includes folders learned from
// other Macs).
func (s *Syncer) LocalFingerprint() (string, error) {
	s.noteSignIn(time.Now()) // the sign-in history terminal sessions are judged by
	s.accounts = nil
	var lines []string
	roots := s.syncRoots(nil, false)
	sort.SliceStable(roots, func(i, j int) bool { return roots[i].prefix < roots[j].prefix })
	for _, root := range roots {
		if root.kind == rootFolder && insideDeferred(roots, root) {
			// Sync sits it out with the folder holding it.
			lines = append(lines, "d\x00"+root.prefix)
			continue
		}
		files, err := s.scanRoot(root)
		if err != nil && root.kind == rootFolder {
			root.deferred = true
			lines = append(lines, "d\x00"+root.prefix+"\x00"+err.Error())
			continue
		}
		if err != nil {
			return "", fmt.Errorf("failed to scan %s: %w", root.dir, err)
		}
		for rel, info := range files {
			lines = append(lines, fmt.Sprintf("f\x00%s%s\x00%d\x00%d\x00%o", root.prefix, rel, info.Size(), info.ModTime().UnixNano(), info.Mode().Perm()))
		}
	}
	for id := range liveSessionIDs(s.claudeDir) {
		lines = append(lines, "l\x00"+id)
	}
	if p := s.sidebarPath(); p != "" {
		// Groups, membership and pins: the layout, not the whole preferences
		// file, which the app rewrites for unrelated settings.
		lines = append(lines, "s\x00"+SidebarFingerprint(p))
	}
	return digest(lines), nil
}

// RemoteFingerprint summarizes the remote objects Sync would consider by key
// and ETag, using a single List call. Objects outside this device's scope,
// reserved metadata and keys with a path token this device can't map are
// left out, as Sync ignores them too; the shared modes manifest, the
// sidebar layout and the account registry are included, so a chmod, a new
// sidebar group or an account choice on another Mac reaches this one
// promptly.
func (s *Syncer) RemoteFingerprint(ctx context.Context) (string, error) {
	objects, err := s.storage.List(ctx, "")
	if err != nil {
		return "", fmt.Errorf("failed to list remote objects: %w", err)
	}
	s.accounts = nil // this Mac's copy of the registry, as the last sync left it
	roots := s.syncRoots(objects, false)
	lines := make([]string, 0, len(objects))
	for _, obj := range objects {
		np, ok, _ := s.npathForKey(obj.Key)
		sidebar := obj.Key == SidebarRemoteKey && s.sidebarPath() != ""
		// The account registry: a choice made on another Mac changes what
		// this one syncs.
		if !ok && obj.Key != ModesManifestKey && !sidebar && obj.Key != AccountsKey {
			continue
		}
		if ok {
			if root, rel := s.rootFor(roots, np); root == nil || !root.inScope(rel) {
				continue
			}
		}
		version := obj.ETag
		if version == "" {
			// Backends without ETags: size and timestamp are the best signal.
			version = fmt.Sprintf("%d/%d", obj.Size, obj.LastModified.UnixNano())
		}
		lines = append(lines, obj.Key+"\x00"+version)
	}
	return digest(lines), nil
}

// Probe computes fingerprints for one configuration. Unlike NewSyncer it
// loads no sync state or encryption key, and creating it makes no network
// call; only Remote talks to storage.
type Probe struct {
	s *Syncer
}

// NewProbe builds a Probe for cfg, creating the storage client.
func NewProbe(cfg *config.Config) (*Probe, error) {
	store, err := storage.New(cfg.GetStorageConfig())
	if err != nil {
		return nil, fmt.Errorf("failed to create storage client: %w", err)
	}
	s := newScopeSyncer(cfg)
	s.storage = store
	return &Probe{s: s}, nil
}

// Local returns the local fingerprint (see LocalFingerprint).
func (p *Probe) Local() (string, error) { return p.s.LocalFingerprint() }

// Remote returns the remote fingerprint (see Syncer.RemoteFingerprint).
func (p *Probe) Remote(ctx context.Context) (string, error) { return p.s.RemoteFingerprint(ctx) }

// newScopeSyncer returns a Syncer carrying only what scope resolution needs
// (roots, excludes, path mapping). It has no storage, state or encryptor and
// must never run a sync.
func newScopeSyncer(cfg *config.Config) *Syncer {
	claudeDir := config.ClaudeDir()
	if cfg.ClaudeDirOverride != "" {
		claudeDir = cfg.ClaudeDirOverride
	}
	homeDir := cfg.HomeDir()
	mapper, err := NewPathMapper(homeDir, cfg.PathMap)
	if err != nil {
		// An invalid path_map makes NewSyncer fail too; fall back to the
		// home-only mapping so probing still works and Sync reports the error.
		mapper, _ = NewPathMapper(homeDir, nil)
	}
	return &Syncer{cfg: cfg, claudeDir: claudeDir, quiet: true, paths: mapper, home: homeDir}
}

func digest(lines []string) string {
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}
