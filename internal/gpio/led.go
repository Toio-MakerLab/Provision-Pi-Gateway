package gpio

import (
	"context"
	"fmt"
	"time"

	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/host/v3"
)

// LEDConfig describes which pin drives a status LED.
type LEDConfig struct {
	Enabled bool
	// Pin is the periph pin name or number, e.g. "22" or "GPIO22" for BCM
	// GPIO22.
	Pin string
}

// LED drives a single GPIO output pin wired to an LED (through a current
// limiting resistor) between the pin and ground: driving the pin High lights
// it, Low turns it off.
type LED struct {
	pin gpio.PinIO
}

// NewLED opens the configured pin as an output, initially off. It returns a
// nil *LED with a nil error when disabled, so callers never need to
// nil-check before calling its methods - mirroring how New handles being
// disabled.
func NewLED(cfg LEDConfig) (*LED, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	if _, err := host.Init(); err != nil {
		return nil, fmt.Errorf("init periph host: %w", err)
	}

	p := gpioreg.ByName(cfg.Pin)
	if p == nil {
		return nil, fmt.Errorf("gpio pin %q not found", cfg.Pin)
	}
	if err := p.Out(gpio.Low); err != nil {
		return nil, fmt.Errorf("configure gpio pin %q as output: %w", cfg.Pin, err)
	}

	return &LED{pin: p}, nil
}

// Set turns the LED on or off. Safe to call on a nil *LED, in which case it
// is a no-op - callers don't need to check whether the LED is configured
// before every call.
func (l *LED) Set(on bool) error {
	if l == nil {
		return nil
	}
	level := gpio.Low
	if on {
		level = gpio.High
	}
	return l.pin.Out(level)
}

// Blink toggles the LED on and off every interval until ctx is cancelled,
// leaving it off on return - e.g. to signal an in-progress action (like a
// shutdown countdown) as distinct from LED.Set's steady states. Safe to call
// on a nil *LED.
func (l *LED) Blink(ctx context.Context, interval time.Duration) {
	if l == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	on := false
	for {
		select {
		case <-ctx.Done():
			_ = l.Set(false)
			return
		case <-ticker.C:
			on = !on
			_ = l.Set(on)
		}
	}
}
