#!/bin/bash
# Builds MacMonitor.app — a self-contained menu bar app bundle.
set -euo pipefail

APP="MacMonitor.app"
BIN="macmonitor"

echo "==> Building binary..."
go build -ldflags "-s -w" -o "$BIN" .

echo "==> Assembling $APP..."
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS"
cp Info.plist "$APP/Contents/Info.plist"
mv "$BIN" "$APP/Contents/MacOS/$BIN"

# Ad-hoc code signature so macOS will launch it without Gatekeeper complaints.
codesign --force --deep --sign - "$APP" 2>/dev/null || \
  echo "    (codesign skipped — app still runs, may prompt on first launch)"

echo "==> Done: $(pwd)/$APP"
echo "    Run it:   open $APP"
echo "    Or dev:   go run ."
