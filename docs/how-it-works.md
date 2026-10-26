---
layout: default
title: How It Works
---

# How It Works

[Home](./index) | [Azure Setup](./azure) | [Security](./security)

---

## What gets synced

Under the default scope (`scope: everything`):

| On your Mac | In storage | Why |
|-------------|------------|-----|
| `~/.claude/**` | same path | Transcripts, prompt history, settings, `CLAUDE.md`, skills, agents, commands, plugins, plans, tasks |
| MCP servers in `~/.claude.json` | `_external/mcp-servers.json` | The servers you added, merged across Macs |
| Every project folder a session works in, `.git` included | `_folders/<path under home>/…` | The code, files and git history a session needs |
| `~/Library/Application Support/Claude/claude-code-sessions/<account>/<org>/local_*.json` | `_desktop/sessions/…` | The Claude desktop app's Code-tab sidebar entries |
| The sidebar's groups, membership and pins (in `claude_desktop_config.json`) | `_desktop/sidebar.json` | The same sidebar layout on every Mac |

Some things stay on each Mac:

- Logins: `.credentials.json` and the rest of `~/.claude.json`.
- Claude Code's per-process and per-Mac folders: `sessions`, `session-env`, `shell-snapshots`, `cache`, `statsig`, `telemetry`, `debug`, `logs`, `ide`, `backups`.
- Build output: `node_modules`, `.venv`, `venv`, `__pycache__`, `*.pyc`, `.pytest_cache`, `.mypy_cache`, `.next`, `.nuxt`, `.gradle`, `.DS_Store`.
- Temporary and lock files.
- `*.desktop-released.json` markers. The desktop app writes one next to a transcript when you delete or archive the session, and it tells that Mac's Claude Code it may expire the transcript; synced, it would expire the session on every Mac.

Every file is gzip-compressed and age-encrypted before upload and stored as `<path>.age`. Files above 32 MB stream through without being held in memory, so there is no size limit.

`scope: sessions` (what v2.0 used) syncs only transcripts, prompt history, plans, tasks and the sidebar. Upgrading moves a v2.0 install to `everything` unless you chose the scope yourself.

## The sync pass

`stradl sync`, the menu bar app and the background agent all run the same reconcile pass, under a per-Mac lock so two passes never overlap. For each file it compares three things: the copy on this Mac, the copy in storage, and the version both sides last agreed on (kept in `~/.stradl/state.json`).

| Situation | Result |
|-----------|--------|
| New on this Mac | Uploaded |
| New in storage | Downloaded |
| Changed only here | Uploaded |
| Changed only in storage | Downloaded |
| Changed on both sides | Merged (see below) |
| Session, prompt history or sidebar entry deleted on this Mac | Stays deleted here; storage and the other Macs keep it |
| Any other file deleted on this Mac | Deleted in storage; the other Macs move it to `~/.stradl/trash` |
| Deleted here, then changed on another Mac | Comes back with the new content |
| Missing from storage | Restored from this Mac |

Sessions are a union: every session any Mac has ever created ends up on every Mac. Everything else mirrors, so the Macs stay identical, but no delete ever destroys anything outright: deleted files wait 30 days in the other Macs' trash, and storage keeps old versions for 30 days.

A pass that would delete more than half of the files a folder has synced, or finds most of what this Mac synced gone from storage, deletes nothing and warns you instead (in the menu bar, the log and the dashboard). If that is what you meant, run `stradl sync --allow-mass-delete`.

### Change detection without clocks

Storage changes are detected by ETag, the version tag Azure assigns on every write, never by comparing timestamps. Laptop clocks drift, and a sync tool that trusts them eventually loses data. Local changes are detected by content hash, with a size-and-mtime fast path so an idle pass reads almost nothing.

### Compare-and-swap uploads

Every upload says "replace the version I last saw" (an `If-Match` precondition). If another Mac wrote the file in between, Azure rejects the upload, and Stradl fetches the newer version, merges, and tries again. Two Macs syncing the same session at the same moment both keep their turns.

### Merging

- **Transcripts and prompt history** are append-only JSONL files. Stradl takes the union of both versions' lines: entries are identified by their `uuid` (or exact content when there is none), the shared history keeps its order, and the two divergent tails are interleaved by timestamp. Claude Code rebuilds the conversation from each entry's `parentUuid`, so turns written on the other Mac survive as a branch, and the newest entry ends the file.
- **Sidebar entries** keep whichever version was active most recently (`lastActivityAt`).
- **Anything else** keeps the newer copy and saves the other beside it as `<file>.conflict.<timestamp>.this-mac` (or `.other-mac`). Inside `.git` and `~/.claude/plugins`, where a stray file would confuse git or Claude Code, the copy goes to `~/.stradl/conflicts/` instead. Conflict copies never sync.

## Project folders

Every folder a session works in syncs, found from the working folder of each sidebar entry and transcript, plus any you list in `extra_folders`. A folder inside a git repository syncs the whole repository. Each folder lands at the same path under home on every Mac (`~/code/app` here is `~/code/app` there), and one that doesn't exist yet is created. Your home folder itself, `~/Library` (where iCloud Drive lives), the Trash and Stradl's and Claude's own folders never sync as project folders.

Set `folder_sync: false` to turn this off, and use `folder_exclude` for patterns to leave out:

```yaml
folder_exclude:
  - "*.mov"
  - "dist/**"
```

### When a folder was already on two Macs

The first time a Mac syncs a folder that another Mac already synced, nobody knows which copy is right, so Stradl doesn't guess by timestamps:

- **A file only one Mac has** goes to both.
- **A file both Macs have with different content:** this Mac's copy stays where it is, and the other Mac's is saved beside it as a `.conflict` copy. The one exception is a file git says you never touched (it matches the last commit); then the other Mac's edit wins, and your untouched copy, which git still has, goes to `~/.stradl/conflicts/`.
- **git's own files** (HEAD, branches, the index) all come from whichever Mac's checkout is ahead, so they always agree with each other. When neither contains the other, the Mac with the newer commit leads.

So a fresh clone on one Mac picks up the uncommitted work another Mac has, and a Mac with new commits is never rolled back by an older copy.

### Git repositories

`.git` syncs like any folder, with a few rules that keep repositories sound:

- **Never mid-command.** While git holds a lock anywhere in `.git` (a commit, checkout or rebase in progress), the folder sits the pass out. A lock left behind for more than 15 minutes, usually by a crashed git, produces a warning.
- **Objects first.** Within a pass, git's objects sync before its refs, index and HEAD, so a branch never points at a commit that hasn't arrived.
- **Branches fast-forward.** When two Macs moved a branch and one commit contains the other, the branch takes the newer one. When they diverged, the branch keeps the newer commit and the other is saved as a branch named `stradl/<branch>-<short commit>`, so no commit becomes unreachable. Merge it, or delete it once you don't need it.

## Remote Control

A session you control from your phone (Remote Control) is served by the Mac it runs on, and the Claude app on every Mac signed in to your account lists it already. A synced copy of its sidebar entry would show it twice, so while Remote Control is on for a session, the other Macs don't take its entry (one they already had moves to the trash). Its transcript keeps syncing. When you turn Remote Control off, the entry changes and syncs as usual.

### Live sessions

A session that is open in a running Claude process is never rewritten underneath it. The process holds the conversation in memory and keeps appending, so changing the file would either be ignored or fork the conversation. Stradl still uploads that Mac's new turns; updates from other Macs wait until the process exits.

### Writes are atomic

A downloaded file replaces the local one with a rename, so nothing ever sees half a file. When the new version simply extends the old one, which is the usual case for a transcript continued elsewhere, Stradl appends just the new lines.

## Different usernames, different folders

Paths are stored portably. `/Users/matt` becomes `${HOME}` in both file paths and file contents on upload, and turns back into the local home folder on download. Claude Code files transcripts under a folder named after the project's absolute path (`-Users-matt-code-app`), and that name is rewritten the same way.

Projects can also live in different places on different Macs. When a sidebar entry is uploaded, Stradl records the git remote of the session's folder. On a Mac where that folder doesn't exist, it looks for a clone of the same repository in `~/code`, `~/Code`, `~/src`, `~/dev`, `~/Developer`, `~/Projects`, `~/projects`, `~/repos`, `~/git`, `~/GitHub`, `~/github`, `~/work`, `~/Work`, `~/workspace`, `~/Documents` and `~/Desktop`, up to four levels deep, and points the session there. A missing folder that isn't a repository, such as one of the desktop app's scratch folders, is created empty so the session still opens.

Clones kept anywhere else aren't found that way. Map those folders explicitly with `path_map` in `~/.stradl/config.yaml`:

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

## Dashboard

The agent also serves a dashboard: **Open Dashboard…** in the menu bar, or `stradl dashboard` in Terminal. It shows:

- **Your Macs:** every Mac syncing into your storage, when each last synced, its Stradl version, and how many sessions it changed this week. After every successful sync each Mac writes a small encrypted heartbeat to `_metadata/devices/` in storage (its name, version, last sync, session count, and the sessions it changed in the last 7 days); the engine never syncs those as files.
- **Last 7 days:** the sessions you worked on, grouped by day, with up to three of that day's prompts, the files Claude edited or wrote, the Macs involved and the time range. Days with nothing collapse to one line.
- **All sessions:** every session on this Mac, searchable and filterable by project, Mac, sync state and archived, with a drawer for each session's details, a timeline of its prompts, replies, edits and commands, and the `claude --resume` command to pick it up in Terminal.
- **Pending:** what the next sync would upload, download, merge or hold for a session that is open in Claude, from the same dry run as `stradl diff`.

Each session's state is one of **synced**, **waiting to upload** or **download**, **merging** (changed on two Macs; the next sync merges them), **open in Claude** (held, per [Live sessions](#live-sessions)), **conflict** (a `.conflict` copy is waiting for `stradl conflicts`), or **not synced**.

Everything is read on this Mac: transcripts, the sync state and the agent's status. The page refreshes every 15 seconds while it is visible, and **Sync now** asks the agent for an immediate sync.

The server listens on `127.0.0.1` only, on a random port, and every request needs a random 256-bit key. The agent records the address, key included, in `~/.stradl/dashboard.json` (0600) for the menu bar and `stradl dashboard`, and removes it when it exits; a new key is made each time the agent starts. Opening the address trades the key for an `HttpOnly`, `SameSite=Strict` cookie and drops it from the address bar. Requests that name any other host are refused, which stops DNS-rebinding attacks, and **Sync now** only works from the dashboard's own page. The page loads nothing from the network: no fonts, scripts or images from anywhere else, enforced by a strict Content-Security-Policy.

Without a running agent, `stradl dashboard` serves the dashboard itself until you press Ctrl-C, and its **Sync now** runs a sync in that process. `stradl dashboard --no-open` prints the address instead of opening the browser.

## Files

```
~/.stradl/
├── config.yaml     storage + encryption settings (0600)
├── age-key.txt     encryption identity derived from your passphrase (0600)
├── state.json      per-file hash, ETag and sync state
├── status.json     the agent's last result (read by the menu bar and `stradl status`)
├── dashboard.json  the running agent's dashboard address and key (0600)
├── agent.log       background agent log
├── trash/          files other Macs deleted, by day, kept 30 days
├── conflicts/      losing copies of conflicts inside git repositories and plugins
├── folders.json    the project folders the last sync covered
└── sync.lock       held while a sync runs
```
