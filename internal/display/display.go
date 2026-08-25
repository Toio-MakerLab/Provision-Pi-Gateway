// Package display drives a small SSD1306 I2C OLED to show Wi-Fi provisioning status.
package display

import (
	"fmt"
	"image"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	"periph.io/x/conn/v3/i2c"
	"periph.io/x/conn/v3/i2c/i2creg"
	"periph.io/x/devices/v3/ssd1306"
	"periph.io/x/devices/v3/ssd1306/image1bit"
	"periph.io/x/host/v3"
)

// Config carries the subset of provisiond configuration needed to open the panel.
// The I2C address is fixed at 0x3C by the underlying ssd1306 driver, which matches
// the address used by virtually all common SSD1306 128x64 modules.
type Config struct {
	Enabled bool
	I2CBus  string
	Width   int
	Height  int
}

// Status mirrors the fields of provisiond's Status struct that are relevant to the
// screen, kept as plain fields so this package has no dependency on package main.
type Status struct {
	APActive        bool
	Connected       bool
	SSID            string
	IPv4            string
	APSSID          string
	ProvisioningURL string
}

// Display renders Status snapshots onto an SSD1306 panel. A nil *Display is valid
// and Render/Close on it are no-ops, so callers never need to nil-check.
type Display struct {
	mu       sync.Mutex
	bus      i2c.BusCloser
	dev      *ssd1306.Dev
	face     font.Face
	lastText string
}

const lineHeight = 13 // basicfont.Face7x13 line pitch in pixels

// New opens the I2C bus and SSD1306 device described by cfg. It returns a nil
// *Display with a nil error when the display is disabled in config, and a non-nil
// error (never a panic) when the hardware can't be reached, so callers can fall back
// to running without a screen.
func New(cfg Config) (*Display, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	if _, err := host.Init(); err != nil {
		return nil, fmt.Errorf("init periph host: %w", err)
	}

	bus, err := i2creg.Open(cfg.I2CBus)
	if err != nil {
		return nil, fmt.Errorf("open i2c bus %q: %w", cfg.I2CBus, err)
	}

	opts := ssd1306.Opts{
		W:       cfg.Width,
		H:       cfg.Height,
		Rotated: false,
	}
	dev, err := ssd1306.NewI2C(bus, &opts)
	if err != nil {
		_ = bus.Close()
		return nil, fmt.Errorf("init ssd1306: %w", err)
	}

	return &Display{bus: bus, dev: dev, face: basicfont.Face7x13}, nil
}

// Render draws a 3-line status screen. It skips the I2C write entirely when the
// content hasn't changed since the last call, to avoid needless bus traffic.
func (d *Display) Render(st Status) error {
	if d == nil {
		return nil
	}

	lines := statusLines(st)
	text := strings.Join(lines, "\n")

	d.mu.Lock()
	defer d.mu.Unlock()

	if text == d.lastText {
		return nil
	}

	img := image1bit.NewVerticalLSB(d.dev.Bounds())
	drawer := font.Drawer{
		Dst:  img,
		Src:  &image.Uniform{C: image1bit.On},
		Face: d.face,
	}
	for i, line := range lines {
		drawer.Dot = fixed.P(0, (i+1)*lineHeight-2)
		drawer.DrawString(line)
	}

	if err := d.dev.Draw(d.dev.Bounds(), img, image.Point{}); err != nil {
		return fmt.Errorf("draw ssd1306: %w", err)
	}
	d.lastText = text
	return nil
}

// Close halts the panel and releases the I2C bus. Safe to call on a nil Display.
func (d *Display) Close() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dev != nil {
		_ = d.dev.Halt()
	}
	if d.bus != nil {
		return d.bus.Close()
	}
	return nil
}

func statusLines(st Status) []string {
	switch {
	case st.Connected:
		return []string{
			"Wi-Fi Connected",
			"SSID: " + truncate(st.SSID, 18),
			"IP: " + orPlaceholder(st.IPv4, "..."),
		}
	case st.APActive:
		return []string{
			"Setup Mode",
			"SSID: " + truncate(st.APSSID, 18),
			truncate(st.ProvisioningURL, 21),
		}
	default:
		return []string{
			"Connecting...",
			"Please wait",
		}
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

func orPlaceholder(s, placeholder string) string {
	if s == "" {
		return placeholder
	}
	return s
}
