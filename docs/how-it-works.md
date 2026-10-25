---
layout: default
title: How It Works
---

# How It Works

[Home](./index) | [Azure Setup](./azure) | [Security](./security)

---

## What gets synced

| On your Mac | In storage | Why |
|-------------|------------|-----|
| `~/.claude/projects/**` | `projects/${HOME}-…/…` | Session transcripts, subagent transcripts, tool results |
| `~/.claude/history.jsonl` | `history.jsonl` | Prompt history (up-arrow) |
| `~/.claude/plans`, `~/.claude/tasks` | same | Plan files and task lists sessions refer to |
| `~/Library/Application Support/Claude/claude-code-sessions/<account>/<org>/local_*.json` | `_desktop/sessions/…` | The Claude desktop app's Code-tab sidebar entries |

Everything else stays on the Mac: credentials, `~/.claude.json`, settings, plugins, running-process records, and the desktop app's private state (tombstones, indexes, scheduled tasks). Every file is gzip-compressed and age-encrypted before upload, and stored as `<path>.age`.

## The sync pass

`stradl sync`, the menu bar app and the background agent all run the same reconcile pass, under a per-Mac lock so two passes never overlap. For each file it compares three things: the copy on this Mac, the copy in storage, and the version both sides last agreed on (kept in `~/.stradl/state.json`).

| Situation | Result |
|-----------|--------|
| New on this Mac | Uploaded |
| New in storage | Downloaded |
| Changed only here | Uploaded |
| Changed only in storage | Downloaded |
| Changed on both sides | Merged (see below) |
| Deleted on this Mac | Stays deleted here; storage and the other Macs keep it |
| Deleted here, then changed on another Mac | Comes back with the new content |
| Missing from storage | Restored from this Mac |

The result is a union: every session any Mac has ever created ends up on every Mac.

### Change detection without clocks

Storage changes are detected by ETag, the version tag Azure assigns on every write, never by comparing timestamps. Laptop clocks drift, and a sync tool that trusts them eventually loses data. Local changes are detected by content hash, with a size-and-mtime fast path so an idle pass reads almost nothing.

### Compare-and-swap uploads

Every upload says "replace the version I last saw" (an `If-Match` precondition). If another Mac wrote the file in between, Azure rejects the upload, and Stradl fetches the newer version, merges, and tries again. Two Macs syncing the same session at the same moment both keep their turns.

### Merging

- **Transcripts and prompt history** are append-only JSONL files. Stradl takes the union of both versions' lines: entries are identified by their `uuid` (or exact content when there is none), the shared history keeps its order, and the two divergent tails are interleaved by timestamp. Claude Code rebuilds the conversation from each entry's `parentUuid`, so turns written on the other Mac survive as a branch, and the newest entry ends the file.
- **Sidebar entries** keep whichever version was active most recently (`lastActivityAt`).
- **Anything else** keeps the newer copy and saves the other beside it as `<file>.conflict.<timestamp>`.

### Live sessions

A session that is open in a running Claude process is never rewritten underneath it. The process holds the conversation in memory and keeps appending, so changing the file would either be ignored or fork the conversation. Stradl still uploads that Mac's new turns; updates from other Macs wait until the process exits.

### Writes are atomic

A downloaded file replaces the local one with a rename, so nothing ever sees half a file. When the new version simply extends the old one, which is the usual case for a transcript continued elsewhere, Stradl appends just the new lines.

## Different usernames, different folders

Paths are stored portably. `/Users/matt` becomes `${HOME}` in both file paths and file contents on upload, and turns back into the local home folder on download. Claude Code files transcripts under a folder named after the project's absolute path (`-Users-matt-code-app`), and that name is rewritten the same way.

Projects can also live in different places on different Macs. When a sidebar entry is uploaded, Stradl records the git remote of the session's folder. On a Mac where that folder doesn't exist, it looks for a clone of the same repository in the usual places under your home folder (`~/code`, `~/Projects`, `~/Developer`, `~/src`, `~/Documents`, and similar, a few levels deep) and points the session there. A missing folder that isn't a repository, such as one of the desktop app's scratch folders, is created empty so the session still opens.

You can also map folders explicitly with `path_map` in `~/.stradl/config.yaml`:

```yaml
path_map:
  ~/work: WORK      # on this Mac
# and on the other Mac:  ~/Projects: WORK
```

## The desktop sidebar

The Claude desktop app keeps one small JSON record per Code-tab session, which points at a transcript (`cliSessionId`) and a working folder (`cwd`). It reads these records when it launches. Stradl syncs them like any other file, with paths and folders remapped for each Mac, so the same sessions appear in every Mac's sidebar.

Because the app only reads its sidebar at launch, sessions that arrive while it is running appear after it restarts. The menu bar app counts them and offers **Restart Claude to show N new sessions**. Sessions already in the sidebar keep working in the meantime.

## The background agent

`stradl agent install` registers a per-user launchd agent (`com.breakpointdata.stradl`) that starts at login and shows the menu bar icon. It syncs:

- right away at start and after the Mac wakes from sleep,
- about 10 seconds after local session files stop changing (it checks every 20 seconds, reading only file sizes and times),
- whenever storage changes (it checks every minute with a single listing),
- and every 5 minutes regardless (`sync_interval_seconds` in the config).

It writes its status to `~/.stradl/status.json` and logs to `~/.stradl/agent.log`. Pausing writes `~/.stradl/paused`, which `stradl agent resume` removes.

## Files

```
~/.stradl/
├── config.yaml     storage + encryption settings (0600)
├── age-key.txt     encryption identity derived from your passphrase (0600)
├── state.json      per-file hash, ETag and sync state
├── status.json     the agent's last result (read by the menu bar and `stradl status`)
├── agent.log       background agent log
└── sync.lock       held while a sync runs
```
