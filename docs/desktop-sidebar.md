---
layout: default
title: Desktop Sidebar
---

# Desktop Sidebar Layout

[Home](./index) | [How It Works](./how-it-works) | [Azure Setup](./azure) | [Security](./security)

---

The Claude desktop app's Code tab lets you sort sessions into folder groups, order them, pin them, and arrange the sidebar's sections. The app carries group names to your other Macs through your Claude account, but not which sessions are in which group, so on a second Mac the groups show up empty. Stradl syncs the layout itself, so every Mac's sidebar looks the same.

## What syncs

The app keeps the layout in its preferences file, `~/Library/Application Support/Claude/claude_desktop_config.json`, under `preferences.epitaxyPrefs`. Stradl syncs these keys and nothing else in that file:

| What you see | Key | Notes |
|--------------|-----|-------|
| Folder groups and their names | `dframe-group-scopes` → `groups` | Matched across Macs by group id |
| Which session is in which group | `dframe-group-scopes` → `assignments` | Session ids are the same on every Mac, because Stradl syncs the session records under the same ids |
| Order of sessions inside each group | `dframe-group-scopes` → `order` | |
| Sidebar sections (Pinned, Routines, Sessions, one per group) | `dframe-code-sections` | Name, position, sort, collapsed or shown, display options, and the app's migration markers |
| Pinned sessions and groups | `starred-local-code-sessions`, `starred-session-groups` | |
| Pinned order and pinned projects | `dframe-local-slice` → `pinnedOrder`, `homeProjectsPinnedOrder` | Folder paths under your home folder point at the same folder under each Mac's own home |

Layouts are kept per Claude account and organization, as the app keeps them.

Everything else in the file stays as each Mac's app wrote it: MCP servers, window and pane layout, filters, notices, and every other preference. Stradl rewrites only the lines of the layout that changed and keeps the rest of the file byte for byte, including its formatting and permissions. It never reads or modifies the app's Chromium Local Storage.

In storage the layout is one object, `_desktop/sidebar.json.age`, compressed, encrypted and bound to its name like every other synced file (see [Security](./security)).

## How changes merge

Stradl compares three versions: the layout on this Mac, the one in storage, and the last one both agreed on (kept in `~/.stradl/sidebar-base.json`). Uploads are compare-and-swap, so two Macs syncing at the same moment both keep their changes.

| Situation | Result |
|-----------|--------|
| A session moved on one Mac | Moves on every Mac |
| A group renamed on one Mac, sessions moved on another | Both changes kept |
| The same session moved to different groups on two Macs | The move that reached storage first wins, on every Mac |
| The same group renamed differently on two Macs | The name that reached storage first wins |
| A group deleted on one Mac, unchanged elsewhere | Deleted everywhere; its sessions become ungrouped |
| A group deleted on one Mac while another Mac renamed it or moved a session into it | The group stays, with the other Mac's changes |
| New groups, sections or pins on any Mac | Added everywhere |
| Sessions reordered on one Mac | That order everywhere (if two Macs reorder the same group, the first to reach storage wins) |

Some safeguards keep one Mac's accident from spreading:

- A missing file, or a file whose sidebar keys are gone (the app starting over with fresh preferences), counts as "nothing changed here", never as deleting every group. The layout from your other Macs is written back instead.
- If the file returns to exactly the layout Stradl replaced, which happens when the app saves its old in-memory copy, Stradl writes the new layout again rather than treating that as you undoing it.
- A layout in a shape this version of Stradl doesn't recognize stops sidebar sync with an error. Nothing in the file is changed.
- Each time Stradl writes the file, the previous version is kept beside it as `claude_desktop_config.json.stradl-backup`.

## When changes show up

The running app holds the sidebar in memory and rewrites the file whenever you change the sidebar, so a change written underneath it would be lost. Stradl therefore writes other Macs' changes only while the app is not running:

- **App not running** (after you quit it, or at login before you open it): the next sync writes the layout, and you see it when you open Claude.
- **App running**: the layout waits in `~/.stradl/sidebar-pending.json` and is written the moment the app quits. **Restart Claude** in the menu bar quits the app, writes the layout and reopens it. Sidebar changes you make in the meantime are merged in, not lost.

Changes you make on this Mac upload at the next sync, whether the app is running or not.

## Limits

- Group membership refers to sessions by id. A group can list a session this Mac doesn't have (one you deleted here, for instance); the app shows only the sessions it has.
- Groups are matched by id. Two groups created separately with the same name on two Macs stay two groups.
- Conflicts resolve per field, with the version that reached storage first winning; there are no conflict copies.
- Turning off `desktop_sync` turns off sidebar sync too.
- The layout lives in the app's private preferences, whose format can change with app updates. Stradl stops with an error when it meets a shape it doesn't know, rather than guessing.

## Verified, and still to verify

Verified:

- Where the layout lives and how it is structured, on desktop app version 3.6556, by inspecting a real Mac's preferences file.
- Group names reach other Macs through the Claude account and membership does not (the gap this feature closes).
- Stradl reads a real preferences file and, given an unchanged layout, reproduces it byte for byte (checked in memory, read-only). On synthetic files in the app's format, a rename or a move changes only the affected lines.
- The merge rules above, in unit tests and simulations of two and three Macs with random edit histories, compare-and-swap races, apps running and quitting, a vanished storage object and a reset preferences file.

Still to verify with a real restart:

- That the app loads the layout from `claude_desktop_config.json` at launch. The app also keeps a copy in its Chromium Local Storage (`LSS-persisted.dframe-group-scopes`). If it prefers that copy, it would show its old layout and save it back to the file. Stradl recognizes an exact save-back and writes the new layout again, but a save-back that also contains a new edit would be merged as an edit.
- The format of entries in the pinned lists, which were empty on the inspected Mac. Stradl treats each entry as opaque and compares entries exactly.
