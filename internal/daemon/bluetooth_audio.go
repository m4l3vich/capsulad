package daemon

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"capsulad/internal/backend"
)

// Bluetooth A2DP sink audio arrives as a PulseAudio source. The framework
// routes it to the master sink with a loopback so a connected source is
// audible; the source's own volume is controlled through AVRCP absolute
// volume, which the volume path forwards.
//
// This is PulseAudio routing, not the framework mixer: the source stream is
// not ducked or re-mixed by the software mixer, matching the vendor design
// where Bluetooth audio bypasses app streams.

// ensurePulseBluetoothModules registers the A2DP/AVRCP endpoints with BlueZ.
// Without them the adapter advertises neither Audio Sink nor Headset and a
// source refuses to connect ("connection-profile-unavailable"). The stock
// vendor btctl supplied them; after retiring it, PulseAudio's standard
// discovery module is the supported owner.
func (d *Daemon) ensurePulseBluetoothModules() {
	if d.dev.Platform() == backend.PlatformSimulator {
		return
	}
	out, err := exec.Command("pactl", "list", "short", "modules").Output()
	if err != nil {
		d.logs.Warnf("bluetooth", "", "pulse modules: %v", err)
		return
	}
	if strings.Contains(string(out), "module-bluez5-discover") || strings.Contains(string(out), "module-bluetooth-discover") {
		return
	}
	loaded, loadErr := exec.Command("pactl", "load-module", "module-bluetooth-discover").Output()
	if loadErr != nil {
		d.logs.Warnf("bluetooth", "", "load pulse bluetooth modules: %v", loadErr)
		return
	}
	d.logs.Infof("bluetooth", "", "loaded PulseAudio Bluetooth discovery (module %s)", strings.TrimSpace(string(loaded)))
}

func (d *Daemon) resetBluetoothRoutes() {
	d.btRouteMu.Lock()
	defer d.btRouteMu.Unlock()
	if d.btRoutes == nil {
		d.btRoutes = map[string]string{}
	}
	for address, module := range d.btRoutes {
		_ = unloadPulseModule(module)
		delete(d.btRoutes, address)
	}
}

func (d *Daemon) ensureBluetoothAudioRoute(address string) {
	if d.dev.Platform() == backend.PlatformSimulator || strings.TrimSpace(address) == "" {
		return
	}
	d.btRouteMu.Lock()
	if d.btRoutes == nil {
		d.btRoutes = map[string]string{}
	}
	if _, ok := d.btRoutes[address]; ok {
		d.btRouteMu.Unlock()
		return
	}
	// Reserve the address so a duplicate connect event does not double-load.
	d.btRoutes[address] = ""
	d.btRouteMu.Unlock()

	source := bluezSourceName(address)
	go func() {
		for i := 0; i < 12; i++ {
			if pulseSourceExists(source) {
				break
			}
			select {
			case <-d.ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
		out, err := exec.Command("pactl", "load-module", "module-loopback",
			"source="+source, "sink=master", "latency_msec=60").Output()
		if err != nil {
			d.logs.Warnf("bluetooth", "", "audio loopback for %s: %v", source, err)
			d.btRouteMu.Lock()
			delete(d.btRoutes, address)
			d.btRouteMu.Unlock()
			return
		}
		module := strings.TrimSpace(string(out))
		d.btRouteMu.Lock()
		if d.btRoutes[address] != "" {
			// A disconnect removed the reservation while the module loaded.
			d.btRouteMu.Unlock()
			_ = unloadPulseModule(module)
			return
		}
		d.btRoutes[address] = module
		d.btRouteMu.Unlock()
		d.logs.Infof("bluetooth", "", "routing source %s to the speaker", source)
	}()
}

func (d *Daemon) removeBluetoothAudioRoute(address string) {
	if d.dev.Platform() == backend.PlatformSimulator {
		return
	}
	d.btRouteMu.Lock()
	module, ok := d.btRoutes[address]
	if ok {
		delete(d.btRoutes, address)
	}
	d.btRouteMu.Unlock()
	if ok && module != "" {
		_ = unloadPulseModule(module)
		d.logs.Infof("bluetooth", "", "removed audio route for %s", address)
	}
}

// ensureConnectedBluetoothRoutes covers a daemon restart while a source is
// already connected: no connect event is emitted for existing state.
func (d *Daemon) ensureConnectedBluetoothRoutes() {
	if d.bt == nil {
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()
	devices, err := d.bt.PairedDevices(ctx)
	if err != nil {
		return
	}
	for _, dev := range devices {
		if dev.Connected {
			d.ensureBluetoothAudioRoute(dev.Address)
		}
	}
}

func bluezSourceName(address string) string {
	return "bluez_source." + strings.ReplaceAll(strings.ToUpper(address), ":", "_") + ".a2dp_source"
}

func pulseSourceExists(source string) bool {
	out, err := exec.Command("pactl", "list", "short", "sources").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == source {
			return true
		}
	}
	return false
}

func unloadPulseModule(index string) error {
	if strings.TrimSpace(index) == "" {
		return nil
	}
	return exec.Command("pactl", "unload-module", strings.TrimSpace(index)).Run()
}
