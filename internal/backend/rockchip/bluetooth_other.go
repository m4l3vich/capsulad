//go:build !linux

package rockchip

import "capsulad/internal/backend"

// probeBluetooth reports no adapter off-target.
func probeBluetooth(string) backend.Bluetooth { return nil }
