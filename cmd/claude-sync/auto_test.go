package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const legacyHooksJSON = `{
  "model": "opus",
  "hooks": {
    "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "claude-sync pull -q"}]}],
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "claude-sync push -q"}, {"type": "command", "command": "say done"}]}]
  }
}`

func writeSettings(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude", "settings.json")
	if body == "" {
		return path
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAutoEnableInstallsNoHooks(t *testing.T) {
	t.Run("no settings file", func(t *testing.T) {
		path := writeSettings(t, "")
		if err := execute(autoEnableCmd()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("auto enable created %s", path)
		}
	})

	t.Run("settings without hooks stay untouched", func(t *testing.T) {
		body := `{"model": "opus"}`
		path := writeSettings(t, body)
		if err := execute(autoEnableCmd()); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != body {
			t.Errorf("auto enable modified settings.json: %s", got)
		}
	})
}

func TestAutoDisableRemovesLegacyHooks(t *testing.T) {
	path := writeSettings(t, legacyHooksJSON)

	if err := execute(autoDisableCmd()); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.Contains(s, "claude-sync") {
		t.Errorf("legacy hooks still present: %s", s)
	}
	if !strings.Contains(s, "say done") || !strings.Contains(s, `"model"`) {
		t.Errorf("unrelated settings were lost: %s", s)
	}

	status, err := legacyHookStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Enabled {
		t.Error("legacyHookStatus() still reports hooks after disable")
	}
}

func TestLegacyHookStatus(t *testing.T) {
	writeSettings(t, legacyHooksJSON)
	status, err := legacyHookStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || !status.HasSessionStart || !status.HasStop {
		t.Errorf("legacyHookStatus() = %+v, want both hooks", status)
	}
	if err := execute(autoStatusCmd()); err != nil {
		t.Errorf("auto status: %v", err)
	}
}
