#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

# Ensure the app exists and is current.
./scripts/package-macos.sh >/dev/null

DMG_NAME="ClusterKit-macos-universal.dmg"
STAGE="dist/dmg-stage"
DMG_TMP="dist/ClusterKit-temp.dmg"
DMG_OUT="dist/$DMG_NAME"

rm -rf "$STAGE" "$DMG_TMP" "$DMG_OUT"
mkdir -p "$STAGE"
cp -R "dist/ClusterKit.app" "$STAGE/ClusterKit.app"
ln -s /Applications "$STAGE/Applications"

cat > "$STAGE/README-FIRST.txt" <<'TXT'
ClusterKit for macOS

Install:
1. Drag ClusterKit.app to Applications.
2. First launch: right-click ClusterKit.app -> Open -> Open.

If macOS says the app is damaged or cannot be opened, run this once in Terminal:

xattr -dr com.apple.quarantine /Applications/ClusterKit.app && chmod +x /Applications/ClusterKit.app/Contents/MacOS/ClusterKit && open /Applications/ClusterKit.app

Why: this is a local unsigned build. Developer ID signing + Apple notarization is needed to remove this warning completely.

Supported Macs: Apple Silicon and Intel, macOS 12+.
TXT

hdiutil create -volname "ClusterKit" -srcfolder "$STAGE" -ov -format UDRW "$DMG_TMP" >/dev/null
hdiutil convert "$DMG_TMP" -format UDZO -imagekey zlib-level=9 -o "$DMG_OUT" >/dev/null
rm -rf "$STAGE" "$DMG_TMP"

echo "Built: $ROOT/$DMG_OUT"
