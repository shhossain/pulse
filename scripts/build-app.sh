#!/usr/bin/env bash
# Builds build/Pulse.app (ad-hoc signed) and build/pulse (CLI).
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)
OUT="$ROOT/build"
APP="$OUT/Pulse.app"
BUNDLE_ID="dev.pulse.menubar"
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
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

echo "› building pulse-app"
MACOSX_DEPLOYMENT_TARGET=13.0 CGO_CFLAGS="-mmacosx-version-min=13.0" CGO_LDFLAGS="-mmacosx-version-min=13.0" CGO_ENABLED=1 go build -trimpath -ldflags "$LDFLAGS" -o "$APP/Contents/MacOS/pulse-app" ./cmd/pulse-app

echo "› building pulse CLI"
CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/pulse" ./cmd/pulse

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

echo "› signing (ad-hoc)"
codesign --force --deep --sign - "$APP"

echo "✓ $APP"
echo "✓ $OUT/pulse"
