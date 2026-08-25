#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

if [[ $# -lt 1 ]]; then
  echo "Usage: $0 <user@host> [arm64|armv7] [remote_dir]" >&2
  echo "  Example: $0 pi@mydevice.local arm64" >&2
  exit 1
fi

TARGET="$1"
ARCH="${2:-arm64}"
REMOTE_DIR="${3:-provision-pi-gateway}"

"$ROOT_DIR/scripts/build.sh" "$ARCH"

# Only what install.sh and the running service actually need on the Pi —
# no Go source, go.mod/go.sum, .git, or docs.
FILES=(
  bin/provisiond
  config/provisiond.env
  systemd/provisiond.service
  systemd/provisiond-reset-wifi.service
  sudoers/rpi-provision
  nftables/90-provisioning.nft
  scripts/install.sh
  scripts/rpi-provision
  scripts/reset-wifi.sh
  scripts/factory-reset.sh
)

echo "Copying $(( ${#FILES[@]} )) files to $TARGET:$REMOTE_DIR/ ..."
ssh "$TARGET" "mkdir -p $REMOTE_DIR"
rsync -av --relative "${FILES[@]}" "$TARGET:$REMOTE_DIR/"

echo
echo "Done. Next steps on the Pi:"
echo "  ssh $TARGET"
echo "  cd $REMOTE_DIR"
echo "  chmod +x scripts/*.sh"
echo "  sudo ./scripts/install.sh"
