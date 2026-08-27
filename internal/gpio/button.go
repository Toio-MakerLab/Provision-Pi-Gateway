// Package gpio watches a physical push button wired to a Raspberry Pi GPIO
// pin and invokes a callback once it has been held down continuously for a
// configured duration - this drives the hardware Wi-Fi reset button, which
// lets someone reset provisioning without going through the HTTP API (or a
// working network) at all.
package gpio

import (
	"context"
	"fmt"
	"time"

	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/host/v3"
)

// pollInterval is how often the pin level is sampled while watching for a
// hold. HoldTime is measured in seconds, so this doesn't need to be any
// finer than that to stay accurate.
const pollInterval = 50 * time.Millisecond

// Config describes which pin to watch and how long it must be held.
type Config struct {
	Enabled bool
	// Pin is the periph pin name or number, e.g. "17" or "GPIO17" for BCM
	// GPIO17.
	Pin string
	// HoldTime is how long the button must be held continuously before the
	// callback passed to Watch fires.
	HoldTime time.Duration
}

// Button watches a single GPIO input pin wired to a normally-open push
// button between the pin and ground. The pin is configured with an internal
// pull-up, so idle reads High and a press pulls it Low.
type Button struct {
	pin      gpio.PinIO
	holdTime time.Duration
}

// New opens the configured pin as a pulled-up input. It returns a nil
// *Button with a nil error when disabled, so callers never need to
// nil-check before calling Watch - mirroring how internal/display.New
// handles being disabled.
func New(cfg Config) (*Button, error) {
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
	if err := p.In(gpio.PullUp, gpio.NoEdge); err != nil {
		return nil, fmt.Errorf("configure gpio pin %q as input: %w", cfg.Pin, err)
	}

	return &Button{pin: p, holdTime: cfg.HoldTime}, nil
}

// Callbacks bundles the hooks Watch invokes as a press progresses. Any field
// may be left nil.
type Callbacks struct {
	// OnPress is called on every poll while the button is held down, with
	// the duration held so far (starting at 0) - e.g. to drive a countdown
	// display while the hold is still in progress.
	OnPress func(held time.Duration)
	// OnRelease is called once when the button is released, whether or not
	// OnHold fired first - e.g. to dismiss a countdown shown by OnPress.
	OnRelease func()
	// OnHold is called once per press, the moment held time reaches
	// HoldTime.
	OnHold func()
}

// Watch blocks polling the pin and drives cb as described on Callbacks. The
// button must be released before OnHold can fire again, so one long press
// triggers it exactly once instead of repeatedly for as long as it's held.
// Safe to call on a nil *Button, in which case it returns immediately.
func (b *Button) Watch(ctx context.Context, cb Callbacks) {
	if b == nil {
		return
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var pressedSince time.Time
	fired := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pressed := b.pin.Read() == gpio.Low
			switch {
			case !pressed:
				if !pressedSince.IsZero() {
					pressedSince = time.Time{}
					fired = false
					if cb.OnRelease != nil {
						cb.OnRelease()
					}
				}
			case pressedSince.IsZero():
				pressedSince = time.Now()
				if cb.OnPress != nil {
					cb.OnPress(0)
				}
			default:
				held := time.Since(pressedSince)
				if cb.OnPress != nil {
					cb.OnPress(held)
				}
				if !fired && held >= b.holdTime {
					fired = true
					if cb.OnHold != nil {
						cb.OnHold()
					}
				}
			}
		}
	}
}
