#!/usr/bin/env bash
# Build the mcserver + mcclient binaries and assemble release folders.
#
# Primary target is Windows (that's where the server and friends run). A macOS
# build is produced too for local development. Uses the project-local Go
# toolchain in .tools/go, so no system Go install is required.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

export GOROOT="$ROOT/.tools/go"
export GOPATH="$ROOT/.tools/gopath"
export PATH="$GOROOT/bin:$PATH"

if [ ! -x "$GOROOT/bin/go" ]; then
  echo "Go toolchain not found in .tools/go — run scripts/setup.sh first." >&2
  exit 1
fi
for p in .tools/bin/packwiz.exe; do
  if [ ! -f "$p" ]; then
    echo "Missing $p — run scripts/setup.sh first." >&2
    exit 1
  fi
done

echo ">> building Windows binaries (amd64)"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o .tools/bin/mcserver.exe ./cmd/mcserver
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o .tools/bin/mcclient.exe ./cmd/mcclient

echo ">> building macOS binaries (dev)"
go build -o .tools/bin/mcserver ./cmd/mcserver
go build -o .tools/bin/mcclient ./cmd/mcclient

echo ">> assembling release/"
rm -rf release
# Server bundle: the admin runs this. Needs packwiz.exe alongside.
mkdir -p release/server-windows
cp .tools/bin/mcserver.exe release/server-windows/
cp .tools/bin/packwiz.exe  release/server-windows/
cp docs/SERVER.md           release/server-windows/README.txt 2>/dev/null || true

# Client bundle: this is the single file you hand to friends. Zero deps.
mkdir -p release/client-windows
cp .tools/bin/mcclient.exe release/client-windows/
cp docs/CLIENT.md          release/client-windows/README.txt 2>/dev/null || true

echo ">> done:"
echo "   release/server-windows/   (mcserver.exe + packwiz.exe — runs on your server)"
echo "   release/client-windows/    (mcclient.exe — hand this to friends)"
