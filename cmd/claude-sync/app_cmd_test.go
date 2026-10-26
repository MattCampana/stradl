package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tawanorg/claude-sync/internal/config"
)

func TestRedactSecrets(t *testing.T) {
	in := `azure_sas_token: "sv=2022-11-02&si=stradl&sr=c&sig=AbC%2Fdef%3D"
url https://x.blob.core.windows.net/c?sv=2022&sr=c&sig=ZZZ&se=1
azure_account_key: base64key==
DefaultEndpointsProtocol=https;AccountKey=abc123;EndpointSuffix=core`
	out := redactSecrets(in)
	for _, secret := range []string{"AbC%2Fdef%3D", "ZZZ", "base64key==", "abc123"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q survived:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "REDACTED") {
		t.Fatal("nothing redacted")
	}
}

func TestConfigSetAndListEdits(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := &config.Config{Scope: config.ScopeEverything, EncryptionKey: filepath.Join(home, ".stradl", "age-key.txt")}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) error {
		c := configCmd()
		c.SetArgs(args)
		c.SetOut(os.Stdout)
		return c.Execute()
	}
	for _, args := range [][]string{
		{"set", "folder_sync", "off"},
		{"set", "sync_interval_seconds", "120"},
		{"add", "extra_folders", filepath.Join(home, "work", "site")},
		{"add", "folder_exclude", "*.mov"},
		{"set", "device_name", "Studio Mac"},
	} {
		if err := run(args...); err != nil {
			t.Fatalf("config %v: %v", args, err)
		}
	}
	got, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	c := describeConfig(got)
	if c.FolderSync || c.SyncIntervalSeconds != 120 || c.DeviceName != "Studio Mac" {
		t.Fatalf("settings not saved: %+v", c)
	}
	if len(got.ExtraFolders) != 1 || !strings.HasSuffix(got.ExtraFolders[0], filepath.Join("work", "site")) || got.FolderExclude[0] != "*.mov" {
		t.Fatalf("lists not saved: %v %v", got.ExtraFolders, got.FolderExclude)
	}
	if err := run("remove", "folder_exclude", "*.mov"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{{"set", "scope", "bogus"}, {"set", "sync_interval_seconds", "5"}, {"set", "nope", "1"}, {"add", "scope", "x"}} {
		if run(bad...) == nil {
			t.Errorf("config %v should fail", bad)
		}
	}
}
