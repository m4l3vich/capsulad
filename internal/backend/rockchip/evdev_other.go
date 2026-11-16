//go:build !linux

package rockchip

import "capsulad/internal/backend"

// probeEvdev requires Linux evdev; other platforms report no touch backend.
func probeEvdev(Config, keyMap) backend.TouchDevice { return nil }
