# Provision Raspberry Pi IoT Gateway

Script `provision-pi-gateway.sh` tự động cấu hình Raspberry Pi OS Lite/Debian thành IoT gateway nhẹ:

- Eclipse Mosquitto MQTT broker chạy trực tiếp qua `systemd`
- MQTT listener LAN tại port `1883`, bắt buộc username/password
- SQLite data directory tại `/srv/iot-data`
- BlueZ Bluetooth service
- `gpiod` tools để kiểm tra GPIO Linux
- System user `iot` không có shell login
- Tùy chọn cài Go gateway binary và tạo `iot-gateway.service`

## Yêu cầu

- Raspberry Pi OS Lite hoặc Debian-based OS
- Internet trong lúc provision để cài package qua `apt`
- User chạy script có quyền `sudo`
- Nếu cài Go gateway: binary phải đúng kiến trúc target Pi

Kiểm tra kiến trúc target:

```bash
uname -m
dpkg --print-architecture
```

Pi Zero 2 W chạy Raspberry Pi OS 64-bit thường trả về:

```text
aarch64
arm64
```

## Sao chép script lên Pi

Từ Mac/Linux development machine:

```bash
scp provision-pi-gateway.sh \
  <USER>@pi-gateway.local:/home/<USER>/
```

Hoặc dùng IP của Pi:

```bash
scp provision-pi-gateway.sh \
  <USER>@192.168.1.50:/home/<USER>/
```

SSH vào Pi:

```bash
ssh <USER>@pi-gateway.local
```

Cấp quyền execute:

```bash
chmod +x ~/provision-pi-gateway.sh
```

## Chạy provisioning cơ bản

Script tự tạo MQTT password ngẫu nhiên nếu không truyền `MQTT_PASSWORD`:

```bash
sudo ~/provision-pi-gateway.sh
```

Sau khi chạy xong, xem MQTT credential:

```bash
sudo cat /etc/iot-gateway/mqtt-password.txt
```

Thông tin mặc định:

| Biến | Giá trị mặc định |
|---|---|
| MQTT username | `esp32` |
| MQTT port | `1883` |
| MQTT listener | `0.0.0.0` / LAN |
| MQTT persistence | `false` |
| SQLite directory | `/srv/iot-data` |
| System user | `iot` |

## Chỉ định MQTT username/password

Ví dụ dùng username `esp32-gateway`:

```bash
sudo MQTT_USER=esp32-gateway \
  MQTT_PASSWORD='THAY_BANG_MAT_KHAU_MANH_RIEN' \
  ~/provision-pi-gateway.sh
```

> Không commit password vào Git. Lệnh có password có thể xuất hiện trong terminal history; trong production, nên inject secret qua secret manager, USB provisioning hoặc file root-only tạm thời.

## Cài Go gateway service

### Build binary trên macOS

Build cho Raspberry Pi OS 64-bit/Pi Zero 2 W:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
go build -trimpath -ldflags='-s -w' \
  -o iot-gateway ./cmd/gateway
```

Nếu target là Raspberry Pi OS 32-bit:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
go build -trimpath -ldflags='-s -w' \
  -o iot-gateway ./cmd/gateway
```

Kiểm tra binary đã build:

```bash
file iot-gateway
```

Copy binary và script lên Pi:

```bash
scp iot-gateway provision-pi-gateway.sh \
  <USER>@pi-gateway.local:/home/<USER>/
```

Trên Pi, chạy:

```bash
chmod +x ~/provision-pi-gateway.sh

sudo INSTALL_GATEWAY_BINARY=/home/<USER>/iot-gateway \
  MQTT_USER=esp32 \
  ~/provision-pi-gateway.sh
```

Script copy binary đến:

```text
/opt/iot-gateway/iot-gateway
```

Và tạo systemd service:

```text
/etc/systemd/system/iot-gateway.service
```

## Cấu trúc file sau provisioning

```text
/etc/mosquitto/conf.d/iot.conf          # Mosquitto LAN config
/etc/mosquitto/passwd                   # MQTT password file, mosquitto:mosquitto, 0600
/etc/iot-gateway/mqtt-password.txt      # MQTT password copy, root:iot, 0640
/etc/iot-gateway/env                    # Runtime config tùy chọn cho Go gateway
/etc/systemd/system/iot-gateway.service # Go gateway systemd unit
/opt/iot-gateway/iot-gateway             # Go gateway binary
/srv/iot-data/                           # SQLite data directory
```

## Runtime configuration Go gateway

Tạo file environment:

```bash
sudo nano /etc/iot-gateway/env
```

Ví dụ:

```bash
LOG_LEVEL=info
DEVICE_ID=pi-gateway-01
MQTT_CLIENT_ID=pi-gateway-01
```

Khóa quyền file, sau đó restart service:

```bash
sudo chown root:iot /etc/iot-gateway/env
sudo chmod 640 /etc/iot-gateway/env
sudo systemctl restart iot-gateway
```

Các biến environment được script đặt sẵn trong unit:

```text
MQTT_BROKER=tcp://127.0.0.1:1883
MQTT_USER=<MQTT_USER>
SQLITE_PATH=/srv/iot-data/telemetry.db
```

Nếu Go application cần password MQTT, đọc `/etc/iot-gateway/mqtt-password.txt` hoặc truyền biến riêng vào `/etc/iot-gateway/env`. Không ghi plaintext password vào `iot-gateway.service`.

## Kiểm tra sau provision

Kiểm tra service:

```bash
sudo systemctl status mosquitto --no-pager
sudo systemctl status bluetooth --no-pager
sudo systemctl status iot-gateway --no-pager
```

Nếu không cài Go binary, service `iot-gateway` chưa tồn tại là bình thường.

Kiểm tra auto-start:

```bash
sudo systemctl is-enabled mosquitto
sudo systemctl is-enabled bluetooth
sudo systemctl is-enabled iot-gateway
```

Kiểm tra port MQTT:

```bash
sudo ss -ltnp | grep ':1883'
```

Kiểm tra quyền file quan trọng:

```bash
sudo ls -l /etc/mosquitto/passwd
sudo ls -ld /srv/iot-data
```

Kết quả kỳ vọng:

```text
-rw------- 1 mosquitto mosquitto ... /etc/mosquitto/passwd
drwxr-x--- 2 iot iot ... /srv/iot-data
```

## Test MQTT local

Đọc username/password:

```bash
MQTT_USER=esp32
MQTT_PASSWORD="$(sudo cat /etc/iot-gateway/mqtt-password.txt)"
```

Terminal 1:

```bash
mosquitto_sub \
  -h 127.0.0.1 \
  -p 1883 \
  -u "$MQTT_USER" \
  -P "$MQTT_PASSWORD" \
  -t 'home/test' \
  -v
```

Terminal 2:

```bash
mosquitto_pub \
  -h 127.0.0.1 \
  -p 1883 \
  -u "$MQTT_USER" \
  -P "$MQTT_PASSWORD" \
  -t 'home/test' \
  -m 'hello from pi gateway'
```

## Log và debug

Mosquitto:

```bash
sudo journalctl -u mosquitto -f
sudo journalctl -u mosquitto -b --no-pager -n 100
```

Go gateway:

```bash
sudo journalctl -u iot-gateway -f
sudo journalctl -u iot-gateway -b --no-pager -n 100
```

Kiểm tra lỗi boot/service:

```bash
sudo systemctl --failed
sudo dmesg -T | grep -i -E 'oom|killed process|error'
```

## Re-run provisioning

Script có thể chạy lại khi cần cập nhật config. Lưu ý: mỗi lần chạy script với default, password MQTT có thể được tạo mới và password file bị ghi đè.

Để giữ password cũ khi re-run:

```bash
sudo MQTT_USER=esp32 \
  MQTT_PASSWORD="$(sudo cat /etc/iot-gateway/mqtt-password.txt)" \
  ~/provision-pi-gateway.sh
```

Nếu có Go binary mới:

```bash
sudo INSTALL_GATEWAY_BINARY=/home/<USER>/iot-gateway \
  MQTT_USER=esp32 \
  MQTT_PASSWORD="$(sudo cat /etc/iot-gateway/mqtt-password.txt)" \
  ~/provision-pi-gateway.sh
```

## Clone image an toàn

Trước khi clone golden image, không giữ MQTT password chung trong image. Xóa hoặc tạo lại credential riêng lúc first boot/provisioning:

```bash
sudo rm -f /etc/mosquitto/passwd
sudo rm -f /etc/iot-gateway/mqtt-password.txt
```

Nếu image có `password_file /etc/mosquitto/passwd` và file đã bị xóa, Mosquitto sẽ không start cho tới khi provision lại. Đây là hành vi an toàn để tránh mọi Pi dùng chung MQTT credential.
