#!/usr/bin/env bash
# Stradl installer. Musical chairs for your Claude sessions.
#
#   /bin/bash -c "$(curl -fsSL https://github.com/REPLACE_OWNER/stradl/releases/latest/download/install.sh)"
#
# Downloads the release for this OS/arch from GitHub Releases, verifies it
# against checksums.txt, and installs it to ~/.local/bin/stradl. Needs no sudo,
# Homebrew, or Go, so it works for a Standard (non-admin) macOS user. On a new
# machine with a terminal it then runs `stradl init` and, on macOS,
# `stradl agent install`. On a Mac that already runs the agent it restarts the
# agent on the new binary, terminal or not.
#
# This file must stay compatible with /bin/bash 3.2 (what macOS ships), so no
# associative arrays, no ${var,,}, and no expanding empty arrays under set -u.
set -euo pipefail

# This copy keeps the placeholder. The release workflow rewrites it in the
# copy it attaches to each release, so .../releases/latest/download/install.sh
# needs no STRADL_REPO.
REPO="${STRADL_REPO:-REPLACE_OWNER/stradl}"

BIN_DIR="$HOME/.local/bin"
BIN="$BIN_DIR/stradl"
CONFIG_FILE="$HOME/.stradl/config.yaml"
AGENT_PLIST="$HOME/Library/LaunchAgents/com.breakpointdata.stradl.plist"

PATH_MARKER="# Added by the Stradl installer: put ~/.local/bin on PATH"
# Single quotes on purpose: $HOME and $PATH expand when the profile is read.
# shellcheck disable=SC2016
PATH_LINE='case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) export PATH="$HOME/.local/bin:$PATH" ;; esac'

# Set as main runs; read by the cleanup trap and the summary.
OS=""
ARCH=""
TMP=""
STAGED=""
UPDATED_PROFILES=""
KNOWN_PROFILES=""
PATH_STATE=""
SETUP_STATE=""
APP_STATE=""
SETUP_FAILED=0

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  BOLD=$'\033[1m'
  DIM=$'\033[2m'
  RED=$'\033[31m'
  GREEN=$'\033[32m'
  YELLOW=$'\033[33m'
  RESET=$'\033[0m'
else
  BOLD="" DIM="" RED="" GREEN="" YELLOW="" RESET=""
fi

info() { printf '%s==>%s %s\n' "$BOLD" "$RESET" "$*"; }
warn() { printf '%swarning:%s %s\n' "$YELLOW" "$RESET" "$*" >&2; }
die() {
  printf '%serror:%s %s\n' "$RED" "$RESET" "$*" >&2
  exit 1
}

# pretty shows a path under $HOME as ~/...
pretty() {
  case "$1" in
    "$HOME"/*) printf '%s' "~${1#"$HOME"}" ;;
    *) printf '%s' "$1" ;;
  esac
}

usage() {
  cat <<'EOF'
Install Stradl, which keeps Claude Code sessions in sync across your Macs.

Usage:
  /bin/bash -c "$(curl -fsSL <url>/install.sh)"
  curl -fsSL <url>/install.sh | bash -s -- [--no-setup]

Options:
  --no-setup   install the binary only; skip `stradl init` and the agent
  -h, --help   show this help

Environment:
  STRADL_REPO      GitHub owner/name to install from
  STRADL_VERSION   install this version (e.g. 1.2.3) instead of the latest
  STRADL_BASE_URL  download the release assets from this URL instead
  STRADL_NO_SETUP  set to 1 to behave like --no-setup
  STRADL_APP_DIR   install Stradl.app here instead of /Applications
EOF
}

cleanup() {
  if [ -n "$STAGED" ]; then rm -f "$STAGED"; fi
  if [ -n "$TMP" ]; then rm -rf "$TMP"; fi
}

detect_platform() {
  case "$(uname -s)" in
    Darwin) OS=darwin ;;
    Linux) OS=linux ;;
    *) die "unsupported OS: $(uname -s) (Stradl runs on macOS and Linux)" ;;
  esac
  case "$(uname -m)" in
    arm64 | aarch64) ARCH=arm64 ;;
    x86_64 | amd64) ARCH=amd64 ;;
    *) die "unsupported CPU architecture: $(uname -m)" ;;
  esac
  # A shell running under Rosetta reports x86_64 on Apple Silicon. Install
  # the native build anyway.
  if [ "$OS" = darwin ] && [ "$ARCH" = amd64 ] &&
    [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
    ARCH=arm64
  fi
}

require() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

sha256() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{ print $1 }'
  else
    sha256sum "$1" | awk '{ print $1 }'
  fi
}

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

# base_url prints where the release assets live.
base_url() {
  if [ -n "${STRADL_BASE_URL:-}" ]; then
    printf '%s' "${STRADL_BASE_URL%/}"
    return 0
  fi
  case "$REPO" in
    *REPLACE_OWNER*)
      die "this copy of install.sh doesn't know which GitHub repo to install from.
Tell it with STRADL_REPO, for example:
  STRADL_REPO=owner/stradl /bin/bash -c \"\$(curl -fsSL <url>/install.sh)\""
      ;;
  esac
  [[ "$REPO" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] ||
    die "STRADL_REPO must look like owner/name, got '$REPO'"
  if [ -n "${STRADL_VERSION:-}" ]; then
    local v="${STRADL_VERSION#v}"
    [[ "$v" =~ ^[0-9A-Za-z.+-]+$ ]] || die "STRADL_VERSION looks wrong: '$STRADL_VERSION'"
    printf '%s' "https://github.com/$REPO/releases/download/v$v"
  else
    printf '%s' "https://github.com/$REPO/releases/latest/download"
  fi
}

fetch() {
  local url="$1" dest="$2"
  local proto="=https,http"
  # Only allow a plain-http URL when it was asked for (local testing); an
  # https download may not be redirected down to http.
  case "$url" in https://*) proto="=https" ;; esac
  if ! curl -fsSL --proto "$proto" --retry 3 --connect-timeout 20 -o "$dest" "$url"; then
    die "download failed: $url
Is there a published release with an asset for $OS/$ARCH?"
  fi
}

# start_bash_profile creates ~/.bash_profile for a bash login shell. bash
# reads only the first of ~/.bash_profile, ~/.bash_login and ~/.profile, so a
# new ~/.bash_profile first sources whichever of the other two bash was
# reading, and nothing the user set up there is lost.
start_bash_profile() {
  local profile="$HOME/.bash_profile" fallback=""
  if [ -e "$profile" ]; then return 0; fi
  if [ -f "$HOME/.bash_login" ]; then
    fallback=".bash_login"
  elif [ -f "$HOME/.profile" ]; then
    fallback=".profile"
  fi
  if [ -z "$fallback" ]; then return 0; fi
  # A failed write is reported by add_path_line, which tries the same file.
  { printf '%s\n%s\n' \
    "# Created by the Stradl installer. bash read ~/$fallback before this file existed." \
    "if [ -f \"\$HOME/$fallback\" ]; then . \"\$HOME/$fallback\"; fi" >"$profile"; } 2>/dev/null || true
}

# add_path_line appends the marked PATH line to a shell profile once.
add_path_line() {
  local profile="$1"
  if [ -f "$profile" ] && grep -qF "$PATH_MARKER" "$profile"; then
    KNOWN_PROFILES="$KNOWN_PROFILES $(pretty "$profile")"
    return 0
  fi
  if ! { printf '\n%s\n%s\n' "$PATH_MARKER" "$PATH_LINE" >>"$profile"; } 2>/dev/null; then
    warn "couldn't update $(pretty "$profile"); add $(pretty "$BIN_DIR") to your PATH yourself"
    return 0
  fi
  UPDATED_PROFILES="$UPDATED_PROFILES $(pretty "$profile")"
}

ensure_path() {
  case ":$PATH:" in
    *":$BIN_DIR:"*)
      PATH_STATE="already on PATH"
      return 0
      ;;
  esac
  local shell_name="${SHELL:-}" had_bash_profile=0
  shell_name="${shell_name##*/}"
  if [ -f "$HOME/.bash_profile" ]; then had_bash_profile=1; fi
  # zsh is the macOS login shell.
  if [ "$OS" = darwin ] || [ "$shell_name" = zsh ] || [ -f "$HOME/.zprofile" ]; then
    add_path_line "$HOME/.zprofile"
  fi
  # A bash login shell reads ~/.bash_profile, so create it when bash is the
  # login shell (start_bash_profile keeps an existing ~/.profile working).
  if [ "$shell_name" = bash ] || [ "$had_bash_profile" = 1 ]; then
    start_bash_profile
    add_path_line "$HOME/.bash_profile"
  fi
  # Other login shells on Linux, and desktop sessions, read ~/.profile.
  if [ "$OS" = linux ] && [ "$shell_name" != zsh ] && [ "$had_bash_profile" = 0 ]; then
    add_path_line "$HOME/.profile"
  fi
  if [ -n "$UPDATED_PROFILES" ]; then
    PATH_STATE="added to${UPDATED_PROFILES}; open a new terminal to pick it up"
  elif [ -n "$KNOWN_PROFILES" ]; then
    PATH_STATE="set up in${KNOWN_PROFILES}; open a new terminal to pick it up"
  else
    PATH_STATE="not on PATH; add $(pretty "$BIN_DIR") to it in your shell profile"
  fi
}

# restart_agent reloads an installed agent so it runs the binary just
# installed. It needs no terminal, so an upgrade over ssh or from a script
# restarts the agent too. The reload waits for the old agent to exit, which
# takes up to 10 seconds when it is in the middle of a sync, longer than
# `stradl agent install` keeps retrying on its own, so retry here as well.
restart_agent() {
  local attempt
  info "Restarting the background agent"
  for attempt in 1 2 3 4; do
    if [ "$attempt" -gt 1 ]; then
      warn "the agent hasn't restarted yet (it may be finishing a sync); trying again"
      sleep 3
    fi
    if "$BIN" agent install </dev/null; then
      SETUP_STATE="$SETUP_STATE; agent restarted"
      return 0
    fi
  done
  SETUP_FAILED=1
  SETUP_STATE="$SETUP_STATE; agent restart failed, run 'stradl agent install'"
}

run_setup() {
  if [ -f "$CONFIG_FILE" ]; then
    SETUP_STATE="already configured ($(pretty "$CONFIG_FILE"))"
    # An upgrade replaced the binary under a running agent; restart it, but
    # only if this Mac had it installed in the first place.
    if [ "$OS" = darwin ] && [ -f "$AGENT_PLIST" ]; then
      restart_agent
    fi
    return 0
  fi

  # `stradl init` asks questions. curl | bash leaves stdin on the pipe, so
  # talk to the terminal directly, and skip setup when there isn't one (CI,
  # ssh without -t).
  if ! { : </dev/tty; } 2>/dev/null; then
    SETUP_STATE="skipped (no terminal); run 'stradl init' to finish"
    return 0
  fi

  echo
  info "Setting up Stradl"
  if ! "$BIN" init </dev/tty; then
    SETUP_FAILED=1
    SETUP_STATE="'stradl init' did not finish; run it again when ready"
    return 0
  fi
  SETUP_STATE="configured"

  if [ "$OS" = darwin ]; then
    echo
    info "Installing the background agent"
    if "$BIN" agent install </dev/tty; then
      SETUP_STATE="configured; background agent installed"
    else
      SETUP_FAILED=1
      SETUP_STATE="configured, but the agent did not install; run 'stradl agent install'"
    fi
  fi
}

# install_app puts Stradl.app, the Mac app, in /Applications, or in
# ~/Applications when /Applications isn't writable without a password. It is
# optional: a release without it, or any problem here, leaves the
# command-line install as it is and says so.
install_app() {
  local base="$1" want got zip dir dest stage old
  [ "$OS" = darwin ] || return 0
  command -v ditto >/dev/null 2>&1 || return 0
  want="$(awk '{ n = $2; sub(/^\*/, "", n); if (n == "Stradl.app.zip") { print $1; exit } }' "$TMP/checksums.txt")"
  [ -n "$want" ] || return 0 # this release has no app
  zip="$TMP/Stradl.app.zip"
  if ! curl -fsSL --proto "=https,http" --retry 3 --connect-timeout 20 -o "$zip" "$base/Stradl.app.zip"; then
    APP_STATE="not installed (download failed); the menu bar app works without it"
    return 0
  fi
  got="$(sha256 "$zip")"
  if [ "$(lower "$want")" != "$(lower "$got")" ]; then
    APP_STATE="not installed (checksum mismatch)"
    return 0
  fi
  dir="${STRADL_APP_DIR:-/Applications}"
  [ -w "$dir" ] || [ -n "${STRADL_APP_DIR:-}" ] || dir="$HOME/Applications"
  mkdir -p "$dir" || { APP_STATE="not installed (can't create $(pretty "$dir"))"; return 0; }
  stage="$TMP/app"
  mkdir -p "$stage"
  if ! ditto -x -k "$zip" "$stage" || [ ! -d "$stage/Stradl.app" ]; then
    APP_STATE="not installed (couldn't unpack it)"
    return 0
  fi
  xattr -dr com.apple.quarantine "$stage/Stradl.app" 2>/dev/null || true
  dest="$dir/Stradl.app"
  old="$dir/.Stradl.app.old.$$"
  if [ -e "$dest" ]; then mv "$dest" "$old" || { APP_STATE="not installed (couldn't replace $(pretty "$dest"))"; return 0; }; fi
  if ditto "$stage/Stradl.app" "$dest"; then
    rm -rf "$old"
    APP_STATE="$(pretty "$dest")"
    info "Installed $(pretty "$dest")"
  else
    rm -rf "$dest"
    [ -e "$old" ] && mv "$old" "$dest"
    APP_STATE="not installed (copy failed)"
  fi
}

main() {
  local setup=1
  case "${STRADL_NO_SETUP:-}" in
    "" | 0 | false | no) ;;
    *) setup=0 ;;
  esac
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --no-setup) setup=0 ;;
      -h | --help)
        usage
        return 0
        ;;
      --) ;;
      *) die "unknown option: $1 (see --help)" ;;
    esac
    shift
  done

  detect_platform
  require curl
  require tar
  command -v shasum >/dev/null 2>&1 || command -v sha256sum >/dev/null 2>&1 ||
    die "shasum or sha256sum is required to verify the download"

  local base asset
  base="$(base_url)"
  asset="stradl_${OS}_${ARCH}.tar.gz"

  local tmp_root="${TMPDIR:-/tmp}"
  TMP="$(mktemp -d "${tmp_root%/}/stradl-install.XXXXXX")"
  trap cleanup EXIT

  info "Downloading $asset"
  fetch "$base/$asset" "$TMP/$asset"
  fetch "$base/checksums.txt" "$TMP/checksums.txt"

  local want got
  want="$(awk -v f="$asset" '{ n = $2; sub(/^\*/, "", n); if (n == f) { print $1; exit } }' "$TMP/checksums.txt")"
  [ -n "$want" ] || die "checksums.txt has no entry for $asset; nothing was installed"
  got="$(sha256 "$TMP/$asset")"
  if [ "$(lower "$want")" != "$(lower "$got")" ]; then
    die "checksum mismatch for $asset; nothing was installed
  expected $want
  got      $got"
  fi
  info "Verified checksum"

  # Extract only the binary, so a stray entry in the archive can't land
  # anywhere else.
  mkdir "$TMP/x"
  tar -xzf "$TMP/$asset" -C "$TMP/x" stradl 2>/dev/null || die "couldn't unpack $asset"
  if [ ! -f "$TMP/x/stradl" ] || [ -L "$TMP/x/stradl" ]; then
    die "$asset doesn't contain a stradl executable"
  fi

  # Refuse to replace a working install with something that won't start.
  local version
  chmod 755 "$TMP/x/stradl"
  version="$("$TMP/x/stradl" --version 2>/dev/null)" ||
    die "the downloaded stradl doesn't run on this machine ($OS/$ARCH); nothing was installed"
  version="${version##* }"

  mkdir -p "$BIN_DIR" || die "couldn't create $(pretty "$BIN_DIR")"
  [ ! -d "$BIN" ] || die "$(pretty "$BIN") is a directory; move it out of the way and retry"
  # Stage next to the target and rename into place: atomic, and safe while
  # the agent is running the old binary.
  STAGED="$BIN_DIR/.stradl.install.$$"
  cp "$TMP/x/stradl" "$STAGED"
  chmod 755 "$STAGED"
  if [ "$OS" = darwin ]; then
    xattr -d com.apple.quarantine "$STAGED" 2>/dev/null || true
  fi
  mv -f "$STAGED" "$BIN"
  STAGED=""
  info "Installed $(pretty "$BIN")"

  install_app "$base"
  ensure_path

  if [ "$setup" = 1 ]; then
    run_setup
  else
    SETUP_STATE="skipped (--no-setup); run 'stradl init' to finish"
    if [ "$OS" = darwin ] && [ -f "$AGENT_PLIST" ]; then
      SETUP_STATE="skipped (--no-setup); run 'stradl agent install' to restart the agent on the new version"
    fi
  fi

  echo
  printf '%sStradl %s is installed.%s\n' "$GREEN$BOLD" "$version" "$RESET"
  printf '  %sbinary%s  %s\n' "$DIM" "$RESET" "$(pretty "$BIN")"
  printf '  %sPATH%s    %s\n' "$DIM" "$RESET" "$PATH_STATE"
  printf '  %ssetup%s   %s\n' "$DIM" "$RESET" "$SETUP_STATE"
  if [ -n "$APP_STATE" ]; then
    printf '  %sapp%s     %s\n' "$DIM" "$RESET" "$APP_STATE"
  fi
  if [ "$PATH_STATE" != "already on PATH" ]; then
    # shellcheck disable=SC2016 # printed for the user to paste, unexpanded
    printf '\nTo use it in this terminal now:\n  export PATH="$HOME/.local/bin:$PATH"\n'
  fi

  if [ "$SETUP_FAILED" = 1 ]; then
    return 1
  fi
}

main "$@"
