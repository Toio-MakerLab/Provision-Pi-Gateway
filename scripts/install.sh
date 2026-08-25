#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALL_USER=provision

if [[ $EUID -ne 0 ]]; then
  echo "Run with sudo: sudo ./scripts/install.sh" >&2
  exit 1
fi

apt-get update
apt-get install -y network-manager nftables i2c-tools

# Enable the I2C interface for the OLED status display. Idempotent: safe to re-run.
if command -v raspi-config >/dev/null 2>&1; then
  raspi-config nonint do_i2c 0
elif ! grep -q '^dtparam=i2c_arm=on' /boot/firmware/config.txt 2>/dev/null; then
  echo 'dtparam=i2c_arm=on' >> /boot/firmware/config.txt
  echo "I2C enabled in /boot/firmware/config.txt; a reboot is required for it to take effect."
fi

systemctl enable --now NetworkManager

id -u "$INSTALL_USER" >/dev/null 2>&1 || \
  useradd --system --user-group --home-dir /var/lib/rpi-provision --create-home --shell /usr/sbin/nologin "$INSTALL_USER"

install -d -o root -g "$INSTALL_USER" -m 0750 /etc/rpi-provision
install -d -o root -g root -m 0755 /usr/local/lib/rpi-provision
install -d -o "$INSTALL_USER" -g "$INSTALL_USER" -m 0750 /var/lib/rpi-provision

install -m 0755 "$ROOT_DIR/scripts/rpi-provision" /usr/local/lib/rpi-provision/rpi-provision
install -m 0640 "$ROOT_DIR/config/provisiond.env" /etc/rpi-provision/provisiond.env
chown root:"$INSTALL_USER" /etc/rpi-provision/provisiond.env

install -m 0755 "$ROOT_DIR/bin/provisiond" /usr/local/bin/provisiond
install -m 0644 "$ROOT_DIR/systemd/provisiond.service" /etc/systemd/system/provisiond.service
install -m 0644 "$ROOT_DIR/systemd/provisiond-reset-wifi.service" /etc/systemd/system/provisiond-reset-wifi.service
install -m 0440 "$ROOT_DIR/sudoers/rpi-provision" /etc/sudoers.d/rpi-provision
visudo -cf /etc/sudoers.d/rpi-provision

install -d -m 0755 /etc/nftables.d
install -m 0644 "$ROOT_DIR/nftables/90-provisioning.nft" /etc/nftables.d/90-provisioning.nft

if ! grep -qF 'include "/etc/nftables.d/*.nft"' /etc/nftables.conf 2>/dev/null; then
  cat >> /etc/nftables.conf <<'EOF'
include "/etc/nftables.d/*.nft"
EOF
fi

systemctl enable --now nftables
systemctl daemon-reload
systemctl enable provisiond.service
# `restart` (not `enable --now`) so re-running this script after a rebuild always
# picks up the new binary/unit file, even if the service was already running.
systemctl restart provisiond.service

echo "Installed. Update /etc/rpi-provision/provisiond.env before production deployment."
echo "Logs: journalctl -u provisiond -f"
