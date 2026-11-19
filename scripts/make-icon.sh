#!/usr/bin/env bash
# Renders assets/icon/pulse.svg (the source of truth) into assets/icon/Pulse.icns.
# Needs rsvg-convert (brew install librsvg) and iconutil (macOS). The .icns is
# committed, so building the app doesn't need either.
set -euo pipefail
cd "$(dirname "$0")/.."

SRC=assets/icon/pulse.svg
SET=$(mktemp -d)/Pulse.iconset
mkdir -p "$SET"
for s in 16 32 128 256 512; do
  rsvg-convert -w $s -h $s "$SRC" -o "$SET/icon_${s}x${s}.png"
  rsvg-convert -w $((s * 2)) -h $((s * 2)) "$SRC" -o "$SET/icon_${s}x${s}@2x.png"
done
iconutil -c icns "$SET" -o assets/icon/Pulse.icns
rsvg-convert -w 512 -h 512 "$SRC" -o assets/icon/pulse-512.png # for the README and releases
rm -rf "$(dirname "$SET")"
echo "✓ assets/icon/Pulse.icns"
