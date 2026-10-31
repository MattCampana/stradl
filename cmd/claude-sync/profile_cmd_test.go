package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tawanorg/claude-sync/internal/agent"
	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/profiles"
)

// An account opens in the profile signed in to it, even when a folder named
// after it exists (made earlier, still signed out).
func TestOpenFindsTheProfileSignedInToTheAccount(t *testing.T) {
	const judge = "53d94190-1990-4286-b69d-42cdf507ba7a"
	root := t.TempDir()
	cfg := &config.Config{
		StateDirOverride:    filepath.Join(root, ".stradl"),
		DesktopDirOverride:  filepath.Join(root, "Claude", "claude-code-sessions"),
		ProfilesDirOverride: filepath.Join(root, "Profiles"),
		ClaudeJSONOverride:  filepath.Join(root, ".claude.json"),
		ClaudeDirOverride:   filepath.Join(root, ".claude"),
		HomeDirOverride:     root,
	}
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(cfg.StateDirOverride, "accounts.json"), `{"accounts":{"`+judge+`":{"email":"mcampana@judge.com"}}}`)
	write(filepath.Join(root, "Claude", "config.json"), `{"lastKnownAccountUuid":"`+judge+`","oauth:tokenCache":"x"}`)
	if err := os.MkdirAll(filepath.Join(cfg.ProfilesDirOverride, judge), 0700); err != nil {
		t.Fatal(err)
	}
	p, name, err := resolveProfile(cfg, judge)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Main || name != "mcampana@judge.com" {
		t.Fatalf("opened %+v (%s), want the main profile, which is signed in to Judge", p, name)
	}
	if p, _, err := resolveProfile(cfg, "main"); err != nil || !p.Main {
		t.Fatalf("a profile ID should still resolve: %+v %v", p, err)
	}
}

// Deployed launchers follow a new color by themselves; nothing happens until
// they are deployed.
func TestLaunchersFollowColorsOnceDeployed(t *testing.T) {
	const judge = "53d94190-1990-4286-b69d-42cdf507ba7a"
	t.Cleanup(agent.SetIconRefresher(func(string) {}))
	root := t.TempDir()
	t.Setenv("HOME", root)
	cfg := &config.Config{
		StateDirOverride:    filepath.Join(root, ".stradl"),
		DesktopDirOverride:  filepath.Join(root, "Claude", "claude-code-sessions"),
		ProfilesDirOverride: filepath.Join(root, "Profiles"),
		ClaudeJSONOverride:  filepath.Join(root, ".claude.json"),
		ClaudeDirOverride:   filepath.Join(root, ".claude"),
		HomeDirOverride:     root,
	}
	if err := os.MkdirAll(cfg.StateDirOverride, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StateDirOverride, "accounts.json"), []byte(`{"accounts":{"`+judge+`":{"email":"mcampana@judge.com"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if launchersStale(cfg) {
		t.Fatal("nothing deployed yet: nothing to follow")
	}
	made, err := deployLaunchers(cfg)
	if err != nil || len(made) != 2 {
		t.Fatalf("deployed %v (%v)", made, err)
	}
	if launchersStale(cfg) {
		t.Fatal("just deployed: up to date")
	}
	if _, err := profiles.FromConfig(cfg).SetColor(judge, "amber"); err != nil {
		t.Fatal(err)
	}
	if !launchersStale(cfg) {
		t.Fatal("a new color should call for a redeploy")
	}
	if done, err := redeployLaunchers(cfg); err != nil || !done || launchersStale(cfg) {
		t.Fatalf("redeploy: %v %v", done, err)
	}
}
