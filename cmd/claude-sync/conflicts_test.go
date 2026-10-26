package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/sync"
)

// captureStdout returns what fn printed to stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

func TestParseConflictPath(t *testing.T) {
	dir := filepath.Join("/h", ".claude", "plans")
	tests := []struct {
		name, ts, origin, label string
	}{
		{"a.md.conflict.20261025-140305.other-mac", "20261025-140305", sync.ConflictFromOtherMac, "from another Mac"},
		{"a.md.conflict.20261025-140305.this-mac", "20261025-140305", sync.ConflictFromThisMac, "from this Mac"},
		{"a.md.conflict.20260208-095132", "20260208-095132", "", "from an unknown device"},
	}
	for _, tt := range tests {
		c, ok := parseConflictPath(filepath.Join(dir, tt.name))
		if !ok {
			t.Fatalf("parseConflictPath(%q) not recognized", tt.name)
		}
		if c.OriginalPath != filepath.Join(dir, "a.md") || c.Timestamp != tt.ts {
			t.Errorf("parseConflictPath(%q) = %+v", tt.name, c)
		}
		if c.Origin != tt.origin || c.source() != tt.label {
			t.Errorf("%s: origin %q (%s), want %q (%s)", tt.name, c.Origin, c.source(), tt.origin, tt.label)
		}
	}
	if c, _ := parseConflictPath(filepath.Join(dir, "a.md.conflict.20261025-140305.other-mac")); c.savedAt() != "2026-10-25 14:03:05" {
		t.Errorf("savedAt() = %q", c.savedAt())
	}
	if _, ok := parseConflictPath(filepath.Join(dir, "notes.md")); ok {
		t.Error("an ordinary file parsed as a conflict copy")
	}
}

// conflictHome builds a temp HOME with three conflicts (one from each
// origin) and a sync state. It returns ~/.claude and the state's contents.
func conflictHome(t *testing.T) (string, []byte) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	plans := filepath.Join(home, ".claude", "plans")
	if err := os.MkdirAll(plans, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"a.md": "current A",
		"a.md.conflict.20261025-140305.other-mac": "other Mac's A",
		"b.md":                                   "current B",
		"b.md.conflict.20261025-140306.this-mac": "this Mac's B",
		"c.md":                                   "current C",
		"c.md.conflict.20260208-095132":          "legacy C",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(plans, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	state := []byte(`{"files":{"plans/a.md":{"path":"plans/a.md","hash":"abc","size":9,"mod_time":"2026-10-25T14:03:05Z","remote_etag":"0x1"}},"device_id":"test"}`)
	if err := os.MkdirAll(config.ConfigDirPath(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.StateFilePath(), state, 0600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".claude"), state
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", filepath.Base(path), err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", filepath.Base(path), got, want)
	}
}

func assertNoConflictCopies(t *testing.T, claudeDir string) {
	t.Helper()
	left, err := findConflicts(claudeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%d conflict copies left", len(left))
	}
}

// assertStateUntouched checks that resolving left state.json alone: the
// engine's recorded state is what makes the next sync upload a kept copy.
func assertStateUntouched(t *testing.T, want []byte) {
	t.Helper()
	got, err := os.ReadFile(config.StateFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("state.json changed:\n%s", got)
	}
}

func TestConflictsListLabelsOrigins(t *testing.T) {
	conflictHome(t)
	out := captureStdout(t, func() {
		if err := execute(conflictsCmd(), "--list"); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{
		"plans/a.md\n", ".conflict copy from another Mac, saved 2026-10-25 14:03:05",
		"plans/b.md\n", ".conflict copy from this Mac, saved 2026-10-25 14:03:06",
		"plans/c.md\n", ".conflict copy from an unknown device",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("list output lacks %q:\n%s", want, out)
		}
	}
}

func TestConflictsBatchLeavesStateUntouched(t *testing.T) {
	for _, keep := range []string{"copy", "remote"} {
		t.Run("--keep "+keep, func(t *testing.T) {
			claudeDir, state := conflictHome(t)
			out := captureStdout(t, func() {
				if err := execute(conflictsCmd(), "--keep", keep); err != nil {
					t.Fatal(err)
				}
			})
			plans := filepath.Join(claudeDir, "plans")
			assertFile(t, filepath.Join(plans, "a.md"), "other Mac's A")
			assertFile(t, filepath.Join(plans, "b.md"), "this Mac's B")
			assertFile(t, filepath.Join(plans, "c.md"), "legacy C")
			assertNoConflictCopies(t, claudeDir)
			assertStateUntouched(t, state)
			if !strings.Contains(out, "Kept the .conflict copy (from another Mac): a.md") ||
				!strings.Contains(out, "Kept the .conflict copy (from this Mac): b.md") {
				t.Errorf("output doesn't label origins:\n%s", out)
			}
		})
	}

	for _, keep := range []string{"current", "local"} {
		t.Run("--keep "+keep, func(t *testing.T) {
			claudeDir, state := conflictHome(t)
			captureStdout(t, func() {
				if err := execute(conflictsCmd(), "--keep", keep); err != nil {
					t.Fatal(err)
				}
			})
			plans := filepath.Join(claudeDir, "plans")
			assertFile(t, filepath.Join(plans, "a.md"), "current A")
			assertFile(t, filepath.Join(plans, "b.md"), "current B")
			assertNoConflictCopies(t, claudeDir)
			assertStateUntouched(t, state)
		})
	}

	t.Run("bad --keep", func(t *testing.T) {
		conflictHome(t)
		captureStdout(t, func() {
			if err := execute(conflictsCmd(), "--keep", "mine"); err == nil {
				t.Error("--keep mine accepted")
			}
		})
	})
}

func TestConflictsBatchKeepsNewestCopy(t *testing.T) {
	claudeDir, _ := conflictHome(t)
	plans := filepath.Join(claudeDir, "plans")
	older := filepath.Join(plans, "a.md.conflict.20261024-090000.other-mac")
	if err := os.WriteFile(older, []byte("older A"), 0600); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		if err := execute(conflictsCmd(), "--keep", "copy"); err != nil {
			t.Fatal(err)
		}
	})
	assertFile(t, filepath.Join(plans, "a.md"), "other Mac's A")
	assertNoConflictCopies(t, claudeDir)
}

func TestConflictsInteractiveLeavesStateUntouched(t *testing.T) {
	claudeDir, state := conflictHome(t)
	conflicts, err := findConflicts(claudeDir)
	if err != nil || len(conflicts) != 3 {
		t.Fatalf("findConflicts() = %d, %v", len(conflicts), err)
	}
	// Newest first: b (this Mac), a (another Mac), c (legacy). Keep b's
	// copy, a's current file, and skip c (old "r"/"l" keys still work).
	out := captureStdout(t, func() {
		if err := interactiveResolveConflicts(strings.NewReader("c\nl\ns\n"), conflicts, claudeDir); err != nil {
			t.Fatal(err)
		}
	})
	plans := filepath.Join(claudeDir, "plans")
	assertFile(t, filepath.Join(plans, "b.md"), "this Mac's B")
	assertFile(t, filepath.Join(plans, "a.md"), "current A")
	assertFile(t, filepath.Join(plans, "c.md.conflict.20260208-095132"), "legacy C")
	assertStateUntouched(t, state)
	for _, want := range []string{"from this Mac", "from another Mac", "from an unknown device", "Resolved 2 of 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	t.Run("end of input quits", func(t *testing.T) {
		claudeDir, state := conflictHome(t)
		conflicts, _ := findConflicts(claudeDir)
		captureStdout(t, func() {
			if err := interactiveResolveConflicts(strings.NewReader("r\n"), conflicts, claudeDir); err != nil {
				t.Fatal(err)
			}
		})
		assertFile(t, filepath.Join(claudeDir, "plans", "b.md"), "this Mac's B")
		assertStateUntouched(t, state)
	})
}
