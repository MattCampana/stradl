//go:build darwin && cgo

package main

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	gosync "sync"
	"sync/atomic"
	"syscall"
	"time"

	"fyne.io/systray"

	"github.com/tawanorg/claude-sync/internal/agent"
	"github.com/tawanorg/claude-sync/internal/agent/icon"
	"github.com/tawanorg/claude-sync/internal/sync"
)

// maxMenuAccounts is how many Claude accounts the menu lists.
const maxMenuAccounts = 8

const trayAvailable = true

// statusRefresh is how often the menu's "Synced 2 min ago" line is redrawn
// when nothing else changes.
const statusRefresh = 15 * time.Second

// runTray runs the sync loop behind a menu bar icon until the user quits or
// the process gets SIGINT/SIGTERM. A clean quit exits 0, which launchd's
// KeepAlive (SuccessfulExit=false) treats as "stay stopped".
func runTray(opts agent.Options) error {
	logger := opts.Log
	go ensureAppInstalled(logger)
	app := agent.NewDesktopApp()
	// Restart Claude applies sidebar changes from other Macs in the moment
	// between the old app quitting and the new one opening.
	app.BeforeLaunch = func(ctx context.Context) { applyPendingSidebar(ctx, logger) }
	refresh := make(chan struct{}, 1)
	poke := func() {
		select {
		case refresh <- struct{}{}:
		default:
		}
	}
	notify := func(title, body string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := agent.Notify(ctx, agent.ExecCommand, title, body); err != nil {
			logger.Printf("notification failed: %v", err)
		}
	}
	opts.OnChange = poke
	opts.Notify = notify
	opts.AppStart = app.StartTime
	r := agent.NewRunner(opts)
	p := opts.Paths
	dashURL, stopDashboard := startAgentDashboard(p, r, logger)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var started atomic.Bool
	var stopOnce gosync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			if started.Load() {
				select {
				case <-done:
				case <-time.After(10 * time.Second):
				}
			}
			stopDashboard()
		})
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		systray.Quit()
	}()

	onReady := func() {
		systray.SetTemplateIcon(icon.MenuBar2x, icon.MenuBar2x)
		systray.SetTooltip("Stradl")

		mStatus := systray.AddMenuItem("Starting…", "")
		mStatus.Disable()
		mAttempt := systray.AddMenuItem("", "") // after a failed sync only
		mAttempt.Disable()
		mAttempt.Hide()
		// The Mac app: shown while it's installed (checked on every redraw,
		// so an app installed later appears without restarting the agent).
		mApp := systray.AddMenuItem("Open Stradl…", "Your Circle, sessions, folders, settings and logs")
		mApp.Hide()
		appClicked := mApp.ClickedCh
		// No dashboard (its server failed to start, which the log says):
		// no menu item, and a nil channel that never fires.
		var dashClicked <-chan struct{}
		if dashURL != "" {
			mDash := systray.AddMenuItem("Open Dashboard…", "See your Macs, this week's sessions and what is synced")
			dashClicked = mDash.ClickedCh
		}
		mSync := systray.AddMenuItem("Sync Now", "Sync with your other Macs now")
		mPause := systray.AddMenuItem("Pause Syncing", "Stop syncing until you resume")
		// Claude accounts, checked when their sessions sync (hidden until
		// the agent has seen one).
		mAccounts := systray.AddMenuItem("Claude Accounts", "Choose which accounts' sessions sync to your other Macs")
		mAccounts.Hide()
		var accountItems []*systray.MenuItem
		var shownAccounts [maxMenuAccounts]sync.Account
		var accountsMu gosync.Mutex
		accountClicked := make(chan int, 1)
		for i := 0; i < maxMenuAccounts; i++ {
			item := mAccounts.AddSubMenuItemCheckbox("", "", false)
			item.Hide()
			accountItems = append(accountItems, item)
			go func(i int, ch <-chan struct{}) {
				for {
					select {
					case <-ctx.Done():
						return
					case <-ch:
						select {
						case accountClicked <- i:
						default:
						}
					}
				}
			}(i, item.ClickedCh)
		}
		systray.AddSeparator()
		mRestart := systray.AddMenuItem("Restart Claude", "Quit and reopen Claude so its sidebar shows sessions from your other Macs")
		mRestart.Hide()
		mLog := systray.AddMenuItem("Open Log", "")
		mFolder := systray.AddMenuItem("Open Stradl Folder", "")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit Stradl", "Stop syncing until you log in again")

		started.Store(true)
		go func() {
			_ = r.Run(ctx)
			close(done)
		}()

		var restarting atomic.Bool
		render := func() {
			s := r.Snapshot()
			s.Paused = agent.IsPaused(p) // the flag is the truth, even mid-sync
			now := time.Now()
			mStatus.SetTitle(agent.StatusLine(s, now))
			if line := agent.AttemptLine(s, now); line != "" {
				mAttempt.SetTitle(line)
				mAttempt.Show()
			} else {
				mAttempt.Hide()
			}
			if s.Paused {
				mPause.SetTitle("Resume Syncing")
			} else {
				mPause.SetTitle("Pause Syncing")
			}
			if _, ok := appPath(); ok {
				mApp.Show()
			} else {
				mApp.Hide()
			}
			if line := agent.RestartLine(s); line != "" && !restarting.Load() {
				mRestart.SetTitle(line)
				mRestart.Show()
			} else {
				mRestart.Hide()
			}
			if cfg, err := opts.LoadConfig(); err == nil {
				list := sync.ListAccounts(cfg)
				accountsMu.Lock()
				for i, item := range accountItems {
					if i >= len(list) {
						item.Hide()
						continue
					}
					a := list[i]
					shownAccounts[i] = a
					title := a.Name
					if a.Active {
						title += " (signed in)"
					}
					item.SetTitle(title)
					if a.Sync == "off" {
						item.Uncheck()
						item.SetTooltip("Its sessions stay on the Mac they were made on. Click to sync them.")
					} else {
						item.Check()
						item.SetTooltip("Its sessions sync to your other Macs. Click to keep them on each Mac.")
					}
					item.Show()
				}
				accountsMu.Unlock()
				if len(list) > 0 {
					mAccounts.Show()
				} else {
					mAccounts.Hide()
				}
			}
		}
		render()

		go func() {
			ticker := time.NewTicker(statusRefresh)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					render()
				case <-refresh:
					render()
				case <-systray.TrayOpenedCh:
					render()
				case <-mSync.ClickedCh:
					r.SyncNow()
				case <-mPause.ClickedCh:
					var err error
					if agent.IsPaused(p) {
						err = r.Resume()
					} else {
						err = r.Pause()
					}
					if err != nil {
						logger.Printf("pause/resume failed: %v", err)
					}
					render()
				case <-mRestart.ClickedCh:
					if !restarting.CompareAndSwap(false, true) {
						continue
					}
					render()
					go func() {
						defer func() {
							restarting.Store(false)
							poke()
						}()
						// Room for the question asked first when sessions are
						// still working (up to two minutes), then the restart.
						rctx, rcancel := context.WithTimeout(ctx, 3*time.Minute)
						defer rcancel()
						logger.Printf("restarting Claude to load new sessions")
						restarted, err := app.RestartIfConfirmed(rctx)
						if err != nil {
							logger.Printf("restart Claude: %v", err)
							notify("Stradl", "Couldn't restart Claude: "+err.Error())
							return
						}
						if !restarted {
							logger.Printf("restart cancelled: Claude sessions are still working")
							return
						}
						// Let the new process appear before re-checking.
						select {
						case <-rctx.Done():
						case <-time.After(3 * time.Second):
						}
						r.RefreshDesktop(rctx)
					}()
				case i := <-accountClicked:
					accountsMu.Lock()
					a := shownAccounts[i]
					accountsMu.Unlock()
					if a.ID == "" {
						continue
					}
					go func() {
						cfg, err := opts.LoadConfig()
						if err != nil {
							logger.Printf("account choice: %v", err)
							return
						}
						s, err := sync.NewSyncer(cfg, true)
						if err != nil {
							logger.Printf("account choice: %v", err)
							return
						}
						actx, acancel := context.WithTimeout(ctx, time.Minute)
						defer acancel()
						on := a.Sync == "off"
						if err := s.SetAccountSync(actx, a.ID, on, time.Now()); err != nil {
							logger.Printf("account choice for %s: %v", a.Name, err)
							notify("Stradl", "Couldn't change "+a.Name+": "+err.Error())
							return
						}
						if on {
							logger.Printf("account %s syncs now", a.Name)
						} else {
							logger.Printf("account %s stays on each Mac now", a.Name)
						}
						poke()
						r.SyncNow()
					}()
				case <-appClicked:
					if p, ok := appPath(); ok {
						openPath(logger, p)
					}
				case <-dashClicked:
					// Not openPath: its error line would put the key in the log.
					if err := openBrowser(dashURL); err != nil {
						logger.Printf("open dashboard: %v", err)
					}
				case <-mLog.ClickedCh:
					openPath(logger, p.Log)
				case <-mFolder.ClickedCh:
					openPath(logger, p.StateDir)
				case <-mQuit.ClickedCh:
					logger.Printf("quit from the menu bar")
					systray.Quit()
					return
				}
			}
		}()
	}

	systray.Run(onReady, stop)
	stop()
	return nil
}

func openPath(logger *agent.Logger, path string) {
	if err := exec.Command("/usr/bin/open", path).Start(); err != nil {
		logger.Printf("open %s: %v", path, err)
	}
}
