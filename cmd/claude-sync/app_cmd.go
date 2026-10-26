package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tawanorg/claude-sync/internal/agent"
	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/sync"
)

// Commands the Stradl app (and people) use to see and change what syncs:
// circle, config, folders, claude restart and diagnostics. Each read has a
// --json form for the app.

func init() {
	extraCommands = append(extraCommands, circleCmd, configCmd, foldersCmd, claudeCmd, diagnosticsCmd)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// --- circle ------------------------------------------------------------------

// circleMac is one Mac in `stradl circle --json`.
type circleMac struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	This       bool      `json:"this"`
	Version    string    `json:"version"`
	OS         string    `json:"os"`
	Arch       string    `json:"arch"`
	LastSyncAt time.Time `json:"lastSyncAt"`
	Sessions   int       `json:"sessions"`
	Recent     int       `json:"recentSessions"` // sessions it changed in the last 7 days
	Errors     int       `json:"errors"`         // files that failed in its last sync
}

func circleCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "circle",
		Short: "List the Macs in your Circle: every Mac syncing into your storage",
		Long: `Your Circle is every Mac that syncs into the same storage. After each sync a
Mac writes a small encrypted heartbeat (its name, version, last sync and
recent sessions) to _metadata/devices/ in storage; this lists them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			s, err := sync.NewSyncer(cfg, true)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			hbs, err := s.Heartbeats(ctx)
			if err != nil {
				return err
			}
			thisID, _ := sync.DeviceID(cfg)
			macs := make([]circleMac, 0, len(hbs))
			for _, h := range hbs {
				macs = append(macs, circleMac{ID: h.ID, Name: h.Name, This: h.ID == thisID, Version: h.Version, OS: h.OS, Arch: h.Arch,
					LastSyncAt: h.LastSyncAt, Sessions: h.Sessions, Recent: len(h.Recent), Errors: h.Errors})
			}
			if asJSON {
				return printJSON(macs)
			}
			fmt.Printf("%sYour Circle%s · %s\n", colorBold, colorReset, agent.Plural(len(macs), "Mac"))
			for _, m := range macs {
				mark := " "
				if m.This {
					mark = colorGreen + "●" + colorReset
				}
				fmt.Printf(" %s %-28s %sv%-8s synced %s · %s%s\n", mark, m.Name, colorDim, m.Version,
					agent.Ago(time.Since(m.LastSyncAt)), agent.Plural(m.Sessions, "session"), colorReset)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.AddCommand(&cobra.Command{
		Use:   "forget <name or id>",
		Short: "Remove a Mac that no longer syncs from the Circle",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			s, err := sync.NewSyncer(cfg, true)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			d, err := s.ForgetDevice(ctx, args[0])
			if err != nil {
				return err
			}
			printSuccess(fmt.Sprintf("Removed %s from the Circle (it comes back if it syncs again)", d.Name))
			return nil
		},
	})
	return cmd
}

// --- config ------------------------------------------------------------------

// appConfig is `stradl config show --json`: the settings the app shows,
// never a secret.
type appConfig struct {
	Version             string   `json:"version"`
	ConfigPath          string   `json:"configPath"`
	StateDir            string   `json:"stateDir"`
	Provider            string   `json:"provider"`
	Account             string   `json:"account,omitempty"`
	Container           string   `json:"container"`
	Endpoint            string   `json:"endpoint,omitempty"`
	Auth                string   `json:"auth"` // "SAS token", "account key", "access key", ...
	Scope               string   `json:"scope"`
	FolderSync          bool     `json:"folderSync"`
	DesktopSync         bool     `json:"desktopSync"`
	MCPSync             bool     `json:"mcpSync"`
	SyncBuildArtifacts  bool     `json:"syncBuildArtifacts"`
	SyncIntervalSeconds int      `json:"syncIntervalSeconds"`
	ExtraFolders        []string `json:"extraFolders"`
	FolderExclude       []string `json:"folderExclude"`
	Exclude             []string `json:"exclude"`
	BuildArtifacts      []string `json:"buildArtifacts"`
	DeviceName          string   `json:"deviceName"`
	DeviceID            string   `json:"deviceId"`
	AgentInstalled      bool     `json:"agentInstalled"`
	AgentRunning        bool     `json:"agentRunning"`
	Paused              bool     `json:"paused"`
	LogPath             string   `json:"logPath"`
	TrashDir            string   `json:"trashDir"`
	ConflictsDir        string   `json:"conflictsDir"`
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func describeConfig(cfg *config.Config) appConfig {
	sc := cfg.GetStorageConfig()
	auth := "access key"
	switch {
	case sc.AzureSASToken != "":
		auth = "SAS token"
	case sc.AzureAccountKey != "":
		auth = "account key"
	case sc.WebDAVPassword != "":
		auth = "password"
	case sc.CredentialsFile != "" || sc.CredentialsJSON != "" || sc.UseDefaultCredentials:
		auth = "Google credentials"
	}
	account := sc.AzureAccount
	if account == "" {
		account = sc.AccountID
	}
	stateDir := sync.StateDir(cfg)
	id, _ := sync.DeviceID(cfg)
	out := appConfig{
		Version: version, ConfigPath: config.ConfigFilePath(), StateDir: stateDir,
		Provider: string(sc.Provider), Account: account, Container: sc.Bucket, Endpoint: sc.Endpoint, Auth: auth,
		Scope: cfg.Scope, FolderSync: cfg.IsFolderSyncEnabled(), DesktopSync: cfg.EffectiveDesktopDir() != "",
		MCPSync: cfg.IsMCPSyncEnabled(), SyncBuildArtifacts: cfg.SyncBuildArtifacts,
		SyncIntervalSeconds: int(cfg.EffectiveSyncInterval() / time.Second),
		ExtraFolders:        nonNil(cfg.ExtraFolders), FolderExclude: nonNil(cfg.FolderExclude), Exclude: nonNil(cfg.Exclude),
		BuildArtifacts: config.BuildArtifacts, DeviceName: sync.DeviceName(cfg), DeviceID: id,
		LogPath:  filepath.Join(stateDir, "agent.log"),
		TrashDir: filepath.Join(stateDir, "trash"), ConflictsDir: filepath.Join(stateDir, "conflicts"),
	}
	if p, err := agentPaths(); err == nil {
		out.AgentInstalled = launchAgentInstalled()
		out.AgentRunning = agent.Running(p)
		out.Paused = agent.IsPaused(p)
		out.LogPath = p.Log
	}
	return out
}

// configBoolKeys are the on/off settings `stradl config set` changes.
var configBoolKeys = map[string]func(*config.Config, bool){
	"folder_sync":          func(c *config.Config, v bool) { c.FolderSync = &v },
	"desktop_sync":         func(c *config.Config, v bool) { c.DesktopSync = &v },
	"mcp_sync":             func(c *config.Config, v bool) { c.SetMCPSync(v) },
	"sync_build_artifacts": func(c *config.Config, v bool) { c.SyncBuildArtifacts = v },
}

// configListKeys are the list settings `stradl config add/remove` change.
var configListKeys = map[string]func(*config.Config) *[]string{
	"extra_folders":  func(c *config.Config) *[]string { return &c.ExtraFolders },
	"folder_exclude": func(c *config.Config) *[]string { return &c.FolderExclude },
	"exclude":        func(c *config.Config) *[]string { return &c.Exclude },
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("%q is not on or off", s)
}

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or change Stradl's settings",
		Long: `Show or change the settings in ~/.stradl/config.yaml without editing it.

  stradl config show [--json]
  stradl config set scope everything|sessions|full
  stradl config set folder_sync|desktop_sync|mcp_sync|sync_build_artifacts on|off
  stradl config set sync_interval_seconds 300
  stradl config set device_name "Studio Mac"
  stradl config add|remove extra_folders ~/work/site
  stradl config add|remove folder_exclude "*.mov"

The background agent picks changes up on its next check, within a minute.
Storage credentials are changed with 'stradl init'.`,
	}
	var asJSON bool
	show := &cobra.Command{
		Use:   "show",
		Short: "Show the current settings (never the storage secret)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			c := describeConfig(cfg)
			if asJSON {
				return printJSON(c)
			}
			row := func(k, v string) { fmt.Printf("  %s%-22s%s %s\n", colorDim, k, colorReset, v) }
			onOff := func(b bool) string {
				if b {
					return "on"
				}
				return "off"
			}
			fmt.Printf("%sStorage%s\n", colorBold, colorReset)
			row("provider", c.Provider)
			if c.Account != "" {
				row("account", c.Account)
			}
			row("container", c.Container)
			row("signed in with", c.Auth)
			fmt.Printf("%sWhat syncs%s\n", colorBold, colorReset)
			row("scope", c.Scope)
			row("folder_sync", onOff(c.FolderSync))
			row("desktop_sync", onOff(c.DesktopSync))
			row("mcp_sync", onOff(c.MCPSync))
			row("sync_build_artifacts", onOff(c.SyncBuildArtifacts))
			row("extra_folders", strings.Join(c.ExtraFolders, ", "))
			row("folder_exclude", strings.Join(c.FolderExclude, ", "))
			fmt.Printf("%sThis Mac%s\n", colorBold, colorReset)
			row("device_name", c.DeviceName)
			row("sync_interval_seconds", strconv.Itoa(c.SyncIntervalSeconds))
			return nil
		},
	}
	show.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	set := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Change a setting",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, val := args[0], args[1]
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			switch {
			case key == "device_name":
				if err := sync.SetDeviceName(cfg, val); err != nil {
					return err
				}
				printSuccess("This Mac is now " + sync.DeviceName(cfg) + " in your Circle")
				return nil
			case key == "scope":
				if !slices.Contains([]string{config.ScopeEverything, config.ScopeSessions, config.ScopeFull}, val) {
					return fmt.Errorf("scope is everything, sessions or full")
				}
				cfg.Scope, cfg.ScopeExplicit = val, true
				if val == config.ScopeEverything && cfg.MCPSync == nil {
					cfg.SetMCPSync(true) // as init and the scope migration do
				}
			case key == "sync_interval_seconds":
				n, err := strconv.Atoi(val)
				if err != nil || n < 30 || n > 86400 {
					return errors.New("sync_interval_seconds is a number of seconds from 30 to 86400")
				}
				cfg.SyncIntervalSeconds = n
			case configBoolKeys[key] != nil:
				b, err := parseBool(val)
				if err != nil {
					return err
				}
				configBoolKeys[key](cfg, b)
			default:
				return fmt.Errorf("unknown setting %q (see 'stradl config --help')", key)
			}
			if err := config.Save(cfg); err != nil {
				return err
			}
			printSuccess(fmt.Sprintf("%s = %s", key, val))
			return nil
		},
	}

	listEdit := func(add bool) *cobra.Command {
		verb := "remove"
		if add {
			verb = "add"
		}
		return &cobra.Command{
			Use:   verb + " <key> <value>",
			Short: strings.ToUpper(verb[:1]) + verb[1:] + " an entry of extra_folders, folder_exclude or exclude",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				key, val := args[0], strings.TrimSpace(args[1])
				get := configListKeys[key]
				if get == nil {
					return fmt.Errorf("%s works on extra_folders, folder_exclude or exclude", verb)
				}
				if val == "" {
					return errors.New("empty value")
				}
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				list := get(cfg)
				same := func(e string) bool { return e == val }
				if key == "extra_folders" {
					// Load expands ~/ entries, so compare absolute paths, whichever
					// form the value comes in.
					val = absPath(val)
					same = func(e string) bool { return filepath.Clean(absPath(e)) == filepath.Clean(val) }
					if add {
						if err := sync.FolderSyncable(cfg, val); err != nil {
							return err
						}
					}
				}
				i := slices.IndexFunc(*list, same)
				switch {
				case add && i >= 0:
					fmt.Printf("%s already has %s\n", key, val)
					return nil
				case add:
					*list = append(*list, val)
				case i < 0:
					return fmt.Errorf("%s doesn't have %s", key, val)
				default:
					*list = slices.Delete(*list, i, i+1)
				}
				if err := config.Save(cfg); err != nil {
					return err
				}
				printSuccess(fmt.Sprintf("%s: %s %s", key, map[bool]string{true: "added", false: "removed"}[add], val))
				if key == "extra_folders" && !add {
					fmt.Println("It keeps syncing while a session works in it; nothing was deleted.")
				}
				return nil
			},
		}
	}
	cmd.AddCommand(show, set, listEdit(true), listEdit(false))
	return cmd
}

// absPath expands a leading ~/ and makes p absolute.
func absPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}


// --- folders -----------------------------------------------------------------

func foldersCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "folders",
		Short: "List the project folders that sync, with what each holds",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			stats, err := sync.FolderStats(cfg)
			if err != nil {
				return err
			}
			if stats == nil {
				stats = []sync.FolderStat{}
			}
			if asJSON {
				return printJSON(stats)
			}
			if !cfg.IsFolderSyncEnabled() {
				fmt.Println("Project folders don't sync (folder_sync is off or the scope isn't everything).")
				return nil
			}
			if len(stats) == 0 {
				fmt.Println("No project folders yet: they are picked up from your sessions at the next sync.")
				return nil
			}
			for _, f := range stats {
				kind := "folder"
				if f.Git {
					kind = "git repo"
				}
				state := ""
				if !f.Exists {
					state = colorYellow + " (missing here)" + colorReset
				}
				fmt.Printf("  ~/%-40s %s%s · %s · %s%s%s\n", f.Path, colorDim, kind, agent.Plural(f.Files, "file"), formatBytes(f.Bytes), colorReset, state)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// --- claude ------------------------------------------------------------------

func claudeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "claude",
		Short: "Control the Claude desktop app",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "restart",
		Short: "Quit and reopen Claude so its sidebar shows what arrived from your other Macs",
		Long: `Quit and reopen the Claude desktop app, applying sidebar groups, membership and
pins from your other Macs in between. When Claude sessions are still working,
Claude is asked first, as the menu bar's Restart Claude does.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := agentPaths()
			if err != nil {
				return err
			}
			logFile, err := agent.OpenLogFile(p.Log, agent.MaxLogSize)
			if err != nil {
				return err
			}
			defer func() { _ = logFile.Close() }()
			logger := agent.NewLogger(logFile)
			app := agent.NewDesktopApp()
			app.BeforeLaunch = func(ctx context.Context) { applyPendingSidebar(ctx, logger) }
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Minute)
			defer cancel()
			restarted, err := app.RestartIfConfirmed(ctx)
			if err != nil {
				return err
			}
			if !restarted {
				return errors.New("not restarted: Claude has sessions still working, or the question went unanswered")
			}
			printSuccess("Restarted Claude")
			return nil
		},
	})
	return cmd
}

// --- diagnostics -------------------------------------------------------------

// secretRe matches values that must never leave the Mac in a diagnostics
// report: SAS signatures and keys in URLs or YAML.
var secretRe = regexp.MustCompile(`(?i)(sig=|sv=[^&\s]*&|azure_sas_token:\s*|azure_account_key:\s*|secret_access_key:\s*|webdav_password:\s*|AccountKey=)[^\s&"']+`)

func redactSecrets(s string) string {
	return secretRe.ReplaceAllString(s, "${1}REDACTED")
}

func diagnosticsCmd() *cobra.Command {
	lines := 200
	cmd := &cobra.Command{
		Use:   "diagnostics",
		Short: "Print a troubleshooting report (settings, status, recent log) with secrets removed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var b strings.Builder
			fmt.Fprintf(&b, "Stradl diagnostics, %s\n", time.Now().Format(time.RFC1123))
			fmt.Fprintf(&b, "version %s · %s/%s · Go %s\n\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
			if cfg, err := config.Load(); err != nil {
				fmt.Fprintf(&b, "config: %v\n", err)
			} else {
				data, _ := json.MarshalIndent(describeConfig(cfg), "", "  ")
				fmt.Fprintf(&b, "settings:\n%s\n\n", data)
			}
			if p, err := agentPaths(); err == nil {
				if data, err := os.ReadFile(p.Status); err == nil {
					fmt.Fprintf(&b, "status.json:\n%s\n\n", data)
				}
				fmt.Fprintf(&b, "last %d log lines (%s):\n", lines, p.Log)
				for _, l := range tailLines(p.Log, lines) {
					b.WriteString(l + "\n")
				}
			}
			fmt.Print(redactSecrets(b.String()))
			return nil
		},
	}
	cmd.Flags().IntVar(&lines, "lines", lines, "how many recent log lines to include")
	return cmd
}

// tailLines returns the last n lines of a file.
func tailLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return []string{"(" + err.Error() + ")"}
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err == nil && fi.Size() > 4<<20 {
		_, _ = f.Seek(fi.Size()-4<<20, 0)
	}
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
		if len(out) > n {
			out = out[1:]
		}
	}
	return out
}
