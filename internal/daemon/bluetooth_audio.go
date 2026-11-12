package daemon

import (
	"context"
	"fmt"
	"math"
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

// ensurePulseBluetoothModules registers the A2DP/AVRCP endpoints with BlueZ
// through PulseAudio. Without them the adapter advertises neither Audio Sink
// nor Headset and a source refuses to connect ("connection-profile-unavailable").
// The stock vendor btctl supplied them; after retiring it, PulseAudio's
// standard discovery module is the supported fallback owner.
func (d *Daemon) ensurePulseBluetoothModules() error {
	if d.dev.Platform() == backend.PlatformSimulator {
		return nil
	}
	loaded, err := pulseBluetoothModulesLoadedE()
	if err != nil {
		return err
	}
	if loaded {
		return nil
	}
	out, loadErr := exec.Command("pactl", "load-module", "module-bluetooth-discover").Output()
	if loadErr != nil {
		return fmt.Errorf("load pulse bluetooth modules: %w", loadErr)
	}
	d.logs.Infof("bluetooth", "", "loaded PulseAudio Bluetooth discovery (module %s)", strings.TrimSpace(string(out)))
	return nil
}

// pulseBluetoothModulesLoaded reports whether PulseAudio owns the Bluetooth
// audio profiles right now.
func pulseBluetoothModulesLoaded() bool {
	loaded, err := pulseBluetoothModulesLoadedE()
	return err == nil && loaded
}

func pulseBluetoothModulesLoadedE() (bool, error) {
	out, err := exec.Command("pactl", "list", "short", "modules").Output()
	if err != nil {
		return false, fmt.Errorf("pulse modules: %w", err)
	}
	return strings.Contains(string(out), "module-bluez5-discover") ||
		strings.Contains(string(out), "module-bluetooth-discover"), nil
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
		// A 60 ms loopback target was too tight on the 1 GHz A35: the source
		// buffer swung by hundreds of milliseconds and the adaptive resampler
		// wobbled audibly. 250 ms absorbs scheduling jitter and keeps the
		// compensated rate near 44.1 kHz.
		out, err := exec.Command("pactl", "load-module", "module-loopback",
			"source="+source, "sink=master",
			"latency_msec=250", "source_dont_move=true", "sink_dont_move=true").Output()
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
		d.setBluetoothSourceFixed(address)
		d.setBluetoothPlaybackGain(d.user.Get().Values.MasterVolume)
		// The transport exists once the source does; re-apply the framework
		// level because A2DP setup can reset absolute volume to the source's
		// cached value.
		d.forceBluetoothVolume()
	}()
}

// ensureDecoupledBluetoothModule reloads PulseAudio's bluez5 device module
// with avrcp_absolute_volume=0. With the default (1) PA mirrors the AVRCP
// transport volume onto the source and applies it again with a cubic curve, so
// the same percentage attenuates twice and low settings sound thin. The module
// is recreated with defaults on each new connection, so this is checked on
// every source connect.
func (d *Daemon) ensureDecoupledBluetoothModule(address string) bool {
	if d.dev.Platform() == backend.PlatformSimulator || strings.TrimSpace(address) == "" {
		return false
	}
	pathAddr := "dev_" + strings.ReplaceAll(strings.ToUpper(address), ":", "_")
	for attempt := 0; attempt < 8; attempt++ {
		out, err := exec.Command("pactl", "list", "short", "modules").Output()
		if err != nil {
			d.logs.Warnf("bluetooth", "", "pulse modules: %v", err)
			return false
		}
		var index, modulePath, args string
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 || fields[1] != "module-bluez5-device" {
				continue
			}
			rest := strings.Join(fields[2:], " ")
			if !strings.Contains(rest, pathAddr) {
				continue
			}
			index, args = fields[0], rest
			for _, field := range fields[2:] {
				if strings.HasPrefix(field, "path=") {
					modulePath = strings.TrimPrefix(field, "path=")
				}
			}
			break
		}
		if index == "" {
			// PulseAudio has not created the device module yet.
			select {
			case <-d.ctx.Done():
				return false
			case <-time.After(300 * time.Millisecond):
			}
			continue
		}
		if strings.Contains(args, "avrcp_absolute_volume=0") {
			return false
		}
		if modulePath == "" {
			modulePath = "/org/bluez/hci0/" + pathAddr
		}
		_ = exec.Command("pactl", "unload-module", index).Run()
		select {
		case <-d.ctx.Done():
			return false
		case <-time.After(150 * time.Millisecond):
		}
		loaded, loadErr := exec.Command("pactl", "load-module", "module-bluez5-device",
			"path="+modulePath, "avrcp_absolute_volume=0").CombinedOutput()
		if loadErr != nil {
			d.logs.Warnf("bluetooth", "", "decouple PA device module: %v (%s)", loadErr, strings.TrimSpace(string(loaded)))
			return false
		}
		d.logs.Infof("bluetooth", "", "decoupled AVRCP volume for %s (module %s)", pathAddr, strings.TrimSpace(string(loaded)))
		source := bluezSourceName(address)
		for i := 0; i < 20; i++ {
			if pulseSourceExists(source) {
				return true
			}
			select {
			case <-d.ctx.Done():
				return true
			case <-time.After(150 * time.Millisecond):
			}
		}
		return true
	}
	return false
}

// setBluetoothSourceFixed holds the local bluez source at a fixed 90 %. The
// source is decoupled from AVRCP, so this is a stable capture gain rather than
// a mirrored remote volume; 90 % avoids unity while keeping full-band audio.
func (d *Daemon) setBluetoothSourceFixed(address string) {
	if d.dev.Platform() == backend.PlatformSimulator || strings.TrimSpace(address) == "" {
		return
	}
	source := bluezSourceName(address)
	_ = exec.Command("pactl", "set-source-volume", source, "90%").Run()
}

// setBluetoothPlaybackGain applies the framework master to the Bluetooth
// loopback as a local safety gain. It is needed because the Mac's AVRCP
// volume is not a reliable attenuation: the slider can move while the stream
// stays near full scale. PulseAudio sink-input volume is cubic, so the cube
// root yields the requested linear gain. The Bluetooth listening level is
// therefore bounded by the master, and the Mac/AVRCP value is the synced
// display and, when honored, an additional attenuation.
func (d *Daemon) setBluetoothPlaybackGain(level float64) {
	if d.dev.Platform() == backend.PlatformSimulator {
		return
	}
	if level < 0 {
		level = 0
	}
	if level > 1 {
		level = 1
	}
	// Perceptual/safety curve: a linear master is still too hot at the low
	// end on this amplifier, and the Mac may not attenuate AVRCP at all. The
	// 1.5 exponent keeps the low end quiet while 40 % stays usable.
	effective := math.Pow(level, 1.5)
	pct := int(math.Round(math.Cbrt(effective) * 100))
	if level <= 0.01 {
		pct = 0
	}
	go func() {
		for attempt := 0; attempt < 6; attempt++ {
			if index := loopbackSinkInputIndex(); index != "" {
				_ = exec.Command("pactl", "set-sink-input-volume", index, fmt.Sprintf("%d%%", pct)).Run()
				return
			}
			select {
			case <-d.ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
}

func loopbackSinkInputIndex() string {
	out, err := exec.Command("pactl", "list", "short", "sink-inputs").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[3] == "module-loopback.c" {
			return fields[0]
		}
	}
	return ""
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
			d.connectBluetoothAudio(dev.Address)
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
