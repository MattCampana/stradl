package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	gosync "sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tawanorg/claude-sync/internal/agent"
	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/dashboard"
	"github.com/tawanorg/claude-sync/internal/sync"
)

func init() {
	extraCommands = append(extraCommands, dashboardCmd)
}

// openBrowser opens a URL in the default browser. Tests replace it.
var openBrowser = func(u string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "/usr/bin/open"
	}
	return exec.Command(name, u).Start()
}

// dashboardServerOptions configure every dashboard server this process
// starts; tests change them.
var dashboardServerOptions dashboard.ServerOptions

func dashboardCmd() *cobra.Command {
	var noOpen, demo bool
	var scenario string
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Open the dashboard: your Macs, this week's sessions and what is synced",
		Long: `Open Stradl's dashboard in your browser. It shows every Mac syncing into your
storage, what you worked on in the last 7 days, each session on this Mac with
its sync state, and what the next sync will do.

The dashboard runs on this Mac only (127.0.0.1) behind a random key, and
nothing it shows leaves the Mac. While the background agent runs, this opens
the agent's dashboard (also in the menu bar: Open Dashboard…). Otherwise this
command serves one itself until you press Ctrl-C.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := agentPaths()
			if err != nil {
				return err
			}
			if !demo {
				if u, ok := agent.LiveDashboard(p); ok {
					return showDashboard(u, noOpen, false)
				}
			}

			var prov dashboard.Provider
			switch {
			case demo && scenario == "empty":
				prov = dashboard.NewEmptyFake(time.Now())
			case demo:
				prov = dashboard.NewFake(time.Now())
			default:
				prov = newStandaloneProvider(p)
			}
			srv := dashboard.NewServer(prov, dashboardServerOptions)
			u, err := srv.Start()
			if err != nil {
				return err
			}
			defer func() { _ = srv.Close() }()

			if err := showDashboard(u, noOpen, true); err != nil {
				return err
			}
			if !quiet {
				what := "Serving the dashboard from this terminal"
				if demo {
					what = "Serving the dashboard with sample data"
				}
				printInfo(what + "; press Ctrl-C to stop.")
			}
			parent := cmd.Context()
			if parent == nil {
				parent = context.Background()
			}
			ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
			defer stop()
			<-ctx.Done()
			return nil
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Print the URL instead of opening the browser")
	cmd.Flags().BoolVar(&demo, "demo", false, "Serve made-up sample data, for trying it out or screenshots")
	cmd.Flags().StringVar(&scenario, "demo-scenario", "", "Sample data to serve with --demo (empty)")
	_ = cmd.Flags().MarkHidden("demo-scenario")
	return cmd
}

// showDashboard prints the URL and opens it unless noOpen. With noOpen the
// URL is printed on a line of its own, for scripts.
func showDashboard(u string, noOpen, standalone bool) error {
	if noOpen {
		fmt.Println(u)
		return nil
	}
	if err := openBrowser(u); err != nil {
		printWarning("Couldn't open the browser (" + err.Error() + "); open this address instead:")
		fmt.Println(u)
		return nil
	}
	if quiet {
		return nil
	}
	if standalone {
		printSuccess("✓ Dashboard opened in your browser.")
	} else {
		printSuccess("✓ Opened the Stradl agent's dashboard in your browser.")
	}
	printInfo(u)
	return nil
}

// startAgentDashboard serves the dashboard for a running agent. See
// serveAgentDashboard.
func startAgentDashboard(p agent.Paths, r *agent.Runner, logger *agent.Logger) (string, func()) {
	prov := &liveProvider{
		Provider: dashboard.NewLocalProvider(dashboard.LocalOptions{
			LoadConfig:   config.Load,
			StatusPath:   p.Status,
			AgentRunning: func() bool { return true },
			SyncNow:      func() error { r.SyncNow(); return nil },
		}),
		syncing: func() bool { return r.Snapshot().Syncing },
		progress: func() dashboard.Progress {
			s := r.Snapshot()
			return dashboard.Progress{Syncing: s.Syncing, Phase: s.SyncPhase, Done: s.SyncDone, Total: s.SyncTotal}
		},
	}
	return serveAgentDashboard(p, prov, logger)
}

// serveAgentDashboard starts the agent's dashboard server and records its
// URL in ~/.stradl/dashboard.json for the menu bar and `stradl dashboard`.
// A server that fails to start is logged and skipped, returning "": the
// agent keeps syncing without it. stop shuts the server down and removes
// the file. The caller holds the agent lock.
func serveAgentDashboard(p agent.Paths, prov dashboard.Provider, logger *agent.Logger) (string, func()) {
	if logger == nil {
		logger = agent.NewLogger(nil)
	}
	srv := dashboard.NewServer(prov, dashboardServerOptions)
	u, err := srv.Start()
	if err != nil {
		logger.Printf("dashboard unavailable: %v", err)
		// Any file there is from an agent that crashed: this one holds the
		// lock now, so nobody else can be serving.
		if err := os.Remove(p.Dashboard); err != nil && !os.IsNotExist(err) {
			logger.Printf("failed to remove %s: %v", p.Dashboard, err)
		}
		return "", func() {}
	}
	pid := os.Getpid()
	if err := agent.WriteDashboardInfo(p.Dashboard, agent.DashboardInfo{URL: u, PID: pid}); err != nil {
		logger.Printf("failed to record the dashboard address: %v", err)
	}
	if parsed, err := url.Parse(u); err == nil {
		logger.Printf("dashboard listening on %s", parsed.Host) // the key stays out of the log
	}
	var once gosync.Once
	return u, func() {
		once.Do(func() {
			if err := agent.RemoveDashboardInfo(p.Dashboard, pid); err != nil {
				logger.Printf("failed to remove %s: %v", p.Dashboard, err)
			}
			_ = srv.Close()
		})
	}
}

// liveProvider adds what this process knows right now to the provider's
// overview, which comes from the agent's status file: whether a sync is
// running, and for a standalone dashboard how its own last sync went.
type liveProvider struct {
	dashboard.Provider
	syncing func() bool
	// progress reports the agent's running sync; nil without an agent.
	progress func() dashboard.Progress
	// last reports this process's own last sync; nil when it runs none.
	last func() (at time.Time, err error)
}

func (l *liveProvider) Overview(ctx context.Context) (*dashboard.Overview, error) {
	ov, err := l.Provider.Overview(ctx)
	if err != nil || ov == nil {
		return ov, err
	}
	// The provider may hand the same cached Overview to every caller, so
	// change a copy. Agent is a value; the pointers in it are replaced, never
	// written through.
	cp := *ov
	ov = &cp
	if l.syncing != nil && l.syncing() {
		ov.Agent.Syncing = true
	}
	if l.progress != nil {
		pr := l.progress()
		ov.Agent.SyncPhase, ov.Agent.SyncDone, ov.Agent.SyncTotal = pr.Phase, pr.Done, pr.Total
	}
	if l.last == nil {
		return ov, nil
	}
	at, syncErr := l.last()
	if at.IsZero() || (ov.Agent.LastAttemptAt != nil && !at.After(*ov.Agent.LastAttemptAt)) {
		return ov, nil
	}
	ov.Agent.LastAttemptAt = &at
	if syncErr == nil {
		ov.Agent.LastSyncAt = &at
		ov.Agent.LastError, ov.Agent.LastErrorSummary = "", ""
	} else {
		ov.Agent.LastError = syncErr.Error()
		ov.Agent.LastErrorSummary = agent.ClassifyError(syncErr).Summary()
	}
	return ov, nil
}

// standaloneSync runs Sync Now for a dashboard served without the agent:
// sync.RunSync in the background, one at a time.
type standaloneSync struct {
	running atomic.Bool
	mu      gosync.Mutex
	at      time.Time
	err     error
	// done, when set, is called after each sync (tests).
	done func()
}

func (s *standaloneSync) start() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !s.running.CompareAndSwap(false, true) {
		return nil // already syncing
	}
	go func() {
		defer s.running.Store(false)
		if s.done != nil {
			defer s.done()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		report, err := syncOnce(ctx, cfg, true, false)
		if errors.Is(err, sync.ErrLocked) {
			return // the agent or a manual sync is already on it
		}
		if err == nil && report != nil && len(report.Errors) > 0 {
			err = fmt.Errorf("%d file(s) failed to sync; first: %w", len(report.Errors), report.Errors[0])
		}
		s.mu.Lock()
		s.at, s.err = time.Now(), err
		s.mu.Unlock()
		if err != nil && !quiet {
			printWarning("Sync failed: " + err.Error())
		}
	}()
	return nil
}

func (s *standaloneSync) last() (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.at, s.err
}

// newStandaloneProvider is the provider for `stradl dashboard` without a
// running agent dashboard: this Mac's data, with Sync Now running syncs in
// this process.
func newStandaloneProvider(p agent.Paths) dashboard.Provider {
	ss := &standaloneSync{}
	return &liveProvider{
		Provider: dashboard.NewLocalProvider(dashboard.LocalOptions{
			LoadConfig:   config.Load,
			StatusPath:   p.Status,
			AgentRunning: func() bool { return agent.Running(p) },
			SyncNow:      ss.start,
		}),
		syncing: ss.running.Load,
		last:    ss.last,
	}
}

// Progress implements dashboard.ProgressProvider for the agent's dashboard.
func (l *liveProvider) Progress() dashboard.Progress {
	if l.progress != nil {
		return l.progress()
	}
	if l.syncing != nil {
		return dashboard.Progress{Syncing: l.syncing()}
	}
	return dashboard.Progress{}
}
