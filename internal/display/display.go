// Package display drives a small SH1106 I2C OLED to show Wi-Fi provisioning status.
//
// This talks to the panel directly over I2C instead of going through
// periph.io/x/devices/v3/ssd1306: 1.3" 128x64 OLED modules (this project's
// target hardware) are near-universally SH1106 silicon sold under an
// "SSD1306" label. SH1106 has a physically wider 132-column GRAM than the
// 128-column visible glass, and - critically - it doesn't support the
// SSD1306-only memory-addressing-mode commands (0x20/0x21/0x22) that
// periph's driver relies on; real SH1106 hardware just ignores them, leaving
// the controller in page-addressing mode regardless of what periph asked
// for. periph's ssd1306.Opts.W is also hard-capped at 128 and a multiple of
// 8 (see its source), so there was never a way to ask it to address the
// extra 4 physical columns either. Together these are why the panel could
// show text but the columns past the visible 128px never cleared: this
// package fixes it by owning the page-addressing write path directly and
// running one full-GRAM (132-column) clear at startup.
package display

import (
	"fmt"
	"image"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	"periph.io/x/conn/v3/i2c"
	"periph.io/x/conn/v3/i2c/i2creg"
	"periph.io/x/devices/v3/ssd1306"
	"periph.io/x/devices/v3/ssd1306/image1bit"
	"periph.io/x/host/v3"
)

// Driver selects which controller chip Render()'s output is written to. Most
// 1.3" 128x64 "SSD1306" modules are actually SH1106 silicon (see the package
// doc comment) so that's the default; DriverSSD1306 exists for panels that
// really are SSD1306 and work fine through periph's own driver.
type Driver string

const (
	DriverSH1106  Driver = "sh1106"
	DriverSSD1306 Driver = "ssd1306"
)

// Config carries the subset of provisiond configuration needed to open the panel.
type Config struct {
	Enabled bool
	Driver  Driver // defaults to DriverSH1106 when empty
	I2CBus  string
	// Address overrides the I2C device address. Most SH1106 modules use 0x3C;
	// some ship at 0x3D. Leave 0 to use the driver default (0x3C).
	Address uint16
	Width   int
	Height  int
}

// panel is the minimal interface both driver backends implement, letting
// Display.Render/Close stay driver-agnostic.
type panel interface {
	draw(pix []byte) error
	halt() error
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

	// Uptime, MemPercent, and DiskPercent are host stats shown on a trailing
	// status line. Zero-value Uptime means "unavailable" and suppresses the
	// line entirely, since a freshly booted device can't be told apart from
	// a caller that never populated these fields.
	Uptime      time.Duration
	MemPercent  int // 0-100
	DiskPercent int // 0-100, root filesystem
}

// Display renders Status snapshots onto an SH1106 panel. A nil *Display is valid
// and Render/Close on it are no-ops, so callers never need to nil-check.
type Display struct {
	mu       sync.Mutex
	bus      i2c.BusCloser
	dev      panel
	face     font.Face
	width    int // visible panel width in px, used to derive how many chars fit per line
	height   int
	lastText string
}

const (
	lineHeight    = 13  // basicfont.Face7x13 line pitch in pixels
	charAdvance   = 7   // basicfont.Face7x13 glyph advance in pixels
	xOffset       = 2   // left padding in px so text doesn't touch the panel's left edge
	rightPadding  = 0   // right padding in px, reserved so the last glyph on a line doesn't touch the panel's right edge either
	defaultWidth  = 128 // fallback visible width when Config.Width is unset
	defaultHeight = 64  // fallback visible height when Config.Height is unset
	defaultAddr   = 0x3C
)

// New opens the I2C bus and the panel driver described by cfg. It returns a nil
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

	addr := cfg.Address
	if addr == 0 {
		addr = defaultAddr
	}
	width := cfg.Width
	if width <= 0 {
		width = defaultWidth
	}
	height := cfg.Height
	if height <= 0 {
		height = defaultHeight
	}

	driver := cfg.Driver
	if driver == "" {
		driver = DriverSH1106
	}

	var dev panel
	switch driver {
	case DriverSH1106:
		dev, err = newSH1106Panel(bus, addr, width, height)
	case DriverSSD1306:
		dev, err = newSSD1306Panel(bus, addr, width, height)
	default:
		err = fmt.Errorf("unknown display driver %q", driver)
	}
	if err != nil {
		_ = bus.Close()
		return nil, err
	}

	return &Display{bus: bus, dev: dev, face: basicfont.Face7x13, width: width, height: height}, nil
}

// newSH1106Panel opens the default raw page-addressing driver (see package doc).
func newSH1106Panel(bus i2c.BusCloser, addr uint16, width, height int) (panel, error) {
	dev := &sh1106Dev{conn: &i2c.Dev{Bus: bus, Addr: addr}, width: width, height: height}
	if err := dev.init(); err != nil {
		return nil, fmt.Errorf("init sh1106: %w", err)
	}
	if err := dev.clearGRAM(); err != nil {
		return nil, fmt.Errorf("clear sh1106 GRAM: %w", err)
	}
	return dev, nil
}

// newSSD1306Panel opens the panel through periph's own ssd1306 driver, for
// modules that are genuinely SSD1306 silicon rather than SH1106.
func newSSD1306Panel(bus i2c.BusCloser, addr uint16, width, height int) (panel, error) {
	opts := ssd1306.Opts{W: width, H: height, Rotated: false}
	// ssd1306.NewI2C always talks to address 0x3C internally, so a non-default
	// address is applied by intercepting Tx() and substituting the real address.
	var i2cBus i2c.Bus = bus
	if addr != 0 && addr != 0x3C {
		i2cBus = &addrOverrideBus{Bus: bus, addr: addr}
	}
	dev, err := ssd1306.NewI2C(i2cBus, &opts)
	if err != nil {
		return nil, fmt.Errorf("init ssd1306: %w", err)
	}
	p := &ssd1306Panel{dev: dev}
	if err := p.flush(); err != nil {
		return nil, fmt.Errorf("flush ssd1306 GRAM: %w", err)
	}
	return p, nil
}

// ssd1306Panel adapts *ssd1306.Dev to the panel interface.
type ssd1306Panel struct {
	dev *ssd1306.Dev
}

func (p *ssd1306Panel) draw(pix []byte) error {
	img := image1bit.NewVerticalLSB(p.dev.Bounds())
	copy(img.Pix, pix)
	return p.dev.Draw(p.dev.Bounds(), img, image.Point{})
}

func (p *ssd1306Panel) halt() error {
	return p.dev.Halt()
}

// flush forces every physical GRAM byte to be written at least once. dev.Draw
// only sends pixels that differ from its internal shadow buffer, which starts
// all-zero by assumption rather than by reading the panel (I2C displays are
// write-only) - so blank regions our status text never reaches would
// otherwise keep whatever noise was in GRAM at power-on forever, since "off"
// in our frame always matches that all-zero shadow and periph concludes
// nothing changed. Drawing an all-on frame then a blank one guarantees two
// full-bounds diffs, so both passes touch every byte regardless of the
// shadow's starting assumption.
func (p *ssd1306Panel) flush() error {
	bounds := p.dev.Bounds()

	on := image1bit.NewVerticalLSB(bounds)
	for i := range on.Pix {
		on.Pix[i] = 0xFF
	}
	if err := p.dev.Draw(bounds, on, image.Point{}); err != nil {
		return err
	}

	off := image1bit.NewVerticalLSB(bounds)
	return p.dev.Draw(bounds, off, image.Point{})
}

// addrOverrideBus wraps an i2c.Bus and replaces whatever address the caller passes
// to Tx with a fixed one, working around ssd1306.NewI2C's hardcoded 0x3C.
type addrOverrideBus struct {
	i2c.Bus
	addr uint16
}

func (b *addrOverrideBus) Tx(_ uint16, w, r []byte) error {
	return b.Bus.Tx(b.addr, w, r)
}

// maxChars returns how many glyphs fit on one line, after reserving xOffset
// px of left padding and rightPadding px of right padding - deriving this
// from the panel's real width (rather than a hardcoded guess) keeps
// truncate() from letting a line run past the right edge.
func (d *Display) maxChars() int {
	n := (d.width - xOffset - rightPadding) / charAdvance
	if n < 0 {
		return 0
	}
	return n
}

// Render draws a 2-3 line status screen. It skips the I2C write entirely when the
// content hasn't changed since the last call, to avoid needless bus traffic.
func (d *Display) Render(st Status) error {
	if d == nil {
		return nil
	}
	return d.drawLines(d.statusLines(st))
}

// ShowLines draws arbitrary text instead of a Status snapshot, using the same
// centered layout and change-skipping as Render. It exists for transient
// screens that have no Status to derive from - e.g. the hardware reset
// button's hold countdown and "Resetting Wi-Fi..." confirmation.
func (d *Display) ShowLines(lines ...string) error {
	if d == nil {
		return nil
	}
	max := d.maxChars()
	truncated := make([]string, len(lines))
	for i, line := range lines {
		truncated[i] = truncate(line, max)
	}
	return d.drawLines(truncated)
}

// drawLines renders lines onto the panel. It skips the I2C write entirely
// when the content hasn't changed since the last call, to avoid needless bus
// traffic.
func (d *Display) drawLines(lines []string) error {
	text := strings.Join(lines, "\n")

	d.mu.Lock()
	defer d.mu.Unlock()

	if text == d.lastText {
		return nil
	}

	img := image1bit.NewVerticalLSB(image.Rect(0, 0, d.width, d.height))
	drawer := font.Drawer{
		Dst:  img,
		Src:  &image.Uniform{C: image1bit.On},
		Face: d.face,
	}
	// Center the block vertically instead of always pinning it to the top -
	// statusLines() returns 2-3 lines, well under what a 64px panel fits at
	// lineHeight=13 (~4-5 lines), so pinning to the top left a large dead
	// band at the bottom on every screen.
	topMargin := (d.height - len(lines)*lineHeight) / 2
	if topMargin < 0 {
		topMargin = 0
	}
	// Center each line horizontally by its actual rendered width instead of
	// pinning every line to xOffset - Face7x13 is monospace (charAdvance px
	// per rune), so a short line like "Setup Mode" can be measured exactly
	// without calling into the font. Never goes below xOffset, so the
	// longest (maxChars-length) line still keeps its left margin.
	panelWidth := d.width
	for i, line := range lines {
		lineWidth := len([]rune(line)) * charAdvance
		x := (panelWidth - lineWidth) / 2
		if x < xOffset {
			x = xOffset
		}
		drawer.Dot = fixed.P(x, topMargin+(i+1)*lineHeight-2)
		drawer.DrawString(line)
	}

	if err := d.dev.draw(img.Pix); err != nil {
		return fmt.Errorf("draw sh1106: %w", err)
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
		_ = d.dev.halt()
	}
	if d.bus != nil {
		return d.bus.Close()
	}
	return nil
}

// statusLines builds the panel's text lines, truncating each full line
// (prefix included) to what maxChars() says actually fits - truncating just
// the value and appending it to a prefix, as this used to do, undercounts
// the line's real width and lets it run past the panel edge.
func (d *Display) statusLines(st Status) []string {
	max := d.maxChars()
	var lines []string
	switch {
	case st.Connected:
		lines = []string{
			truncate("Wi-Fi Connected", max),
			truncate("SSID: "+st.SSID, max),
			truncate("IP: "+orPlaceholder(st.IPv4, "..."), max),
		}
	case st.APActive:
		lines = []string{
			truncate("Setup Mode", max),
			truncate("SSID: "+st.APSSID, max),
			truncate(st.ProvisioningURL, max),
		}
	default:
		second := "Please wait"
		if st.SSID != "" {
			second = "SSID: " + st.SSID
		}
		lines = []string{
			truncate("Connecting...", max),
			truncate(second, max),
		}
	}
	if st.Uptime > 0 {
		sysLine := fmt.Sprintf("Up %s M%d%% D%d%%", formatUptime(st.Uptime), st.MemPercent, st.DiskPercent)
		lines = append(lines, truncate(sysLine, max))
	}
	return lines
}

// formatUptime renders d as a compact "<unit><unit>" pair (e.g. "2d3h",
// "3h12m", "12m") - kept short since it has to share a line with the memory
// and disk percentages within maxChars().
func formatUptime(d time.Duration) string {
	totalSeconds := int64(d.Seconds())
	days := totalSeconds / 86400
	hours := (totalSeconds % 86400) / 3600
	minutes := (totalSeconds % 3600) / 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// truncate clips s to at most max *runes*, not bytes - slicing by byte index
// (s[:n]) can cut a multi-byte UTF-8 sequence in half, leaving stray bytes
// that render as extra/garbled glyphs past where the line should end. SSIDs
// and provisioning text can contain non-ASCII characters, so this has to be
// rune-aware even though basicfont.Face7x13 itself is ASCII-only.
//
// No cut marker is appended - a hard clip. Any marker character risks the
// same problem that ruled out "…" (U+2026 isn't in Face7x13's supported
// ranges, and font.Drawer.DrawString ignores Face.Glyph's ok return, so an
// unsupported rune silently draws the replacement-char box glyph instead of
// being skipped), and on a display this small the marker itself reads as
// stray content at the exact spot viewers report as "extra" pixels.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

func orPlaceholder(s, placeholder string) string {
	if s == "" {
		return placeholder
	}
	return s
}
