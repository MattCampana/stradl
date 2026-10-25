package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/storage"
	"github.com/tawanorg/claude-sync/internal/sync"
)

// execute runs cmd with exactly args. (Execute with no SetArgs, or SetArgs
// with nil, would parse the test binary's own flags from os.Args.)
func execute(cmd *cobra.Command, args ...string) error {
	if args == nil {
		args = []string{}
	}
	cmd.SetArgs(args)
	return cmd.Execute()
}

// syncSpy replaces the push/pull seams for one test and records the calls.
type syncSpy struct {
	syncs    int
	mcpCalls []bool // push flag of each MCP call
	report   *sync.SyncReport
	err      error
}

func installSyncSpy(t *testing.T, mcpEnabled bool) *syncSpy {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A config whose storage and key don't exist: any legacy code path that
	// builds a real Syncer fails, so a passing command proves it only went
	// through the seams.
	cfg := &config.Config{
		Storage: &storage.StorageConfig{
			Provider:      storage.ProviderAzure,
			Bucket:        "stradl",
			AzureAccount:  "fakeaccount",
			AzureSASToken: testSAS,
		},
		EncryptionKey: home + "/.stradl/missing-key.txt",
	}
	if mcpEnabled {
		cfg.SetMCPSync(true)
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	spy := &syncSpy{report: &sync.SyncReport{}}
	oldSync, oldMCP, oldQuiet := runTwoWaySync, runMCPSync, quiet
	runTwoWaySync = func(ctx context.Context, c *config.Config) (*sync.SyncReport, error) {
		spy.syncs++
		if c.GetStorageConfig().AzureAccount != "fakeaccount" {
			t.Errorf("two-way sync got the wrong config: %+v", c.GetStorageConfig())
		}
		return spy.report, spy.err
	}
	runMCPSync = func(ctx context.Context, c *config.Config, push bool) error {
		spy.mcpCalls = append(spy.mcpCalls, push)
		return nil
	}
	quiet = true
	t.Cleanup(func() { runTwoWaySync, runMCPSync, quiet = oldSync, oldMCP, oldQuiet })
	return spy
}

func TestPushAndPullRunTwoWaySync(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  func() *cobra.Command
	}{
		{"push", pushCmd},
		{"pull", pullCmd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := installSyncSpy(t, false)
			if err := execute(tc.cmd()); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if spy.syncs != 1 {
				t.Errorf("%s ran the two-way sync %d times, want 1", tc.name, spy.syncs)
			}
			if len(spy.mcpCalls) != 0 {
				t.Errorf("%s ran MCP sync without --include-mcp", tc.name)
			}
		})
	}
}

func TestPushPullIncludeMCP(t *testing.T) {
	spy := installSyncSpy(t, false)

	if err := execute(pushCmd(), "--include-mcp"); err != nil {
		t.Fatalf("push --include-mcp: %v", err)
	}
	if err := execute(pullCmd(), "--include-mcp"); err != nil {
		t.Fatalf("pull --include-mcp: %v", err)
	}

	if spy.syncs != 2 {
		t.Errorf("two-way sync ran %d times, want 2", spy.syncs)
	}
	if len(spy.mcpCalls) != 2 || spy.mcpCalls[0] != true || spy.mcpCalls[1] != false {
		t.Errorf("MCP calls = %v, want [push=true, push=false]", spy.mcpCalls)
	}
}

func TestPushPullHonorMCPConfig(t *testing.T) {
	spy := installSyncSpy(t, true)
	if err := execute(pushCmd()); err != nil {
		t.Fatal(err)
	}
	if len(spy.mcpCalls) != 1 || !spy.mcpCalls[0] {
		t.Errorf("mcp_sync: true should push MCP configs; calls = %v", spy.mcpCalls)
	}
}

func TestPushPullReportFailures(t *testing.T) {
	spy := installSyncSpy(t, false)
	spy.report = &sync.SyncReport{Errors: []error{errors.New("boom"), errors.New("bang")}}
	if err := execute(pushCmd()); err == nil || !strings.Contains(err.Error(), "2 file(s) failed") {
		t.Errorf("push with per-file errors = %v", err)
	}

	spy.report = nil
	spy.err = sync.ErrLocked
	if err := execute(pullCmd()); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Errorf("pull while locked = %v", err)
	}
}

func TestPullDryRunDoesNotSync(t *testing.T) {
	spy := installSyncSpy(t, false)
	if err := execute(pullCmd(), "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if spy.syncs != 0 {
		t.Errorf("pull --dry-run ran a sync")
	}
}

func TestPullForceFlagStillAccepted(t *testing.T) {
	spy := installSyncSpy(t, false)
	if err := execute(pullCmd(), "--force"); err != nil {
		t.Fatalf("pull --force: %v", err)
	}
	if spy.syncs != 1 {
		t.Errorf("pull --force ran %d syncs, want 1", spy.syncs)
	}
}
