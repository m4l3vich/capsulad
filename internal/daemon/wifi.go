package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// maxWifiSSIDBytes is the Wi-Fi SSID limit. The provisioning AP must always be
// able to start, so a longer Bluetooth name is shortened rather than passed
// through to the Wi-Fi layer's own 1..32-byte rule.
const maxWifiSSIDBytes = 32

// wifiAPSSID derives the provisioning SSID from a speaker name. The Bluetooth
// name may be up to 248 UTF-8 bytes, so the result is the first 32 bytes on a
// rune boundary: never longer than the Wi-Fi limit and never invalid UTF-8.
func wifiAPSSID(name string) string {
	name = strings.TrimSpace(name)
	if len(name) <= maxWifiSSIDBytes {
		return name
	}
	cut := maxWifiSSIDBytes
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	// A trailing space can land on the rune boundary; dropping it keeps the
	// advertised SSID tidy without changing any other byte.
	return strings.TrimSpace(name[:cut])
}

// wifiAPName returns the SSID the provisioning access point must advertise.
// The requirement is identity with the actual Bluetooth speaker name, so the
// live BlueZ adapter alias is authoritative; when it is longer than an SSID it
// is shortened to the first 32 UTF-8 bytes (rune-aligned). The saved host name
// is only a fallback for a device without Bluetooth; an empty answer fails the
// AP rather than inventing a name.
func (d *Daemon) wifiAPName(ctx context.Context) (string, error) {
	statusCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if d.bt != nil {
		if status, err := d.bt.Status(statusCtx); err == nil {
			if name := strings.TrimSpace(status.Name); name != "" {
				return wifiAPSSID(name), nil
			}
		}
	}
	if d.user != nil {
		if name := strings.TrimSpace(d.user.Get().Values.Hostname); name != "" {
			return wifiAPSSID(name), nil
		}
	}
	return "", fmt.Errorf("no Bluetooth advertised name or host name is available")
}

// runWifi performs the bounded startup association attempt in the background.
// The daemon API and UI are already serving, so a slow radio cannot delay the
// speaker; the Wi-Fi service reports connecting, connected or AP fallback.
func (d *Daemon) runWifi() {
	if d.wifiMgr == nil {
		return
	}
	if err := d.wifiMgr.Start(d.ctx); err != nil {
		d.logs.Warnf("wifi", "", "startup: %v", err)
	}
}
