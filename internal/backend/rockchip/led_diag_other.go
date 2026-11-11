//go:build !linux || !(amd64 || arm64)

package rockchip

import (
	"errors"

	"capsulad/internal/backend"
)

// ErrNoLEDDiag reports that the raw LED diagnostics need Linux.
var ErrNoLEDDiag = errors.New("raw LED diagnostics are only implemented for linux/amd64 and linux/arm64")

// VendorMap is empty off-target.
func VendorMap() []int { return nil }

// WriteRawMultiCtrl is unavailable off-target.
func WriteRawMultiCtrl(string, []byte) error { return ErrNoLEDDiag }

// WriteRingPixels is unavailable off-target.
func WriteRingPixels(string, []backend.RGB, float64) error { return ErrNoLEDDiag }
