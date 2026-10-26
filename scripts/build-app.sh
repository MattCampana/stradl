#!/usr/bin/env bash
# Builds Stradl.app, the native Mac app, as a universal (arm64 + x86_64)
# bundle with its icon, ad-hoc signed.
#
#   scripts/build-app.sh [output-dir]        (default: dist)
#   VERSION=2.2.0 scripts/build-app.sh dist
#
# Needs Apple's Command Line Tools (swift, lipo, iconutil, sips, codesign);
# Xcode is not required.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
out="${1:-$root/dist}"
version="${VERSION:-dev}"
pkg="$root/apps/macos"
app="$out/Stradl.app"

mkdir -p "$out"
rm -rf "$app"

bins=()
for arch in arm64 x86_64; do
  swift build -c release --package-path "$pkg" \
    --triple "$arch-apple-macosx13.0" --scratch-path "$pkg/.build/$arch" >&2
  bins+=("$pkg/.build/$arch/release/Stradl")
done

mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
lipo -create -output "$app/Contents/MacOS/Stradl" "${bins[@]}"

# The icon: every size macOS asks for, from the 1024 px master.
iconset="$(mktemp -d)/AppIcon.iconset"
mkdir -p "$iconset"
master="$root/assets/brand/appicon-1024.png"
for size in 16 32 128 256 512; do
  sips -z "$size" "$size" "$master" --out "$iconset/icon_${size}x${size}.png" >/dev/null
  double=$((size * 2))
  sips -z "$double" "$double" "$master" --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$iconset" -o "$app/Contents/Resources/AppIcon.icns"
rm -rf "$(dirname "$iconset")"

short="${version#v}"
build="$(printf '%s' "$short" | sed -E 's/[^0-9.].*$//')"
[[ -n "$build" ]] || build="0"
cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Stradl</string>
  <key>CFBundleDisplayName</key><string>Stradl</string>
  <key>CFBundleIdentifier</key><string>com.breakpointdata.stradl.app</string>
  <key>CFBundleExecutable</key><string>Stradl</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>${short}</string>
  <key>CFBundleVersion</key><string>${build}</string>
  <key>LSMinimumSystemVersion</key><string>13.0</string>
  <key>LSApplicationCategoryType</key><string>public.app-category.utilities</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSHumanReadableCopyright</key><string>Musical chairs for your Claude sessions. MIT licensed.</string>
</dict>
</plist>
PLIST

codesign --force --sign - --timestamp=none "$app" >/dev/null
echo "$app"
