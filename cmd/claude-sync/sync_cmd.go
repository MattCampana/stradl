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

func syncCmd() *cobra.Command {
	var wait bool
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Two-way sync with every other device (safe to run any time)",
		Long: `Reconcile this device with remote storage in both directions.

Every session created on any device ends up on every device. A conversation
continued on two devices before they synced is merged, so no turn is lost.
Deleting a session here only removes it here. A session open in a running
Claude process is never rewritten underneath it; its updates wait until the
process exits.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			start := time.Now()
			report, err := sync.RunSync(ctx, cfg, quiet, wait)
			if errors.Is(err, sync.ErrLocked) {
				if !quiet {
					printInfo("Another sync is already running; it covers the same files.")
				}
				return nil
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
	cmd.Flags().BoolVar(&wait, "wait", false, "Wait for a running sync to finish instead of skipping")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "Give up after this long")
	return cmd
}

func printSyncReport(r *sync.SyncReport, took time.Duration) {
	line := func(sym, color string, n int, what string) {
		if n > 0 {
			fmt.Printf("  %s%s%s %d %s\n", color, sym, colorReset, n, what)
		}
	}
	if !r.Changed() && len(r.Deferred) == 0 && len(r.Errors) == 0 && len(r.Skipped) == 0 {
		printSuccess(fmt.Sprintf("✓ Everything is in sync (%s)", took.Round(100*time.Millisecond)))
		return
	}
	line("↑", colorCyan, len(r.Uploaded), "uploaded")
	line("↓", colorCyan, len(r.Downloaded), "downloaded")
	line("⇄", colorCyan, len(r.Merged), "merged (changed on two devices)")
	line("↺", colorCyan, len(r.Restored), "restored to remote")
	line("…", colorDim, len(r.Deferred), "waiting for an open session to close")
	line("!", colorYellow, len(r.Conflicts), "conflicts kept as .conflict copies")
	line("?", colorYellow, len(r.Skipped), "skipped (unknown path token; check path_map)")
	if n := len(r.NewDesktopSessions); n > 0 {
		printWarning(fmt.Sprintf("%d new session(s) from other devices: restart the Claude app to see them in the sidebar.", n))
	}
	for _, err := range r.Errors {
		fmt.Printf("  %s✗%s %v\n", colorYellow, colorReset, err)
	}
	printSuccess(fmt.Sprintf("✓ Sync finished in %s", took.Round(100*time.Millisecond)))
}
