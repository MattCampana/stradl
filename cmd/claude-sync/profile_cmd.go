package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tawanorg/claude-sync/internal/agent"
	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/profiles"
	"github.com/tawanorg/claude-sync/internal/sync"
)

// profile: the Claude app opened signed in to the account asked for. Each
// account has a Claude app profile of its own (see package profiles), so
// every account stays signed in; the reads have a --json form for the app.

func init() {
	extraCommands = append(extraCommands, profileCmd, openCmd)
}

// profileView is one row of `stradl profile list --json`: an account and the
// profile that opens it, or a profile no one has signed in to yet.
type profileView struct {
	Account  *sync.Account    `json:"account,omitempty"`
	Profile  profiles.Profile `json:"profile"`
	SignedIn bool             `json:"signedIn"`
	Open     bool             `json:"open"`
}

func profileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "profile",
		Aliases: []string{"profiles"},
		Short:   "Open Claude signed in to any of your accounts",
		Long: `Open the Claude app signed in to the account you choose.

Each Claude account gets a Claude app profile of its own, so every one stays
signed in: the main profile is the app's own folder, signed in to whichever
account it is; the others live in ~/Library/Application Support/Stradl/Profiles.
A new profile needs one sign-in, the first time it opens. All profiles share
the sidebar sessions, and they sync as before. Several profiles can be open
at once.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return listProfiles(false) },
	}
	var asJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List your accounts and the Claude profile each opens in",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return listProfiles(asJSON) },
	}
	list.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.AddCommand(list, openCmd(), profileNewCmd(), profileConvertCmd(), profileLaunchersCmd(), profileColorCmd())
	return cmd
}

func profileRows(cfg *config.Config) []profileView {
	env := profiles.FromConfig(cfg)
	list := env.List()
	used := make(map[string]bool)
	var rows []profileView
	for _, a := range sync.ListAccounts(cfg) {
		a := a
		p, ok := profiles.ForAccount(list, a.ID)
		if ok {
			used[p.ID] = true
		}
		rows = append(rows, profileView{Account: &a, Profile: p, SignedIn: ok && (p.SignedIn || p.Main) && p.Account == a.ID, Open: ok && p.Running && p.Account == a.ID})
	}
	for _, p := range list {
		if !used[p.ID] && (p.Account == "" || !p.Main) {
			rows = append(rows, profileView{Profile: p, SignedIn: p.SignedIn, Open: p.Running})
		}
	}
	return rows
}

func listProfiles(asJSON bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	rows := profileRows(cfg)
	if asJSON {
		if rows == nil {
			rows = []profileView{}
		}
		return printJSON(rows)
	}
	if len(rows) == 0 {
		fmt.Println("No Claude accounts seen yet: they are picked up at the next sync.")
		return nil
	}
	for _, r := range rows {
		name, where := "(new profile, not signed in yet)", ""
		if r.Account != nil {
			name = r.Account.Name
		}
		switch {
		case r.Profile.ID == "":
			where = "no profile yet"
		case r.Profile.Main:
			where = "Claude's own profile"
		case r.Account != nil:
			where = "its own profile"
		default:
			where = "profile " + r.Profile.ID
		}
		dot, state := "  ", ""
		if r.Open {
			dot, state = colorGreen+"● "+colorReset, colorDim+" · open"+colorReset
		}
		if r.Profile.ID != "" && !r.SignedIn {
			state += colorYellow + " · sign in once" + colorReset
		}
		fmt.Printf("%s%-34s %s%s%s%s\n", dot, name, colorDim, where, colorReset, state)
	}
	fmt.Printf("\nOpen one: stradl open <account>\n")
	return nil
}

// resolveProfile finds who's profile: an account (see sync.FindAccount),
// opened in the profile signed in to it, or made one when it has none; else
// a profile ID (main, new-…).
func resolveProfile(cfg *config.Config, who string) (profiles.Profile, string, error) {
	env := profiles.FromConfig(cfg)
	a, aerr := sync.FindAccount(sync.ListAccounts(cfg), who)
	if aerr == nil {
		if p, ok := profiles.ForAccount(env.List(), a.ID); ok {
			return p, a.Name, nil
		}
		var p profiles.Profile
		err := withSyncLock(cfg, func() (err error) {
			p, err = env.Ensure(a.ID)
			return err
		})
		return p, a.Name, err
	}
	if p, ok := env.Get(who); ok {
		return p, p.ID, nil
	}
	return profiles.Profile{}, "", aerr
}

// withSyncLock runs fn holding the sync lock, so a profile being made copies
// its preferences and their sidebar base while no sync changes them.
func withSyncLock(cfg *config.Config, fn func() error) error {
	lockPath := config.LockFilePath()
	if cfg.StateDirOverride != "" {
		lockPath = filepath.Join(cfg.StateDirOverride, config.LockFile)
	}
	release, err := sync.AcquireLock(lockPath, true, 2*time.Minute)
	if err != nil {
		return fmt.Errorf("waiting for the sync to finish: %w", err)
	}
	defer release()
	return fn()
}

func openCmd() *cobra.Command {
	var alone, force, asJSON bool
	cmd := &cobra.Command{
		Use:   "open [account]",
		Short: "Open Claude signed in to an account (none: choose from a list)",
		Long: `Open the Claude app signed in to an account: its ID (or the first
characters), its email or its name. With no account, Stradl asks which.

The first time an account's profile opens, sign in to that account in it;
it stays signed in from then on. --alone first quits Claude's other open
profiles, so a sign-in that comes back through the browser reaches this one.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			who := ""
			if len(args) == 1 {
				who = args[0]
			} else if who, err = chooseAccount(cmd.Context(), cfg); err != nil || who == "" {
				return err
			}
			p, name, err := resolveProfile(cfg, who)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			env := profiles.FromConfig(cfg)
			if alone {
				if err := quitOthers(ctx, env, p, force); err != nil {
					return err
				}
				p, _ = env.Get(p.ID)
			}
			if err := env.Open(ctx, agent.ExecCommand, p); err != nil {
				return err
			}
			if asJSON {
				return printJSON(p)
			}
			printSuccess("Opening Claude as " + name)
			if !p.SignedIn && !p.Main {
				fmt.Printf("  %sThis profile isn't signed in yet: sign in to %s once, and it stays signed in.%s\n", colorDim, name, colorReset)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&alone, "alone", false, "quit Claude's other open profiles first (for signing in)")
	cmd.Flags().BoolVar(&force, "force", false, "with --alone, quit even while sessions are working")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the profile as JSON")
	return cmd
}

// quitOthers quits every open Claude profile but keep, unless sessions are
// in the middle of a turn (force overrides).
func quitOthers(ctx context.Context, env profiles.Env, keep profiles.Profile, force bool) error {
	app := agent.NewDesktopApp()
	if n := app.BusySessions(); n > 0 && !force {
		return fmt.Errorf("%d Claude session(s) still working; wait for them, or pass --force", n)
	}
	for _, p := range env.List() {
		if p.Running && p.ID != keep.ID {
			if err := profiles.Quit(ctx, agent.ExecCommand, p.PID, 20*time.Second, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// chooseAccount asks which account to open, in a dialog: "" when cancelled.
// Names travel as script arguments, so none can break out of the script.
func chooseAccount(ctx context.Context, cfg *config.Config) (string, error) {
	accounts := sync.ListAccounts(cfg)
	if len(accounts) == 0 {
		return "", errors.New("no Claude accounts seen yet: open Claude and sign in, and Stradl picks it up")
	}
	args := []string{
		"-e", "on run argv",
		"-e", "tell me to activate",
		"-e", `set r to choose from list argv with title "Stradl" with prompt "Open Claude as:" OK button name "Open" default items {item 1 of argv}`,
		"-e", "if r is false then return \"\"",
		"-e", "return item 1 of r",
		"-e", "end run",
	}
	names := make(map[string]string)
	for _, a := range accounts {
		label := a.Name
		if a.Email != "" && a.Email != a.Name {
			label += " (" + a.Email + ")"
		}
		names[label] = a.ID
		args = append(args, label)
	}
	out, err := agent.ExecCommand(ctx, "/usr/bin/osascript", args...)
	if err != nil {
		return "", fmt.Errorf("couldn't ask which account: %w", err)
	}
	return names[strings.TrimSpace(string(out))], nil
}

func profileNewCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "new",
		Short: "Open a new Claude profile to sign in to another account",
		Long: `Make a Claude app profile for an account Stradl hasn't seen yet and open it.
Sign in to the account there; Stradl learns it and keeps that profile for it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			env := profiles.FromConfig(cfg)
			var p profiles.Profile
			if err := withSyncLock(cfg, func() (err error) {
				p, err = env.New()
				return err
			}); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			if err := env.Open(ctx, agent.ExecCommand, p); err != nil {
				return err
			}
			if asJSON {
				return printJSON(p)
			}
			printSuccess("Opened a new Claude profile: sign in to the other account there")
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the profile as JSON")
	return cmd
}

func profileConvertCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "convert",
		Short: "Give every account you use a Claude profile (each sync does this too)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			var ids []string
			for _, a := range sync.ListAccounts(cfg) {
				ids = append(ids, a.ID)
			}
			var made []profiles.Profile
			if err := withSyncLock(cfg, func() (err error) {
				made, err = profiles.FromConfig(cfg).Convert(ids)
				return err
			}); err != nil {
				return err
			}
			if asJSON {
				if made == nil {
					made = []profiles.Profile{}
				}
				return printJSON(made)
			}
			if len(made) == 0 {
				fmt.Println("Every account already has a Claude profile.")
				return nil
			}
			for _, p := range made {
				printSuccess("Made a Claude profile for " + accountName(cfg, p.For) + " (sign in once when it first opens)")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the profiles made as JSON")
	return cmd
}

func accountName(cfg *config.Config, id string) string {
	if a, err := sync.FindAccount(sync.ListAccounts(cfg), id); err == nil {
		return a.Name
	}
	return id
}

func profileLaunchersCmd() *cobra.Command {
	var remove, asJSON bool
	cmd := &cobra.Command{
		Use:   "launchers",
		Short: "Add a Claude app for each account to ~/Applications",
		Long: `Add small apps to ~/Applications that open Claude signed in to an account:
"Claude (<account>)" for each, its icon showing the account's name on a band
of its color, and "Claude (Choose Account)", which asks. They show in
Launchpad, Spotlight and the Dock like any app. Once they are there, naming
an account, changing its color ('stradl profile color') or a new account
updates them by itself. --remove takes them away.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if remove {
				n, err := agent.RemoveLaunchers(launchersDir())
				if err != nil {
					return err
				}
				_ = profiles.FromConfig(cfg).SetDeployed(nil)
				printSuccess(fmt.Sprintf("Removed %d Claude launchers", n))
				return nil
			}
			made, err := deployLaunchers(cfg)
			if err != nil {
				return err
			}
			if asJSON {
				if made == nil {
					made = []string{}
				}
				return printJSON(made)
			}
			for _, m := range made {
				printSuccess("Added " + filepath.Join("~/Applications", filepath.Base(m)))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&remove, "remove", false, "remove the launchers Stradl added")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the launchers written as JSON")
	return cmd
}

// launchersDir is ~/Applications, where the launchers go.
func launchersDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Applications")
}

// launcherPlan returns the launchers every account gets and, per account,
// what its launcher shows ("name|color"), to tell when they need redrawing.
func launcherPlan(cfg *config.Config) ([]agent.Launcher, map[string]string) {
	accounts := sync.ListAccounts(cfg)
	ids := make([]string, len(accounts))
	for i, a := range accounts {
		ids[i] = a.ID
	}
	colors := profiles.FromConfig(cfg).Colors(ids)
	var ls []agent.Launcher
	shows := make(map[string]string, len(accounts))
	for _, a := range accounts {
		badge := profiles.BadgeName(a.Label, a.Email, a.ID)
		ls = append(ls, agent.Launcher{Name: "Claude (" + a.Name + ")", ID: a.ID, Args: []string{"open", a.ID}, Badge: badge, Color: colors[a.ID]})
		shows[a.ID] = a.Name + "|" + badge + "|" + colors[a.ID]
	}
	ls = append(ls, agent.Launcher{Name: "Claude (Choose Account)", ID: "choose", Args: []string{"open"}})
	return ls, shows
}

// deployLaunchers writes every account's launcher to ~/Applications and
// records what they show.
func deployLaunchers(cfg *config.Config) ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	ls, shows := launcherPlan(cfg)
	made, err := agent.WriteLaunchers(launchersDir(), exe, claudeIcon(), ls)
	if err != nil {
		return made, err
	}
	return made, profiles.FromConfig(cfg).SetDeployed(shows)
}

// launchersStale reports whether launchers were deployed and what they
// should show changed since (a name, a color, an account).
func launchersStale(cfg *config.Config) bool {
	deployed := profiles.FromConfig(cfg).Deployed()
	if len(deployed) == 0 || !agent.LaunchersDeployed(launchersDir()) {
		return false
	}
	_, shows := launcherPlan(cfg)
	return !maps.Equal(shows, deployed)
}

// redeployLaunchers updates the launchers when they are stale (see
// launchersStale). It reports whether it redeployed.
func redeployLaunchers(cfg *config.Config) (bool, error) {
	if !launchersStale(cfg) {
		return false, nil
	}
	_, err := deployLaunchers(cfg)
	return err == nil, err
}

func profileColorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "color <account> <color>",
		Short: "Choose the color of an account's launcher (#RRGGBB or a name)",
		Long: `Choose the color of the band on an account's launcher icon: #RRGGBB, or one
of pink, violet, azure, teal, amber, coral, lime, magenta. Launchers already
in ~/Applications are redrawn.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			a, err := sync.FindAccount(sync.ListAccounts(cfg), args[0])
			if err != nil {
				return err
			}
			hex, err := profiles.FromConfig(cfg).SetColor(a.ID, args[1])
			if err != nil {
				return err
			}
			if _, err := redeployLaunchers(cfg); err != nil {
				return fmt.Errorf("color saved, but the launchers weren't updated: %w", err)
			}
			printSuccess(fmt.Sprintf("%s's launcher is %s", a.Name, hex))
			return nil
		},
	}
}

// claudeIcon returns the Claude app's icon file, "" when not found.
func claudeIcon() string {
	const app = "/Applications/Claude.app"
	out, err := exec.Command("/usr/bin/plutil", "-extract", "CFBundleIconFile", "raw", app+"/Contents/Info.plist").Output()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(out))
	if filepath.Ext(name) == "" {
		name += ".icns"
	}
	p := filepath.Join(app, "Contents", "Resources", filepath.Base(name))
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}
