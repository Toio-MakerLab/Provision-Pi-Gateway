#!/usr/bin/env bash
set -Eeuo pipefail

if [[ "${EUID}" -ne 0 ]]; then
  echo "Run with: sudo bash provision-pi-gateway.sh"
  exit 1
fi

GATEWAY_USER="${GATEWAY_USER:-iot}"
GATEWAY_GROUP="${GATEWAY_GROUP:-iot}"
MQTT_USER="${MQTT_USER:-esp32}"
MQTT_PASSWORD="${MQTT_PASSWORD:-}"
MQTT_LISTENER_PORT="${MQTT_LISTENER_PORT:-1883}"
MQTT_MAX_CONNECTIONS="${MQTT_MAX_CONNECTIONS:-30}"
MQTT_MAX_QUEUED_MESSAGES="${MQTT_MAX_QUEUED_MESSAGES:-100}"
MQTT_MESSAGE_SIZE_LIMIT="${MQTT_MESSAGE_SIZE_LIMIT:-65536}"
INSTALL_GATEWAY_BINARY="${INSTALL_GATEWAY_BINARY:-}"

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "Missing command: $1"
}

require_cmd apt
require_cmd systemctl
require_cmd install

export DEBIAN_FRONTEND=noninteractive

apt update
apt install -y mosquitto mosquitto-clients sqlite3 bluez gpiod

if ! getent group "$GATEWAY_GROUP" >/dev/null; then
  groupadd --system "$GATEWAY_GROUP"
fi

if ! id -u "$GATEWAY_USER" >/dev/null 2>&1; then
  useradd \
    --system \
    --gid "$GATEWAY_GROUP" \
    --home-dir /opt/iot-gateway \
    --create-home \
    --shell /usr/sbin/nologin \
    "$GATEWAY_USER"
fi

install -d -o "$GATEWAY_USER" -g "$GATEWAY_GROUP" -m 0750 /srv/iot-data
install -d -o root -g root -m 0755 /opt/iot-gateway
install -d -o root -g root -m 0755 /etc/iot-gateway
install -d -o root -g root -m 0755 /etc/mosquitto/conf.d

if [[ -z "$MQTT_PASSWORD" ]]; then
  MQTT_PASSWORD="$(tr -dc 'A-Za-z0-9_@%+=' </dev/urandom | head -c 24)"
  echo "Generated an MQTT password. Save it now; it will not be printed again by this script."
fi

umask 077
printf '%s\n' "$MQTT_PASSWORD" > /etc/iot-gateway/mqtt-password.txt
chown root:"$GATEWAY_GROUP" /etc/iot-gateway/mqtt-password.txt
chmod 0640 /etc/iot-gateway/mqtt-password.txt

mosquitto_passwd -b -c /etc/mosquitto/passwd "$MQTT_USER" "$MQTT_PASSWORD"
chown mosquitto:mosquitto /etc/mosquitto/passwd
chmod 0600 /etc/mosquitto/passwd

cat > /etc/mosquitto/conf.d/iot.conf <<EOF
listener ${MQTT_LISTENER_PORT} 0.0.0.0

allow_anonymous false
password_file /etc/mosquitto/passwd

persistence false
max_connections ${MQTT_MAX_CONNECTIONS}
max_queued_messages ${MQTT_MAX_QUEUED_MESSAGES}
message_size_limit ${MQTT_MESSAGE_SIZE_LIMIT}

log_dest syslog
connection_messages false
EOF

chown root:root /etc/mosquitto/conf.d/iot.conf
chmod 0644 /etc/mosquitto/conf.d/iot.conf

if [[ -n "$INSTALL_GATEWAY_BINARY" ]]; then
  [[ -f "$INSTALL_GATEWAY_BINARY" ]] || fail "Gateway binary not found: $INSTALL_GATEWAY_BINARY"
  install -o root -g root -m 0755 "$INSTALL_GATEWAY_BINARY" /opt/iot-gateway/iot-gateway

  cat > /etc/systemd/system/iot-gateway.service <<EOF
[Unit]
Description=IoT Gateway
Wants=network-online.target
After=network-online.target mosquitto.service
Requires=mosquitto.service

[Service]
Type=simple
User=${GATEWAY_USER}
Group=${GATEWAY_GROUP}
WorkingDirectory=/opt/iot-gateway
ExecStart=/opt/iot-gateway/iot-gateway
Restart=always
RestartSec=5
Environment=MQTT_BROKER=tcp://127.0.0.1:${MQTT_LISTENER_PORT}
Environment=MQTT_USER=${MQTT_USER}
EnvironmentFile=-/etc/iot-gateway/env
Environment=SQLITE_PATH=/srv/iot-data/telemetry.db
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

  chmod 0644 /etc/systemd/system/iot-gateway.service
  systemctl daemon-reload
  systemctl enable iot-gateway.service
fi

systemctl enable mosquitto.service bluetooth.service
systemctl restart mosquitto.service

if [[ -n "$INSTALL_GATEWAY_BINARY" ]]; then
  systemctl restart iot-gateway.service
fi

if ! systemctl is-active --quiet mosquitto.service; then
  journalctl -u mosquitto.service -b --no-pager -n 100 >&2 || true
  fail "Mosquitto did not start"
fi

echo "Provisioning completed."
echo "MQTT host: $(hostname -I | awk '{print $1}')"
echo "MQTT port: ${MQTT_LISTENER_PORT}"
echo "MQTT user: ${MQTT_USER}"
echo "MQTT password saved at: /etc/iot-gateway/mqtt-password.txt"

if [[ -n "$INSTALL_GATEWAY_BINARY" ]]; then
  echo "Gateway service: $(systemctl is-active iot-gateway.service)"
fi
