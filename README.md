<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brand/lockup-dark.svg">
    <img src="assets/brand/lockup-light.svg" alt="stradl" width="360">
  </picture>
</p>

<p align="center"><strong>Musical chairs for your Claude sessions.</strong></p>

<p align="center">
  <a href="https://github.com/MattCampana/stradl/releases/latest"><img src="https://img.shields.io/github/v/release/MattCampana/stradl?label=release" alt="Latest release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="MIT license"></a>
  <img src="https://img.shields.io/badge/macOS-13%2B-black?logo=apple" alt="macOS 13+">
</p>

---

Stradl keeps every Claude Code session on every one of your Macs, along with everything the sessions worked on. Start a conversation on your desk Mac, sit down at your laptop, and it's already there: in the Claude desktop app's sidebar and in `claude --resume`, with its project folder, git history and files.

- **Every session, everywhere.** Three sessions on one Mac, two on another and four on a third become nine on all three. Nothing is overwritten, and nothing is left behind.
- **Everything a session touched.** The project folders your sessions work in sync too, git repositories included, plus all of `~/.claude`: settings, skills, agents, commands, plugins, plans. Logins stay on each Mac.
- **Built for the Claude desktop app.** Sessions show up in the Code tab's sidebar on every Mac, in the same groups and with the same pins, not just in the terminal.
- **Different usernames are fine.** `/Users/matt` on one Mac and `/Users/breakpoint` on another just work.
- **Projects can live in different folders on each Mac.** Stradl finds each project on each Mac by its git remote, so `~/code/app` here and `~/Projects/app` there are the same project.
- **Conversations never fork silently.** Continue the same session on two Macs before they sync and Stradl merges both sides, so every turn survives.
- **Your storage, your keys.** Sessions are compressed and encrypted on your Mac with [age](https://age-encryption.org) before they leave it. Azure (or R2, S3, GCS, WebDAV) only ever sees ciphertext.
- **Set and forget.** A small menu bar app syncs in the background, at login, after you wake your Mac, and whenever a session changes.

## Install

You need about ten minutes: two for Azure, then a couple per Mac.

### 1. Create your storage (once)

Open [Azure Cloud Shell](https://shell.azure.com) (Bash) and run:

```bash
curl -fsSL https://github.com/MattCampana/stradl/releases/latest/download/azure-setup.sh | bash -s -- \
  --resource-group rg-stradl --location eastus --account <yourname>stradl
```

Pick a storage account name of 3 to 24 lowercase letters and digits. The script creates a private container with versioning, 30-day soft delete and a rule that clears out old versions after 30 days, then prints a **SAS token** scoped to that one container. Save the account name, the container name and the token in your password manager. Details are in [docs/azure.md](docs/azure.md).

### 2. Install on each Mac

```bash
/bin/bash -c "$(curl -fsSL https://github.com/MattCampana/stradl/releases/latest/download/install.sh)"
```

The installer needs no admin rights, no Homebrew and no Go. It downloads the release for your Mac, verifies its checksum, installs `stradl` into `~/.local/bin`, then walks you through setup:

1. Choose **Azure Blob Storage** and paste the account, the container and the SAS token (the token stays hidden).
2. Choose an **encryption passphrase**. Use the same passphrase on every Mac; it never leaves your Mac and can't be recovered, so keep it in your password manager.
3. The menu bar app installs and starts at login.

Repeat on your other Macs with the same storage details and the same passphrase.

> Use Stradl in one macOS login per person. If a Mac has a separate work login, don't install it there: whatever is in that login's `~/.claude` would sync into your storage.

## Using it

Look for the Stradl mark in the menu bar:

| Menu item | What it does |
|-----------|--------------|
| **Synced 2 min ago · 9 sessions** | Status at a glance |
| **Open Dashboard…** | Your Macs, this week's sessions and the sync state of each one, in your browser |
| **Sync Now** | Sync immediately |
| **Pause Syncing** | Stop syncing until you resume (handy on a metered connection) |
| **Restart Claude to show N new sessions** | Appears when sessions arrive from another Mac; see below |
| **Open Log** | `~/.stradl/agent.log` |

**The dashboard shows what is synced and what you worked on.** Choose **Open Dashboard…** (or run `stradl dashboard`) for a page with every Mac syncing into your storage and when each last synced, the last 7 days of sessions day by day (prompts, files touched, which Mac), a searchable list of every session on this Mac with its sync state, and what the next sync will do. It runs on this Mac only, at a private `127.0.0.1` address, and nothing on it leaves the Mac.

**New sessions from another Mac appear after Claude restarts.** The Claude desktop app reads its sidebar when it launches, so Stradl lets you know when new sessions arrive and offers to restart Claude for you. Sessions already in your sidebar update in place.

**An open session is never rewritten underneath you.** If a session is running on this Mac, updates from other Macs wait until you close it.

**Deleting a session only deletes it on that Mac.** The other Macs keep their copy, and so does your storage. If you continue that session elsewhere later, it comes back.

**Deleting a file in a project folder deletes it on every Mac,** the way it would in one folder. Nothing is lost: on the other Macs it moves to `~/.stradl/trash` for 30 days, and storage keeps old versions for 30 days too. If one sync would delete more than half of a folder at once, Stradl stops and asks you to confirm with `stradl sync --allow-mass-delete`.

**Sessions you use from your phone show once.** A session you control from the Claude app on your phone (Remote Control) already appears on every Mac through your Claude account, so Stradl doesn't add a second copy. It syncs normally once Remote Control is off.

### Commands

| Command | |
|---------|---|
| `stradl sync` | Two-way sync now (`--allow-mass-delete` to confirm a large delete) |
| `stradl status` | Agent status, last sync, and what's waiting to sync |
| `stradl dashboard` | Open the dashboard in your browser (`--no-open` prints its address) |
| `stradl diff` | The files the next sync would upload, download, merge, delete or move to the trash |
| `stradl agent install` / `uninstall` | Start or stop the background app at login |
| `stradl agent pause` / `resume` | Pause or resume background syncing |
| `stradl update` | Update to the latest release |
| `stradl init` | Set up storage and encryption again |

## How it works

Every sync compares each file on your Mac with the copy in storage and with the version both last agreed on:

- **New on either side:** copied to the other side.
- **Changed on one side:** that side wins.
- **Changed on both sides:** transcripts are merged line by line (every message is kept, in time order); sidebar entries keep the most recently active version; any other file keeps the newer copy and saves the other as a `.conflict` copy.
- **A project folder that was already on two Macs:** your copy of each file stays put and the other Mac's is saved beside it, except that a file git says you never touched takes the other Mac's edit. In a git repository the Mac that is ahead wins, and a branch two Macs moved apart keeps both commits (the other one as `stradl/<branch>-<commit>`).
- **Deleted:** sessions, prompt history and sidebar entries stay deleted on that Mac only; every other file is deleted everywhere, into the other Macs' trash.
- **Missing from storage:** restored from this Mac.

Remote changes are detected by version tags, not timestamps, so a Mac with a wrong clock can't lose data. Uploads are compare-and-swap: if two Macs sync the same file at the same moment, the slower one merges instead of overwriting. Paths inside sessions are stored with a portable `${HOME}` token and rewritten for each Mac on download.

## Security

- Encryption happens on your Mac. Storage holds only age-encrypted, gzip-compressed blobs.
- The SAS token can reach exactly one container. Revoke it at any time by deleting its stored access policy in the Azure portal.
- Your passphrase derives the encryption key on each Mac and is never stored or uploaded. Lose it and the synced copies can't be decrypted (your local sessions are unaffected).
- Config lives in `~/.stradl/config.yaml` with permissions `0600`. Tokens are redacted from logs.
- Blob versioning and 30-day soft delete give you an undo for anything that goes wrong: whatever is overwritten or deleted stays recoverable for at least 30 days.

## Limitations

- **Build output isn't synced** (`node_modules`, `.venv`, `__pycache__`, `.next` and the like): each Mac rebuilds its own. Set `sync_build_artifacts: true` in `~/.stradl/config.yaml` to include it, and `folder_exclude` to leave out more.
- **A folder git is working in waits.** While git holds a lock in a repository (a commit or checkout in progress), that folder sits the sync out and catches up on the next one.
- **macOS only** for the menu bar app. Linux builds run the same sync engine headless (`stradl agent run --headless`).
- **A project you haven't cloned on a Mac** can't be opened there; the session still syncs, and the Claude app tells you the folder is missing. Stradl looks for clones in `~/code`, `~/Code`, `~/src`, `~/dev`, `~/Developer`, `~/Projects`, `~/projects`, `~/repos`, `~/git`, `~/GitHub`, `~/github`, `~/work`, `~/Work`, `~/workspace`, `~/Documents` and `~/Desktop`, up to four levels deep, and matches them by git remote. Clone the repo into one of those folders, or map the folder you use with `path_map` in `~/.stradl/config.yaml` (see [How it works](docs/how-it-works.md#different-usernames-different-folders)).
- **Terminal sessions expire after 30 days of inactivity** under Claude Code's default `cleanupPeriodDays`, on every Mac. Sessions started in the Claude desktop app are exempt by default, and the exemption travels with the transcript. To keep terminal sessions longer, raise `cleanupPeriodDays` in `~/.claude/settings.json` on each Mac.
- The binaries aren't notarized by Apple. The curl installer doesn't trigger Gatekeeper; a browser download would.

## Building from source

```bash
make build          # bin/stradl for this Mac
make test           # go test ./...
make release VERSION=0.1.0 RELEASE_REPO=you/stradl   # all four release archives + checksums
```

The brand assets are generated from [assets/brand/src](assets/brand/src) (`python3 assets/brand/src/build.py`).

## Credits

Stradl is built on [tawanorg/claude-sync](https://github.com/tawanorg/claude-sync) (MIT): its encryption, storage adapters and path mapping are the foundation the union sync engine, the desktop sidebar sync and the menu bar app sit on. Thanks, Tawan.

MIT licensed. See [LICENSE](LICENSE).
