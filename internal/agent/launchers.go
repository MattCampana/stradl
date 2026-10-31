package agent

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Launchers: small apps in ~/Applications that run a stradl command, so the
// Claude app can be opened signed in to an account from Launchpad, Spotlight
// or the Dock. Each is a bundle whose executable is a shell script; Stradl
// marks its own (StradlLauncher in Info.plist) and never touches any other.

// Launcher is one launcher app: its name, a stable ID for its bundle
// identifier, and the stradl arguments it runs. Its icon is the app icon
// with Badge on a band of Color (#RRGGBB); without them, the plain app icon.
type Launcher struct {
	Name  string
	ID    string
	Args  []string
	Badge string
	Color string
}

const launcherMark = "StradlLauncher"

var unsafeName = regexp.MustCompile(`[/:\x00-\x1f]`)

// WriteLaunchers writes ls into dir, running exe, with icon (an .icns file,
// "" for none), and removes Stradl's launchers that aren't in ls. An app of
// the same name Stradl didn't make is left alone. It returns the apps
// written.
func WriteLaunchers(dir, exe, icon string, ls []Launcher) ([]string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	keep := make(map[string]bool)
	var made []string
	for _, l := range ls {
		app := filepath.Join(dir, unsafeName.ReplaceAllString(l.Name, "-")+".app")
		if _, err := os.Stat(app); err == nil && !isLauncher(app) {
			continue // someone else's app
		}
		if err := writeLauncher(app, exe, icon, l); err != nil {
			return made, err
		}
		keep[app] = true
		made = append(made, app)
	}
	old, _ := filepath.Glob(filepath.Join(dir, "*.app"))
	for _, app := range old {
		if !keep[app] && isLauncher(app) {
			if err := os.RemoveAll(app); err != nil {
				return made, err
			}
		}
	}
	return made, nil
}

// RemoveLaunchers removes every launcher Stradl wrote into dir.
func RemoveLaunchers(dir string) (int, error) {
	apps, _ := filepath.Glob(filepath.Join(dir, "*.app"))
	n := 0
	for _, app := range apps {
		if isLauncher(app) {
			if err := os.RemoveAll(app); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

func isLauncher(app string) bool {
	data, err := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
	return err == nil && strings.Contains(string(data), "<key>"+launcherMark+"</key>")
}

func writeLauncher(app, exe, icon string, l Launcher) error {
	tmp := app + ".stradl-tmp"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "Contents", "MacOS"), 0755); err != nil {
		return err
	}
	iconKey := ""
	if err := os.MkdirAll(filepath.Join(tmp, "Contents", "Resources"), 0755); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "stradl-icon-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	if drawn := launcherIcon(work, l); drawn != "" {
		icon = drawn // the account's own icon; icon is the fallback
	}
	if icon != "" {
		if err := copyPlain(icon, filepath.Join(tmp, "Contents", "Resources", "AppIcon.icns")); err == nil {
			iconKey = "\t<key>CFBundleIconFile</key>\n\t<string>AppIcon</string>\n"
		}
	}
	id := "com.breakpointdata.stradl.launcher." + regexp.MustCompile(`[^A-Za-z0-9.-]`).ReplaceAllString(l.ID, "-")
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key>
	<string>launcher</string>
	<key>CFBundleIdentifier</key>
	<string>` + xmlEscape(id) + `</string>
	<key>CFBundleName</key>
	<string>` + xmlEscape(l.Name) + `</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>1.0</string>
` + iconKey + `	<key>LSUIElement</key>
	<true/>
	<key>` + launcherMark + `</key>
	<true/>
</dict>
</plist>
`
	if err := os.WriteFile(filepath.Join(tmp, "Contents", "Info.plist"), []byte(plist), 0644); err != nil {
		return err
	}
	script := "#!/bin/sh\n# Opens Claude through Stradl (stradl profile launchers).\nexec " + shellQuote(exe)
	for _, a := range l.Args {
		script += " " + shellQuote(a)
	}
	script += "\n"
	if err := os.WriteFile(filepath.Join(tmp, "Contents", "MacOS", "launcher"), []byte(script), 0755); err != nil {
		return err
	}
	if err := os.RemoveAll(app); err != nil {
		return err
	}
	if err := os.Rename(tmp, app); err != nil {
		return fmt.Errorf("failed to add %s: %w", filepath.Base(app), err)
	}
	// Finder and the Dock cache icons by bundle: re-register it so a new
	// color or name shows at once. Best effort.
	refreshIcon(app)
	return nil
}

// refreshIcon re-registers a launcher with Launch Services. Tests replace it.
var refreshIcon = func(app string) { _ = exec.Command(lsregister, "-f", app).Run() }

const lsregister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"

// LaunchersDeployed reports whether dir holds any launcher Stradl wrote.
func LaunchersDeployed(dir string) bool {
	apps, _ := filepath.Glob(filepath.Join(dir, "*.app"))
	for _, app := range apps {
		if isLauncher(app) {
			return true
		}
	}
	return false
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func copyPlain(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
