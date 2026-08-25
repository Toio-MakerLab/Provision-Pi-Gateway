1. Chọn hệ điều hành & kiến trúc build

Pi Zero 2 W dùng chip BCM2710A1 (Cortex‑A53, giống Pi 3) nên hỗ trợ 64-bit.

- Nếu flash Raspberry Pi OS Lite (64-bit) → dùng binary đã build sẵn ở bin/provisiond (arm64, đã test cross-compile OK).
- Nếu bạn dùng bản 32-bit (đôi khi vẫn phổ biến trên Zero vì RAM thấp) → build lại bằng:
cd ~/Documents/IoT/provision-pi-gateway
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags='-s -w' -o bin/provisiond ./cmd/provisiond

Khuyến nghị: flash 64-bit Lite, nhẹ, đủ dùng cho việc này, tránh phải build 2 bản.

2. Chuẩn bị Pi trước khi cắm vào

Dùng Raspberry Pi Imager, ở phần "Advanced options" (bánh răng):
- Bật SSH (dùng password hoặc key của bạn)
- Cấu hình Wi-Fi tạm (không bắt buộc — mục đích của provisiond chính là để không cần bước này, nhưng lần đầu SSH vào thì cần)
- Đặt hostname, ví dụ mydevice.local

3. Đấu nối màn hình OLED SSD1306 (I2C)

Header 40 chân của Pi Zero 2 W dùng chung layout với các Pi khác:

┌──────┬──────────────────────────┐
│ OLED │ Pi Zero 2 W (pin vật lý) │
├──────┼──────────────────────────┤
│ VCC  │ Pin 1 (3.3V)             │
├──────┼──────────────────────────┤
│ GND  │ Pin 6 (GND)              │
├──────┼──────────────────────────┤
│ SDA  │ Pin 3 (GPIO2 / SDA1)     │
├──────┼──────────────────────────┤
│ SCL  │ Pin 5 (GPIO3 / SCL1)     │
└──────┴──────────────────────────┘

Đây chính là bus i2c-1 mặc định mà install.sh sẽ bật.

4. Copy code lên Pi

Từ máy Mac:
cd ~/Documents/IoT/provision-pi-gateway
rsync -av --exclude .git . pi@mydevice.local:~/provision-pi-gateway/
(hoặc scp -r nếu không có rsync)

5. Cài đặt trên Pi

SSH vào Pi rồi chạy:
ssh pi@mydevice.local
cd ~/provision-pi-gateway
chmod +x scripts/*.sh
sudo ./scripts/install.sh

Script này sẽ tự động:
- Cài network-manager, nftables, i2c-tools
- Bật I2C (raspi-config nonint do_i2c 0, hoặc thêm dtparam=i2c_arm=on vào config.txt)
- Tạo user hệ thống provision
- Cài binary vào /usr/local/bin/provisiond, unit systemd, sudoers rule, nftables rule
- Enable + start provisiond.service

Nếu I2C được bật lần đầu qua việc ghi vào config.txt (không có raspi-config), bạn cần reboot để có hiệu lực:
sudo reboot

6. ⚠️ Đổi mật khẩu mặc định trước khi dùng thật

Trước khi triển khai thật, sửa /etc/rpi-provision/provisiond.env:
sudo nano /etc/rpi-provision/provisiond.env
Bắt buộc đổi:
- AP_PASSWORD=ChangeMe-123 → mật khẩu mạnh hơn cho SoftAP setup
- ADMIN_TOKEN= → đặt token nếu muốn bảo vệ endpoint admin

Sau khi sửa:
sudo systemctl restart provisiond

7. Kiểm tra

# trạng thái service
systemctl status provisiond

# xem log realtime (bao gồm cảnh báo nếu OLED không kết nối được)
sudo journalctl -u provisiond -f

# kiểm tra I2C thấy màn hình chưa (địa chỉ cố định 0x3C)
i2cdetect -y 1
Nếu i2cdetect không thấy thiết bị ở 0x3C, kiểm tra lại dây nối hoặc module OLED của bạn dùng địa chỉ 0x3D (trường hợp này code hiện tại chưa hỗ trợ đổi địa chỉ — cần sửa thêm nếu gặp phải).

8. Test luồng provisioning thực tế

1. Pi chưa có Wi-Fi đã lưu → nó tự bật AP MyDevice-Setup (hoặc SSID bạn đặt trong .env)
2. Màn hình OLED hiển thị "Setup Mode" + SSID + địa chỉ portal
3. Dùng điện thoại kết nối vào AP đó, mở http://192.168.12.1:8080, chọn Wi-Fi nhà bạn, nhập mật khẩu
4. Pi kết nối Wi-Fi thật, tắt AP, OLED chuyển sang hiển thị "Wi-Fi Connected" + SSID + IP

Nếu muốn reset để test lại từ đầu:
sudo ~/provision-pi-gateway/scripts/reset-wifi.sh

./scripts/deploy.sh pi@mydevice.local          # build arm64 + copy
./scripts/deploy.sh pi@mydevice.local armv7    # cho Pi OS 32-bit

9. API Endpoints (HTTP_ADDRESS, mặc định 192.168.12.1:8080)

Endpoint public (không cần token, dùng cho portal setup Wi-Fi):

GET /
  Trang HTML portal setup Wi-Fi (form chọn SSID + nhập mật khẩu).

GET /healthz
  Health check. Response: {"ok": true}

GET /api/status
  Trạng thái hiện tại của thiết bị.
  Response: {"interface","ap_active","connected","ssid","ipv4","provisioning_url"}

GET /api/wifi/scan
  Quét các mạng Wi-Fi xung quanh (nmcli rescan).
  Response: [{"ssid","signal","security"}, ...]

POST /api/wifi/connect
  Kết nối vào một mạng Wi-Fi. Nếu thành công, AP setup sẽ tự tắt sau ~2s.
  Body: {"ssid": "...", "password": "..."}
  Response 200: {"ok": true, "message": "..."}
  Response 409: đang có một lần connect khác chạy dở
  Response 502: kết nối thất bại (sai SSID/mật khẩu), AP được bật lại

Endpoint admin (yêu cầu header X-Admin-Token, bị tắt hoàn toàn nếu ADMIN_TOKEN trống):

DELETE /api/wifi/profile
  Xoá riêng profile Wi-Fi đang dùng (WIFI_PROFILE, mặc định "device-wifi") rồi bật lại AP setup.
  curl -X DELETE http://<pi-ip>:8080/api/wifi/profile -H "X-Admin-Token: <token>"

POST /api/reset-network
  Xoá TẤT CẢ profile Wi-Fi đã lưu (trừ AP profile) rồi bật lại AP setup.
  curl -X POST http://<pi-ip>:8080/api/reset-network -H "X-Admin-Token: <token>"

POST /api/reboot
  Reboot thiết bị. Chỉ hoạt động nếu ALLOW_REBOOT=true trong provisiond.env (mặc định false → 403).
  curl -X POST http://<pi-ip>:8080/api/reboot -H "X-Admin-Token: <token>"

Không có mDNS (mạng lạ/router chặn) — quét mạng

Từ máy khác cùng Wi-Fi:
# macOS/Linux
arp -a | grep -i b8:27:eb   # Raspberry Pi MAC prefix cũ, hoặc dc:a6:32 / e4:5f:01 cho Pi mới
# hoặc
nmap -sn 192.168.1.0/24     # đổi đúng subnet router nhà bạn
Hoặc vào trang quản trị router (DHCP client list) — thường có tên pi-gateway.
