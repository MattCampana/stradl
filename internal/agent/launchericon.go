package agent

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// launcherBase is the app icon (1024 px), the ground every launcher's icon
// is drawn on. assets/brand/unison/render.sh writes it.
//
//go:embed launcher-base.png
var launcherBase []byte

// BadgeIcon draws a launcher icon: base (a 1024 px PNG) with label on a band
// of color (#RRGGBB), as a PNG. Nil where it can't be drawn (it needs macOS).
var BadgeIcon func(base []byte, label, color string) ([]byte, error)

// launcherIcon returns an .icns file in dir for a launcher: the app icon with
// l's name on a band of its color, or the plain app icon when it has none.
// "" when it can't be made here (the launcher then takes fallback).
func launcherIcon(dir string, l Launcher) string {
	png := launcherBase
	if l.Badge != "" && l.Color != "" && BadgeIcon != nil {
		drawn, err := BadgeIcon(launcherBase, l.Badge, l.Color)
		if err != nil {
			return ""
		}
		png = drawn
	}
	icns, err := makeICNS(dir, png)
	if err != nil {
		return ""
	}
	return icns
}

// makeICNS turns a 1024 px PNG into an .icns in dir (sips sizes it, iconutil
// packs it, as scripts/build-app.sh does).
func makeICNS(dir string, png []byte) (string, error) {
	if _, err := os.Stat("/usr/bin/iconutil"); err != nil {
		return "", err
	}
	set := filepath.Join(dir, "AppIcon.iconset")
	if err := os.MkdirAll(set, 0755); err != nil {
		return "", err
	}
	master := filepath.Join(dir, "master.png")
	if err := os.WriteFile(master, png, 0644); err != nil {
		return "", err
	}
	for _, s := range []struct {
		name string
		px   int
	}{
		{"icon_16x16", 16}, {"icon_16x16@2x", 32}, {"icon_32x32", 32}, {"icon_32x32@2x", 64},
		{"icon_128x128", 128}, {"icon_128x128@2x", 256}, {"icon_256x256", 256}, {"icon_256x256@2x", 512},
		{"icon_512x512", 512}, {"icon_512x512@2x", 1024},
	} {
		px := fmt.Sprint(s.px)
		if out, err := exec.Command("/usr/bin/sips", "-z", px, px, master, "--out", filepath.Join(set, s.name+".png")).CombinedOutput(); err != nil {
			return "", fmt.Errorf("sips: %v: %s", err, out)
		}
	}
	icns := filepath.Join(dir, "AppIcon.icns")
	if out, err := exec.Command("/usr/bin/iconutil", "-c", "icns", set, "-o", icns).CombinedOutput(); err != nil {
		return "", fmt.Errorf("iconutil: %v: %s", err, out)
	}
	return icns, nil
}
