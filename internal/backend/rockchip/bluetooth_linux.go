//go:build linux

package rockchip

import (
	"fmt"
	"os/exec"
	"strings"

	"capsulad/internal/backend"
)

// hciBluetooth drives the stock adapter through hciconfig, which is present on
// the Buildroot image (bluez-tools/bluetoothctl are not). It is deliberately a
// facade: pairing, agents and profiles stay with the vendor stack, and the
// framework reports what it actually observes.
type hciBluetooth struct {
	adapter string
}

func probeBluetooth(adapter string) backend.Bluetooth {
	if adapter == "" {
		adapter = "hci0"
	}
	if !nodeExists("", "/sys/class/bluetooth/"+adapter) {
		return nil
	}
	return &hciBluetooth{adapter: adapter}
}

func (b *hciBluetooth) Status() backend.BluetoothStatus {
	out, err := exec.Command("hciconfig", b.adapter).CombinedOutput()
	if err != nil {
		return backend.BluetoothStatus{
			Present: true, Adapter: b.adapter,
			Detail: "hciconfig failed: " + strings.TrimSpace(string(out)),
		}
	}
	text := string(out)
	up := strings.Contains(text, "UP RUNNING") || strings.Contains(text, "UP")
	discoverable := strings.Contains(text, "PSCAN") || strings.Contains(text, "ISCAN")
	detail := "adapter reachable via hciconfig; the vendor btcd/btctl stack owns pairing"
	if !up {
		detail = "adapter is down; enable it with the vendor stack or bring hci0 up"
	}
	return backend.BluetoothStatus{
		Present: true, Powered: up, Discoverable: discoverable,
		Adapter: b.adapter, Detail: detail,
	}
}

func (b *hciBluetooth) SetDiscoverable(on bool) error {
	mode := "noscan"
	if on {
		mode = "piscan"
	}
	out, err := exec.Command("hciconfig", b.adapter, mode).CombinedOutput()
	if err != nil {
		return fmt.Errorf("hciconfig %s %s: %v (%s)", b.adapter, mode, err, strings.TrimSpace(string(out)))
	}
	return nil
}
