package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/tawanorg/claude-sync/internal/agent"
	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/sync"
)

// previewSync asks the engine what a sync would do right now, without
// changing anything.
func previewSync(ctx context.Context) (*sync.SyncReport, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	s, err := sync.NewSyncer(cfg, true)
	if err != nil {
		return nil, err
	}
	return s.Preview(ctx)
}

// pendingWork reports whether a preview found anything to do or report.
func pendingWork(r *sync.SyncReport) bool {
	return r.Changed() || len(r.Deferred)+len(r.Skipped)+len(r.Warnings)+len(r.InICloud) > 0
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the background agent, the last sync, and what is waiting to sync",
		RunE: func(cmd *cobra.Command, args []string) error {
			if p, err := agentPaths(); err == nil {
				st, readErr := agent.ReadStatus(p.Status)
				if readErr != nil && !agent.IsNotExist(readErr) {
					return readErr
				}
				var job *agent.JobState
				if runtime.GOOS == "darwin" {
					s := newLaunchd(p, "").State()
					job = &s
				}
				printAgentStatus(os.Stdout, p, job, st, agent.IsPaused(p), readErr == nil, time.Now())
				fmt.Println()
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			r, err := previewSync(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("%sWaiting to sync%s\n", colorBold, colorReset)
			if !pendingWork(r) {
				printSuccess("✓ Nothing: this Mac and remote storage match")
				return nil
			}
			row := func(n int, what string) {
				if n > 0 {
					fmt.Printf("  %d %s\n", n, what)
				}
			}
			row(len(r.Uploaded), "to upload")
			row(len(r.Downloaded), "to download")
			row(len(r.Merged), "changed on two devices (will merge)")
			row(len(r.Restored), "to restore to remote")
			row(len(r.Deleted), "deleted here, to delete on every Mac")
			row(len(r.Trashed), "deleted on another Mac, to move to ~/.stradl/trash")
			row(len(r.Deferred), "waiting for an open session or a git command to finish")
			row(len(r.InICloud), "only in iCloud on this Mac (downloading; they sync once they arrive)")
			row(len(r.Skipped), "skipped (unknown path token; check path_map)")
			for _, w := range r.Warnings {
				printWarning(w)
			}
			fmt.Println("\n  Run 'stradl sync' to sync now, or 'stradl diff' for the file list.")
			return nil
		},
	}
}

func diffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff",
		Short: "List the files the next sync would upload, download, merge or delete",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			r, err := previewSync(ctx)
			if err != nil {
				return err
			}
			if !pendingWork(r) {
				printSuccess("✓ In sync: nothing to upload, download, merge or delete")
				return nil
			}
			list := func(sym, title string, paths []string) {
				if len(paths) == 0 {
					return
				}
				fmt.Printf("%s (%d):\n", title, len(paths))
				for _, p := range paths {
					fmt.Printf("  %s %s\n", sym, p)
				}
				fmt.Println()
			}
			list("↑", "Upload", r.Uploaded)
			list("↓", "Download", r.Downloaded)
			list("⇄", "Merge (changed on two devices)", r.Merged)
			list("↺", "Restore to remote", r.Restored)
			list("✗", "Delete on every Mac (deleted here)", r.Deleted)
			list("⌫", "Move to ~/.stradl/trash (deleted on another Mac)", r.Trashed)
			list("…", "Waiting for an open session or a git command to finish", r.Deferred)
			list("☁", "Only in iCloud on this Mac (downloading)", r.InICloud)
			list("?", "Skipped (unknown path token)", r.Skipped)
			for _, w := range r.Warnings {
				printWarning(w)
			}
			return nil
		},
	}
}
