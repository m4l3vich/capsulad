//go:build linux && (amd64 || arm64)

package rockchip

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"

	"capsulad/internal/backend"
)

// multiCtrlVendorMap is the permutation the stock voice-agent applies between
// its JSON frame order and the driver's 30 records. The mapping is recovered
// from the binary and its exact orientation is being calibrated against the
// physical display, which is why the diagnostic path exposes it explicitly.
var multiCtrlVendorMap = []int{
	1, 28, 21, 17, 22, 29, 24, 26, 25, 19, 18, 23, 27, 20, 16,
	15, 0, 2, 7, 5, 6, 4, 3, 10, 12, 11, 14, 13, 8, 9,
}

// VendorMap returns the recovered JSON-order permutation.
func VendorMap() []int {
	out := make([]int, len(multiCtrlVendorMap))
	copy(out, multiCtrlVendorMap)
	return out
}

// WriteRingPixels writes a WS2812B frame through the framework's encoder, so a
// technician sees exactly what a preset would put on the ring (channel
// permutation included). Brightness is the preset-scale multiplier.
func WriteRingPixels(path string, pixels []backend.RGB, brightness float64) error {
	if path == "" {
		path = "/dev/topleds"
	}
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	count, err := unix.IoctlGetInt(int(f.Fd()), ledsIoctlGetCount)
	if err != nil {
		return fmt.Errorf("read WS2812B channel count: %w", err)
	}
	buffer := encodeWS2812Frame(pixels, brightness, count)
	request := ws2812SetRequest{Count: uint32(len(buffer)), Data: uintptr(unsafe.Pointer(&buffer[0]))}
	if err := ioctlPointer(f, ledsWS2812BSet, unsafe.Pointer(&request)); err != nil {
		return fmt.Errorf("set ring frame: %w", err)
	}
	runtime.KeepAlive(buffer)
	return nil
}

// WriteRawMultiCtrl writes arbitrary per-channel brightness to the vendor
// IS31FL3236 node. It bypasses the framework frame mapping so a technician can
// identify the physical channel order during bring-up.
func WriteRawMultiCtrl(path string, values []byte) error {
	if path == "" {
		path = "/dev/led_multi_ctrl"
	}
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	count, err := unix.IoctlGetInt(int(f.Fd()), ledsIoctlGetCount)
	if err != nil {
		return fmt.Errorf("read channel count: %w", err)
	}
	records := make([]multiCtrlRecord, count)
	for i := range records {
		records[i].Enable = 1
		if i < len(values) {
			records[i].Brightness = uint32(values[i])
		}
	}
	if err := ioctlPointer(f, ledsMultiCtrlSet, unsafe.Pointer(&records[0])); err != nil {
		return fmt.Errorf("set raw frame: %w", err)
	}
	runtime.KeepAlive(records)
	return nil
}
