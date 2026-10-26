package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/tawanorg/claude-sync/internal/storage"
	"gopkg.in/yaml.v3"
)

const (
	ConfigDir  = ".stradl"
	ConfigFile = "config.yaml"
	StateFile  = "state.json"
	AgeKeyFile = "age-key.txt"
	LockFile   = "sync.lock"
	StatusFile = "status.json"

	// FoldersFile lists the project folders the last sync covered, so the
	// background agent's cheap local probe watches them too.
	FoldersFile = "folders.json"

	// MCPRemoteKey is the remote storage key for synced MCP server configs.
	// The _external/ prefix separates it from ~/.claude/-relative files.
	MCPRemoteKey = "_external/mcp-servers.json"

	// Sync scopes control what is synced. ScopeEverything (the default)
	// syncs all of ~/.claude except per-Mac state, plus every project folder
	// sessions work in; ScopeFull syncs the ~/.claude folders in SyncPaths;
	// ScopeSessions limits syncing to portable conversation data only.
	ScopeEverything = "everything"
	ScopeFull       = "full"
	ScopeSessions   = "sessions"
)

// BuildArtifacts are names of files and folders a Mac builds or caches for
// itself. They are left out of every sync root by default
// (sync_build_artifacts: true includes them): each Mac regenerates them, and
// they are large and often platform-specific. An entry starting with "*"
// matches a name suffix.
var BuildArtifacts = []string{
	"node_modules", ".venv", "venv", "__pycache__", "*.pyc", ".pytest_cache",
	".mypy_cache", ".next", ".nuxt", ".gradle", ".DS_Store", ".Spotlight-V100",
	".Trashes",
}

type Config struct {
	// New storage configuration (preferred)
	Storage *storage.StorageConfig `yaml:"storage,omitempty"`

	// Legacy R2-only fields (for backward compatibility)
	AccountID       string `yaml:"account_id,omitempty"`
	AccessKeyID     string `yaml:"access_key_id,omitempty"`
	SecretAccessKey string `yaml:"secret_access_key,omitempty"`
	Bucket          string `yaml:"bucket,omitempty"`
	Endpoint        string `yaml:"endpoint,omitempty"`

	// Common fields
	EncryptionKey string `yaml:"encryption_key_path"`

	// Exclude patterns (glob-style) for paths to skip during sync
	Exclude []string `yaml:"exclude,omitempty"`

	// Scope selects what to sync: "everything" (the default), "full" or
	// "sessions" (portable conversation data only). See ScopedSyncPaths.
	Scope string `yaml:"scope,omitempty"`

	// ScopeExplicit records that the user chose Scope (init --scope, paths
	// add/remove). A scope nobody chose moves to the current default when the
	// config loads.
	ScopeExplicit bool `yaml:"scope_explicit,omitempty"`

	// SyncPaths overrides the scope-based default paths when non-empty.
	// Use GetEffectiveSyncPaths() to get the actual paths to sync.
	SyncPaths []string `yaml:"sync_paths,omitempty"`

	// MCPSync enables syncing MCP server configs from ~/.claude.json.
	// Pointer type allows distinguishing between unset (nil), enabled (true),
	// and explicitly disabled (false). Nil is treated as disabled for backward
	// compatibility with existing configs.
	MCPSync *bool `yaml:"mcp_sync,omitempty"`

	// PathMap maps local directory prefixes to shared token names so project
	// sessions stay resumable across devices with different layouts.
	// The home directory is always mapped (token HOME); add entries here when
	// project roots differ beyond that, e.g.:
	//   path_map:
	//     ~/work: WORK        # this device keeps projects in ~/work
	// with the other device mapping its own location to the same token:
	//   path_map:
	//     ~/Projects: WORK
	PathMap map[string]string `yaml:"path_map,omitempty"`

	// DesktopSync also syncs the Claude desktop app's Code-tab session index
	// (the sidebar entries under DesktopSessionsDir), so sessions appear in
	// the desktop sidebar on every device, not only in `claude --resume`.
	// Nil (unset) means enabled wherever the desktop app's folder exists.
	DesktopSync *bool `yaml:"desktop_sync,omitempty"`

	// SyncIntervalSeconds is how often the background agent reconciles with
	// remote storage when nothing changed locally (default 300).
	SyncIntervalSeconds int `yaml:"sync_interval_seconds,omitempty"`

	// FolderSync syncs the project folders sessions work in (the working
	// folders of desktop sidebar sessions and transcripts, ExtraFolders, and
	// folders other Macs sync), git history included, under the "everything"
	// scope. Nil (unset) means enabled.
	FolderSync *bool `yaml:"folder_sync,omitempty"`

	// ExtraFolders are more folders under the home directory to sync (~ is
	// expanded).
	ExtraFolders []string `yaml:"extra_folders,omitempty"`

	// FolderExclude adds glob patterns for files to leave out of folder sync,
	// matched against the path inside each project folder (same syntax as
	// exclude).
	FolderExclude []string `yaml:"folder_exclude,omitempty"`

	// SyncBuildArtifacts includes the BuildArtifacts names, which are
	// otherwise left out of every sync root.
	SyncBuildArtifacts bool `yaml:"sync_build_artifacts,omitempty"`

	// AllowMassDelete lets one sync delete more than half of a folder's files
	// on every Mac (stradl sync --allow-mass-delete). Never saved.
	AllowMassDelete bool `yaml:"-"`

	// ClaudeDirOverride allows overriding the default ~/.claude path (for testing)
	ClaudeDirOverride string `yaml:"-"`

	// DesktopDirOverride allows overriding DesktopSessionsDir (for testing)
	DesktopDirOverride string `yaml:"-"`

	// StateDirOverride allows overriding the state file directory (for testing)
	StateDirOverride string `yaml:"-"`

	// ClaudeJSONOverride allows overriding the ~/.claude.json path (for testing)
	ClaudeJSONOverride string `yaml:"-"`

	// HomeDirOverride allows overriding the home directory used for path
	// mapping and project folders (for testing)
	HomeDirOverride string `yaml:"-"`
}

// SyncPaths defines which paths under ~/.claude to sync in the default "full" scope.
var SyncPaths = []string{
	"CLAUDE.md",
	"settings.json",
	"settings.local.json",
	"agents",
	"commands",
	"skills",
	"plugins",
	"projects",
	"plans",
	"tasks",
	"history.jsonl",
	"paste-cache",
	"rules",
	"workflows",
}

// SessionSyncPaths is the subset synced in the "sessions" scope: portable,
// high-value conversation data and its per-project work state. It deliberately
// excludes plugins/ (which bundles non-portable node_modules and .venv trees),
// along with machine-specific settings, skills, agents, and commands.
var SessionSyncPaths = []string{
	"projects",
	"history.jsonl",
	"tasks",
	"plans",
	"paste-cache", // content-addressed large pastes that history.jsonl refers to
}

// EverythingSyncPaths is the "everything" scope: all of ~/.claude, minus the
// per-Mac state the sync engine leaves out (credentials, running-process
// records, caches, logs).
var EverythingSyncPaths = []string{"."}

// ScopedSyncPaths returns the sync path set for the given scope. "everything"
// covers all of ~/.claude; "sessions" limits syncing to SessionSyncPaths;
// "full", empty, or any unrecognized value returns the complete SyncPaths so
// existing configs keep their behavior.
func ScopedSyncPaths(scope string) []string {
	switch scope {
	case ScopeEverything:
		return EverythingSyncPaths
	case ScopeSessions:
		return SessionSyncPaths
	}
	return SyncPaths
}

// ErrNoHomeDir is returned when the user's home directory cannot be determined.
var ErrNoHomeDir = fmt.Errorf("could not determine home directory (is $HOME set?)")

func ConfigDirPath() string {
	path, _ := ConfigDirPathE()
	return path
}

// ConfigDirPathE returns the config directory path or an error if home dir is unavailable.
func ConfigDirPathE() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ErrNoHomeDir
	}
	return filepath.Join(home, ConfigDir), nil
}

func ConfigFilePath() string {
	return filepath.Join(ConfigDirPath(), ConfigFile)
}

func StateFilePath() string {
	return filepath.Join(ConfigDirPath(), StateFile)
}

// LockFilePath is the advisory lock serializing sync runs on this device.
func LockFilePath() string {
	return filepath.Join(ConfigDirPath(), LockFile)
}

// StatusFilePath holds the background agent's last-sync status for the menu
// bar and `status`.
func StatusFilePath() string {
	return filepath.Join(ConfigDirPath(), StatusFile)
}

func AgeKeyFilePath() string {
	return filepath.Join(ConfigDirPath(), AgeKeyFile)
}

func ClaudeDir() string {
	path, _ := ClaudeDirE()
	return path
}

// ClaudeDirE returns the Claude directory path or an error if home dir is unavailable.
func ClaudeDirE() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ErrNoHomeDir
	}
	return filepath.Join(home, ".claude"), nil
}

// ClaudeJSONPath returns the path to ~/.claude.json where global MCP servers are configured.
func ClaudeJSONPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

func Load() (*Config, error) {
	configPath := ConfigFilePath()

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config not found: run 'stradl init' first")
		}
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// Expand ~ in encryption key path
	if cfg.EncryptionKey != "" && cfg.EncryptionKey[0] == '~' {
		home, _ := os.UserHomeDir()
		cfg.EncryptionKey = filepath.Join(home, cfg.EncryptionKey[1:])
	}

	// Expand ~ in path_map keys
	if len(cfg.PathMap) > 0 {
		home, _ := os.UserHomeDir()
		expanded := make(map[string]string, len(cfg.PathMap))
		for p, name := range cfg.PathMap {
			if p != "" && p[0] == '~' {
				p = filepath.Join(home, p[1:])
			}
			expanded[p] = name
		}
		cfg.PathMap = expanded
	}

	// Expand ~ in extra_folders
	for i, p := range cfg.ExtraFolders {
		if p == "~" || strings.HasPrefix(p, "~/") {
			home, _ := os.UserHomeDir()
			cfg.ExtraFolders[i] = filepath.Join(home, p[1:])
		}
	}

	// Set default endpoint for Cloudflare R2
	if cfg.Endpoint == "" && cfg.AccountID != "" {
		cfg.Endpoint = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.AccountID)
	}

	if cfg.migrateScope() {
		scopeNote.Do(func() {
			fmt.Fprintf(os.Stderr, "Note: Stradl now syncs everything: all of ~/.claude, your MCP servers and the project folders your sessions use. To sync less, set scope in %s.\n", configPath)
		})
	}

	return &cfg, nil
}

// scopeNote prints the scope migration note once per process.
var scopeNote sync.Once

// migrateScope moves "sessions", which init wrote by default before
// "everything" existed, to the "everything" default with MCP server sync on,
// unless the user chose it. The change lives in memory until the next Save
// writes it. It reports whether anything changed.
func (c *Config) migrateScope() bool {
	if c.ScopeExplicit || c.Scope != ScopeSessions {
		return false
	}
	c.Scope = ScopeEverything
	c.SetMCPSync(true)
	return true
}

func Save(cfg *Config) error {
	configDir := ConfigDirPath()
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to serialize config: %w", err)
	}

	configPath := ConfigFilePath()
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	return nil
}

func Exists() bool {
	_, err := os.Stat(ConfigFilePath())
	return err == nil
}

// GetStorageConfig returns the storage configuration, migrating from legacy format if needed
func (c *Config) GetStorageConfig() *storage.StorageConfig {
	// If new format is already configured, use it
	if c.Storage != nil && c.Storage.Provider != "" {
		return c.Storage
	}

	// Migrate from legacy R2 format
	return &storage.StorageConfig{
		Provider:        storage.ProviderR2,
		Bucket:          c.Bucket,
		AccountID:       c.AccountID,
		AccessKeyID:     c.AccessKeyID,
		SecretAccessKey: c.SecretAccessKey,
		Endpoint:        c.Endpoint,
	}
}

// IsLegacyConfig returns true if using the legacy R2-only config format
func (c *Config) IsLegacyConfig() bool {
	return c.Storage == nil && c.AccountID != ""
}

// GetEffectiveSyncPaths returns the paths to sync: custom SyncPaths if set,
// otherwise the scope-based defaults.
//
// Scope is a ceiling, not merely a default. Under ScopeSessions the custom list
// is intersected with SessionSyncPaths so a sync_paths entry can never widen a
// sessions-scoped config back into non-portable trees like plugins/ (which
// bundles node_modules and .venv). Under "full" scope the custom list wins
// outright, since there is nothing narrower to protect.
//
// This matters because `claude-sync paths add|remove` materializes the entire
// default path list into SyncPaths, so most configs carrying a sync_paths block
// never explicitly opted into one.
//
// The result is never empty: an empty set would make DetectChanges observe zero
// local files and report every tracked file as a deletion, wiping the remote on
// the next push. If a custom list shares nothing with the scope, the scope
// defaults are used instead.
func (c *Config) GetEffectiveSyncPaths() []string {
	scoped := ScopedSyncPaths(c.Scope)
	if len(c.SyncPaths) == 0 || c.Scope == ScopeEverything {
		// "everything" already covers any custom path.
		return scoped
	}

	if c.Scope != ScopeSessions {
		return c.SyncPaths
	}

	allowed := make(map[string]struct{}, len(scoped))
	for _, p := range scoped {
		allowed[p] = struct{}{}
	}

	// Preserve the user's ordering, keeping only in-scope entries.
	within := make([]string, 0, len(c.SyncPaths))
	for _, p := range c.SyncPaths {
		if _, ok := allowed[p]; ok {
			within = append(within, p)
		}
	}

	if len(within) == 0 {
		return scoped
	}
	return within
}

// DesktopSessionsDir returns the Claude desktop app's Code-tab session index
// directory (~/Library/Application Support/Claude/claude-code-sessions). It
// holds one <account>/<org>/local_<id>.json record per sidebar entry.
func DesktopSessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "Claude", "claude-code-sessions")
}

// EffectiveDesktopDir returns the desktop session index directory to sync,
// or "" when desktop sync is disabled.
func (c *Config) EffectiveDesktopDir() string {
	if c.DesktopSync != nil && !*c.DesktopSync {
		return ""
	}
	if c.DesktopDirOverride != "" {
		return c.DesktopDirOverride
	}
	return DesktopSessionsDir()
}

// IsEverything reports whether the "everything" scope is in effect.
func (c *Config) IsEverything() bool {
	return c.Scope == ScopeEverything
}

// IsFolderSyncEnabled reports whether project folders sync: under the
// "everything" scope unless folder_sync is false.
func (c *Config) IsFolderSyncEnabled() bool {
	return c.IsEverything() && (c.FolderSync == nil || *c.FolderSync)
}

// HomeDir returns the home directory sync paths are relative to.
func (c *Config) HomeDir() string {
	if c.HomeDirOverride != "" {
		return c.HomeDirOverride
	}
	home, _ := os.UserHomeDir()
	return home
}

// IsBuildArtifact reports whether any segment of the slash path rel names a
// build artifact (see BuildArtifacts), unless sync_build_artifacts is on.
func (c *Config) IsBuildArtifact(rel string) bool {
	if c.SyncBuildArtifacts {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		for _, name := range BuildArtifacts {
			if seg == name || (strings.HasPrefix(name, "*") && strings.HasSuffix(seg, name[1:])) {
				return true
			}
		}
	}
	return false
}

// IsFolderExcluded reports whether rel (relative to a project folder)
// matches a folder_exclude pattern.
func (c *Config) IsFolderExcluded(rel string) bool {
	if len(c.FolderExclude) == 0 {
		return false
	}
	return (&Config{Exclude: c.FolderExclude}).IsExcluded(rel)
}

// EffectiveSyncInterval returns the background reconcile interval.
func (c *Config) EffectiveSyncInterval() time.Duration {
	if c.SyncIntervalSeconds < 30 {
		return 5 * time.Minute
	}
	return time.Duration(c.SyncIntervalSeconds) * time.Second
}

// IsMCPSyncEnabled returns true if MCP sync is explicitly enabled.
// Returns false if MCPSync is nil (unset) or false.
func (c *Config) IsMCPSyncEnabled() bool {
	return c.MCPSync != nil && *c.MCPSync
}

// SetMCPSync sets the MCP sync state. Pass true to enable, false to explicitly disable.
func (c *Config) SetMCPSync(enabled bool) {
	c.MCPSync = &enabled
}

// IsExcluded returns true if the given relative path matches any exclude pattern.
// Patterns support:
//   - Full doublestar glob syntax including ** for recursive matching
//   - Examples: "**/.git/**", "*.tmp", "plugins/cache/**", "projects/*/node_modules/**"
//   - Directory prefix (e.g. "plugins/marketplace" matches everything under it)
//   - Filename glob (e.g. "*.tmp" matches "foo/bar/file.tmp")
func (c *Config) IsExcluded(relPath string) bool {
	// Normalize path separators for consistent matching
	relPath = filepath.ToSlash(relPath)

	for _, pattern := range c.Exclude {
		// Normalize pattern separators
		pattern = filepath.ToSlash(pattern)

		// Use doublestar for full glob matching including ** support
		matched, err := doublestar.Match(pattern, relPath)
		if err == nil && matched {
			return true
		}

		// Try glob match on filename only (for patterns like "*.tmp")
		// but only if the pattern doesn't contain path separators
		if !strings.Contains(pattern, "/") && (strings.Contains(pattern, "*") || strings.Contains(pattern, "?")) {
			if matched, _ := doublestar.Match(pattern, filepath.Base(relPath)); matched {
				return true
			}
		}

		// Also match if the path starts with the pattern as a directory prefix
		// This lets "plugins/marketplace" exclude everything under that dir
		if !strings.Contains(pattern, "*") && !strings.Contains(pattern, "?") {
			if len(relPath) > len(pattern) && relPath[:len(pattern)] == pattern &&
				relPath[len(pattern)] == '/' {
				return true
			}
			// Exact match for non-glob patterns
			if relPath == pattern {
				return true
			}
		}
	}
	return false
}
