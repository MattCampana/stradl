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
)

const trayAvailable = true

// statusRefresh is how often the menu's "Synced 2 min ago" line is redrawn
// when nothing else changes.
const statusRefresh = 15 * time.Second

// runTray runs the sync loop behind a menu bar icon until the user quits or
// the process gets SIGINT/SIGTERM. A clean quit exits 0, which launchd's
// KeepAlive (SuccessfulExit=false) treats as "stay stopped".
func runTray(opts agent.Options) error {
	logger := opts.Log
	app := agent.NewDesktopApp()
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
		mSync := systray.AddMenuItem("Sync Now", "Sync with your other Macs now")
		mPause := systray.AddMenuItem("Pause Syncing", "Stop syncing until you resume")
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
			mStatus.SetTitle(agent.StatusLine(s, time.Now()))
			if s.Paused {
				mPause.SetTitle("Resume Syncing")
			} else {
				mPause.SetTitle("Pause Syncing")
			}
			if line := agent.RestartLine(s); line != "" && !restarting.Load() {
				mRestart.SetTitle(line)
				mRestart.Show()
			} else {
				mRestart.Hide()
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
						rctx, rcancel := context.WithTimeout(ctx, 45*time.Second)
						defer rcancel()
						logger.Printf("restarting Claude to load new sessions")
						if err := app.Restart(rctx); err != nil {
							logger.Printf("restart Claude: %v", err)
							notify("Stradl", "Couldn't restart Claude: "+err.Error())
							return
						}
						// Let the new process appear before re-checking.
						select {
						case <-rctx.Done():
						case <-time.After(3 * time.Second):
						}
						r.RefreshDesktop(rctx)
					}()
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
