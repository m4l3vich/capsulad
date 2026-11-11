//go:build !linux

package rockchip

import "capsulad/internal/backend"

// probeEvdev requires Linux evdev; other platforms report no touch backend.
func probeEvdev(string, keyMap) backend.TouchDevice { return nil }
