#!/usr/bin/env bash
set -euo pipefail
touch /boot/firmware/RESET_WIFI
sync
systemctl reboot
