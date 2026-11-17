//go:build !linux || (!amd64 && !arm64)

package linuxdev

import (
	"errors"

	"capsulad/internal/backend"
)

// ErrNoVendorLED reports that vendor LED char devices are only usable on the
// supported Linux targets.
var ErrNoVendorLED = errors.New("vendor LED char devices are only implemented for linux/amd64 and linux/arm64")

// WS2812Device is an inert placeholder so the package compiles off-target.
type WS2812Device struct{ channels int }

// OpenWS2812Device is unavailable off-target.
func OpenWS2812Device(string) (*WS2812Device, error) { return nil, ErrNoVendorLED }

// Channels reports zero off-target.
func (d *WS2812Device) Channels() int { return 0 }

// Write is unavailable off-target.
func (d *WS2812Device) Write([]byte) error { return ErrNoVendorLED }

// Close is a no-op off-target.
func (d *WS2812Device) Close() error { return nil }

// WS2812Ring is an inert placeholder off-target.
type WS2812Ring struct{}

// OpenWS2812Ring is unavailable off-target.
func OpenWS2812Ring(string, int, []int, float64) (*WS2812Ring, error) {
	return nil, ErrNoVendorLED
}

func (r *WS2812Ring) Caps() backend.RingCaps            { return backend.RingCaps{} }
func (r *WS2812Ring) ResourceID() string                { return "light.ring.main" }
func (r *WS2812Ring) WriteRing(backend.RingFrame) error { return ErrNoVendorLED }
func (r *WS2812Ring) Close() error                      { return nil }

// WriteWS2812Frame is unavailable off-target.
func WriteWS2812Frame(string, []backend.RGB, float64, []int, float64) error {
	return ErrNoVendorLED
}

var _ backend.RingDevice = (*WS2812Ring)(nil)
