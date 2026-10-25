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
# under /bin/bash to keep it honest about bash 3.2.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="0.0.0-installtest"
MARKER="# Added by the Stradl installer"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/stradl-installtest.XXXXXX")"
SERVER_PID=""
cleanup() {
  if [ -n "$SERVER_PID" ]; then kill "$SERVER_PID" 2>/dev/null || true; fi
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

# run_install HOME BASE_URL [args...] runs install.sh piped into bash, the way
# `curl ... | bash -s --` does, and leaves output in $OUT and status in $RC.
OUT=""
RC=0
run_install() {
  local home="$1" base="$2"
  shift 2
  mkdir -p "$home"
  set +e
  OUT="$(env -i HOME="$home" PATH="/usr/bin:/bin:/usr/sbin:/sbin" SHELL="$LOGIN_SHELL" \
    TMPDIR="$WORK" STRADL_BASE_URL="$base" STRADL_NO_SETUP="${NO_SETUP-1}" \
    "$SH" -s -- "$@" <"$ROOT/install.sh" 2>&1)"
  RC=$?
  set -e
}

marker_count() {
  local f="$1"
  if [ -f "$f" ]; then grep -cF "$MARKER" "$f" || true; else echo 0; fi
}

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

# 3. Setup on an already-configured machine is non-interactive.
mkdir -p "$H1/.stradl"
: >"$H1/.stradl/config.yaml"
NO_SETUP=0 run_install "$H1" "$URL/good"
case "$RC:$OUT" in
  0:*"already configured"* | 0:*"no terminal"*) pass "setup skips init when already configured" ;;
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

# 7. Placeholder repo without STRADL_REPO gives a friendly error.
set +e
OUT="$(env -i HOME="$WORK/home5" PATH="/usr/bin:/bin" "$SH" -s -- --no-setup <"$ROOT/install.sh" 2>&1)"
RC=$?
set -e
case "$RC:$OUT" in
  0:*) fail "placeholder repo was accepted" ;;
  *STRADL_REPO*) pass "placeholder repo asks for STRADL_REPO" ;;
  *) fail "placeholder repo (rc=$RC): $OUT" ;;
esac

# 8. Unknown flags are rejected.
run_install "$WORK/home6" "$URL/good" --bogus
if [ "$RC" -ne 0 ]; then pass "unknown option rejected"; else fail "unknown option accepted"; fi

echo
if [ "$FAILURES" -gt 0 ]; then
  echo "$FAILURES install test(s) failed"
  exit 1
fi
echo "all install tests passed"
