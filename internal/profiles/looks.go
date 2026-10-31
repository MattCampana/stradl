package profiles

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Each account's launcher (see agent.WriteLaunchers) shows the app icon
// with the account's name on a band of its own color. Colors are this
// Mac's choice, kept in launchers.json in the state directory with what
// was last deployed.

// Palette is the band colors offered, in order; an account gets the first
// one no other account has until it is given another.
var Palette = []NamedColor{
	{"Pink", "#FF3D9A"}, {"Violet", "#8A5CFF"}, {"Azure", "#1EC8FF"}, {"Teal", "#19C39C"},
	{"Amber", "#FFB020"}, {"Coral", "#FF6B4A"}, {"Lime", "#7ED957"}, {"Magenta", "#E040FB"},
}

// NamedColor is a palette entry.
type NamedColor struct {
	Name string `json:"name"`
	Hex  string `json:"hex"`
}

const looksFile = "launchers.json"

// looks is launchers.json.
type looks struct {
	Colors map[string]string `json:"colors,omitempty"`
	// Deployed is what the launchers in ~/Applications show, account to
	// "name|color", so a change of either can redeploy them.
	Deployed map[string]string `json:"deployed,omitempty"`
}

func (e Env) readLooks() looks {
	var l looks
	if data, err := os.ReadFile(filepath.Join(e.StateDir, looksFile)); err == nil {
		_ = json.Unmarshal(data, &l)
	}
	if l.Colors == nil {
		l.Colors = make(map[string]string)
	}
	return l
}

func (e Env) writeLooks(l looks) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(e.StateDir, 0700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(e.StateDir, looksFile), data, 0600)
}

var hexColorRe = regexp.MustCompile(`^#?([0-9A-Fa-f]{6}|[0-9A-Fa-f]{3})$`)

// ParseColor reads a color: a palette name (any case) or #RGB / #RRGGBB.
// It returns #RRGGBB in upper case.
func ParseColor(s string) (string, error) {
	s = strings.TrimSpace(s)
	for _, c := range Palette {
		if strings.EqualFold(s, c.Name) {
			return c.Hex, nil
		}
	}
	m := hexColorRe.FindStringSubmatch(s)
	if m == nil {
		names := make([]string, len(Palette))
		for i, c := range Palette {
			names[i] = strings.ToLower(c.Name)
		}
		return "", fmt.Errorf("%q is not a color: use #RRGGBB or one of %s", s, strings.Join(names, ", "))
	}
	h := strings.ToUpper(m[1])
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	return "#" + h, nil
}

// Colors returns each account's band color: the one chosen for it, else
// the first palette color no other account has (accounts in ID order).
// New choices are saved, so an account keeps its color as others come.
func (e Env) Colors(accounts []string) map[string]string {
	l := e.readLooks()
	ids := append([]string(nil), accounts...)
	sort.Strings(ids)
	used := make(map[string]bool)
	for _, c := range l.Colors {
		used[strings.ToUpper(c)] = true
	}
	changed := false
	for _, id := range ids {
		if _, ok := l.Colors[id]; ok {
			continue
		}
		pick := Palette[len(l.Colors)%len(Palette)].Hex
		for _, c := range Palette {
			if !used[c.Hex] {
				pick = c.Hex
				break
			}
		}
		l.Colors[id], used[pick], changed = pick, true, true
	}
	if changed {
		_ = e.writeLooks(l) // best effort: the colors still show
	}
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		out[id] = l.Colors[id]
	}
	return out
}

// SetColor gives acct's launcher band a color (see ParseColor).
func (e Env) SetColor(acct, color string) (string, error) {
	hex, err := ParseColor(color)
	if err != nil {
		return "", err
	}
	l := e.readLooks()
	l.Colors[strings.ToLower(acct)] = hex
	return hex, e.writeLooks(l)
}

// Deployed returns what the deployed launchers show, account to
// "name|color"; empty when none were deployed.
func (e Env) Deployed() map[string]string { return e.readLooks().Deployed }

// SetDeployed records what the launchers now show (nil: none).
func (e Env) SetDeployed(d map[string]string) error {
	l := e.readLooks()
	l.Deployed = d
	return e.writeLooks(l)
}

// consumerDomains are mail providers whose domain says nothing about the
// account, so its badge uses the mailbox name instead.
var consumerDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "icloud.com": true, "me.com": true, "mac.com": true,
	"outlook.com": true, "hotmail.com": true, "live.com": true, "yahoo.com": true, "proton.me": true,
	"protonmail.com": true, "fastmail.com": true, "aol.com": true,
}

// BadgeName is the short name an account's launcher shows: its nickname,
// else its email's organization (judge.com: Judge; owd.com: OWD), else
// the mailbox name, else the start of its ID.
func BadgeName(label, email, id string) string {
	if s := strings.TrimSpace(label); s != "" {
		return s
	}
	local, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(email)), "@")
	if ok && domain != "" {
		name := local
		if !consumerDomains[domain] {
			name, _, _ = strings.Cut(domain, ".")
		}
		if len(name) <= 3 {
			return strings.ToUpper(name)
		}
		return strings.ToUpper(name[:1]) + name[1:]
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
