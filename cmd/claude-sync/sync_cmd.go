package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/sync"
)

func init() {
	extraCommands = append(extraCommands, syncCmd)
}

// syncOnce runs one sync pass under the device lock. A variable so tests can
// stand in for the engine.
var syncOnce = sync.RunSync

func syncCmd() *cobra.Command {
	var noWait, allowMassDelete bool
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Two-way sync with every other device (safe to run any time)",
		Long: `Reconcile this device with remote storage in both directions.

Every session created on any device ends up on every device. A conversation
continued on two devices before they synced is merged, so no turn is lost.
Deleting a session here only removes it here. A session open in a running
Claude process is never rewritten underneath it; its updates wait until the
process exits.

Any other file deleted here (a setting, a skill, a file in a project folder)
is deleted on every Mac; each other Mac moves its copy to ~/.stradl/trash,
where it stays for 30 days. A file changed on another Mac meanwhile is kept
instead. A sync that would delete more than half of a folder's files deletes
none and warns; --allow-mass-delete lets it go ahead.

When another sync is already running (the background agent's, say), this one
waits up to 2 minutes for it to finish and then runs, so it always includes
your latest changes. --no-wait skips it instead.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			cfg.AllowMassDelete = allowMassDelete
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			start := time.Now()
			report, err := syncOnce(ctx, cfg, quiet, false)
			if errors.Is(err, sync.ErrLocked) {
				// The running sync may have started before the latest
				// changes, so it doesn't stand in for this one.
				if noWait {
					if !quiet {
						printInfo("Another sync is running, so this one was skipped (--no-wait).")
					}
					return nil
				}
				if !quiet {
					printInfo("Another sync is running; waiting for it to finish...")
				}
				report, err = syncOnce(ctx, cfg, quiet, true)
				if errors.Is(err, sync.ErrLocked) {
					return fmt.Errorf("another sync is still running after 2 minutes; try again shortly, or check 'stradl status'")
				}
			}
			if err != nil {
				return err
			}
			if !quiet {
				printSyncReport(report, time.Since(start))
			}
			if len(report.Errors) > 0 {
				return fmt.Errorf("%d file(s) failed to sync", len(report.Errors))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Skip this sync, instead of waiting, when another one is running")
	cmd.Flags().BoolVar(&allowMassDelete, "allow-mass-delete", false, "Delete files on every Mac even when that removes more than half of a folder's files")
	cmd.Flags().Bool("wait", true, "Wait for a running sync to finish (the default)")
	_ = cmd.Flags().MarkDeprecated("wait", "waiting is now the default; pass --no-wait to skip instead")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "Give up after this long")
	return cmd
}

func printSyncReport(r *sync.SyncReport, took time.Duration) {
	line := func(sym, color string, n int, what string) {
		if n > 0 {
			fmt.Printf("  %s%s%s %d %s\n", color, sym, colorReset, n, what)
		}
	}
	if !r.Changed() && len(r.Deferred)+len(r.Errors)+len(r.Skipped)+len(r.Warnings) == 0 {
		printSuccess(fmt.Sprintf("✓ Everything is in sync (%s)", took.Round(100*time.Millisecond)))
		return
	}
	line("↑", colorCyan, len(r.Uploaded), "uploaded")
	line("↓", colorCyan, len(r.Downloaded), "downloaded")
	line("⇄", colorCyan, len(r.Merged), "merged (changed on two devices)")
	line("↺", colorCyan, len(r.Restored), "restored to remote")
	line("✗", colorCyan, len(r.Deleted), "deleted on every Mac (deleted here)")
	line("⌫", colorCyan, len(r.Trashed), "moved to ~/.stradl/trash (deleted on another Mac)")
	line("…", colorDim, len(r.Deferred), "waiting for an open session or a git command to finish")
	line("!", colorYellow, len(r.Conflicts), "conflicts kept as .conflict copies")
	line("?", colorYellow, len(r.Skipped), "skipped (unknown path token; check path_map)")
	if n := len(r.NewDesktopSessions); n > 0 {
		printWarning(fmt.Sprintf("%d new session(s) from other devices: restart the Claude app to see them in the sidebar.", n))
	}
	for _, w := range r.Warnings {
		printWarning(w)
	}
	for _, err := range r.Errors {
		fmt.Printf("  %s✗%s %v\n", colorYellow, colorReset, err)
	}
	printSuccess(fmt.Sprintf("✓ Sync finished in %s", took.Round(100*time.Millisecond)))
}
