#!/usr/bin/env bash
# Build the Stradl release assets.
#
# Usage: scripts/build-release.sh VERSION REPO OUTDIR
#
#   VERSION  semver without the leading "v" (one is stripped if present)
#   REPO     GitHub owner/name the binary updates itself from (main.releaseRepo)
#   OUTDIR   directory that receives the tarballs and checksums.txt
#
# Writes stradl_<os>_<arch>.tar.gz for darwin/linux x arm64/amd64, each holding
# a single `stradl` executable plus LICENSE, and checksums.txt in
# `shasum -a 256` format. These names are the contract install.sh and
# `stradl update` download against, so don't rename them casually.
#
# Runs on macOS only: the darwin builds need cgo (the menu bar links AppKit)
# and are ad-hoc codesigned. The linux builds are pure Go (CGO_ENABLED=0).
set -euo pipefail

usage() {
  echo "Usage: $0 VERSION REPO OUTDIR" >&2
  echo "  e.g. $0 1.2.3 breakpointdata/stradl dist" >&2
  exit 2
}

die() {
  echo "build-release: $*" >&2
  exit 1
}

[ "$#" -eq 3 ] || usage

VERSION="${1#v}"
REPO="$2"
OUTDIR="$3"

# Both values are spliced into -ldflags, which go splits on spaces, so reject
# anything that could smuggle in extra flags.
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$ ]] ||
  die "VERSION must be semver like 1.2.3 or 1.2.3-rc.1, got '$1'"
[[ "$REPO" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] ||
  die "REPO must look like owner/name, got '$REPO'"

[ "$(uname -s)" = "Darwin" ] ||
  die "must run on macOS (darwin builds need cgo and codesign)"
for tool in go clang codesign lipo shasum tar; do
  command -v "$tool" >/dev/null 2>&1 || die "missing required tool: $tool"
done

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mkdir -p "$OUTDIR"
OUTDIR="$(cd "$OUTDIR" && pwd)"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

LDFLAGS="-s -w -X main.version=${VERSION} -X main.releaseRepo=${REPO}"

# cgo links with clang, which otherwise stamps the build host's macOS version
# as the minimum, and dyld refuses to load the binary on anything older. 13.0
# (Ventura) is at or above the floor of every Go toolchain go.mod allows; raise
# it if ld warns that go.o "was built for newer macOS version".
export MACOSX_DEPLOYMENT_TARGET="${MACOSX_DEPLOYMENT_TARGET:-13.0}"

# Keep the build reproducible-ish and independent of the caller's environment.
export GOFLAGS="-trimpath"
export GOWORK=off

LICENSE_FILE="$ROOT/LICENSE"
if [ ! -f "$LICENSE_FILE" ]; then
  echo "build-release: warning: $LICENSE_FILE not found; tarballs will ship without it" >&2
  LICENSE_FILE=""
fi

ASSETS=()

build() {
  local os="$1" arch="$2"
  local stage="$WORK/${os}_${arch}"
  local asset="stradl_${os}_${arch}.tar.gz"
  mkdir -p "$stage"

  echo "==> $os/$arch"
  if [ "$os" = "darwin" ]; then
    # clang calls amd64 x86_64. Passing -arch explicitly lets an Apple Silicon
    # host build the Intel binary (and vice versa) with the same toolchain.
    local carch="$arch"
    [ "$arch" = "amd64" ] && carch="x86_64"
    (cd "$ROOT" && env GOOS=darwin GOARCH="$arch" CGO_ENABLED=1 \
      CC=clang CXX=clang++ \
      CGO_CFLAGS="-O2 -g -arch $carch" \
      CGO_CXXFLAGS="-O2 -g -arch $carch" \
      CGO_LDFLAGS="-arch $carch" \
      go build -ldflags "$LDFLAGS" -o "$stage/stradl" ./cmd/claude-sync)

    local got
    got="$(lipo -archs "$stage/stradl")"
    [ "$got" = "$carch" ] || die "$os/$arch: built for '$got', expected '$carch'"

    # Apple Silicon refuses to exec unsigned arm64 code, and the linker only
    # signs some outputs, so give every darwin binary an ad-hoc signature.
    # (curl does not set the quarantine flag, so Gatekeeper never sees it.)
    codesign --force --sign - "$stage/stradl"
    codesign --verify "$stage/stradl"
  else
    (cd "$ROOT" && env GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
      go build -ldflags "$LDFLAGS" -o "$stage/stradl" ./cmd/claude-sync)
  fi
  chmod 755 "$stage/stradl"

  local files=(stradl)
  if [ -n "$LICENSE_FILE" ]; then
    cp "$LICENSE_FILE" "$stage/LICENSE"
    chmod 644 "$stage/LICENSE"
    files+=(LICENSE)
  fi

  # COPYFILE_DISABLE keeps bsdtar from adding AppleDouble ._ entries; the
  # uid/gid flags keep the build user's name out of the archive.
  rm -f "$OUTDIR/$asset"
  COPYFILE_DISABLE=1 tar -czf "$OUTDIR/$asset" \
    --uid 0 --gid 0 --uname root --gname root \
    -C "$stage" "${files[@]}"
  ASSETS+=("$asset")
}

build darwin arm64
build darwin amd64
build linux arm64
build linux amd64

(cd "$OUTDIR" && shasum -a 256 "${ASSETS[@]}" >checksums.txt)

echo
echo "Built stradl $VERSION ($REPO) into $OUTDIR:"
(cd "$OUTDIR" && ls -l "${ASSETS[@]}" checksums.txt)
