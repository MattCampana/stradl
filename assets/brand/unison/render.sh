#!/bin/bash
# Renders the Unison app icon and menu bar glyph from appicon.svg and
# glyph.svg with WebKit (svgpng.swift: full SVG, blending and blur, on a
# transparent canvas). Needs the Command Line Tools (swiftc).
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../../.." && pwd)"
tool="$(mktemp -d)/svgpng"
swiftc -O -o "$tool" "$here/svgpng.swift"
"$tool" "$here/appicon.svg" "$root/assets/brand/appicon-1024.png" 1024
"$tool" "$here/appicon.svg" "$root/assets/brand/appicon-512.png" 512
"$tool" "$here/glyph.svg" "$root/internal/agent/icon/menubar.png" 22 2
"$tool" "$here/glyph.svg" "$root/internal/agent/icon/menubar@2x.png" 44 4
"$tool" "$here/glyph.svg" "$root/assets/brand/menubar-16.png" 16 1
"$tool" "$here/glyph.svg" "$root/assets/brand/menubar-22.png" 22 2
"$tool" "$here/glyph.svg" "$root/assets/brand/menubar-44.png" 44 4
cp "$here/appicon.svg" "$root/assets/brand/appicon.svg"
cp "$here/glyph.svg" "$root/assets/brand/menubar.svg"
