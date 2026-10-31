package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/sync"
)

// accounts: the Claude accounts the Circle knows (a Mac can be signed in to
// several over time) and which of them sync. Each read has a --json form for
// the app.

func init() {
	extraCommands = append(extraCommands, accountsCmd)
}

func accountsCmd() *cobra.Command {
	var asJSON, refresh bool
	cmd := &cobra.Command{
		Use:   "accounts",
		Short: "List your Claude accounts and which ones sync",
		Long: `List the Claude accounts your Macs have used and which ones sync.

An account that doesn't sync keeps its sessions, and the project folders only
its sessions use, on the Mac they were made on. Choose with
'stradl accounts sync <account> on|off' (for every Mac in your Circle).
Nothing already in storage is deleted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			var list []sync.Account
			if refresh {
				if list, err = refreshAccounts(cmd.Context(), cfg); err != nil {
					return err
				}
			} else {
				list = sync.ListAccounts(cfg)
			}
			if list == nil {
				list = []sync.Account{}
			}
			if asJSON {
				return printJSON(list)
			}
			if len(list) == 0 {
				fmt.Println("No Claude accounts seen yet: they are picked up at the next sync.")
				return nil
			}
			for _, a := range list {
				state := map[string]string{
					"on":  colorGreen + "syncs" + colorReset,
					"off": colorYellow + "doesn't sync" + colorReset,
					"ask": colorDim + "syncs (not chosen yet)" + colorReset,
				}[a.Sync]
				dot, extra := "  ", ""
				if a.Active {
					dot = colorGreen + "● " + colorReset
					extra = colorDim + " · signed in to Claude" + colorReset
				}
				if a.Terminal && !a.Active {
					extra += colorDim + " · signed in to Claude Code in the terminal" + colorReset
				}
				fmt.Printf("%s%-34s %s %s· %d sessions%s\n", dot, a.Name, state, colorDim, a.Sessions, colorReset+extra)
				if a.Email != "" && a.Email != a.Name {
					fmt.Printf("  %s%s%s\n", colorDim, a.Email, colorReset)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "read the Circle's account list from storage first")
	cmd.AddCommand(accountsSyncCmd(), accountsLabelCmd(), accountsHeldCmd(), accountsAssignCmd())
	return cmd
}

func accountsHeldCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "held",
		Short: "Sessions waiting because their account isn't known",
		Long: `While an account doesn't sync, a session on this Mac whose account Stradl
can't tell (written before Stradl knew this Mac's sign-ins, or while
switching accounts) waits instead of syncing. Give them an account with
'stradl accounts assign <account> --held'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			held := sync.HeldSessions(cfg)
			if held == nil {
				held = []sync.HeldSession{}
			}
			if asJSON {
				return printJSON(held)
			}
			if len(held) == 0 {
				fmt.Println("No sessions are waiting for an account.")
				return nil
			}
			for _, h := range held {
				fmt.Printf("  %s  %s%s%s\n", h.ID, colorDim, h.Cwd, colorReset)
			}
			fmt.Printf("\nGive them an account: stradl accounts assign <account> --held\n")
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func accountsAssignCmd() *cobra.Command {
	var held bool
	cmd := &cobra.Command{
		Use:   "assign <account> [session...]",
		Short: "Say which account sessions belong to (--held: all waiting ones)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			sessions := args[1:]
			if held {
				for _, h := range sync.HeldSessions(cfg) {
					sessions = append(sessions, h.ID)
				}
			}
			if len(sessions) == 0 {
				return fmt.Errorf("name sessions, or --held for all waiting ones")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			s, a, err := resolveAccount(ctx, cfg, args[0])
			if err != nil {
				return err
			}
			if err := s.AssignSessions(ctx, a.ID, sessions); err != nil {
				return err
			}
			fmt.Printf("%s✓%s %d sessions belong to %s; they sync by its choice from the next sync\n", colorGreen, colorReset, len(sessions), a.Name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&held, "held", false, "every session waiting for an account")
	return cmd
}

func refreshAccounts(ctx context.Context, cfg *config.Config) ([]sync.Account, error) {
	s, err := sync.NewSyncer(cfg, true)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return s.RefreshAccounts(ctx)
}

// resolveAccount refreshes the account list and finds who in it.
func resolveAccount(ctx context.Context, cfg *config.Config, who string) (*sync.Syncer, sync.Account, error) {
	s, err := sync.NewSyncer(cfg, true)
	if err != nil {
		return nil, sync.Account{}, err
	}
	list, err := s.RefreshAccounts(ctx)
	if err != nil {
		return nil, sync.Account{}, err
	}
	a, err := sync.FindAccount(list, who)
	return s, a, err
}

func accountsSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync <account> on|off",
		Short: "Choose whether an account's sessions sync (for every Mac)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			on := strings.EqualFold(args[1], "on")
			if !on && !strings.EqualFold(args[1], "off") {
				return fmt.Errorf("say on or off, not %q", args[1])
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			s, a, err := resolveAccount(ctx, cfg, args[0])
			if err != nil {
				return err
			}
			if err := s.SetAccountSync(ctx, a.ID, on, time.Now()); err != nil {
				return err
			}
			if on {
				fmt.Printf("%s✓%s %s syncs on every Mac in your Circle\n", colorGreen, colorReset, a.Name)
			} else {
				fmt.Printf("%s✓%s %s stays on each Mac now; nothing already synced was deleted\n", colorGreen, colorReset, a.Name)
			}
			return nil
		},
	}
}

func accountsLabelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "label <account> <name>",
		Short: "Give an account a name every Mac shows (empty to clear)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			s, a, err := resolveAccount(ctx, cfg, args[0])
			if err != nil {
				return err
			}
			if err := s.SetAccountLabel(ctx, a.ID, args[1], time.Now()); err != nil {
				return err
			}
			// Its launcher shows the name: redraw it (when deployed).
			if _, err := redeployLaunchers(cfg); err != nil {
				printWarning("Named, but the launchers weren't updated: " + err.Error())
			}
			fmt.Printf("%s✓%s Named %s\n", colorGreen, colorReset, a.ID)
			return nil
		},
	}
}
