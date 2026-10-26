# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository Purpose

**Stradl** ("Musical chairs for your Claude sessions.") is a Go CLI plus macOS menu bar agent that keeps every Claude Code session on every one of a person's Macs, including the Claude desktop app's Code-tab sidebar, with union semantics: every session any device creates ends up on all devices. Files are gzip-compressed and age-encrypted before upload to storage the user owns (Azure Blob primarily; R2, S3, GCS, S3-compatible and WebDAV also work). It's a fork of tawanorg/claude-sync (MIT); the Go module path and `cmd/claude-sync` directory keep upstream's names to ease merges, but the binary, config dir and all user-facing text are `stradl`.

## Common Commands

```bash
make build            # bin/stradl for this Mac (cgo on darwin, for the menu bar)
make test             # go test ./...
make check            # gofmt -l, go vet, go test -short
make lint             # golangci-lint (.golangci.yml) + shellcheck on install.sh and scripts/
make vulncheck        # govulncheck, as CI runs it
make release VERSION=0.1.0 RELEASE_REPO=owner/stradl   # 4 archives + checksums.txt in dist/

go test -v -run TestSync ./internal/sync/            # multi-device engine tests (in-memory CAS store)
go test ./internal/storage/azure/                    # needs Azurite on 127.0.0.1:10000 (azurite-blob --loose); skipped otherwise
scripts/test-install.sh dist                          # end-to-end installer test against a local HTTP server
scripts/test-azure-setup.sh                           # azure-setup.sh against a fake az CLI
```

Version and release repo are injected at build time: `-ldflags "-X main.version=<semver> -X main.releaseRepo=<owner>/stradl"`.

## Architecture

- **CLI** — `cmd/claude-sync/`. Cobra root in `main.go`; other files register commands via `extraCommands` from `init()`: `sync_cmd.go` (`sync`), `status_cmd.go` (`status`, `diff` — dry-run previews), `agent_cmd.go` (`agent run|install|uninstall|restart|pause|resume|status`), `azure_wizard.go`, `release.go` (`update`, `changelog` against `releaseRepo`). `push`/`pull` are aliases for the two-way sync. `tray_darwin.go` is the fyne.io/systray menu bar (darwin+cgo only; `tray_other.go` otherwise).
- **Sync engine** — `internal/sync/engine.go`. `Syncer.Sync` / `Syncer.Preview` run one reconcile pass over two roots: `~/.claude` (scope paths) and the desktop session index (`config.DesktopSessionsDir()`, namespaced `_desktop/sessions/`, only `<acct>/<org>/local_*.json`). Per file: local hash vs state, remote ETag vs state (never timestamps), then upload (compare-and-swap via `storage.Conditional`), download (atomic rename or append-only), or `reconcileBoth` (JSONL line-union merge in `merge.go`, newest-`lastActivityAt` for desktop records, `.conflict` copy otherwise). Local deletes stay local (`FileState.LocalDeleted`); vanished remote copies are restored. `live.go` defers remote changes to transcripts owned by a running Claude process (`~/.claude/sessions/<pid>.json`). `desktop.go` adds a git-remote hint on upload and remaps/creates the session folder on download. `run.go` wraps a pass in the device lock (`lock.go`, flock on `~/.stradl/sync.lock`). `probe.go` provides cheap local/remote fingerprints for the agent.
- **Agent** — `internal/agent/`. `Runner` loop (local poll 20s + debounce, remote probe 60s, full interval, wake detection, pause file, Sync Now), `status.json`, log rotation, launchd plist (`com.breakpointdata.stradl`), desktop-app restart helper, menu bar icons in `icon/` (generated from `assets/brand`).
- **Storage** — `internal/storage/`. `Storage` interface plus optional `Conditional` (`UploadIf`, `DownloadWithETag`, sentinels `ErrPreconditionFailed`, `ErrNotFound`). `azure/` implements both (SAS or account key; ETags normalized unquoted; SAS signatures redacted from errors). Adapters self-register via `init()`; anything calling `storage.New` must blank-import them.
- **Crypto** — `internal/crypto/`. age X25519; passphrase mode uses Argon2id with a **fixed salt** so every device derives the same key. Don't "fix" the salt.
- **Config** — `internal/config/`. `~/.stradl/config.yaml` (0600). `scope: sessions` is the default for new installs; `desktop_sync` (nil = on), `sync_interval_seconds`, `path_map`.
- **Paths** — `internal/sync/paths.go`. `${HOME}` (and `path_map` tokens) in remote keys and in file content; project dir names use Claude Code's encoding (non-alphanumerics → `-`).

## Distribution

- `.github/workflows/release.yml`: on `v*` tags, a macos-15 runner checks it got the go.mod `toolchain` Go, tests, runs govulncheck and `scripts/build-release.sh` (darwin arm64/amd64 with cgo and `MACOSX_DEPLOYMENT_TARGET=13.0`, linux arm64/amd64 without), and publishes `stradl_<os>_<arch>.tar.gz` + `checksums.txt` + `install.sh` + `azure-setup.sh` with `gh release create`.
- go.mod's `toolchain` line is the Go release binaries are built with (setup-go reads it); bump it for Go security releases.
- `install.sh` (curl one-liner from `.../releases/latest/download/install.sh`) installs to `~/.local/bin` with checksum verification and adds the PATH line (`~/.zprofile`; `~/.bash_profile` for a bash login shell; `~/.profile` on Linux). On a new machine with a TTY it runs `stradl init` and `stradl agent install`; on a Mac with the agent plist it reruns `stradl agent install` without needing a TTY, retrying while the old agent finishes a sync. `scripts/azure-setup.sh` provisions Azure from Cloud Shell, including a lifecycle rule that deletes previous blob versions after 30 days.
- `install.sh` on main keeps the `REPLACE_OWNER/stradl` placeholder: release.yml rewrites the copy it attaches to each release, which is the one the README links. Only README, docs and the `release.go` default get the real owner when publishing (release builds inject it via ldflags anyway).

## Gotchas

- **The desktop app reads its sidebar only at launch.** Newly synced `local_*.json` records appear after Claude restarts; the agent counts them (`pendingDesktopRestart`) and offers a restart.
- **Never rewrite a live transcript.** Keep the `live.go` deferral; a running Claude process holds the conversation in memory.
- **ETags, not clocks.** Don't reintroduce timestamp comparisons for change detection; compare normalized ETags (`normETag`).
- **Union semantics.** Don't add remote deletes to the default path. Deletes are local-only by design.
- **Long project paths.** Claude Code truncates encoded project dir names over ~200 characters with a hash suffix; the `${HOME}` prefix rewrite still applies, but such dirs won't match the hash Claude Code would compute for the other device's path. The desktop app finds transcripts by session ID, so sessions still open there.
- **Symlinks are skipped** by `GetLocalFiles`.
- **Do not add destructive operations** to the default code path without an explicit flag or confirmation.
