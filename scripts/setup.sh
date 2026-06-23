#!/usr/bin/env bash
# One-time bootstrap: fetch a project-local Go toolchain and build the packwiz
# binaries (macOS for local dev + Windows to ship). Everything lands in .tools/
# (gitignored). Re-run any time to refresh.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
mkdir -p .tools/bin

# --- Go toolchain (local, no sudo) ---------------------------------------
if [ ! -x ".tools/go/bin/go" ]; then
  echo ">> downloading Go toolchain"
  GOVER="$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)"
  HOST_OS="$(uname -s | tr 'A-Z' 'a-z')"   # darwin / linux
  HOST_ARCH="$(uname -m)"
  case "$HOST_ARCH" in
    arm64|aarch64) HOST_ARCH=arm64 ;;
    x86_64|amd64)  HOST_ARCH=amd64 ;;
  esac
  curl -fsSL "https://go.dev/dl/${GOVER}.${HOST_OS}-${HOST_ARCH}.tar.gz" -o /tmp/go.tar.gz
  rm -rf .tools/go
  tar -xzf /tmp/go.tar.gz -C .tools/
fi

export GOROOT="$ROOT/.tools/go"
export GOPATH="$ROOT/.tools/gopath"
export PATH="$GOROOT/bin:$PATH"
echo ">> $(go version)"

# --- packwiz (built from source, pinned via @latest) ----------------------
echo ">> building packwiz (host + windows)"
go install github.com/packwiz/packwiz@latest
HOSTBIN="$(go env GOPATH)/bin"
cp "$HOSTBIN/packwiz" .tools/bin/packwiz 2>/dev/null || true

GOOS=windows GOARCH=amd64 go install github.com/packwiz/packwiz@latest
cp "$(go env GOPATH)/bin/windows_amd64/packwiz.exe" .tools/bin/packwiz.exe

echo ">> setup complete. Next: scripts/build.sh"
