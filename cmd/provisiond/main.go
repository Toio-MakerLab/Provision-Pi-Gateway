package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"example.com/rpi-provisioning/internal/display"
)

type Config struct {
	Interface      string
	APProfile      string
	APSSID         string
	APPassword     string
	APAddress      string
	HTTPAddress    string
	WiFiProfile    string
	ConnectTimeout time.Duration
	RetryTimeout   time.Duration
	ResetFlag      string
	AllowReboot    bool
	AdminToken     string

	OLEDEnabled bool
	OLEDI2CBus  string
	OLEDWidth   int
	OLEDHeight  int
	OLEDRefresh time.Duration
}

type Service struct {
	cfg     Config
	logger  *slog.Logger
	disp    *display.Display
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

func getenvInt(key string, fallback int) int {
	v := getenv(key, "")
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func configFromEnv() Config {
	return Config{
		Interface:      getenv("WIFI_INTERFACE", "wlan0"),
		APProfile:      getenv("AP_PROFILE", "device-provision-ap"),
		APSSID:         getenv("AP_SSID", "MyDevice-Setup"),
		APPassword:     getenv("AP_PASSWORD", "ChangeMe-123"),
		APAddress:      getenv("AP_ADDRESS", "192.168.12.1"),
		HTTPAddress:    getenv("HTTP_ADDRESS", ":8080"),
		WiFiProfile:    getenv("WIFI_PROFILE", "device-wifi"),
		ConnectTimeout: getenvDuration("CONNECT_TIMEOUT", 35*time.Second),
		RetryTimeout:   getenvDuration("RETRY_TIMEOUT", 25*time.Second),
		ResetFlag:      getenv("RESET_FLAG", "/boot/firmware/RESET_WIFI"),
		AllowReboot:    getenvBool("ALLOW_REBOOT", false),
		AdminToken:     getenv("ADMIN_TOKEN", ""),

		OLEDEnabled: getenvBool("OLED_ENABLED", true),
		OLEDI2CBus:  getenv("OLED_I2C_BUS", ""),
		OLEDWidth:   getenvInt("OLED_WIDTH", 128),
		OLEDHeight:  getenvInt("OLED_HEIGHT", 64),
		OLEDRefresh: getenvDuration("OLED_REFRESH", 5*time.Second),
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

	disp, err := display.New(display.Config{
		Enabled: cfg.OLEDEnabled,
		I2CBus:  cfg.OLEDI2CBus,
		Width:   cfg.OLEDWidth,
		Height:  cfg.OLEDHeight,
	})
	if err != nil {
		logger.Warn("OLED display unavailable; continuing without it", "error", err)
	} else {
		s.disp = disp
	}
	defer func() {
		if err := s.disp.Close(); err != nil {
			logger.Warn("closing OLED display", "error", err)
		}
	}()

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

	// HTTP_ADDRESS binds to a specific interface IP (e.g. the AP address), which
	// NetworkManager can take a moment to actually assign after "connection up"
	// reports active. Retry the bind instead of failing on the first race.
	listener, err := waitForListener(ctx, cfg.HTTPAddress, 20*time.Second)
	if err != nil {
		logger.Error("cannot bind HTTP address", "address", cfg.HTTPAddress, "error", err)
		os.Exit(1)
	}

	go func() {
		logger.Info("HTTP provisioning server started", "address", cfg.HTTPAddress)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped", "error", err)
			cancel()
		}
	}()

	go s.runDisplay(ctx)

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
}

// runDisplay periodically mirrors s.status() onto the OLED panel, if any. It reuses
// the exact same status computation as the HTTP /api/status endpoint so the screen
// never disagrees with the portal.
func (s *Service) runDisplay(ctx context.Context) {
	if s.disp == nil {
		return
	}
	ticker := time.NewTicker(s.cfg.OLEDRefresh)
	defer ticker.Stop()

	render := func() {
		st, err := s.status(ctx)
		if err != nil {
			return
		}
		dst := display.Status{
			APActive:        st.APActive,
			Connected:       st.Connected,
			SSID:            st.SSID,
			IPv4:            st.IPv4,
			APSSID:          s.cfg.APSSID,
			ProvisioningURL: st.ProvisioningURL,
		}
		if err := s.disp.Render(dst); err != nil {
			s.logger.Warn("render OLED display", "error", err)
		}
	}

	render()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			render()
		}
	}
}

func waitForListener(ctx context.Context, addr string, timeout time.Duration) (net.Listener, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if l, err := net.Listen("tcp", addr); err == nil {
			return l, nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
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
	mux.HandleFunc("GET /api/wifi/scan", s.requireTokenUnlessAP(s.handleScan))
	mux.HandleFunc("POST /api/wifi/connect", s.requireTokenUnlessAP(s.handleConnect))
	mux.HandleFunc("POST /api/ap/enable", s.requireToken(s.handleEnableAP))
	mux.HandleFunc("DELETE /api/wifi/profile", s.requireToken(s.handleDeleteProfile))
	mux.HandleFunc("POST /api/reset-network", s.requireToken(s.handleResetNetwork))
	mux.HandleFunc("POST /api/reboot", s.requireToken(s.handleReboot))
	return securityHeaders(s.limitBody(mux))
}

// isFromAP reports whether the request was accepted on the setup AP's own address.
// HTTP_ADDRESS binds every interface (":8080"), so without this check a phone
// setting up Wi-Fi for the first time via the AP and any device already on the
// same home LAN would both hit the exact same unauthenticated endpoints.
func (s *Service) isFromAP(r *http.Request) bool {
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	host, _, err := net.SplitHostPort(local.String())
	if err != nil {
		host = local.String()
	}
	return host == s.cfg.APAddress
}

// requireTokenUnlessAP allows self-service Wi-Fi setup with no token for clients
// connected directly to the setup AP (knowing the AP SSID/password already proves
// physical proximity), but requires the admin token for the same call when reached
// over the real Wi-Fi network — otherwise anyone else on that network could silently
// repoint the device's Wi-Fi or read scan results.
func (s *Service) requireTokenUnlessAP(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.isFromAP(r) {
			next(w, r)
			return
		}
		s.requireToken(next)(w, r)
	}
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

func (s *Service) handleEnableAP(w http.ResponseWriter, r *http.Request) {
	if err := s.enableAP(r.Context()); err != nil {
		s.logger.Error("manual AP enable failed", "error", err)
		writeError(w, http.StatusInternalServerError, "cannot start setup AP")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Setup AP is active."})
}

func (s *Service) handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	if err := s.forgetWiFiProfile(r.Context()); err != nil {
		s.logger.Error("delete Wi-Fi profile failed", "error", err)
		writeError(w, http.StatusInternalServerError, "cannot delete Wi-Fi profile")
		return
	}
	if err := s.enableAP(r.Context()); err != nil {
		s.logger.Error("enable AP after profile delete failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Wi-Fi profile removed but AP could not start")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Wi-Fi profile removed; setup AP is active."})
}

func (s *Service) handleResetNetwork(w http.ResponseWriter, r *http.Request) {
	if err := s.forgetWiFi(r.Context()); err != nil {
		s.logger.Error("network reset failed", "error", err)
		writeError(w, http.StatusInternalServerError, "cannot reset Wi-Fi")
		return
	}
	if err := s.enableAP(r.Context()); err != nil {
		s.logger.Error("enable AP after network reset failed", "error", err)
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

	// Retries because ap-up frequently races a just-finished disconnect (e.g. a
	// deleted or failed STA profile): nmcli reports the device as free before
	// wlan0 has actually settled, and "connection up" fails transiently.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		lastErr = run(ctx, "sudo", "/usr/local/lib/rpi-provision/rpi-provision", "ap-up", s.cfg.Interface, s.cfg.APProfile, s.cfg.APSSID, s.cfg.APPassword, s.cfg.APAddress)
		if lastErr == nil {
			s.apOn = true
			return nil
		}
	}
	return lastErr
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

func (s *Service) forgetWiFiProfile(ctx context.Context) error {
	return run(ctx, "sudo", "/usr/local/lib/rpi-provision/rpi-provision", "delete-wifi-profile", s.cfg.WiFiProfile)
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
		// indexHTML embeds its <script> and <style> inline, so both need 'unsafe-inline';
		// without script-src explicitly listed here it would inherit default-src, which
		// silently blocks the inline script and leaves the page stuck on its static
		// "Loading status…" placeholder with no console-visible network error.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'")
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
<body><main class="card"><h1>Connect this device to Wi‑Fi</h1><p class="muted">Choose your network and enter its password. The setup Wi‑Fi will disconnect after success.</p><div id="status">Loading status…</div><form id="form"><label for="ssid">Wi‑Fi network</label><select id="ssid" required><option value="">Scan is running…</option></select><label for="password">Password</label><input id="password" type="password" maxlength="63" autocomplete="current-password" placeholder="Leave empty for open networks"><button id="submit" type="submit">Connect</button></form><button id="rescan" type="button">Scan again</button>
<details id="admin"><summary>Admin</summary><label for="token">Admin token</label><input id="token" type="password" autocomplete="off" placeholder="Required unless connected to the setup Wi‑Fi"><button id="openap" type="button">Open setup AP</button><button id="forget" type="button">Forget saved Wi‑Fi</button></details>
</main>
<script>
const $=id=>document.getElementById(id), status=$('status');
function msg(text,kind=''){status.textContent=text;status.className=kind}
function token(){return sessionStorage.getItem('adminToken')||''}
async function api(path,opts={}){opts.headers=Object.assign({},opts.headers,token()?{'X-Admin-Token':token()}:{});const r=await fetch(path,opts);let b;try{b=await r.json()}catch{b={error:'Invalid server response'}}if(!r.ok)throw new Error(b.error||b.message||'Request failed');return b}
async function loadStatus(){try{const s=await api('/api/status');msg(s.connected?('Connected: '+(s.ssid||'Wi‑Fi')+' '+(s.ipv4||'')):('Setup AP active. Open '+(s.provisioning_url||location.origin)),'ok')}catch(e){msg(e.message,'error')}}
async function scan(){const select=$('ssid');select.innerHTML='<option>Scanning…</option>';try{const ns=await api('/api/wifi/scan');select.innerHTML='<option value="">Select a Wi‑Fi network</option>';for(const n of ns){const o=document.createElement('option');o.value=n.ssid;o.textContent=n.ssid+' — '+n.signal+'% '+(n.security?('('+n.security+')'):'');select.appendChild(o)}if(ns.length===0)msg('No visible Wi‑Fi networks. Move closer to the router and scan again.','error')}catch(e){select.innerHTML='<option value="">Scan failed</option>';msg(e.message,'error')}}
$('form').addEventListener('submit',async e=>{e.preventDefault();const b=$('submit');b.disabled=true;msg('Connecting… this can take up to 35 seconds.');try{const r=await api('/api/wifi/connect',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({ssid:$('ssid').value,password:$('password').value})});msg(r.message,'ok')}catch(e){msg(e.message,'error')}finally{b.disabled=false}});
$('token').value=token();
$('token').addEventListener('change',()=>sessionStorage.setItem('adminToken',$('token').value));
$('openap').addEventListener('click',async()=>{try{const r=await api('/api/ap/enable',{method:'POST'});msg(r.message,'ok')}catch(e){msg(e.message,'error')}});
$('forget').addEventListener('click',async()=>{if(!confirm('Forget the saved Wi‑Fi network and open the setup AP?'))return;try{const r=await api('/api/wifi/profile',{method:'DELETE'});msg(r.message,'ok')}catch(e){msg(e.message,'error')}});
$('rescan').onclick=scan;loadStatus();scan();
</script></body></html>`
