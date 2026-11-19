#!/usr/bin/env bash
# Builds build/Pulse.app and build/pulse (CLI).
#
#   VERSION=v1.2.3        the version to stamp (default: git describe); the release
#                         workflow passes the tag so every file agrees
#   ARCHS="arm64 amd64"   universal binaries (default: this Mac's architecture)
#   SIGN_IDENTITY="Developer ID Application: …"   sign for distribution
#                         (hardened runtime + timestamp); default: ad-hoc
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)
OUT="$ROOT/build"
APP="$OUT/Pulse.app"
BUNDLE_ID="dev.pulse.menubar"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || true)"
# macOS wants numbers: x.y.z from a release tag (else 0.0.0), and a build
# number that always grows (the commit count).
PLIST_VERSION="$(echo "$VERSION" | sed -nE 's/^v?([0-9]+\.[0-9]+\.[0-9]+).*/\1/p')"
PLIST_VERSION="${PLIST_VERSION:-0.0.0}"
BUILD_NUMBER="$(git rev-list --count HEAD 2>/dev/null || echo 1)"
LDFLAGS="-s -w -X main.version=$VERSION -X main.commit=$COMMIT"

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

echo "› building web UI"
(
  cd web
  if command -v pnpm >/dev/null; then pnpm install --frozen-lockfile >/dev/null && pnpm run build >/dev/null
  else npm ci >/dev/null && npm run build >/dev/null; fi
)

ARCHS="${ARCHS:-$(go env GOARCH)}"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
for arch in $ARCHS; do
  case $arch in arm64) carch=arm64 ;; amd64) carch=x86_64 ;; *) echo "unknown arch $arch" >&2; exit 2 ;; esac
  echo "› building pulse-app ($arch)"
  GOOS=darwin GOARCH=$arch CC="clang -arch $carch" MACOSX_DEPLOYMENT_TARGET=13.0 \
    CGO_CFLAGS="-mmacosx-version-min=13.0" CGO_LDFLAGS="-mmacosx-version-min=13.0" CGO_ENABLED=1 \
    go build -trimpath -ldflags "$LDFLAGS" -o "$TMP/pulse-app-$arch" ./cmd/pulse-app
  echo "› building pulse CLI ($arch)"
  GOOS=darwin GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$TMP/cli-$arch" ./cmd/pulse
done
lipo -create -output "$APP/Contents/MacOS/pulse-app" "$TMP"/pulse-app-*
lipo -create -output "$OUT/pulse" "$TMP"/cli-*
# The CLI ships inside the app too, so the DMG is all anyone needs.
cp "$OUT/pulse" "$APP/Contents/MacOS/pulse"

cp assets/icon/Pulse.icns "$APP/Contents/Resources/Pulse.icns"

cat > "$APP/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Pulse</string>
  <key>CFBundleDisplayName</key><string>Pulse</string>
  <key>CFBundleIdentifier</key><string>${BUNDLE_ID}</string>
  <key>CFBundleExecutable</key><string>pulse-app</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleIconFile</key><string>Pulse</string>
  <key>NSHumanReadableCopyright</key><string>© 2026 Pulse contributors. MIT License.</string>
  <key>CFBundleVersion</key><string>${BUILD_NUMBER}</string>
  <key>CFBundleShortVersionString</key><string>${PLIST_VERSION}</string>
  <key>LSMinimumSystemVersion</key><string>13.0</string>
  <key>LSUIElement</key><true/>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSAppTransportSecurity</key>
  <dict><key>NSAllowsLocalNetworking</key><true/></dict>
</dict>
</plist>
EOF

if [ -n "${SIGN_IDENTITY:-}" ]; then
  echo "› signing ($SIGN_IDENTITY)"
  # Inside out: the bundled CLI, then the app. Hardened runtime + secure
  # timestamp are what notarization requires.
  codesign --force --options runtime --timestamp --sign "$SIGN_IDENTITY" "$APP/Contents/MacOS/pulse"
  codesign --force --options runtime --timestamp --sign "$SIGN_IDENTITY" "$APP"
  codesign --force --options runtime --timestamp --sign "$SIGN_IDENTITY" "$OUT/pulse"
else
  echo "› signing (ad-hoc)"
  codesign --force --sign - "$APP/Contents/MacOS/pulse"
  codesign --force --sign - "$APP"
fi
codesign --verify --strict "$APP"

# The full version (v stripped), for file names: make-dmg.sh reads it.
echo "${VERSION#v}" > "$OUT/version.txt"

echo "✓ $APP"
echo "✓ $OUT/pulse"
