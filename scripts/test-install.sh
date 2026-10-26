#!/usr/bin/env bash
# End-to-end test for install.sh against locally served release assets.
#
# Usage: scripts/test-install.sh [DIST_DIR]
#
# With no DIST_DIR it builds the assets first: the full release set via
# scripts/build-release.sh on macOS, or just the host's tarball elsewhere.
# The assets are served from 127.0.0.1 with python3's http.server and
# install.sh runs against throwaway HOME directories, so nothing touches the
# real ~/.local/bin, shell profiles, or ~/.stradl. On macOS the installer runs
# under /bin/bash to keep it honest about bash 3.2. The agent-restart cases
# install a stand-in stradl that only logs its arguments, so launchd and the
# real agent are never touched either.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="0.0.0-installtest"
MARKER="# Added by the Stradl installer"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/stradl-installtest.XXXXXX")"
SERVER_PID=""
cleanup() {
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

FAILURES=0
pass() { printf 'ok    %s\n' "$*"; }
fail() {
  printf 'FAIL  %s\n' "$*"
  FAILURES=$((FAILURES + 1))
}

case "$(uname -s)" in
  Darwin) OS=darwin SH=/bin/bash LOGIN_SHELL=/bin/zsh PROFILE=.zprofile ;;
  Linux) OS=linux SH="$(command -v bash)" LOGIN_SHELL=/bin/bash PROFILE=.profile ;;
  *) echo "unsupported OS" >&2 && exit 1 ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) ARCH=arm64 ;;
  *) ARCH=amd64 ;;
esac
ASSET="stradl_${OS}_${ARCH}.tar.gz"
PY="$(command -v python3)"

sha() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$@"
  else
    sha256sum "$@"
  fi
}

# --- assets -----------------------------------------------------------------

DIST="${1:-}"
if [ -z "$DIST" ]; then
  DIST="$WORK/dist"
  if [ "$OS" = darwin ]; then
    "$ROOT/scripts/build-release.sh" "$VERSION" example/stradl "$DIST"
  else
    echo "==> building $ASSET for the host"
    mkdir -p "$DIST" "$WORK/stage"
    (cd "$ROOT" && CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=$VERSION" -o "$WORK/stage/stradl" ./cmd/claude-sync)
    tar -czf "$DIST/$ASSET" -C "$WORK/stage" stradl
    (cd "$DIST" && sha "$ASSET" >checksums.txt)
  fi
fi
DIST="$(cd "$DIST" && pwd)"
[ -f "$DIST/$ASSET" ] || {
  echo "missing $DIST/$ASSET" >&2
  exit 1
}
WANT_VERSION="$(tar -xzOf "$DIST/$ASSET" stradl >"$WORK/probe" && chmod +x "$WORK/probe" && "$WORK/probe" --version)"
WANT_VERSION="${WANT_VERSION##* }"

# Tampered copies: asset bytes changed, and checksum line missing.
mkdir -p "$WORK/srv/good" "$WORK/srv/badsum" "$WORK/srv/noentry" "$WORK/srv/empty"
cp "$DIST/$ASSET" "$DIST/checksums.txt" "$WORK/srv/good/"
cp "$DIST/$ASSET" "$DIST/checksums.txt" "$WORK/srv/badsum/"
printf 'x' >>"$WORK/srv/badsum/$ASSET"
cp "$DIST/$ASSET" "$WORK/srv/noentry/"
grep -v " $ASSET\$" "$DIST/checksums.txt" >"$WORK/srv/noentry/checksums.txt" || true

# A stand-in stradl for the agent-restart cases. It logs each call (and
# whether stdin is a terminal) to ~/fake-stradl.log and fails the first N
# calls, N read from ~/fake-fail-count, the way `stradl agent install` fails
# while the old agent is still shutting down.
mkdir -p "$WORK/fake" "$WORK/srv/fake"
cat >"$WORK/fake/stradl" <<'EOF'
#!/bin/sh
case "${1:-}" in
  --version)
    echo "stradl version 0.0.0-fake"
    exit 0
    ;;
esac
if [ -t 0 ]; then stdin=tty; else stdin=notty; fi
echo "$* $stdin" >>"$HOME/fake-stradl.log"
fails=0
if [ -f "$HOME/fake-fail-count" ]; then fails="$(cat "$HOME/fake-fail-count")"; fi
if [ "$(grep -c . "$HOME/fake-stradl.log")" -le "$fails" ]; then
  echo "launchctl bootstrap failed: exit status 5: Bootstrap failed: 5: Input/output error" >&2
  exit 1
fi
EOF
chmod 755 "$WORK/fake/stradl"
tar -czf "$WORK/srv/fake/$ASSET" -C "$WORK/fake" stradl
(cd "$WORK/srv/fake" && sha "$ASSET" >checksums.txt)

# --- server -----------------------------------------------------------------

PORT="$(python3 -I -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
python3 -I -m http.server --bind 127.0.0.1 --directory "$WORK/srv" "$PORT" >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
URL="http://127.0.0.1:$PORT"
for _ in $(seq 1 50); do
  curl -fsS -o /dev/null "$URL/good/checksums.txt" 2>/dev/null && break
  sleep 0.1
done
curl -fsS -o /dev/null "$URL/good/checksums.txt" || {
  echo "http server did not come up" >&2
  exit 1
}

# NOTTY runs a command in a new session, without a controlling terminal, so
# the installer sees "no terminal" whether or not this script runs in one.
# shellcheck disable=SC2016 # Python source, not shell
NOTTY='
import os, sys
try:
    os.setsid()
except OSError:  # already a process group leader: setsid in a child
    pid = os.fork()
    if pid:
        sys.exit(os.waitstatus_to_exitcode(os.waitpid(pid, 0)[1]))
    os.setsid()
os.execvp(sys.argv[1], sys.argv[1:])
'

# run_install HOME BASE_URL [args...] runs install.sh piped into bash, the way
# `curl ... | bash -s --` does, without a terminal, and leaves output in $OUT
# and status in $RC. NO_SETUP=0 lets setup run; RUN_SHELL sets $SHELL.
OUT=""
RC=0
run_install() {
  local home="$1" base="$2"
  shift 2
  mkdir -p "$home"
  set +e
  OUT="$("$PY" -I -c "$NOTTY" env -i HOME="$home" PATH="/usr/bin:/bin:/usr/sbin:/sbin" \
    SHELL="${RUN_SHELL:-$LOGIN_SHELL}" TMPDIR="$WORK" STRADL_BASE_URL="$base" \
    STRADL_NO_SETUP="${NO_SETUP-1}" "$SH" -s -- "$@" <"$ROOT/install.sh" 2>&1)"
  RC=$?
  set -e
}

count_lines() {
  local pattern="$1" f="$2"
  if [ -f "$f" ]; then grep -cF -- "$pattern" "$f" || true; else echo 0; fi
}

marker_count() { count_lines "$MARKER" "$1"; }

# --- cases ------------------------------------------------------------------

H1="$WORK/home1"

# 1. Fresh install, via bash -c "$(curl ...)" (the README one-liner).
mkdir -p "$H1"
set +e
OUT="$(env -i HOME="$H1" PATH="/usr/bin:/bin:/usr/sbin:/sbin" SHELL="$LOGIN_SHELL" TMPDIR="$WORK" \
  STRADL_BASE_URL="$URL/good" STRADL_NO_SETUP=1 "$SH" -c "$(cat "$ROOT/install.sh")" 2>&1)"
RC=$?
set -e
if [ "$RC" -eq 0 ] && [ -x "$H1/.local/bin/stradl" ]; then
  pass "fresh install lands ~/.local/bin/stradl"
else
  fail "fresh install (rc=$RC): $OUT"
fi
got="$("$H1/.local/bin/stradl" --version 2>&1 || true)"
if [ "${got##* }" = "$WANT_VERSION" ]; then
  pass "installed binary runs ($got)"
else
  fail "installed binary --version: $got"
fi
if [ "$(marker_count "$H1/$PROFILE")" = 1 ]; then
  pass "PATH line added to $PROFILE"
else
  fail "PATH line count after first run: $(marker_count "$H1/$PROFILE")"
fi
case "$OUT" in *"Stradl $WANT_VERSION is installed."*) pass "summary printed" ;; *) fail "summary missing: $OUT" ;; esac
if compgen -G "$H1/.local/bin/.stradl.install.*" >/dev/null; then
  fail "staging file left behind"
else
  pass "no staging leftovers"
fi

# 2. Second run, piped: still exactly one PATH line.
run_install "$H1" "$URL/good" --no-setup
if [ "$RC" -eq 0 ]; then pass "re-install succeeds"; else fail "re-install (rc=$RC): $OUT"; fi
if [ "$(marker_count "$H1/$PROFILE")" = 1 ]; then
  pass "PATH line still added once after two runs"
else
  fail "PATH line count after second run: $(marker_count "$H1/$PROFILE")"
fi

# 3. Setup on an already-configured machine needs no terminal.
mkdir -p "$H1/.stradl"
: >"$H1/.stradl/config.yaml"
NO_SETUP=0 run_install "$H1" "$URL/good"
case "$RC:$OUT" in
  0:*"no terminal"*) fail "setup with existing config asked for a terminal: $OUT" ;;
  0:*"already configured"*) pass "setup skips init when already configured" ;;
  *) fail "setup with existing config (rc=$RC): $OUT" ;;
esac

# 4. Checksum mismatch: fails, installs nothing, keeps the old binary.
before="$(sha "$H1/.local/bin/stradl" | awk '{print $1}')"
run_install "$H1" "$URL/badsum"
after="$(sha "$H1/.local/bin/stradl" | awk '{print $1}')"
case "$OUT" in *"checksum mismatch"*) mismatch=1 ;; *) mismatch=0 ;; esac
if [ "$RC" -ne 0 ] && [ "$mismatch" = 1 ] && [ "$before" = "$after" ]; then
  pass "checksum mismatch fails and leaves the existing binary alone"
else
  fail "checksum mismatch over existing install (rc=$RC): $OUT"
fi
H2="$WORK/home2"
run_install "$H2" "$URL/badsum"
if [ "$RC" -ne 0 ] && [ ! -e "$H2/.local/bin/stradl" ] && [ ! -e "$H2/$PROFILE" ]; then
  pass "checksum mismatch on a fresh machine installs nothing"
else
  fail "checksum mismatch on fresh home (rc=$RC): $OUT"
fi

# 5. checksums.txt without an entry for the asset.
run_install "$WORK/home3" "$URL/noentry"
case "$RC:$OUT" in
  0:*) fail "missing checksum entry was accepted" ;;
  *"no entry"*) pass "missing checksum entry is refused" ;;
  *) fail "missing checksum entry (rc=$RC): $OUT" ;;
esac

# 6. Release not found.
run_install "$WORK/home4" "$URL/empty"
case "$RC:$OUT" in
  0:*) fail "404 was accepted" ;;
  *"download failed"*) pass "missing release fails cleanly" ;;
  *) fail "missing release (rc=$RC): $OUT" ;;
esac

# 7. The placeholder repo gives a friendly error. STRADL_REPO is passed
# explicitly, so this holds whatever default this copy of install.sh has.
set +e
OUT="$(env -i HOME="$WORK/home5" PATH="/usr/bin:/bin" STRADL_REPO=REPLACE_OWNER/stradl \
  "$SH" -s -- --no-setup <"$ROOT/install.sh" 2>&1)"
RC=$?
set -e
case "$RC:$OUT" in
  0:*) fail "placeholder repo was accepted" ;;
  *STRADL_REPO*) pass "placeholder repo asks for STRADL_REPO" ;;
  *) fail "placeholder repo (rc=$RC): $OUT" ;;
esac
set +e
OUT="$(env -i HOME="$WORK/home5" PATH="/usr/bin:/bin" STRADL_REPO="not a repo" \
  "$SH" -s -- --no-setup <"$ROOT/install.sh" 2>&1)"
RC=$?
set -e
case "$RC:$OUT" in
  0:*) fail "malformed STRADL_REPO was accepted" ;;
  *"owner/name"*) pass "malformed STRADL_REPO is refused" ;;
  *) fail "malformed STRADL_REPO (rc=$RC): $OUT" ;;
esac

# 8. Unknown flags are rejected.
run_install "$WORK/home6" "$URL/good" --bogus
if [ "$RC" -ne 0 ]; then pass "unknown option rejected"; else fail "unknown option accepted"; fi

# 9. bash as the login shell, with an existing ~/.profile: ~/.bash_profile is
# created, keeps ~/.profile working, and puts ~/.local/bin on PATH once.
H9="$WORK/home9"
mkdir -p "$H9"
printf 'export STRADL_FROM_PROFILE=yes\n' >"$H9/.profile"
RUN_SHELL=/bin/bash run_install "$H9" "$URL/good"
RUN_SHELL=/bin/bash run_install "$H9" "$URL/good"
# shellcheck disable=SC2016 # the literal line install.sh writes
if [ "$RC" -eq 0 ] && [ "$(marker_count "$H9/.bash_profile")" = 1 ] &&
  [ "$(count_lines '. "$HOME/.profile"' "$H9/.bash_profile")" = 1 ]; then
  pass "bash login shell: ~/.bash_profile created once, sourcing ~/.profile"
else
  fail "bash login shell with ~/.profile (rc=$RC): $OUT; .bash_profile: $(cat "$H9/.bash_profile" 2>&1)"
fi
# shellcheck disable=SC2016 # expanded by the login shell
login="$(env -i HOME="$H9" PATH="/usr/bin:/bin" TERM=dumb /bin/bash -l -c \
  'printf "%s|%s" "${STRADL_FROM_PROFILE:-}" "$PATH"' 2>/dev/null </dev/null || true)"
path_hits="$(printf '%s' "${login#*|}" | tr ':' '\n' | grep -cxF "$H9/.local/bin" || true)"
if [ "${login%%|*}" = yes ] && [ "$path_hits" = 1 ]; then
  pass "bash login shell reads ~/.profile and gets ~/.local/bin on PATH once"
else
  fail "bash login shell environment: $login"
fi

# 10. bash as the login shell, no profiles at all: ~/.bash_profile is created
# with just the PATH line.
H10="$WORK/home10"
RUN_SHELL=/bin/bash run_install "$H10" "$URL/good"
RUN_SHELL=/bin/bash run_install "$H10" "$URL/good"
if [ "$RC" -eq 0 ] && [ "$(marker_count "$H10/.bash_profile")" = 1 ] &&
  [ "$(count_lines "Created by the Stradl installer" "$H10/.bash_profile")" = 0 ]; then
  pass "bash login shell: ~/.bash_profile created with the PATH line"
else
  fail "bash login shell without profiles (rc=$RC): $OUT; .bash_profile: $(cat "$H10/.bash_profile" 2>&1)"
fi

# 11-14. Upgrading a machine that runs the agent restarts it without a
# terminal, retrying while the old agent is still busy. These use the
# stand-in stradl, so launchd is never called.
agent_home() {
  local home="$1" fails="$2"
  mkdir -p "$home/.stradl" "$home/Library/LaunchAgents"
  : >"$home/.stradl/config.yaml"
  : >"$home/Library/LaunchAgents/com.breakpointdata.stradl.plist"
  printf '%s\n' "$fails" >"$home/fake-fail-count"
}
fake_calls() { count_lines "agent install" "$1/fake-stradl.log"; }

H11="$WORK/home11"
agent_home "$H11" 0
NO_SETUP=0 run_install "$H11" "$URL/fake"
if [ "$OS" = darwin ]; then
  if [ "$RC" -eq 0 ] && [ "$(fake_calls "$H11")" = 1 ] &&
    [ "$(count_lines "agent install notty" "$H11/fake-stradl.log")" = 1 ]; then
    case "$OUT" in
      *"agent restarted"*) pass "existing agent restarted without a terminal" ;;
      *) fail "agent restart not reported: $OUT" ;;
    esac
  else
    fail "agent restart without a terminal (rc=$RC, calls=$(fake_calls "$H11")): $OUT"
  fi
else
  if [ "$RC" -eq 0 ] && [ "$(fake_calls "$H11")" = 0 ]; then
    pass "no agent restart on $OS"
  else
    fail "agent restart attempted on $OS (rc=$RC): $OUT"
  fi
fi

if [ "$OS" = darwin ]; then
  H12="$WORK/home12"
  agent_home "$H12" 1
  NO_SETUP=0 run_install "$H12" "$URL/fake"
  case "$RC:$(fake_calls "$H12"):$OUT" in
    0:2:*"trying again"*"agent restarted"*) pass "busy agent: restart retried until it took" ;;
    *) fail "busy agent restart (rc=$RC, calls=$(fake_calls "$H12")): $OUT" ;;
  esac

  H13="$WORK/home13"
  agent_home "$H13" 99
  NO_SETUP=0 run_install "$H13" "$URL/fake"
  case "$RC:$(fake_calls "$H13"):$OUT" in
    0:*) fail "agent that never restarts was reported as fine: $OUT" ;;
    *:4:*"agent restart failed"*) pass "agent that never restarts fails the install after 4 tries" ;;
    *) fail "agent that never restarts (rc=$RC, calls=$(fake_calls "$H13")): $OUT" ;;
  esac

  # Not configured and no terminal: init can't run, and the agent is left
  # alone (it has nothing to sync without a config).
  H14="$WORK/home14"
  agent_home "$H14" 0
  rm "$H14/.stradl/config.yaml"
  NO_SETUP=0 run_install "$H14" "$URL/fake"
  case "$RC:$(fake_calls "$H14"):$OUT" in
    0:0:*"no terminal"*) pass "unconfigured machine without a terminal skips setup" ;;
    *) fail "unconfigured machine without a terminal (rc=$RC, calls=$(fake_calls "$H14")): $OUT" ;;
  esac
fi

echo
if [ "$FAILURES" -gt 0 ]; then
  echo "$FAILURES install test(s) failed"
  exit 1
fi
echo "all install tests passed"
