//go:build !linux || (!amd64 && !arm64)

package rockchip

import "capsulad/internal/backend"

// Vendor LED char devices are only usable on Linux. The device tree probe and
// frame encoders still build everywhere so offline inspection and tests work.

func openWS2812BRing(Config, *ws2812RingNode) (backend.RingDevice, error) {
	return nil, errLEDOffline
}

func openMultiCtrlSegment(Config, *multiCtrlNode) (backend.SegmentDevice, error) {
	return nil, errLEDOffline
}
