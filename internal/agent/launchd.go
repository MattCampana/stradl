package agent

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// Launchctl runs launchctl. Tests substitute a fake so they never touch the
// real launchd.
type Launchctl interface {
	Run(args ...string) (string, error)
}

// SystemLaunchctl runs /bin/launchctl.
type SystemLaunchctl struct{}

// Run executes launchctl with args and returns its combined output.
func (SystemLaunchctl) Run(args ...string) (string, error) {
	out, err := exec.Command("/bin/launchctl", args...).CombinedOutput()
	return string(out), err
}

// Launchd installs and controls the agent as a per-user launchd job.
type Launchd struct {
	Paths      Paths
	UID        int
	Executable string // absolute path to the stradl binary
	Ctl        Launchctl
	// Sleep waits between bootstrap retries; defaults to time.Sleep.
	Sleep func(time.Duration)
}

// Domain is the per-user GUI launchd domain, gui/<uid>.
func (l *Launchd) Domain() string { return fmt.Sprintf("gui/%d", l.UID) }

// Target is the job's service target, gui/<uid>/<Label>.
func (l *Launchd) Target() string { return l.Domain() + "/" + Label }

var plistTmpl = template.Must(template.New("plist").Funcs(template.FuncMap{"x": xmlEscape}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{x .Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{x .Executable}}</string>
		<string>agent</string>
		<string>run</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>LimitLoadToSessionType</key>
	<string>Aqua</string>
	<key>StandardOutPath</key>
	<string>{{x .Log}}</string>
	<key>StandardErrorPath</key>
	<string>{{x .Log}}</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>{{x .Home}}</string>
	</dict>
</dict>
</plist>
`))

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// RenderPlist returns the job definition. KeepAlive restarts the agent after
// a crash but not after a clean exit, so "Quit Stradl" stays quit until the
// next login.
func (l *Launchd) RenderPlist() ([]byte, error) {
	var b bytes.Buffer
	err := plistTmpl.Execute(&b, map[string]string{
		"Label":      Label,
		"Executable": l.Executable,
		"Log":        l.Paths.Log,
		"Home":       l.Paths.Home,
	})
	return b.Bytes(), err
}

// Install writes the plist and (re)loads the job, starting the agent now
// and at every login.
func (l *Launchd) Install() error {
	if !filepath.IsAbs(l.Executable) {
		return fmt.Errorf("executable path must be absolute: %s", l.Executable)
	}
	data, err := l.RenderPlist()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.Paths.Plist), 0755); err != nil {
		return fmt.Errorf("failed to create %s: %w", filepath.Dir(l.Paths.Plist), err)
	}
	if err := os.MkdirAll(l.Paths.StateDir, 0700); err != nil {
		return err
	}
	if err := writeFileAtomic(l.Paths.Plist, data); err != nil {
		return fmt.Errorf("failed to write %s: %w", l.Paths.Plist, err)
	}
	if err := os.Chmod(l.Paths.Plist, 0644); err != nil {
		return err
	}

	_, _ = l.Ctl.Run("bootout", l.Target()) // not loaded yet is fine

	// bootout returns before launchd has fully let go of the old job, and
	// bootstrap fails with an I/O error until it has; retry briefly.
	sleep := l.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	var out string
	for attempt := 0; attempt < 10; attempt++ {
		if attempt > 0 {
			sleep(300 * time.Millisecond)
		}
		if out, err = l.Ctl.Run("bootstrap", l.Domain(), l.Paths.Plist); err == nil {
			return nil
		}
	}
	return fmt.Errorf("launchctl bootstrap failed: %v: %s", err, strings.TrimSpace(out))
}

// Uninstall stops the job and removes the plist. Missing pieces are fine.
func (l *Launchd) Uninstall() error {
	_, _ = l.Ctl.Run("bootout", l.Target())
	if err := os.Remove(l.Paths.Plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Restart kills and relaunches the running job.
func (l *Launchd) Restart() error {
	if out, err := l.Ctl.Run("kickstart", "-k", l.Target()); err != nil {
		return fmt.Errorf("launchctl kickstart failed: %v: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// JobState describes the job as launchd sees it.
type JobState struct {
	Installed bool // plist present
	Loaded    bool // known to launchd
	PID       int  // 0 when not running
	LastExit  string
}

var (
	pidRe      = regexp.MustCompile(`(?m)^\s*pid = (\d+)`)
	lastExitRe = regexp.MustCompile(`(?m)^\s*last exit code = (.+)$`)
)

// State queries launchd for the job.
func (l *Launchd) State() JobState {
	var st JobState
	if _, err := os.Stat(l.Paths.Plist); err == nil {
		st.Installed = true
	}
	out, err := l.Ctl.Run("print", l.Target())
	if err != nil {
		return st
	}
	st.Loaded = true
	if m := pidRe.FindStringSubmatch(out); m != nil {
		st.PID, _ = strconv.Atoi(m[1])
	}
	if m := lastExitRe.FindStringSubmatch(out); m != nil {
		st.LastExit = strings.TrimSpace(m[1])
	}
	return st
}
