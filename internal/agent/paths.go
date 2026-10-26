// Package agent runs Stradl in the background: a long-lived loop that syncs
// when either side changes, the launchd job that keeps it running, and the
// status file the menu bar and `stradl agent status` read.
package agent

import (
	"path/filepath"

	"github.com/tawanorg/claude-sync/internal/config"
)

// Label is the launchd job label.
const Label = "com.breakpointdata.stradl"

// Paths locates the agent's files for one home directory.
type Paths struct {
	Home      string
	StateDir  string // ~/.stradl
	Log       string // agent log (also launchd's stdout/stderr)
	Lock      string // single-instance guard
	Pause     string // present while syncing is paused, by any process
	Status    string // last-sync status, written after every sync
	Dashboard string // the running agent's dashboard URL (dashboard.json)
	Plist     string // ~/Library/LaunchAgents/<Label>.plist
}

// PathsFor returns the agent paths under home.
func PathsFor(home string) Paths {
	state := filepath.Join(home, config.ConfigDir)
	return Paths{
		Home:      home,
		StateDir:  state,
		Log:       filepath.Join(state, "agent.log"),
		Lock:      filepath.Join(state, "agent.lock"),
		Pause:     filepath.Join(state, "paused"),
		Status:    filepath.Join(state, config.StatusFile),
		Dashboard: filepath.Join(state, "dashboard.json"),
		Plist:     filepath.Join(home, "Library", "LaunchAgents", Label+".plist"),
	}
}
