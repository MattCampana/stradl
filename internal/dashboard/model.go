// Package dashboard serves Stradl's local dashboard: which Macs are syncing,
// what was worked on in the last few days, and the sync state of every
// session. It runs inside the background agent (menu bar → Open Dashboard) or
// standalone via `stradl dashboard`, binds to 127.0.0.1 only, and reads
// transcripts locally; nothing it shows leaves the Mac.
package dashboard

import (
	"context"
	"time"
)

// Provider supplies the dashboard's data. The real implementation reads
// ~/.claude, ~/.stradl and the device heartbeats in remote storage; tests use
// a fake.
type Provider interface {
	// Overview is GET /api/overview.
	Overview(ctx context.Context) (*Overview, error)
	// Session is GET /api/sessions/{id}; ErrNotFound when there is no such session.
	Session(ctx context.Context, id string) (*SessionDetail, error)
	// SyncNow asks for an immediate sync (POST /api/sync). It returns at once.
	SyncNow() error
}

// Overview is everything the dashboard's main page shows.
type Overview struct {
	GeneratedAt time.Time `json:"generatedAt"`
	ThisDevice  string    `json:"thisDevice"` // this Mac's device name
	Agent       AgentInfo `json:"agent"`
	Devices     []Device  `json:"devices"` // every Mac seen syncing into this container, this one first
	Totals      Totals    `json:"totals"`
	Days        []Day     `json:"days"`     // the last 7 days, newest first; days with no activity included (empty Sessions)
	Sessions    []Session `json:"sessions"` // every session on this Mac, most recent activity first
	Pending     Pending   `json:"pending"`
	Warnings    []string  `json:"warnings,omitempty"` // non-fatal problems (remote unreachable, unreadable file)
}

// AgentInfo mirrors the background agent's status file.
type AgentInfo struct {
	Running               bool       `json:"running"`
	Paused                bool       `json:"paused"`
	Syncing               bool       `json:"syncing"`
	LastSyncAt            *time.Time `json:"lastSyncAt,omitempty"`    // last clean sync
	LastAttemptAt         *time.Time `json:"lastAttemptAt,omitempty"` // last attempt, clean or not
	LastError             string     `json:"lastError,omitempty"`
	LastErrorSummary      string     `json:"lastErrorSummary,omitempty"` // short human line ("Storage token expired or invalid")
	Version               string     `json:"version"`
	PendingDesktopRestart int        `json:"pendingDesktopRestart"` // sessions that appear in the Claude sidebar after Claude restarts
}

// Device is one Mac syncing into the container, from its heartbeat.
type Device struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	This       bool      `json:"this"`
	Version    string    `json:"version"`
	LastSyncAt time.Time `json:"lastSyncAt"`
	Healthy    bool      `json:"healthy"`  // synced within the last 24 hours without errors
	Sessions   int       `json:"sessions"` // sessions on that Mac at its last sync
	// RecentSessionIDs are sessions that Mac changed in the last 7 days.
	RecentSessionIDs []string `json:"recentSessionIds"`
}

// Totals summarize this Mac's synced data.
type Totals struct {
	Sessions    int   `json:"sessions"`
	Projects    int   `json:"projects"`
	Messages    int   `json:"messages"` // user + assistant turns across all sessions
	Files       int   `json:"files"`    // files in the sync scope (transcripts, tool results, sidebar records, ...)
	SyncedFiles int   `json:"syncedFiles"`
	Bytes       int64 `json:"bytes"`
}

// Pending is what the next sync would do (the engine's dry-run preview).
type Pending struct {
	Upload    int           `json:"upload"`
	Download  int           `json:"download"`
	Merge     int           `json:"merge"`
	Deferred  int           `json:"deferred"` // held while a session is open in Claude
	Conflicts int           `json:"conflicts"`
	Files     []PendingFile `json:"files"` // at most 200
}

// PendingFile is one file the next sync would touch.
type PendingFile struct {
	Path   string `json:"path"`
	Action string `json:"action"` // upload | download | merge | restore | deferred
}

// Session is one Claude Code conversation on this Mac.
type Session struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`       // custom title, else sidebar title, else first prompt
	Project        string    `json:"project"`     // folder name of the working directory
	ProjectPath    string    `json:"projectPath"` // working directory
	Branch         string    `json:"branch,omitempty"`
	Surface        string    `json:"surface"` // "desktop", "cli" or another Claude Code entrypoint
	StartedAt      time.Time `json:"startedAt"`
	LastActivityAt time.Time `json:"lastActivityAt"`
	UserTurns      int       `json:"userTurns"`
	AssistantTurns int       `json:"assistantTurns"`
	FirstPrompt    string    `json:"firstPrompt"` // at most 280 characters
	LastPrompt     string    `json:"lastPrompt"`  // at most 280 characters
	FilesTouched   []string  `json:"filesTouched"`
	SizeBytes      int64     `json:"sizeBytes"`
	// SyncState: synced | pending-upload | pending-download | merging |
	// held (open in a running Claude process) | conflict | not-synced.
	SyncState string   `json:"syncState"`
	Live      bool     `json:"live"`
	Archived  bool     `json:"archived"`
	InSidebar bool     `json:"inSidebar"` // has a Claude desktop sidebar record on this Mac
	Devices   []string `json:"devices"`   // Macs that changed it in the last 7 days
}

// Day groups the sessions active on one calendar day (local time).
type Day struct {
	Date     string       `json:"date"`  // YYYY-MM-DD
	Label    string       `json:"label"` // "Today", "Yesterday", "Mon Oct 20"
	Sessions []DaySession `json:"sessions"`
}

// DaySession is what happened in one session on one day.
type DaySession struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Project      string    `json:"project"`
	Turns        int       `json:"turns"`        // user prompts that day
	Prompts      []string  `json:"prompts"`      // up to 3 of that day's prompts, at most 200 characters each
	FilesTouched []string  `json:"filesTouched"` // files edited or written that day, up to 8
	Devices      []string  `json:"devices"`
	FirstAt      time.Time `json:"firstAt"`
	LastAt       time.Time `json:"lastAt"`
}

// SessionDetail is GET /api/sessions/{id}: a session plus its timeline.
type SessionDetail struct {
	Session
	Timeline []TimelineEntry `json:"timeline"` // oldest first, at most 500 entries
}

// TimelineEntry is one notable event in a session.
type TimelineEntry struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // prompt | reply | edit | write | command | compact
	Text string    `json:"text"` // at most 400 characters
	Path string    `json:"path,omitempty"`
}

// ErrNotFound is returned by Provider.Session for an unknown session.
var ErrNotFound = errNotFound{}

type errNotFound struct{}

func (errNotFound) Error() string { return "session not found" }
