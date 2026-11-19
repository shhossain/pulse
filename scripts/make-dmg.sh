#!/usr/bin/env bash
# Packs build/Pulse.app into build/Pulse-<version>.dmg: the app, a link to
# /Applications to drag it onto, and the Pulse volume icon.
#
#   SIGN_IDENTITY="Developer ID Application: …"   also sign the disk image
set -euo pipefail
cd "$(dirname "$0")/.."

APP=build/Pulse.app
[ -d "$APP" ] || { echo "build the app first: ./scripts/build-app.sh" >&2; exit 1; }
# The version build-app.sh stamped (the full one: 1.2.0-rc.1, not the plist's 1.2.0).
VERSION=$(cat build/version.txt 2>/dev/null || /usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$APP/Contents/Info.plist")
NAME="Pulse-${VERSION}"
DMG="build/${NAME}.dmg"

STAGE=$(mktemp -d)
RW="$STAGE.rw.dmg"
MNT=""
cleanup() {
  [ -n "$MNT" ] && hdiutil detach -quiet -force "$MNT" 2>/dev/null || true
  rm -rf "$STAGE" "$RW"
}
trap cleanup EXIT
ditto "$APP" "$STAGE/Pulse.app" # ditto keeps signatures and extended attributes intact
cp LICENSE "$STAGE/LICENSE.txt"
ln -s /Applications "$STAGE/Applications"
cp assets/icon/Pulse.icns "$STAGE/.VolumeIcon.icns"

rm -f "$DMG"
# Make a read-write image, flag the custom volume icon, then compress it.
hdiutil create -quiet -volname "Pulse" -srcfolder "$STAGE" -fs HFS+ -format UDRW -ov "$RW"
MNT=$(hdiutil attach -nobrowse -noverify -noautoopen "$RW" | awk -F'\t' '/\/Volumes\//{print $NF}')
SetFile -a C "$MNT" 2>/dev/null || xattr -wx com.apple.FinderInfo \
  "0000000000000000040000000000000000000000000000000000000000000000" "$MNT"
# Spotlight or Finder may still hold the fresh volume for a moment.
for i in 1 2 3 4 5; do
  hdiutil detach -quiet "$MNT" && break
  sleep $i
  [ $i = 5 ] && hdiutil detach -quiet -force "$MNT"
done
MNT=""
hdiutil convert -quiet "$RW" -format UDZO -imagekey zlib-level=9 -o "$DMG"

if [ -n "${SIGN_IDENTITY:-}" ]; then
  codesign --force --timestamp --sign "$SIGN_IDENTITY" "$DMG"
fi
hdiutil verify -quiet "$DMG"
echo "✓ $DMG ($(du -h "$DMG" | cut -f1))"
