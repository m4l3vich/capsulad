//go:build !linux

package linuxdev

import "capsulad/internal/backend"

// ProbeEvdev requires Linux evdev; other platforms report no input device.
func ProbeEvdev(string, string, string, KeyMap) backend.TouchDevice { return nil }
