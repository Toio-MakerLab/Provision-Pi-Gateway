#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

ARCH="${1:-arm64}"
OUT="$ROOT_DIR/bin/provisiond"

case "$ARCH" in
  arm64)
    GOOS=linux GOARCH=arm64 ;;
  armv7|arm)
    GOOS=linux GOARCH=arm GOARM=7 ;;
  *)
    echo "Usage: $0 [arm64|armv7]" >&2
    echo "  arm64  - Raspberry Pi OS 64-bit (default; Pi Zero 2 W, Pi 3/4/5)" >&2
    echo "  armv7  - Raspberry Pi OS 32-bit (Pi Zero/Zero W, older 32-bit installs)" >&2
    exit 1
    ;;
esac

mkdir -p "$ROOT_DIR/bin"
echo "Building provisiond for $ARCH (GOOS=$GOOS GOARCH=$GOARCH ${GOARM:+GOARM=$GOARM})..."
CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" ${GOARM:+GOARM=$GOARM} \
  go build -trimpath -ldflags='-s -w' -o "$OUT" ./cmd/provisiond

echo "Built: $OUT"
file "$OUT" 2>/dev/null || true
