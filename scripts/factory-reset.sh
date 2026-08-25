#!/usr/bin/env bash
set -euo pipefail

systemctl stop provisiond.service || true
/usr/local/lib/rpi-provision/rpi-provision forget-wifi wlan0 device-provision-ap || true
rm -rf /var/lib/mydevice/*
rm -f /etc/mydevice/device.env
sync
systemctl restart provisiond.service
