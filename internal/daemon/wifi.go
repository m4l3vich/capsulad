package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// wifiAPName returns the SSID the provisioning access point must advertise.
// The requirement is identity with the actual Bluetooth speaker name, so the
// live BlueZ adapter alias is authoritative. The saved host name is only a
// fallback for a device without Bluetooth; an empty answer fails the AP rather
// than inventing a name.
func (d *Daemon) wifiAPName(ctx context.Context) (string, error) {
	statusCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if d.bt != nil {
		if status, err := d.bt.Status(statusCtx); err == nil {
			if name := strings.TrimSpace(status.Name); name != "" {
				return name, nil
			}
		}
	}
	if d.user != nil {
		if name := strings.TrimSpace(d.user.Get().Values.Hostname); name != "" {
			return name, nil
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
