package dashboard

import (
	"context"
	"sort"
	"strings"
	gosync "sync"
	"time"
)

// Fake is a Provider with made-up but realistic data: three Macs, a week of
// sessions across a handful of projects, every sync state. Tests use it, and
// `stradl dashboard --demo` serves it for screenshots. Everything in it is
// synthetic.
type Fake struct {
	mu  gosync.Mutex
	now func() time.Time

	ov      *Overview
	details map[string]*SessionDetail

	// SyncErr, when set, is what SyncNow returns.
	SyncErr error
	// syncCalls counts SyncNow calls; syncUntil is when the pretend sync
	// started by the last call finishes.
	syncCalls int
	syncUntil time.Time
}

// fakeSyncTime is how long a pretend sync takes.
const fakeSyncTime = 2500 * time.Millisecond

// NewFake returns a Fake whose data is laid out around now (local time).
func NewFake(now time.Time) *Fake {
	f := &Fake{now: time.Now, details: map[string]*SessionDetail{}}
	f.ov = buildFake(now, f.details)
	return f
}

// NewEmptyFake returns a Fake for a Mac that was just set up: one device,
// no sessions yet.
func NewEmptyFake(now time.Time) *Fake {
	days := make([]Day, 7)
	for i := range days {
		d := now.AddDate(0, 0, -i)
		days[i] = Day{Date: d.Format("2006-01-02"), Label: dayLabel(i, d), Sessions: []DaySession{}}
	}
	synced := now.Add(-30 * time.Second)
	return &Fake{now: time.Now, details: map[string]*SessionDetail{}, ov: &Overview{
		GeneratedAt: now,
		ThisDevice:  "MacBook Pro",
		Agent:       AgentInfo{Running: true, Version: "2.1.0", LastSyncAt: &synced, LastAttemptAt: &synced},
		Devices: []Device{{ID: "mbp", Name: "MacBook Pro", This: true, Version: "2.1.0", LastSyncAt: synced,
			Healthy: true, RecentSessionIDs: []string{}}},
		Days:     days,
		Sessions: []Session{},
		Pending:  Pending{Files: []PendingFile{}},
	}}
}

// SyncCalls reports how many times SyncNow was called.
func (f *Fake) SyncCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncCalls
}

// Overview returns a copy of the sample overview, showing a sync in
// progress for a moment after SyncNow.
func (f *Fake) Overview(ctx context.Context) (*Overview, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ov := *f.ov
	ov.GeneratedAt = f.now()
	if !f.syncUntil.IsZero() {
		if ov.GeneratedAt.Before(f.syncUntil) {
			ov.Agent.Syncing = true
		} else {
			at := f.syncUntil
			ov.Agent.LastSyncAt, ov.Agent.LastAttemptAt = &at, &at
			ov.Agent.LastError, ov.Agent.LastErrorSummary = "", ""
			if len(ov.Devices) > 0 {
				devices := append([]Device(nil), ov.Devices...)
				devices[0].LastSyncAt = at
				ov.Devices = devices
			}
		}
	}
	return &ov, nil
}

// Session returns the sample detail for id, or ErrNotFound.
func (f *Fake) Session(ctx context.Context, id string) (*SessionDetail, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.details[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *d
	return &cp, nil
}

// SyncNow starts a pretend sync that finishes a couple of seconds later.
func (f *Fake) SyncNow() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncCalls++
	if f.SyncErr != nil {
		return f.SyncErr
	}
	f.syncUntil = f.now().Add(fakeSyncTime)
	return nil
}

// fakeSession is one sample conversation; its segments are the stretches
// of work on different days.
type fakeSession struct {
	id, title, project, branch, surface string
	state                               string
	live, archived, sidebar             bool
	segs                                []fakeSeg
}

type fakeSeg struct {
	daysAgo, hour, min, mins int // start (local) and length in minutes
	device                   string
	prompts                  []string
	files                    []string
	commands                 []string
}

const (
	macPro    = "MacBook Pro"
	macStudio = "Mac Studio"
	macAir    = "MacBook Air"
	demoHome  = "/Users/demo"
)

var fakeSessions = []fakeSession{
	{id: "5f0c2a7e-9d41-4b8e-a3c2-1e7b6d90f412", title: "Local dashboard for synced sessions", project: "stradl", branch: "feat/dashboard",
		surface: "desktop", state: "held", live: true, sidebar: true, segs: []fakeSeg{
			{daysAgo: 0, hour: 9, min: 12, mins: 154, device: macPro,
				prompts: []string{
					"Can we give the app a dashboard? I want to see which files and sessions are synced and what I worked on this week.",
					"Make the session drawer explain the sync state in plain words, not just a badge.",
					"Empty days should collapse to one muted line so the week stays scannable.",
					"Run the tests again and fix whatever lint reports.",
				},
				files:    []string{"internal/dashboard/server.go", "internal/dashboard/web/app.js", "internal/dashboard/web/app.css", "internal/dashboard/web/index.html", "cmd/claude-sync/dashboard_cmd.go"},
				commands: []string{"go test -short ./internal/dashboard/...", "golangci-lint run ./..."}},
		}},
	{id: "a8d7e1c4-2b36-4f59-8e0a-7c4d1b2e9f63", title: "Flaky upload retry test", project: "stradl", branch: "main",
		surface: "cli", state: "pending-download", sidebar: false, segs: []fakeSeg{
			{daysAgo: 0, hour: 8, min: 2, mins: 41, device: macStudio,
				prompts: []string{
					"TestUploadRetry fails about one run in twenty on CI. Find out why.",
					"Use the fake clock instead of sleeping; keep the assertion on the attempt count.",
				},
				files:    []string{"internal/sync/engine_test.go", "internal/sync/clock.go"},
				commands: []string{"go test -count=50 -run TestUploadRetry ./internal/sync/"}},
		}},
	{id: "c1b9f5d2-6e47-4a08-b3d1-9f2e8a7c6b54", title: "Invoice PDF line items", project: "invoice-api", branch: "line-items",
		surface: "desktop", state: "pending-upload", sidebar: true, segs: []fakeSeg{
			{daysAgo: 0, hour: 13, min: 30, mins: 26, device: macPro,
				prompts: []string{
					"Line items with long descriptions overflow the PDF table. Wrap them and repeat the header on each page.",
					"Add a golden-file test for a three-page invoice.",
				},
				files:    []string{"src/pdf/table.ts", "src/pdf/table.test.ts", "test/golden/three-page.pdf"},
				commands: []string{"npm test -- table"}},
		}},
	{id: "e4a3c7b1-8f25-4d69-a0e2-5b1c9d8f7a36", title: "Review Azure union sync before release", project: "stradl", branch: "feat/azure-union-sync",
		surface: "desktop", state: "merging", sidebar: true, segs: []fakeSeg{
			{daysAgo: 2, hour: 16, min: 5, mins: 70, device: macStudio,
				prompts: []string{
					"Review the Azure adapter for anything that could lose a blob version.",
					"What happens if two Macs upload the same transcript in the same second?",
				},
				files:    []string{"internal/storage/azure/azure.go", "internal/sync/merge.go"},
				commands: []string{"go test ./internal/storage/azure/"}},
			{daysAgo: 1, hour: 10, min: 40, mins: 95, device: macPro,
				prompts: []string{
					"Pick up where we left off: the ETag normalization question.",
					"Write the release notes for 2.1.0 from the commits since 2.0.0.",
					"Tag it once CI is green.",
				},
				files:    []string{"internal/storage/azure/etag.go", "CHANGELOG.md"},
				commands: []string{"git log --oneline v2.0.0..HEAD", "gh run watch"}},
		}},
	{id: "b2f8d6a9-3c17-4e5b-9a4d-8e6f1c0b2d75", title: "Homelab DNS over Tailscale", project: "homelab", branch: "",
		surface: "cli", state: "synced", sidebar: false, segs: []fakeSeg{
			{daysAgo: 1, hour: 20, min: 15, mins: 48, device: macPro,
				prompts: []string{
					"Point the tailnet's DNS at the Pi-hole but keep MagicDNS names working.",
					"Write it down in the README so I remember next time.",
				},
				files:    []string{"dns/pihole.toml", "README.md"},
				commands: []string{"tailscale status", "dig @100.64.0.2 nas.home.arpa"}},
		}},
	{id: "d6c4b8e2-1a93-4f7d-b5e8-2c9a7d3f1e08", title: "Field notes offline mode", project: "field-notes", branch: "offline",
		surface: "desktop", state: "synced", sidebar: true, segs: []fakeSeg{
			{daysAgo: 2, hour: 11, min: 0, mins: 120, device: macAir,
				prompts: []string{
					"Notes taken offline should queue and upload when the connection comes back.",
					"Show a small badge on notes that haven't uploaded yet.",
					"What's the simplest way to test this without turning off Wi-Fi?",
				},
				files:    []string{"app/sync/queue.swift", "app/views/NoteRow.swift", "app/sync/QueueTests.swift"},
				commands: []string{"xcodebuild test -scheme FieldNotes"}},
		}},
	{id: "f9e2a1d7-4b58-4c3e-8d6f-0a7b3e5c9d21", title: "zsh startup is slow again", project: "dotfiles", branch: "",
		surface: "cli", state: "synced", sidebar: false, segs: []fakeSeg{
			{daysAgo: 2, hour: 22, min: 10, mins: 18, device: macPro,
				prompts:  []string{"New shells take almost a second to start. Profile it and lazy-load whatever is slow."},
				files:    []string{".zshrc", "zsh/lazy-nvm.zsh"},
				commands: []string{"zsh -i -c exit", "zprof"}},
		}},
	{id: "0b7d3f9c-5e26-4a81-9c7b-3d8e2f6a1c49", title: "Pricing page copy", project: "website", branch: "pricing",
		surface: "desktop", state: "synced", sidebar: true, segs: []fakeSeg{
			{daysAgo: 4, hour: 15, min: 20, mins: 55, device: macStudio,
				prompts: []string{
					"Tighten the pricing page copy. Shorter sentences, no exclamation marks.",
					"Make the FAQ answer about refunds match the terms page.",
				},
				files: []string{"content/pricing.mdx", "content/faq.mdx"}},
		}},
	{id: "3e8a6c2f-7d19-4b4e-a2c5-6f9d1b8e3a70", title: "Rate limits for the invoice API", project: "invoice-api", branch: "rate-limits",
		surface: "cli", state: "conflict", sidebar: false, segs: []fakeSeg{
			{daysAgo: 6, hour: 9, min: 45, mins: 80, device: macAir,
				prompts: []string{
					"Add per-key rate limiting: 60 requests a minute, burst of 10.",
					"Return Retry-After on 429s and document it.",
				},
				files:    []string{"src/middleware/rateLimit.ts", "src/middleware/rateLimit.test.ts", "docs/api.md"},
				commands: []string{"npm test -- rateLimit"}},
		}},
	{id: "7a1f4d8b-2c65-4e9a-b3f7-9e0c5a2d6b18", title: "Menu bar icon states", project: "stradl", branch: "main",
		surface: "desktop", state: "synced", sidebar: true, segs: []fakeSeg{
			{daysAgo: 6, hour: 14, min: 0, mins: 65, device: macPro,
				prompts: []string{
					"The menu bar icon should show syncing and offline states without looking busy.",
					"Generate the 16 and 22 point PNGs from the SVG masters.",
				},
				files:    []string{"assets/brand/menubar-syncing.svg", "assets/brand/menubar-offline.svg", "internal/agent/icon/icon.go"},
				commands: []string{"python3 assets/brand/src/build.py"}},
		}},
	// Older than a week: only in "All sessions".
	{id: "9c5e2b7a-6f31-4d8c-a7e9-1b4f8d2c5e63", title: "Migrate config to YAML", project: "stradl", branch: "main",
		surface: "cli", state: "synced", segs: []fakeSeg{
			{daysAgo: 12, hour: 10, min: 0, mins: 90, device: macStudio,
				prompts: []string{"Move the config from JSON to YAML and keep reading the old file once."},
				files:   []string{"internal/config/config.go", "internal/config/config_test.go"}},
		}},
	{id: "2d9b6e4f-8a17-4c5b-9e3d-7f1a2c8b6d94", title: "Weekly expenses script", project: "homelab", branch: "",
		surface: "cli", state: "synced", segs: []fakeSeg{
			{daysAgo: 16, hour: 19, min: 30, mins: 35, device: macAir,
				prompts: []string{"Sum last week's card export by category and print a table."},
				files:   []string{"scripts/expenses.py"}},
		}},
	{id: "6f3a8d1c-4e92-4b7f-8c5a-2e9d7b1f4a36", title: "Onboarding flow sketches", project: "field-notes", branch: "onboarding",
		surface: "desktop", state: "synced", archived: true, sidebar: true, segs: []fakeSeg{
			{daysAgo: 23, hour: 14, min: 10, mins: 60, device: macPro,
				prompts: []string{"Three screens max for onboarding. Skip the account step until they create a second note."},
				files:   []string{"app/views/Onboarding.swift"}},
		}},
	{id: "8b4e1f7d-9c26-4a3e-b8d5-6a2f9e1c7b40", title: "Scratch: regex for log lines", project: "scratch", branch: "",
		surface: "cli", state: "not-synced", segs: []fakeSeg{
			{daysAgo: 31, hour: 11, min: 5, mins: 8, device: macPro,
				prompts: []string{"A regex that pulls the request ID and latency out of these log lines."}},
		}},
	{id: "4c7a2e9f-1b58-4d6c-a3e7-8f2b5d9a1c63", title: "Book club site", project: "website", branch: "main",
		surface: "desktop", state: "synced", archived: true, sidebar: true, segs: []fakeSeg{
			{daysAgo: 44, hour: 21, min: 0, mins: 50, device: macStudio,
				prompts: []string{"A one-page site listing this year's books with a vote button."},
				files:   []string{"content/books.mdx", "components/Vote.tsx"}},
		}},
}

var fakeReplies = []string{
	"Read through the relevant files first. The state you need is already tracked, so this is mostly wiring, not new bookkeeping.",
	"Done. The change is small and covered by a new test; the existing tests still pass.",
	"Found the cause and fixed it at the source rather than working around it. Ran the related tests a few times to be sure.",
	"Here's the plan: keep the current behavior as the default and put the new path behind a flag until it has run for a week.",
	"Updated both files and left a short comment explaining why the order matters.",
	"That works, with one caveat: a second device can briefly see the old value until its next sync.",
}

func buildFake(now time.Time, details map[string]*SessionDetail) *Overview {
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	day0 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	ov := &Overview{
		GeneratedAt: now,
		ThisDevice:  macPro,
		Agent: AgentInfo{
			Running: true, LastSyncAt: ago(2 * time.Minute), LastAttemptAt: ago(2 * time.Minute),
			Version: "2.1.0", PendingDesktopRestart: 2,
		},
	}

	var sessions []Session
	days := make([]Day, 7)
	for i := range days {
		d := day0.AddDate(0, 0, -i)
		days[i] = Day{Date: d.Format("2006-01-02"), Label: dayLabel(i, d), Sessions: []DaySession{}}
	}
	recent := map[string][]string{}
	projects := map[string]bool{}
	messages := 0
	var bytes int64

	for n, fs := range fakeSessions {
		path := demoHome + "/code/" + fs.project
		s := Session{
			ID: fs.id, Title: fs.title, Project: fs.project, ProjectPath: path, Branch: fs.branch,
			Surface: fs.surface, SyncState: fs.state, Live: fs.live, Archived: fs.archived, InSidebar: fs.sidebar,
			Devices: []string{}, FilesTouched: []string{},
		}
		var timeline []TimelineEntry
		seen := map[string]bool{}
		for si, seg := range fs.segs {
			start := day0.AddDate(0, 0, -seg.daysAgo).Add(time.Duration(seg.hour)*time.Hour + time.Duration(seg.min)*time.Minute)
			end := start.Add(time.Duration(seg.mins) * time.Minute)
			if end.After(now) && seg.daysAgo == 0 {
				// Keep today's sessions in the past, and still today, whatever
				// the hour: squeeze them into the part of today gone by.
				avail := max(now.Sub(day0)-time.Duration(5+n)*time.Minute, 0)
				scale := float64(avail) / float64(end.Sub(day0))
				start = day0.Add(time.Duration(float64(start.Sub(day0)) * scale))
				end = day0.Add(time.Duration(float64(end.Sub(day0)) * scale))
			}
			if si == 0 {
				s.StartedAt = start
				s.FirstPrompt = clip(seg.prompts[0], 280)
			}
			s.LastActivityAt = end
			s.LastPrompt = clip(seg.prompts[len(seg.prompts)-1], 280)
			s.UserTurns += len(seg.prompts)
			s.AssistantTurns += len(seg.prompts) + len(seg.files) + len(seg.commands)
			var files []string
			for _, f := range seg.files {
				abs := path + "/" + f
				files = append(files, abs)
				if !seen[abs] {
					seen[abs] = true
					s.FilesTouched = append(s.FilesTouched, abs)
				}
			}
			if seg.daysAgo < 7 {
				if !hasString(s.Devices, seg.device) {
					s.Devices = append(s.Devices, seg.device)
				}
				if !hasString(recent[seg.device], fs.id) {
					recent[seg.device] = append(recent[seg.device], fs.id)
				}
				prompts := make([]string, 0, 3)
				for _, p := range seg.prompts {
					if len(prompts) == 3 {
						break
					}
					prompts = append(prompts, clip(p, 200))
				}
				ds := DaySession{ID: fs.id, Title: fs.title, Project: fs.project, Turns: len(seg.prompts), Prompts: prompts,
					FilesTouched: first(files, 8), Devices: []string{seg.device}, FirstAt: start, LastAt: end}
				days[seg.daysAgo].Sessions = append(days[seg.daysAgo].Sessions, ds)
			}
			timeline = append(timeline, fakeTimeline(seg, start, end, path, len(timeline))...)
		}
		if len(fs.segs[0].prompts) > 2 && len(timeline) > 12 {
			// A long session gets compacted once, part way through.
			at := timeline[len(timeline)/2].At
			timeline = append(timeline, TimelineEntry{At: at, Kind: "compact", Text: "Conversation compacted to free up context"})
			sort.SliceStable(timeline, func(i, j int) bool { return timeline[i].At.Before(timeline[j].At) })
		}
		s.SizeBytes = int64(40_000 + 38_000*len(timeline) + 9_000*n)
		bytes += s.SizeBytes
		messages += s.UserTurns + s.AssistantTurns
		projects[fs.project] = true
		sessions = append(sessions, s)
		details[fs.id] = &SessionDetail{Session: s, Timeline: timeline}
	}

	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].LastActivityAt.After(sessions[j].LastActivityAt) })
	for i := range days {
		ss := days[i].Sessions
		sort.SliceStable(ss, func(a, b int) bool { return ss[a].LastAt.After(ss[b].LastAt) })
	}
	ov.Sessions = sessions
	ov.Days = days

	ov.Devices = []Device{
		{ID: "c02f-mbp", Name: macPro, This: true, Version: "2.1.0", LastSyncAt: now.Add(-2 * time.Minute), Healthy: true,
			Sessions: len(sessions) - 1, RecentSessionIDs: orEmpty(recent[macPro])},
		{ID: "a7d1-studio", Name: macStudio, Version: "2.1.0", LastSyncAt: now.Add(-9 * time.Minute), Healthy: true,
			Sessions: len(sessions) - 2, RecentSessionIDs: orEmpty(recent[macStudio])},
		{ID: "f3e8-air", Name: macAir, Version: "2.0.0", LastSyncAt: now.Add(-50 * time.Hour), Healthy: false,
			Sessions: len(sessions) - 4, RecentSessionIDs: orEmpty(recent[macAir])},
	}

	proj := func(id string) string { return "projects/-Users-demo-code-" + projectOf(id) + "/" + id + ".jsonl" }
	ov.Pending = Pending{
		Upload: 2, Download: 1, Merge: 1, Deferred: 1, Conflicts: 1,
		Files: []PendingFile{
			{Path: proj("c1b9f5d2-6e47-4a08-b3d1-9f2e8a7c6b54"), Action: "upload"},
			{Path: "_desktop/sessions/acct-3f9a/org-81c2/local_c1b9f5d2-6e47-4a08-b3d1-9f2e8a7c6b54.json", Action: "upload"},
			{Path: proj("a8d7e1c4-2b36-4f59-8e0a-7c4d1b2e9f63"), Action: "download"},
			{Path: proj("e4a3c7b1-8f25-4d69-a0e2-5b1c9d8f7a36"), Action: "merge"},
			{Path: proj("5f0c2a7e-9d41-4b8e-a3c2-1e7b6d90f412"), Action: "deferred"},
		},
	}
	ov.Totals = Totals{
		Sessions: len(sessions), Projects: len(projects), Messages: messages,
		Files: 1288, SyncedFiles: 1283, Bytes: bytes + 61_400_000,
	}
	ov.Warnings = []string{"MacBook Air hasn't synced in over a day; it may be asleep or offline."}
	return ov
}

// fakeTimeline spreads a segment's prompts, replies, edits and commands
// evenly between start and end.
func fakeTimeline(seg fakeSeg, start, end time.Time, root string, offset int) []TimelineEntry {
	var out []TimelineEntry
	n := len(seg.prompts)
	step := end.Sub(start) / time.Duration(n)
	fi, ci := 0, 0
	for i, p := range seg.prompts {
		at := start.Add(time.Duration(i) * step)
		out = append(out, TimelineEntry{At: at, Kind: "prompt", Text: clip(p, 400)})
		t := at.Add(step / 5)
		// Hand out this prompt's share of the edits and commands.
		for ; fi < len(seg.files)*(i+1)/n; fi++ {
			kind := "edit"
			if strings.HasSuffix(seg.files[fi], "_test.go") || strings.Contains(seg.files[fi], "test") || fi%3 == 2 {
				kind = "write"
			}
			verb := map[string]string{"edit": "Edited ", "write": "Wrote "}[kind]
			out = append(out, TimelineEntry{At: t, Kind: kind, Text: verb + seg.files[fi], Path: root + "/" + seg.files[fi]})
			t = t.Add(step / 10)
		}
		for ; ci < len(seg.commands)*(i+1)/n; ci++ {
			out = append(out, TimelineEntry{At: t, Kind: "command", Text: seg.commands[ci]})
			t = t.Add(step / 10)
		}
		out = append(out, TimelineEntry{At: t.Add(step / 10), Kind: "reply", Text: fakeReplies[(offset+i)%len(fakeReplies)]})
	}
	return out
}

func dayLabel(daysAgo int, d time.Time) string {
	switch daysAgo {
	case 0:
		return "Today"
	case 1:
		return "Yesterday"
	}
	return d.Format("Mon Jan 2")
}

func projectOf(id string) string {
	for _, s := range fakeSessions {
		if s.id == id {
			return s.project
		}
	}
	return ""
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func first(s []string, n int) []string {
	out := make([]string, 0, n)
	for _, v := range s {
		if len(out) == n {
			break
		}
		out = append(out, v)
	}
	return out
}

// hasString and orEmpty are named apart from the provider's own helpers,
// which share this package.
func hasString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
