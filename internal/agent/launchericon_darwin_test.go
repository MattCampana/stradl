//go:build darwin && cgo

package agent

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestALauncherIconShowsItsAccountsColor(t *testing.T) {
	out, err := BadgeIcon(launcherBase, "Judge", "#1EC8FF")
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 1024 || b.Dy() != 1024 {
		t.Fatalf("size %v", b)
	}
	// Left of the text, on the band: the account's color.
	r, g, b, a := img.At(200, 1024-264).RGBA()
	if a>>8 != 255 || r>>8 > 0x60 || g>>8 < 0xB0 || b>>8 < 0xE0 {
		t.Fatalf("band pixel = %02x%02x%02x/%02x, want about #1EC8FF", r>>8, g>>8, b>>8, a>>8)
	}
	// Outside the icon body: transparent.
	if _, _, _, a := img.At(4, 4).RGBA(); a != 0 {
		t.Fatal("the corner should stay transparent")
	}
	if _, err := BadgeIcon(launcherBase, "x", "blue"); err == nil {
		t.Fatal("a color that isn't #RRGGBB was drawn")
	}
}

func TestEachLauncherGetsItsOwnIcon(t *testing.T) {
	old := refreshIcon
	refreshIcon = func(string) {}
	t.Cleanup(func() { refreshIcon = old })
	dir := t.TempDir()
	made, err := WriteLaunchers(dir, "/bin/stradl", "", []Launcher{
		{Name: "Claude (Judge)", ID: "j", Badge: "Judge", Color: "#1EC8FF"},
		{Name: "Claude (Breakpoint)", ID: "b", Badge: "Breakpoint", Color: "#FF3D9A"},
	})
	if err != nil || len(made) != 2 {
		t.Fatalf("made %v (%v)", made, err)
	}
	a, _ := os.ReadFile(filepath.Join(made[0], "Contents", "Resources", "AppIcon.icns"))
	b, _ := os.ReadFile(filepath.Join(made[1], "Contents", "Resources", "AppIcon.icns"))
	if len(a) == 0 || len(b) == 0 || bytes.Equal(a, b) {
		t.Fatal("each launcher should have an icon of its own")
	}
	plist, _ := os.ReadFile(filepath.Join(made[0], "Contents", "Info.plist"))
	if !strings.Contains(string(plist), "<key>CFBundleIconFile</key>") {
		t.Fatal("the launcher doesn't name its icon")
	}
	if !LaunchersDeployed(dir) {
		t.Fatal("LaunchersDeployed should see them")
	}
}
