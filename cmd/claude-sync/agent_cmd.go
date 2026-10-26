package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tawanorg/claude-sync/internal/agent"
	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/sync"
)

func init() {
	extraCommands = append(extraCommands, agentCmd)
}

// launchctl is swapped out in tests so they never touch the real launchd.
var launchctl agent.Launchctl = agent.SystemLaunchctl{}

func agentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Keep syncing in the background (menu bar app, starts at login)",
		Long: `The agent syncs on its own: when a session changes on this Mac, when another
Mac uploads something, after waking from sleep, and every few minutes.

  stradl agent install    start it now and at every login (macOS)
  stradl agent status     show what it is doing
  stradl agent pause      stop syncing until 'stradl agent resume'`,
	}
	cmd.AddCommand(
		agentRunCmd(),
		agentInstallCmd(),
		agentUninstallCmd(),
		agentStatusCmd(),
		agentRestartCmd(),
		agentPauseCmd(true),
		agentPauseCmd(false),
	)
	return cmd
}

func agentPaths() (agent.Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return agent.Paths{}, config.ErrNoHomeDir
	}
	return agent.PathsFor(home), nil
}

func agentRunCmd() *cobra.Command {
	var headless bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the agent in the foreground (launchd runs this)",
		Long: `Run the sync agent until it is stopped. On macOS it shows the Stradl menu bar
icon unless --headless is given. Only one agent runs per user; a second one
waits for the first to stop and then takes over.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := agentPaths()
			if err != nil {
				return err
			}
			sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			release, err := acquireAgentLock(sigCtx, p, os.Stderr, isTerminal(os.Stderr))
			stopped := sigCtx.Err() != nil
			stopSignals()
			if err != nil {
				if stopped {
					return nil // Ctrl-C or SIGTERM while waiting
				}
				return err
			}
			defer release()
			if stopped {
				return nil
			}

			logFile, err := agent.OpenLogFile(p.Log, agent.MaxLogSize)
			if err != nil {
				return fmt.Errorf("failed to open log: %w", err)
			}
			defer func() { _ = logFile.Close() }()
			var w io.Writer = logFile
			if isTerminal(os.Stdout) {
				w = io.MultiWriter(logFile, os.Stdout)
			}

			opts := agent.Options{
				Paths:   p,
				Version: version,
				Device:  deviceName(),
				Log:     agent.NewLogger(w),
			}
			if runtime.GOOS == "darwin" {
				opts.AppStart = agent.NewDesktopApp().StartTime
			}
			if trayAvailable && !headless {
				return runTray(opts)
			}
			return runHeadless(opts)
		},
	}
	cmd.Flags().BoolVar(&headless, "headless", false, "Run without the menu bar icon")
	return cmd
}

// agentLockPoll is how often a second agent checks whether the first one
// has stopped. Tests shorten it.
var agentLockPoll = agent.LockPoll

// acquireAgentLock takes the single-instance agent lock. While another
// agent holds it, it says so once on stderr (a timestamped line, since under
// launchd stderr is the agent log) and waits to take over. Exiting instead
// would end a launchd-started agent for good: KeepAlive restarts only after
// a failure, so nothing would run once the other agent went away. It returns
// ctx's error if ctx ends while waiting.
func acquireAgentLock(ctx context.Context, p agent.Paths, stderr io.Writer, tty bool) (func(), error) {
	try := func() (func(), error) { return sync.AcquireLock(p.Lock, false, 0) }
	return agent.WaitForLock(ctx, try, agentLockPoll, func() {
		who := "another Stradl agent is running"
		if pid := agent.LockHolder(p.Lock); pid != "" {
			who += " (pid " + pid + ")"
		}
		if tty {
			fmt.Fprintf(stderr, "%s%s. Waiting to take over when it stops; press Ctrl-C to exit.\n",
				strings.ToUpper(who[:1]), who[1:])
			return
		}
		agent.NewLogger(stderr).Printf("%s; waiting to take over when it stops", who)
	})
}

// runHeadless runs the sync loop, and the dashboard, until SIGINT or
// SIGTERM.
func runHeadless(opts agent.Options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := agent.NewRunner(opts)
	_, stopDashboard := startAgentDashboard(opts.Paths, r, opts.Log)
	defer stopDashboard()
	return r.Run(ctx)
}

func newLaunchd(p agent.Paths, exe string) *agent.Launchd {
	return &agent.Launchd{Paths: p, UID: os.Getuid(), Executable: exe, Ctl: launchctl}
}

func requireMacOS(what string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("%s uses launchd and is only available on macOS; elsewhere, run 'stradl agent run --headless' from your init system", what)
	}
	return nil
}

func agentInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Start the agent now and at every login",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireMacOS("agent install"); err != nil {
				return err
			}
			p, err := agentPaths()
			if err != nil {
				return err
			}
			exe, err := currentExecutable()
			if err != nil {
				return err
			}
			if looksTemporary(exe) {
				return fmt.Errorf("this stradl binary is in a temporary location (%s), probably from 'go run'; install a built stradl first so launchd has a stable path to start", exe)
			}
			if err := newLaunchd(p, exe).Install(); err != nil {
				return err
			}
			if quiet {
				return nil
			}
			printSuccess("✓ Stradl agent installed: it is running now and starts at every login.")
			printInfo("Binary: " + exe)
			printInfo("Job:    " + p.Plist)
			printInfo("Log:    " + p.Log)
			if !config.Exists() {
				printWarning("Not set up yet: run 'stradl init'. The agent starts syncing as soon as you do.")
			}
			return nil
		},
	}
}

func agentUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the agent and remove it from login items",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireMacOS("agent uninstall"); err != nil {
				return err
			}
			p, err := agentPaths()
			if err != nil {
				return err
			}
			if err := newLaunchd(p, "").Uninstall(); err != nil {
				return err
			}
			if !quiet {
				printSuccess("✓ Stradl agent stopped and removed. Your synced sessions and settings are untouched.")
			}
			return nil
		},
	}
}

func agentRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the background agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireMacOS("agent restart"); err != nil {
				return err
			}
			p, err := agentPaths()
			if err != nil {
				return err
			}
			l := newLaunchd(p, "")
			if !l.State().Loaded {
				return fmt.Errorf("the agent is not installed; run 'stradl agent install'")
			}
			if err := l.Restart(); err != nil {
				return err
			}
			if !quiet {
				printSuccess("✓ Stradl agent restarted.")
			}
			return nil
		},
	}
}

func agentPauseCmd(pause bool) *cobra.Command {
	use, short := "resume", "Resume background syncing"
	if pause {
		use, short = "pause", "Pause background syncing (manual 'stradl sync' still works)"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := agentPaths()
			if err != nil {
				return err
			}
			if err := agent.SetPaused(p, pause); err != nil {
				return err
			}
			if quiet {
				return nil
			}
			if pause {
				printSuccess("✓ Syncing paused. Run 'stradl agent resume' to continue.")
			} else {
				printSuccess("✓ Syncing resumed. The agent syncs within the next 20 seconds.")
			}
			return nil
		},
	}
}

func agentStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the agent is running and how the last sync went",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := agentPaths()
			if err != nil {
				return err
			}
			st, readErr := agent.ReadStatus(p.Status)
			if readErr != nil && !agent.IsNotExist(readErr) {
				return readErr
			}
			if asJSON {
				data, err := json.MarshalIndent(st, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}
			var job *agent.JobState
			if runtime.GOOS == "darwin" {
				s := newLaunchd(p, "").State()
				job = &s
			}
			printAgentStatus(os.Stdout, p, job, st, agent.IsPaused(p), readErr == nil, time.Now())
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print the raw status file")
	return cmd
}

// printAgentStatus renders `stradl agent status`. job is nil where launchd
// doesn't exist.
func printAgentStatus(w io.Writer, p agent.Paths, job *agent.JobState, st *agent.Status, paused, haveStatus bool, now time.Time) {
	row := func(label, value string) {
		fmt.Fprintf(w, "  %s%-12s%s %s\n", colorDim, label, colorReset, value)
	}
	fmt.Fprintf(w, "%sStradl agent%s\n", colorBold, colorReset)

	if job != nil {
		switch {
		case job.PID > 0:
			row("Service", fmt.Sprintf("%srunning%s (pid %d)", colorGreen, colorReset, job.PID))
		case job.Loaded:
			detail := ""
			if job.LastExit != "" {
				detail = " (last exit: " + job.LastExit + ")"
			}
			row("Service", colorYellow+"loaded, not running"+colorReset+detail+"; 'stradl agent restart' starts it")
		case job.Installed:
			row("Service", colorYellow+"installed, not loaded"+colorReset+"; run 'stradl agent install'")
		default:
			row("Service", "not installed; run 'stradl agent install' to sync in the background")
		}
	}
	if paused {
		row("Syncing", colorYellow+"paused"+colorReset+"; run 'stradl agent resume'")
	} else {
		row("Syncing", "on")
	}

	// Last sync is the last clean one; a failure since shows as an attempt.
	synced := haveStatus && !st.LastSyncAt.IsZero()
	if !synced {
		row("Last sync", "never")
	} else {
		took := time.Duration(st.LastDurationMs) * time.Millisecond
		row("Last sync", fmt.Sprintf("%s (took %s)", agent.Ago(now.Sub(st.LastSyncAt)), took.Round(100*time.Millisecond)))
	}
	if haveStatus && st.AttemptFailed() {
		row("Last attempt", fmt.Sprintf("%s %s(failed: %s)%s", agent.Ago(now.Sub(st.LastAttemptAt)),
			colorYellow, st.ErrorSummary(), colorReset))
	}
	if synced {
		row("Result", fmt.Sprintf("↑ %d uploaded  ↓ %d downloaded  ⇄ %d merged  … %d waiting  ! %d conflicts",
			st.Uploaded, st.Downloaded, st.Merged, st.Deferred, st.Conflicts))
	}
	if synced || (haveStatus && !st.LastAttemptAt.IsZero()) {
		row("Sessions", agent.Plural(st.SessionsTotal, "session")+" on this Mac")
	}
	if n := st.PendingDesktopRestart; n > 0 {
		row("Claude app", colorYellow+agent.Plural(n, "new session")+colorReset+" will show in the sidebar after Claude restarts")
	}
	if st.LastError != "" {
		row("Error", colorYellow+st.LastError+colorReset)
	}
	if st.AgentVersion != "" {
		row("Version", st.AgentVersion)
	}
	if st.Device != "" {
		row("Device", st.Device)
	}
	row("Log", p.Log)
}

func currentExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to locate the stradl binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Abs(exe)
}

// looksTemporary reports whether exe lives somewhere that disappears, such
// as a `go run` build directory.
func looksTemporary(exe string) bool {
	if strings.Contains(exe, string(filepath.Separator)+"go-build") {
		return true
	}
	for _, dir := range []string{os.TempDir(), "/tmp", "/private/tmp"} {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		if dir != "" && strings.HasPrefix(exe, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func deviceName() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(h, ".local")
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
