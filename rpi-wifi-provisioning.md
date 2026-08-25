# Raspberry Pi Wi‑Fi Provisioning Appliance

Gói triển khai này tạo một giải pháp provisioning Wi‑Fi cho Raspberry Pi gồm:

- NetworkManager (`nmcli`) quản lý STA Wi‑Fi và SoftAP
- Go HTTP server phục vụ portal cấu hình nội bộ
- Service chạy khi boot để quyết định STA hay AP mode
- GPIO long-press reset network / factory reset
- Recovery flag trên boot partition: `/boot/firmware/RESET_WIFI`
- systemd hardening và `sudoers` whitelist

> Mục tiêu: Khi Pi không kết nối được Wi‑Fi đã lưu, nó bật AP `MyDevice-Setup-XXXX`. Người dùng truy cập `http://192.168.12.1`, chọn SSID và nhập mật khẩu. Sau khi kết nối thành công, AP dừng.

---

## 1. Kiến trúc

```text
boot
 └─ provisiond
     ├─ RESET_WIFI flag hoặc GPIO long press? → xóa profile Wi‑Fi
     ├─ kiểm tra Wi‑Fi saved profile có kết nối được không
     │   ├─ có → stop AP, normal mode
     │   └─ không → start SoftAP
     └─ HTTP portal :8080, chỉ bind gateway AP 192.168.12.1

HTTP portal
 ├─ GET  /                 giao diện HTML
 ├─ GET  /api/status       trạng thái Wi‑Fi/AP
 ├─ GET  /api/wifi/scan    scan SSID
 ├─ POST /api/wifi/connect lưu + activate Wi‑Fi
 ├─ POST /api/reset-network
 └─ POST /api/reboot
```

Portal dùng port `8080` trong code, sau đó firewall redirect port 80 của AP vào 8080. Nếu không cần captive-like convenience, người dùng mở `http://192.168.12.1:8080` và có thể bỏ phần redirect.

---

## 2. Yêu cầu

- Raspberry Pi OS Bookworm hoặc mới hơn, có NetworkManager.
- Wi‑Fi interface mặc định: `wlan0`.
- Go 1.22+ khi build trên Pi; hoặc cross-compile từ máy ARM64/x86_64.
- Có quyền root khi cài đặt.

Cài dependencies:

```bash
sudo apt update
sudo apt install -y network-manager nftables gpiod
sudo systemctl enable --now NetworkManager
```

Kiểm tra:

```bash
nmcli general status
nmcli device status
```

Nếu hệ thống còn dùng `dhcpcd`/`wpa_supplicant` để quản lý `wlan0`, không để chúng tranh quyền với NetworkManager.

---

## 3. Cấu trúc thư mục

```text
rpi-wifi-provisioning/
├── go.mod
├── cmd/provisiond/main.go
├── config/provisiond.env
├── systemd/provisiond.service
├── systemd/provisiond-reset-wifi.service
├── scripts/install.sh
├── scripts/reset-wifi.sh
├── scripts/factory-reset.sh
├── scripts/rpi-provision
├── sudoers/rpi-provision
└── nftables/90-provisioning.nft
```

---

## 4. Mã Go

Tạo `go.mod`:

```go
module example.com/rpi-provisioning

go 1.22
```

Tạo `cmd/provisiond/main.go`:

```go
package main

import (
    "context"
    "crypto/subtle"
    "encoding/json"
    "errors"
    "fmt"
    "html/template"
    "log/slog"
    "net"
    "net/http"
    "os"
    "os/exec"
    "os/signal"
    "path/filepath"
    "regexp"
    "sort"
    "strings"
    "sync"
    "syscall"
    "time"
)

type Config struct {
    Interface       string
    APProfile       string
    APSSID          string
    APPassword      string
    APAddress       string
    HTTPAddress     string
    WiFiProfile     string
    ConnectTimeout  time.Duration
    RetryTimeout    time.Duration
    ResetFlag       string
    AllowReboot     bool
    AdminToken      string
}

type Service struct {
    cfg     Config
    logger  *slog.Logger
    mu      sync.Mutex
    apOn    bool
    connect bool
}

type Status struct {
    Interface       string `json:"interface"`
    APActive        bool   `json:"ap_active"`
    Connected       bool   `json:"connected"`
    SSID            string `json:"ssid,omitempty"`
    IPv4            string `json:"ipv4,omitempty"`
    ProvisioningURL string `json:"provisioning_url,omitempty"`
}

type Network struct {
    SSID     string `json:"ssid"`
    Signal   int    `json:"signal"`
    Security string `json:"security"`
}

type connectRequest struct {
    SSID     string `json:"ssid"`
    Password string `json:"password"`
}

func getenv(key, fallback string) string {
    if v := strings.TrimSpace(os.Getenv(key)); v != "" {
        return v
    }
    return fallback
}

func getenvBool(key string, fallback bool) bool {
    switch strings.ToLower(getenv(key, "")) {
    case "1", "true", "yes", "on":
        return true
    case "0", "false", "no", "off":
        return false
    default:
        return fallback
    }
}

func getenvDuration(key string, fallback time.Duration) time.Duration {
    v := getenv(key, "")
    if v == "" {
        return fallback
    }
    d, err := time.ParseDuration(v)
    if err != nil {
        return fallback
    }
    return d
}

func configFromEnv() Config {
    return Config{
        Interface:      getenv("WIFI_INTERFACE", "wlan0"),
        APProfile:      getenv("AP_PROFILE", "device-provision-ap"),
        APSSID:         getenv("AP_SSID", "MyDevice-Setup"),
        APPassword:     getenv("AP_PASSWORD", "ChangeMe-123"),
        APAddress:      getenv("AP_ADDRESS", "192.168.12.1"),
        HTTPAddress:    getenv("HTTP_ADDRESS", "192.168.12.1:8080"),
        WiFiProfile:    getenv("WIFI_PROFILE", "device-wifi"),
        ConnectTimeout: getenvDuration("CONNECT_TIMEOUT", 35*time.Second),
        RetryTimeout:   getenvDuration("RETRY_TIMEOUT", 25*time.Second),
        ResetFlag:      getenv("RESET_FLAG", "/boot/firmware/RESET_WIFI"),
        AllowReboot:    getenvBool("ALLOW_REBOOT", false),
        AdminToken:     getenv("ADMIN_TOKEN", ""),
    }
}

func main() {
    cfg := configFromEnv()
    if len(cfg.APPassword) < 8 {
        slog.Error("AP_PASSWORD must contain at least 8 characters")
        os.Exit(1)
    }
    if net.ParseIP(cfg.APAddress) == nil {
        slog.Error("invalid AP_ADDRESS", "value", cfg.APAddress)
        os.Exit(1)
    }

    logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
    s := &Service{cfg: cfg, logger: logger}

    ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer cancel()

    if err := s.bootstrap(ctx); err != nil {
        logger.Error("bootstrap failed", "error", err)
        _ = s.enableAP(context.Background())
    }

    server := &http.Server{
        Addr:              cfg.HTTPAddress,
        Handler:           s.routes(),
        ReadHeaderTimeout: 5 * time.Second,
        ReadTimeout:       10 * time.Second,
        WriteTimeout:      15 * time.Second,
        IdleTimeout:       60 * time.Second,
    }

    go func() {
        logger.Info("HTTP provisioning server started", "address", cfg.HTTPAddress)
        if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
            logger.Error("HTTP server stopped", "error", err)
            cancel()
        }
    }()

    <-ctx.Done()
    shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer shutdownCancel()
    _ = server.Shutdown(shutdownCtx)
}

func (s *Service) bootstrap(ctx context.Context) error {
    if _, err := os.Stat(s.cfg.ResetFlag); err == nil {
        s.logger.Warn("reset flag exists; clearing Wi-Fi profiles", "path", s.cfg.ResetFlag)
        if err := s.forgetWiFi(ctx); err != nil {
            return err
        }
        if err := os.Remove(s.cfg.ResetFlag); err != nil {
            return fmt.Errorf("remove reset flag: %w", err)
        }
        return s.enableAP(ctx)
    } else if !errors.Is(err, os.ErrNotExist) {
        return fmt.Errorf("read reset flag: %w", err)
    }

    if s.waitForWiFi(ctx, s.cfg.RetryTimeout) {
        return s.disableAP(ctx)
    }
    return s.enableAP(ctx)
}

func (s *Service) routes() http.Handler {
    mux := http.NewServeMux()
    mux.HandleFunc("GET /", s.handleIndex)
    mux.HandleFunc("GET /healthz", s.handleHealth)
    mux.HandleFunc("GET /api/status", s.handleStatus)
    mux.HandleFunc("GET /api/wifi/scan", s.handleScan)
    mux.HandleFunc("POST /api/wifi/connect", s.handleConnect)
    mux.HandleFunc("POST /api/reset-network", s.requireToken(s.handleResetNetwork))
    mux.HandleFunc("POST /api/reboot", s.requireToken(s.handleReboot))
    return securityHeaders(s.limitBody(mux))
}

func (s *Service) handleIndex(w http.ResponseWriter, r *http.Request) {
    if r.URL.Path != "/" {
        http.NotFound(w, r)
        return
    }
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    _, _ = w.Write([]byte(indexHTML))
}

func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
    writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
    st, err := s.status(r.Context())
    if err != nil {
        writeError(w, http.StatusInternalServerError, "cannot read network status")
        return
    }
    writeJSON(w, http.StatusOK, st)
}

func (s *Service) handleScan(w http.ResponseWriter, r *http.Request) {
    networks, err := s.scan(r.Context())
    if err != nil {
        s.logger.Warn("Wi-Fi scan failed", "error", err)
        writeError(w, http.StatusBadGateway, "Wi-Fi scan failed")
        return
    }
    writeJSON(w, http.StatusOK, networks)
}

func (s *Service) handleConnect(w http.ResponseWriter, r *http.Request) {
    var req connectRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeError(w, http.StatusBadRequest, "invalid JSON")
        return
    }
    req.SSID = strings.TrimSpace(req.SSID)
    if !validSSID(req.SSID) {
        writeError(w, http.StatusBadRequest, "SSID is invalid")
        return
    }
    if len(req.Password) > 63 {
        writeError(w, http.StatusBadRequest, "password is too long")
        return
    }

    s.mu.Lock()
    if s.connect {
        s.mu.Unlock()
        writeError(w, http.StatusConflict, "another connection attempt is active")
        return
    }
    s.connect = true
    s.mu.Unlock()
    defer func() {
        s.mu.Lock()
        s.connect = false
        s.mu.Unlock()
    }()

    ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ConnectTimeout+15*time.Second)
    defer cancel()

    if err := s.connectWiFi(ctx, req.SSID, req.Password); err != nil {
        s.logger.Warn("Wi-Fi connect failed", "ssid", req.SSID, "error", err)
        _ = s.enableAP(context.Background())
        writeJSON(w, http.StatusBadGateway, map[string]any{
            "ok":      false,
            "message": "Cannot join this Wi-Fi. Check SSID and password, then retry.",
        })
        return
    }

    go func() {
        time.Sleep(2 * time.Second)
        if err := s.disableAP(context.Background()); err != nil {
            s.logger.Error("disable AP after successful provisioning", "error", err)
        }
    }()
    writeJSON(w, http.StatusOK, map[string]any{
        "ok":      true,
        "message": "Wi-Fi connected. Your phone will lose the setup network shortly.",
    })
}

func (s *Service) handleResetNetwork(w http.ResponseWriter, r *http.Request) {
    if err := s.forgetWiFi(r.Context()); err != nil {
        s.logger.Error("network reset failed", "error", err)
        writeError(w, http.StatusInternalServerError, "cannot reset Wi-Fi")
        return
    }
    if err := s.enableAP(r.Context()); err != nil {
        writeError(w, http.StatusInternalServerError, "Wi-Fi was reset but AP could not start")
        return
    }
    writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Wi-Fi profiles removed; setup AP is active."})
}

func (s *Service) handleReboot(w http.ResponseWriter, r *http.Request) {
    if !s.cfg.AllowReboot {
        writeError(w, http.StatusForbidden, "reboot endpoint is disabled")
        return
    }
    writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "message": "Device is rebooting."})
    go func() {
        time.Sleep(time.Second)
        if err := run(context.Background(), "sudo", "/usr/local/lib/rpi-provision/rpi-provision", "reboot"); err != nil {
            s.logger.Error("reboot failed", "error", err)
        }
    }()
}

func (s *Service) status(ctx context.Context) (Status, error) {
    state, err := runOutput(ctx, "nmcli", "-t", "-f", "GENERAL.STATE,GENERAL.CONNECTION,IP4.ADDRESS", "device", "show", s.cfg.Interface)
    if err != nil {
        return Status{}, err
    }
    active, _ := s.apActive(ctx)
    st := Status{Interface: s.cfg.Interface, APActive: active}
    for _, line := range strings.Split(strings.TrimSpace(state), "\n") {
        parts := strings.SplitN(line, ":", 2)
        if len(parts) != 2 {
            continue
        }
        switch parts[0] {
        case "GENERAL.STATE":
            st.Connected = strings.HasPrefix(parts[1], "100")
        case "GENERAL.CONNECTION":
            if parts[1] != "--" && parts[1] != s.cfg.APProfile {
                st.SSID = parts[1]
            }
        case "IP4.ADDRESS[1]":
            st.IPv4 = strings.Split(parts[1], "/")[0]
        }
    }
    if st.APActive {
        st.ProvisioningURL = "http://" + s.cfg.APAddress
    }
    return st, nil
}

func (s *Service) scan(ctx context.Context) ([]Network, error) {
    out, err := runOutput(ctx, "nmcli", "-t", "-e", "yes", "-f", "SSID,SIGNAL,SECURITY", "device", "wifi", "list", "ifname", s.cfg.Interface, "--rescan", "yes")
    if err != nil {
        return nil, err
    }
    bySSID := make(map[string]Network)
    for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
        fields := splitEscapedColon(line)
        if len(fields) < 3 {
            continue
        }
        ssid := strings.TrimSpace(fields[0])
        if ssid == "" || !validSSID(ssid) {
            continue
        }
        var signal int
        _, _ = fmt.Sscanf(fields[1], "%d", &signal)
        n := Network{SSID: ssid, Signal: signal, Security: fields[2]}
        if previous, exists := bySSID[ssid]; !exists || n.Signal > previous.Signal {
            bySSID[ssid] = n
        }
    }
    list := make([]Network, 0, len(bySSID))
    for _, n := range bySSID {
        list = append(list, n)
    }
    sort.Slice(list, func(i, j int) bool { return list[i].Signal > list[j].Signal })
    return list, nil
}

func (s *Service) connectWiFi(ctx context.Context, ssid, password string) error {
    _ = s.enableAP(ctx)
    _ = run(ctx, "sudo", "/usr/local/lib/rpi-provision/rpi-provision", "delete-wifi-profile", s.cfg.WiFiProfile)

    args := []string{"sudo", "/usr/local/lib/rpi-provision/rpi-provision", "connect", s.cfg.Interface, s.cfg.WiFiProfile, ssid, password}
    if err := run(ctx, args[0], args[1:]...); err != nil {
        return err
    }
    if !s.waitForWiFi(ctx, s.cfg.ConnectTimeout) {
        return errors.New("timed out waiting for Wi-Fi IPv4 connectivity")
    }
    return nil
}

func (s *Service) waitForWiFi(ctx context.Context, timeout time.Duration) bool {
    deadline := time.Now().Add(timeout)
    for time.Now().Before(deadline) {
        if ctx.Err() != nil {
            return false
        }
        out, err := runOutput(ctx, "nmcli", "-t", "-f", "GENERAL.STATE,GENERAL.CONNECTION,IP4.ADDRESS", "device", "show", s.cfg.Interface)
        if err == nil && connectedToSTA(out, s.cfg.APProfile) {
            return true
        }
        time.Sleep(2 * time.Second)
    }
    return false
}

func connectedToSTA(out, apProfile string) bool {
    ready, connection, hasIP := false, "", false
    for _, line := range strings.Split(out, "\n") {
        p := strings.SplitN(line, ":", 2)
        if len(p) != 2 {
            continue
        }
        switch p[0] {
        case "GENERAL.STATE":
            ready = strings.HasPrefix(p[1], "100")
        case "GENERAL.CONNECTION":
            connection = p[1]
        case "IP4.ADDRESS[1]":
            hasIP = p[1] != ""
        }
    }
    return ready && hasIP && connection != "" && connection != "--" && connection != apProfile
}

func (s *Service) apActive(ctx context.Context) (bool, error) {
    out, err := runOutput(ctx, "nmcli", "-t", "-f", "NAME", "connection", "show", "--active")
    if err != nil {
        return false, err
    }
    for _, v := range strings.Split(strings.TrimSpace(out), "\n") {
        if v == s.cfg.APProfile {
            return true, nil
        }
    }
    return false, nil
}

func (s *Service) enableAP(ctx context.Context) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    active, err := s.apActive(ctx)
    if err == nil && active {
        s.apOn = true
        return nil
    }
    err = run(ctx, "sudo", "/usr/local/lib/rpi-provision/rpi-provision", "ap-up", s.cfg.Interface, s.cfg.APProfile, s.cfg.APSSID, s.cfg.APPassword, s.cfg.APAddress)
    if err == nil {
        s.apOn = true
    }
    return err
}

func (s *Service) disableAP(ctx context.Context) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    err := run(ctx, "sudo", "/usr/local/lib/rpi-provision/rpi-provision", "ap-down", s.cfg.APProfile)
    if err == nil {
        s.apOn = false
    }
    return err
}

func (s *Service) forgetWiFi(ctx context.Context) error {
    return run(ctx, "sudo", "/usr/local/lib/rpi-provision/rpi-provision", "forget-wifi", s.cfg.Interface, s.cfg.APProfile)
}

func (s *Service) requireToken(next http.HandlerFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if s.cfg.AdminToken == "" {
            writeError(w, http.StatusForbidden, "administrative endpoint disabled")
            return
        }
        got := r.Header.Get("X-Admin-Token")
        if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AdminToken)) != 1 {
            writeError(w, http.StatusUnauthorized, "invalid administrative token")
            return
        }
        next(w, r)
    }
}

func (s *Service) limitBody(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.Method == http.MethodPost {
            r.Body = http.MaxBytesReader(w, r.Body, 4096)
        }
        next.ServeHTTP(w, r)
    })
}

func securityHeaders(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("X-Content-Type-Options", "nosniff")
        w.Header().Set("X-Frame-Options", "DENY")
        w.Header().Set("Referrer-Policy", "no-referrer")
        w.Header().Set("Cache-Control", "no-store")
        w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'")
        next.ServeHTTP(w, r)
    })
}

func validSSID(ssid string) bool {
    return len(ssid) > 0 && len([]byte(ssid)) <= 32 && !strings.ContainsAny(ssid, "\x00\r\n")
}

var lineUnsafe = regexp.MustCompile(`[\x00\r\n]`)

func splitEscapedColon(s string) []string {
    var out []string
    var b strings.Builder
    escaped := false
    for _, r := range s {
        if escaped {
            b.WriteRune(r)
            escaped = false
            continue
        }
        if r == '\\' {
            escaped = true
            continue
        }
        if r == ':' {
            out = append(out, b.String())
            b.Reset()
            continue
        }
        b.WriteRune(r)
    }
    if escaped {
        b.WriteRune('\\')
    }
    out = append(out, b.String())
    return out
}

func run(ctx context.Context, name string, args ...string) error {
    if lineUnsafe.MatchString(name) {
        return errors.New("unsafe command")
    }
    cmd := exec.CommandContext(ctx, name, args...)
    cmd.Env = append(os.Environ(), "LC_ALL=C")
    data, err := cmd.CombinedOutput()
    if err != nil {
        return fmt.Errorf("%s failed: %w: %s", name, err, strings.TrimSpace(string(data)))
    }
    return nil
}

func runOutput(ctx context.Context, name string, args ...string) (string, error) {
    cmd := exec.CommandContext(ctx, name, args...)
    cmd.Env = append(os.Environ(), "LC_ALL=C")
    data, err := cmd.CombinedOutput()
    if err != nil {
        return "", fmt.Errorf("%s failed: %w: %s", name, err, strings.TrimSpace(string(data)))
    }
    return string(data), nil
}

func writeJSON(w http.ResponseWriter, code int, body any) {
    w.Header().Set("Content-Type", "application/json; charset=utf-8")
    w.WriteHeader(code)
    _ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, code int, message string) {
    writeJSON(w, code, map[string]any{"ok": false, "error": message})
}

const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Device Wi-Fi setup</title>
<style>
:root{color-scheme:light dark}body{font-family:system-ui,sans-serif;margin:0;background:#f4f7fb;color:#162033}.card{max-width:540px;margin:32px auto;padding:24px;background:#fff;border-radius:12px;box-shadow:0 6px 20px #0002}h1{margin-top:0;font-size:1.4rem}label{display:block;margin-top:14px;font-weight:600}input,select,button{box-sizing:border-box;width:100%;margin-top:6px;padding:11px;border-radius:8px;border:1px solid #aab4c4;font:inherit}button{border:0;background:#0759c7;color:#fff;cursor:pointer;font-weight:700;margin-top:20px}button:disabled{opacity:.6;cursor:wait}#status{padding:10px;border-radius:8px;background:#eaf1ff;margin:12px 0;white-space:pre-wrap}.muted{font-size:.9rem;color:#596579}.error{background:#ffe8e8!important;color:#8b1111}.ok{background:#e6f8eb!important;color:#125e26}@media(prefers-color-scheme:dark){body{background:#121820;color:#e8edf8}.card{background:#1b2430}input,select{background:#111923;color:#e8edf8;border-color:#526277}#status{background:#1a355d}.error{background:#4e1b22!important}.ok{background:#153d24!important}}</style>
</head>
<body><main class="card"><h1>Connect this device to Wi‑Fi</h1><p class="muted">Choose your network and enter its password. The setup Wi‑Fi will disconnect after success.</p><div id="status">Loading status…</div><form id="form"><label for="ssid">Wi‑Fi network</label><select id="ssid" required><option value="">Scan is running…</option></select><label for="password">Password</label><input id="password" type="password" maxlength="63" autocomplete="current-password" placeholder="Leave empty for open networks"><button id="submit" type="submit">Connect</button></form><button id="rescan" type="button">Scan again</button></main>
<script>
const $=id=>document.getElementById(id), status=$('status');
function msg(text,kind=''){status.textContent=text;status.className=kind}
async function api(path,opts){const r=await fetch(path,opts);let b;try{b=await r.json()}catch{b={error:'Invalid server response'}}if(!r.ok)throw new Error(b.error||b.message||'Request failed');return b}
async function loadStatus(){try{const s=await api('/api/status');msg(s.connected?`Connected: ${s.ssid||'Wi‑Fi'} ${s.ipv4||''}`:`Setup AP active. Open ${s.provisioning_url||location.origin}`,'ok')}catch(e){msg(e.message,'error')}}
async function scan(){const select=$('ssid');select.innerHTML='<option>Scanning…</option>';try{const ns=await api('/api/wifi/scan');select.innerHTML='<option value="">Select a Wi‑Fi network</option>';for(const n of ns){const o=document.createElement('option');o.value=n.ssid;o.textContent=`${n.ssid} — ${n.signal}% ${n.security?'('+n.security+')':''}`;select.appendChild(o)}if(ns.length===0)msg('No visible Wi‑Fi networks. Move closer to the router and scan again.','error')}catch(e){select.innerHTML='<option value="">Scan failed</option>';msg(e.message,'error')}}
$('form').addEventListener('submit',async e=>{e.preventDefault();const b=$('submit');b.disabled=true;msg('Connecting… this can take up to 35 seconds.');try{const r=await api('/api/wifi/connect',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({ssid:$('ssid').value,password:$('password').value})});msg(r.message,'ok')}catch(e){msg(e.message,'error')}finally{b.disabled=false}});
$('rescan').onclick=scan;loadStatus();scan();
</script></body></html>`

func init() {
    _ = filepath.Separator
}
```

> Ghi chú: `filepath` chỉ được giữ để compiler không báo import dư thừa nếu bạn muốn mở rộng phần factory reset path. Có thể bỏ cả import và `init()` nếu không cần.

Thay hai đoạn cuối của file Go trên bằng việc **xóa** import `path/filepath` và hàm `init()`; đây là phiên bản sạch hơn:

```go
// Xóa: "path/filepath"
// Xóa toàn bộ:
// func init() { _ = filepath.Separator }
```

---

## 5. Privileged wrapper

Web daemon không chạy root. Nó chỉ gọi wrapper với tập lệnh con cố định.

Tạo `scripts/rpi-provision`:

```bash
#!/usr/bin/env bash
set -euo pipefail

NMCLI=/usr/bin/nmcli
SYSTEMCTL=/usr/bin/systemctl
WIFI_INTERFACE_DEFAULT=wlan0

fail() { echo "rpi-provision: $*" >&2; exit 1; }
valid_no_newline() { [[ "$1" != *$'\n'* && "$1" != *$'\r'* && "$1" != *$'\0'* ]]; }
valid_profile() { [[ "$1" =~ ^[A-Za-z0-9._-]{1,64}$ ]]; }
valid_ifname() { [[ "$1" =~ ^[A-Za-z0-9._-]{1,15}$ ]]; }
valid_ssid() { [[ -n "$1" && ${#1} -le 32 ]] && valid_no_newline "$1"; }
valid_password() { [[ ${#1} -le 63 ]] && valid_no_newline "$1"; }
valid_ipv4() { [[ "$1" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; }

cmd="${1:-}"
shift || true

case "$cmd" in
  ap-up)
    [[ $# -eq 5 ]] || fail "usage: ap-up IFACE PROFILE SSID PASSWORD ADDRESS"
    iface="$1" profile="$2" ssid="$3" password="$4" address="$5"
    valid_ifname "$iface" && valid_profile "$profile" && valid_ssid "$ssid" && valid_password "$password" && valid_ipv4 "$address" || fail "invalid argument"
    if ! "$NMCLI" -t -f NAME connection show | grep -Fxq "$profile"; then
      "$NMCLI" connection add type wifi ifname "$iface" con-name "$profile" ssid "$ssid" \
        wifi.mode ap ipv4.method shared ipv4.addresses "${address}/24" ipv6.method disabled \
        connection.autoconnect no
      "$NMCLI" connection modify "$profile" wifi-sec.key-mgmt wpa-psk wifi-sec.psk "$password"
    else
      "$NMCLI" connection modify "$profile" connection.interface-name "$iface" 802-11-wireless.ssid "$ssid" \
        802-11-wireless.mode ap ipv4.method shared ipv4.addresses "${address}/24" ipv6.method disabled \
        connection.autoconnect no wifi-sec.key-mgmt wpa-psk wifi-sec.psk "$password"
    fi
    "$NMCLI" connection up "$profile" ifname "$iface"
    ;;

  ap-down)
    [[ $# -eq 1 ]] && valid_profile "$1" || fail "usage: ap-down PROFILE"
    "$NMCLI" connection down "$1" || true
    ;;

  connect)
    [[ $# -eq 4 ]] || fail "usage: connect IFACE PROFILE SSID PASSWORD"
    iface="$1" profile="$2" ssid="$3" password="$4"
    valid_ifname "$iface" && valid_profile "$profile" && valid_ssid "$ssid" && valid_password "$password" || fail "invalid argument"
    "$NMCLI" device wifi connect "$ssid" password "$password" ifname "$iface" name "$profile"
    "$NMCLI" connection modify "$profile" connection.autoconnect yes connection.autoconnect-priority 100
    ;;

  delete-wifi-profile)
    [[ $# -eq 1 ]] && valid_profile "$1" || fail "usage: delete-wifi-profile PROFILE"
    "$NMCLI" connection delete "$1" 2>/dev/null || true
    ;;

  forget-wifi)
    [[ $# -eq 2 ]] || fail "usage: forget-wifi IFACE AP_PROFILE"
    iface="$1" ap_profile="$2"
    valid_ifname "$iface" && valid_profile "$ap_profile" || fail "invalid argument"
    mapfile -t profiles < <("$NMCLI" -t -f NAME,TYPE connection show | awk -F: '$2 == "802-11-wireless" {print $1}')
    for p in "${profiles[@]}"; do
      [[ "$p" == "$ap_profile" ]] && continue
      "$NMCLI" connection delete "$p" || true
    done
    "$NMCLI" device disconnect "$iface" || true
    ;;

  reboot)
    [[ $# -eq 0 ]] || fail "usage: reboot"
    "$SYSTEMCTL" reboot
    ;;

  *) fail "unknown command" ;;
esac
```

Cấp quyền thực thi:

```bash
chmod 0755 scripts/rpi-provision
```

### Lưu ý về profile Wi‑Fi mở

Wrapper trên yêu cầu `password` vì hướng đến WPA2/WPA3 provisioning. Nếu bạn thực sự cần Wi‑Fi open network, bổ sung một action riêng như `connect-open`, không tự bỏ password tùy theo input HTTP.

---

## 6. Cấu hình môi trường

Tạo `config/provisiond.env`:

```ini
WIFI_INTERFACE=wlan0
WIFI_PROFILE=device-wifi

AP_PROFILE=device-provision-ap
AP_SSID=MyDevice-Setup
AP_PASSWORD=ChangeMe-123
AP_ADDRESS=192.168.12.1
HTTP_ADDRESS=192.168.12.1:8080

CONNECT_TIMEOUT=35s
RETRY_TIMEOUT=25s
RESET_FLAG=/boot/firmware/RESET_WIFI

# Chỉ bật nếu API reboot là cần thiết.
ALLOW_REBOOT=false

# Đặt chuỗi ngẫu nhiên tối thiểu 32 ký tự nếu muốn bật endpoint reset network.
# Tạo bằng: openssl rand -hex 32
ADMIN_TOKEN=
```

Đổi `AP_PASSWORD` trước khi deploy. Tốt hơn: sinh password riêng theo serial/MAC khi image được provision ở factory.

---

## 7. systemd units

Tạo `systemd/provisiond.service`:

```ini
[Unit]
Description=Raspberry Pi Wi-Fi provisioning portal
Wants=NetworkManager.service
After=NetworkManager.service network-online.target

[Service]
Type=simple
User=provision
Group=provision
EnvironmentFile=/etc/rpi-provision/provisiond.env
ExecStart=/usr/local/bin/provisiond
Restart=on-failure
RestartSec=3

NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
RestrictRealtime=true
SystemCallArchitectures=native
ReadWritePaths=/run /var/lib/rpi-provision

[Install]
WantedBy=multi-user.target
```

Tạo `systemd/provisiond-reset-wifi.service`:

```ini
[Unit]
Description=Request Wi-Fi reset through boot flag
ConditionPathExists=/boot/firmware/RESET_WIFI
Before=provisiond.service

[Service]
Type=oneshot
ExecStart=/usr/local/lib/rpi-provision/rpi-provision forget-wifi wlan0 device-provision-ap
ExecStart=/usr/bin/rm -f /boot/firmware/RESET_WIFI

[Install]
WantedBy=multi-user.target
```

Unit reset là tùy chọn vì `provisiond` cũng đã xử lý flag. Chỉ nên chọn **một** nơi xử lý để tránh race; khuyến nghị ở bundle này là để `provisiond` xử lý và **không enable** `provisiond-reset-wifi.service`.

---

## 8. sudoers và quyền tối thiểu

Tạo `sudoers/rpi-provision`:

```sudoers
Cmnd_Alias RPI_PROVISION = /usr/local/lib/rpi-provision/rpi-provision ap-up *, \
                           /usr/local/lib/rpi-provision/rpi-provision ap-down *, \
                           /usr/local/lib/rpi-provision/rpi-provision connect *, \
                           /usr/local/lib/rpi-provision/rpi-provision delete-wifi-profile *, \
                           /usr/local/lib/rpi-provision/rpi-provision forget-wifi *, \
                           /usr/local/lib/rpi-provision/rpi-provision reboot

provision ALL=(root) NOPASSWD: RPI_PROVISION
```

Kiểm tra syntax bằng `visudo` trong quá trình install. Vì wrapper validate toàn bộ argument và không chạy shell từ input HTTP, rule wildcard ở đây được giới hạn bởi action whitelist của wrapper.

---

## 9. nftables: chỉ cho portal qua AP

Tạo `nftables/90-provisioning.nft`:

```nft
#!/usr/sbin/nft -f

table inet rpi_provision {
  chain input {
    type filter hook input priority 0; policy accept;
    iifname "wlan0" ip daddr 192.168.12.1 tcp dport 8080 accept
  }

  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
    iifname "wlan0" ip daddr 192.168.12.1 tcp dport 80 redirect to :8080
  }
}
```

Do `provisiond` bind vào `192.168.12.1:8080`, HTTP UI không xuất hiện trên Ethernet hay địa chỉ Wi‑Fi STA. Rule redirect giúp client mở `http://192.168.12.1` thay vì nhập port.

> Nếu Pi của bạn đang có nftables policy/firewall riêng, hãy tích hợp hai chain này vào ruleset hiện có thay vì overwrite toàn bộ `/etc/nftables.conf`.

---

## 10. GPIO reset button

Bundle code chính không đọc GPIO để tránh phụ thuộc library/board mapping. Cách ổn định là một systemd service riêng chạy `gpiomon` (libgpiod) hoặc một tiny Go GPIO helper.

Ví dụ phần cứng:

```text
GPIO17 (physical pin 11) --- push button --- GND
```

Dùng pull-up nội bộ trong helper. Hành vi:

- Giữ 5 giây: `touch /boot/firmware/RESET_WIFI && reboot`.
- Giữ 12 giây: touch `/var/lib/rpi-provision/FACTORY_RESET` rồi reboot.

Tạo `scripts/reset-wifi.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail
touch /boot/firmware/RESET_WIFI
sync
systemctl reboot
```

Tạo `scripts/factory-reset.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

systemctl stop provisiond.service || true
/usr/local/lib/rpi-provision/rpi-provision forget-wifi wlan0 device-provision-ap || true
rm -rf /var/lib/mydevice/*
rm -f /etc/mydevice/device.env
sync
systemctl restart provisiond.service
```

Factory reset cần được tùy biến theo dữ liệu ứng dụng của bạn. Không dùng `rm -rf /etc`, không xóa SSH host key hay OS files trừ khi có recovery image được thiết kế riêng.

---

## 11. Installer

Tạo `scripts/install.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALL_USER=provision

if [[ $EUID -ne 0 ]]; then
  echo "Run with sudo: sudo ./scripts/install.sh" >&2
  exit 1
fi

apt-get update
apt-get install -y network-manager nftables
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
systemctl enable --now provisiond.service

echo "Installed. Update /etc/rpi-provision/provisiond.env before production deployment."
echo "Logs: journalctl -u provisiond -f"
```

Cấp quyền thực thi:

```bash
chmod 0755 scripts/install.sh scripts/reset-wifi.sh scripts/factory-reset.sh
```

---

## 12. Build và deploy

### Build trực tiếp trên Raspberry Pi

```bash
mkdir -p bin
go build -trimpath -ldflags='-s -w' -o bin/provisiond ./cmd/provisiond
sudo ./scripts/install.sh
```

### Cross-compile từ macOS Apple Silicon cho Raspberry Pi 64-bit

```bash
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -ldflags='-s -w' -o bin/provisiond ./cmd/provisiond

scp -r . pi@raspberrypi.local:/home/pi/rpi-wifi-provisioning
ssh pi@raspberrypi.local 'cd ~/rpi-wifi-provisioning && sudo ./scripts/install.sh'
```

Với Raspberry Pi OS 32-bit thay `GOARCH=arm` và build target phù hợp, thường:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -o bin/provisiond ./cmd/provisiond
```

---

## 13. Test checklist

```bash
# Xem trạng thái daemon và log
systemctl status provisiond
journalctl -u provisiond -f

# Xem profile NetworkManager
nmcli connection show
nmcli connection show --active
nmcli device status

# Bắt đầu provisioning thủ công
sudo nmcli connection down device-wifi || true
sudo nmcli connection up device-provision-ap

# Kiểm tra portal trên Pi
curl http://192.168.12.1:8080/healthz
curl http://192.168.12.1:8080/api/status
curl http://192.168.12.1:8080/api/wifi/scan

# Test recovery Wi-Fi
sudo touch /boot/firmware/RESET_WIFI
sudo reboot
```

Sau reboot có `RESET_WIFI`, daemon xóa profile Wi‑Fi trừ AP profile, xóa flag và bật AP. Kết nối điện thoại/laptop vào SSID AP rồi mở:

```text
http://192.168.12.1
```

---

## 14. Điều chỉnh production

- Không dùng SSID/password AP giống nhau trên mọi thiết bị. Sinh `AP_SSID=MyDevice-<4-byte-MAC>` và password duy nhất trong quá trình manufacturing.
- Dùng HTTPS chỉ khi bạn có cách phân phối/trust certificate phù hợp. Với provisioning LAN tạm thời, WPA2 AP password mạnh + HTTP bind riêng AP thường thực tế hơn certificate tự ký.
- Đặt `ADMIN_TOKEN` ngẫu nhiên hoặc bỏ hẳn endpoint `reset-network`/`reboot` khỏi public portal.
- Với gateway cần online liên tục, dùng USB Wi‑Fi adapter thứ hai: một interface cho STA uplink, một interface riêng cho AP provisioning.
- Nếu cần captive portal hoàn chỉnh, thêm DNS server cục bộ trả mọi A record về `192.168.12.1`, nhưng phải test trên Android/iOS/Windows vì captive portal detection khác nhau.
- Không log request body hoặc output lệnh `nmcli device wifi connect`, vì chúng có thể chứa Wi‑Fi password.

## 15. Lỗi thường gặp

| Hiện tượng | Kiểm tra/khắc phục |
|---|---|
| `wlan0` unavailable | `nmcli device status`; xác nhận Wi‑Fi không bị rfkill: `rfkill list` |
| Hotspot không lên | `journalctl -u NetworkManager -b`; kiểm tra card/driver hỗ trợ AP mode bằng `iw list` |
| Portal không truy cập được | `ip addr show wlan0`, `nmcli connection show --active`, `ss -ltnp | grep 8080` |
| Wi‑Fi connect thất bại | kiểm tra SSID/password, country/regulatory domain, signal và log NetworkManager |
| AP/STATION đá nhau | một radio `wlan0` có giới hạn concurrency; tắt AP trước khi station connect hoặc dùng Wi‑Fi USB thứ hai |
| `sudo` bị hỏi password | `visudo -cf /etc/sudoers.d/rpi-provision`; xác nhận process thực sự chạy bằng user `provision` |

## 16. Trước khi chạy: sửa một chi tiết code

Trong `main.go` được in ở phần 4, hãy đảm bảo import list **không chứa** `"path/filepath"` và cuối file **không chứa** hàm `init()` placeholder. Đây là lỗi dư thừa dễ tránh khi copy code.
