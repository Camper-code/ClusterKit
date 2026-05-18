#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
APP="dist/ClusterKit.app"
ZIP="dist/ClusterKit-macos-universal.zip"
rm -rf "$APP" "$ZIP" dist/ClusterKit-macos-arm64.zip dist/ClusterKit-macos-x64.zip dist/ClusterKit-macos-universal
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources" dist/build

CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o dist/build/ClusterKit-arm64 ./cmd/clusterkit
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -o dist/build/ClusterKit-amd64 ./cmd/clusterkit
lipo -create -output "$APP/Contents/MacOS/ClusterKit-bin" dist/build/ClusterKit-arm64 dist/build/ClusterKit-amd64
chmod 755 "$APP/Contents/MacOS/ClusterKit-bin"
cat > "$APP/Contents/MacOS/ClusterKit" <<'WRAP'
#!/usr/bin/env bash
DIR="$(cd "$(dirname "$0")" && pwd)"
BIN="$DIR/ClusterKit-bin"

# If launched from an existing terminal/command file, use the current terminal.
if [ -t 0 ]; then
  exec "$BIN" "$@"
fi

# If double-clicked as a .app in Finder, macOS gives us no visible console.
# Create a user-writable .command launcher and ask Terminal.app to open it.
# This avoids Apple Events / osascript permission issues on other Macs.
SUPPORT="$HOME/Library/Application Support/ClusterKit"
mkdir -p "$SUPPORT"
LAUNCHER="$SUPPORT/Run ClusterKit.command"
cat > "$LAUNCHER" <<CMD
#!/usr/bin/env bash
clear
cd "\$HOME"
exec "$BIN"
CMD
chmod +x "$LAUNCHER"
exec /usr/bin/open -a Terminal "$LAUNCHER"
WRAP
chmod 755 "$APP/Contents/MacOS/ClusterKit"

cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleExecutable</key><string>ClusterKit</string>
  <key>CFBundleIdentifier</key><string>ai.openclaw.clusterkit</string>
  <key>CFBundleName</key><string>ClusterKit</string>
  <key>CFBundleDisplayName</key><string>ClusterKit</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>0.1.1</string>
  <key>CFBundleVersion</key><string>0.1.1</string>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST

# Ad-hoc signing makes copied apps behave better than a raw unsigned bundle.
if command -v codesign >/dev/null 2>&1; then
  codesign --force --deep --sign - "$APP" >/dev/null 2>&1 || true
fi

STAGE="dist/ClusterKit-macos-universal"
mkdir -p "$STAGE"
cp -R "$APP" "$STAGE/"
cat > "$STAGE/README-FIRST.txt" <<'TXT'
ClusterKit for macOS

FIRST LAUNCH — recommended:
1. Double-click ClusterKit.app.
2. If macOS blocks it, open Terminal and paste this once:

   cd ~/Downloads/ClusterKit-macos-universal && xattr -dr com.apple.quarantine ClusterKit.app && chmod +x ClusterKit.app/Contents/MacOS/ClusterKit && open ClusterKit.app

If you unzipped it somewhere else, replace ~/Downloads/ClusterKit-macos-universal with the folder path.

Double-clicking ClusterKit.app opens Terminal with the ClusterKit TUI.

If double-click still does nothing, run this from the folder containing ClusterKit.app:

   ./ClusterKit.app/Contents/MacOS/ClusterKit

Alternative:
- Right-click ClusterKit.app -> Open -> Open
- Or System Settings -> Privacy & Security -> Open Anyway

Supported Macs: Apple Silicon and Intel, macOS 12+.
TXT
cat > "$STAGE/UNBLOCK-AND-OPEN.txt" <<'TXT'
Copy/paste into Terminal from inside the ClusterKit-macos-universal folder:

xattr -dr com.apple.quarantine ClusterKit.app && chmod +x ClusterKit.app/Contents/MacOS/ClusterKit && open ClusterKit.app

Terminal UI:
open ClusterKit.app

Direct terminal fallback:
./ClusterKit.app/Contents/MacOS/ClusterKit
TXT
cat > "$STAGE/ClusterKit Terminal.command" <<'CMD'
#!/usr/bin/env bash
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
APP="$DIR/ClusterKit.app"
/usr/bin/xattr -dr com.apple.quarantine "$APP" 2>/dev/null || true
/bin/chmod +x "$APP/Contents/MacOS/ClusterKit" 2>/dev/null || true
exec "$APP/Contents/MacOS/ClusterKit-bin"
CMD
chmod 755 "$STAGE/ClusterKit Terminal.command"
cat > "$STAGE/Open ClusterKit.command.txt" <<'TXT'
If you want a double-click launcher, rename this file from:
Open ClusterKit.command.txt
to:
Open ClusterKit.command

Then double-click it after running the Terminal unblock command once.

Script content:
#!/usr/bin/env bash
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
APP="$DIR/ClusterKit.app"
/usr/bin/xattr -dr com.apple.quarantine "$APP" 2>/dev/null || true
/bin/chmod +x "$APP/Contents/MacOS/ClusterKit" 2>/dev/null || true
/usr/bin/open "$APP"
TXT
(cd dist && zip -qry "$(basename "$ZIP")" "$(basename "$STAGE")")
(cd dist && tar -czf ClusterKit-macos-universal.tar.gz "$(basename "$STAGE")")
cp "$ZIP" dist/ClusterKit-macos-arm64.zip
rm -rf dist/build

echo "Built: $ROOT/$ZIP"
echo "Built: $ROOT/dist/ClusterKit-macos-universal.tar.gz"
