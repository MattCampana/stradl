package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
)

// TestRealTranscripts parses this Mac's own ~/.claude read-only and logs
// counts, never content. It needs no storage (the dry run fails fast without
// a storage config) and keeps Stradl's state in a temp folder.
//
//	STRADL_DASHBOARD_REAL=1 go test -run TestRealTranscripts -v ./internal/dashboard/
func TestRealTranscripts(t *testing.T) {
	if os.Getenv("STRADL_DASHBOARD_REAL") == "" {
		t.Skip("set STRADL_DASHBOARD_REAL=1 to parse this Mac's transcripts (read-only)")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	cfg := &config.Config{
		Scope:              config.ScopeSessions,
		ClaudeDirOverride:  filepath.Join(home, ".claude"),
		DesktopDirOverride: config.DesktopSessionsDir(),
		StateDirOverride:   state,
	}
	p := newLocalProvider(LocalOptions{
		LoadConfig: func() (*config.Config, error) { c := *cfg; return &c, nil },
		StatusPath: filepath.Join(state, "status.json"),
	})

	start := time.Now()
	ov, err := p.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first := time.Since(start)

	var turns, prompts, files, inSidebar, live, active int
	states := map[string]int{}
	surfaces := map[string]int{}
	for _, s := range ov.Sessions {
		turns += s.UserTurns + s.AssistantTurns
		prompts += s.UserTurns
		files += len(s.FilesTouched)
		states[s.SyncState]++
		surfaces[s.Surface]++
		if s.InSidebar {
			inSidebar++
		}
		if s.Live {
			live++
		}
		if s.StartedAt.IsZero() || s.LastActivityAt.Before(s.StartedAt) {
			t.Errorf("session %s has an impossible time span", s.ID)
		}
	}
	for _, d := range ov.Days {
		active += len(d.Sessions)
	}
	bad := 0
	for _, s := range p.scanner.cache {
		bad += s.summary.BadRows
	}
	t.Logf("first overview %s; sessions %d, projects %d, messages %d (prompts %d), files touched %d, in sidebar %d, live %d",
		first.Round(time.Millisecond), len(ov.Sessions), ov.Totals.Projects, turns, prompts, files, inSidebar, live)
	t.Logf("surfaces %v; sync states %v; session-days in the last week %d; malformed rows %d; scope files %d (%d bytes)",
		surfaces, states, active, bad, ov.Totals.Files, ov.Totals.Bytes)
	t.Logf("warnings: %d", len(ov.Warnings))

	// A second pass reuses every parsed transcript.
	p.invalidate(false)
	start = time.Now()
	if _, err := p.Overview(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Logf("cached overview %s", time.Since(start).Round(time.Millisecond))

	if len(ov.Sessions) > 0 {
		d, err := p.Session(context.Background(), ov.Sessions[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		kinds := map[string]int{}
		for _, e := range d.Timeline {
			kinds[e.Kind]++
		}
		t.Logf("newest session timeline: %d entries %v", len(d.Timeline), kinds)
	}
}
