package agent

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeLaunchctl records calls instead of talking to launchd.
type fakeLaunchctl struct {
	calls     []string
	failFirst map[string]int // subcommand -> remaining failures
	outputs   map[string]string
	errs      map[string]error
}

func (f *fakeLaunchctl) Run(args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	sub := args[0]
	if f.failFirst[sub] > 0 {
		f.failFirst[sub]--
		return "Bootstrap failed: 5: Input/output error", errors.New("exit status 5")
	}
	return f.outputs[sub], f.errs[sub]
}

func testLaunchd(t *testing.T, ctl *fakeLaunchctl) *Launchd {
	t.Helper()
	return &Launchd{
		Paths:      PathsFor(t.TempDir()),
		UID:        501,
		Executable: "/Applications/Stradl & Co/<bin>/stradl",
		Ctl:        ctl,
		Sleep:      func(time.Duration) {},
	}
}

// decodePlist parses the plist subset launchd jobs use (dict, array, string,
// true, false) into Go values, failing on malformed XML.
func decodePlist(t *testing.T, data []byte) map[string]any {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = true
	var value func(start xml.StartElement) any
	value = func(start xml.StartElement) any {
		switch start.Name.Local {
		case "true", "false":
			_ = dec.Skip()
			return start.Name.Local == "true"
		case "string", "key":
			var s string
			if err := dec.DecodeElement(&s, &start); err != nil {
				t.Fatal(err)
			}
			return s
		}
		var arr []any
		dict := map[string]any{}
		var key string
		for {
			tok, err := dec.Token()
			if err != nil {
				t.Fatalf("plist is not well-formed XML: %v\n%s", err, data)
			}
			switch el := tok.(type) {
			case xml.StartElement:
				v := value(el)
				switch {
				case start.Name.Local == "array":
					arr = append(arr, v)
				case el.Name.Local == "key":
					key = v.(string)
				default:
					dict[key] = v
				}
			case xml.EndElement:
				if start.Name.Local == "array" {
					return arr
				}
				return dict
			}
		}
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			t.Fatal("no plist element")
		}
		if err != nil {
			t.Fatalf("plist is not well-formed XML: %v\n%s", err, data)
		}
		if el, ok := tok.(xml.StartElement); ok && el.Name.Local == "dict" {
			return value(el).(map[string]any)
		}
	}
}

func TestRenderPlist(t *testing.T) {
	l := testLaunchd(t, &fakeLaunchctl{})
	data, err := l.RenderPlist()
	if err != nil {
		t.Fatal(err)
	}
	got := decodePlist(t, data)

	want := map[string]any{
		"Label":                  Label,
		"ProgramArguments":       []any{l.Executable, "agent", "run"},
		"RunAtLoad":              true,
		"KeepAlive":              map[string]any{"SuccessfulExit": false},
		"ProcessType":            "Interactive",
		"LimitLoadToSessionType": "Aqua",
		"StandardOutPath":        l.Paths.Log,
		"StandardErrorPath":      l.Paths.Log,
		"EnvironmentVariables":   map[string]any{"HOME": l.Paths.Home},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("plist =\n%#v\nwant\n%#v", got, want)
	}
	// Special characters in paths are escaped, not injected.
	if !strings.Contains(string(data), "Stradl &amp; Co/&lt;bin&gt;/stradl") {
		t.Errorf("executable path not escaped:\n%s", data)
	}
}

func TestLaunchdInstall(t *testing.T) {
	ctl := &fakeLaunchctl{failFirst: map[string]int{"bootstrap": 2}}
	l := testLaunchd(t, ctl)
	if err := l.Install(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(l.Paths.Plist)
	if err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	if fi.Mode().Perm() != 0644 {
		t.Errorf("plist mode = %v, want 0644", fi.Mode().Perm())
	}
	want := []string{
		"bootout gui/501/" + Label,
		"bootstrap gui/501 " + l.Paths.Plist,
		"bootstrap gui/501 " + l.Paths.Plist,
		"bootstrap gui/501 " + l.Paths.Plist,
	}
	if strings.Join(ctl.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(ctl.calls, "\n"), strings.Join(want, "\n"))
	}
	if _, err := os.Stat(l.Paths.StateDir); err != nil {
		t.Errorf("state dir for the log not created: %v", err)
	}
}

func TestLaunchdInstallGivesUp(t *testing.T) {
	ctl := &fakeLaunchctl{failFirst: map[string]int{"bootstrap": 100}}
	l := testLaunchd(t, ctl)
	err := l.Install()
	if err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("err = %v", err)
	}
}

func TestLaunchdInstallNeedsAbsolutePath(t *testing.T) {
	l := testLaunchd(t, &fakeLaunchctl{})
	l.Executable = "stradl"
	if err := l.Install(); err == nil {
		t.Fatal("relative executable accepted")
	}
}

func TestLaunchdUninstallAndRestart(t *testing.T) {
	ctl := &fakeLaunchctl{errs: map[string]error{"bootout": errors.New("not loaded")}}
	l := testLaunchd(t, ctl)
	if err := os.MkdirAll(l.Paths.Plist[:strings.LastIndex(l.Paths.Plist, "/")], 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.Paths.Plist, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := l.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.Paths.Plist); !os.IsNotExist(err) {
		t.Fatal("plist not removed")
	}
	if err := l.Uninstall(); err != nil {
		t.Fatalf("second uninstall: %v", err)
	}
	if err := l.Restart(); err != nil {
		t.Fatal(err)
	}
	if last := ctl.calls[len(ctl.calls)-1]; last != "kickstart -k gui/501/"+Label {
		t.Fatalf("restart ran %q", last)
	}
}

func TestLaunchdState(t *testing.T) {
	ctl := &fakeLaunchctl{outputs: map[string]string{"print": `gui/501/com.breakpointdata.stradl = {
	active count = 1
	path = /Users/a/Library/LaunchAgents/com.breakpointdata.stradl.plist
	state = running
	program = /usr/local/bin/stradl
	pid = 4242
	last exit code = (never exited)
}`}}
	l := testLaunchd(t, ctl)
	st := l.State()
	if !st.Loaded || st.PID != 4242 || st.LastExit != "(never exited)" || st.Installed {
		t.Fatalf("state = %+v", st)
	}

	ctl.errs = map[string]error{"print": errors.New("Could not find service")}
	if st := l.State(); st.Loaded || st.PID != 0 {
		t.Fatalf("state for missing job = %+v", st)
	}
}
