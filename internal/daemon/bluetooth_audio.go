package daemon

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"strings"
	"time"

	"capsulad/internal/audio"
	"capsulad/internal/backend"
)

// Bluetooth A2DP sink audio arrives as a PulseAudio source. The framework
// routes it to the master sink with a loopback so a connected source is
// audible; the listening level is the framework master applied locally, and the
// AVRCP absolute volume is a display mirror of that master in both directions.
//
// This is PulseAudio routing, not the framework mixer: the source stream is
// not ducked or re-mixed by the software mixer, matching the vendor design
// where Bluetooth audio bypasses app streams.

// pulseCommand runs pactl for the Bluetooth owner-selection paths. It is a
// variable so tests can exercise registration ordering without a host
// PulseAudio; production always reaches the device's pactl.
var pulseCommand = func(args ...string) ([]byte, error) {
	return exec.Command("pactl", args...).Output()
}

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
	out, loadErr := pulseCommand("load-module", "module-bluetooth-discover")
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
	out, err := pulseCommand("list", "short", "modules")
	if err != nil {
		return false, fmt.Errorf("pulse modules: %w", err)
	}
	return strings.Contains(string(out), "module-bluez5-discover") ||
		strings.Contains(string(out), "module-bluetooth-discover"), nil
}

// pulseStreamsBluetoothSource reports whether PulseAudio is carrying a connected
// source's audio right now: a bluez5 source is RUNNING, or this daemon installed
// a loopback route for a source (which only happens in PulseAudio owner mode). A
// loaded module set with an idle or suspended source is not streaming and may be
// retired in favour of the native sink.
func (d *Daemon) pulseStreamsBluetoothSource() bool {
	d.btRouteMu.Lock()
	routed := len(d.btRoutes) > 0
	d.btRouteMu.Unlock()
	if routed {
		return true
	}
	out, err := pulseCommand("list", "short", "sources")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		// pactl list short sources: index name driver sample-spec state.
		if len(fields) < 2 || !strings.Contains(fields[1], "bluez_source.") {
			continue
		}
		if strings.EqualFold(fields[len(fields)-1], "RUNNING") {
			return true
		}
	}
	return false
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
		// The reservation is the map entry itself (its value is empty until
		// the module is known). A disconnect or resync removes the entry while
		// the module loads; a missing key must unload it, not resurrect a route
		// for a source that is gone.
		if _, reserved := d.btRoutes[address]; !reserved {
			d.btRouteMu.Unlock()
			_ = unloadPulseModule(module)
			return
		}
		d.btRoutes[address] = module
		d.btRouteMu.Unlock()
		d.logs.Infof("bluetooth", "", "routing source %s to the speaker", source)
		d.setBluetoothSourceFixed(address)
		// A new route starts at the framework level rather than at whatever the
		// previous route left behind.
		d.forgetBluetoothGain()
		d.applyBluetoothGain(d.user.Get().Values.MasterVolume)
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

// setBluetoothPlaybackGain applies the canonical framework master taper to the
// Bluetooth loopback as a local safety gain. This fallback bypasses the
// framework mixer, so it must carry the same curve the mixer would have applied;
// it is not a second stage on the native sink. The device module is decoupled
// from AVRCP (avrcp_absolute_volume=0), so the transport volume the daemon
// mirrors to the source changes the source's display and not the stream. The
// local gain is needed because the Mac's AVRCP volume is not a reliable
// attenuation: the slider can move while the stream stays near full scale.
// PulseAudio sink-input volume is cubic, so the cube root yields the requested
// linear gain.
func (d *Daemon) setBluetoothPlaybackGain(level float64) {
	if d.dev.Platform() == backend.PlatformSimulator {
		return
	}
	// The level is a control position, not an amplitude; use the one taper the
	// mixer uses so the two Bluetooth routes sound the same.
	pct := bluetoothSinkInputPercent(level)
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

// bluetoothSinkInputPercent converts a master position to the PulseAudio
// sink-input percentage that produces its canonical gain. PA applies a cubic
// curve to sink-input volume, so the cube root of the taper is the percentage
// whose output equals the framework mixer's gain.
func bluetoothSinkInputPercent(level float64) int {
	return int(math.Round(math.Cbrt(audio.MasterVolumeGain(level)) * 100))
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
	d.btRouteMu.Lock()
	module, ok := d.btRoutes[address]
	if ok {
		delete(d.btRoutes, address)
	}
	d.btRouteMu.Unlock()
	if !ok || module == "" {
		return
	}
	if d.dev.Platform() == backend.PlatformSimulator {
		return
	}
	_ = unloadPulseModule(module)
	d.logs.Infof("bluetooth", "", "removed audio route for %s", address)
}

// restoredBluetoothSource is one source that was already connected when the
// daemon started, plus the completion of its profile request. Session
// verification waits on AudioReady instead of racing the request.
type restoredBluetoothSource struct {
	Address    string
	AudioReady <-chan struct{}
}

// ensureConnectedBluetoothRoutes covers a daemon restart while a source is
// already connected: no connect event is emitted for existing state. It returns
// the sources it restored so the caller can verify their audio link.
func (d *Daemon) ensureConnectedBluetoothRoutes() []restoredBluetoothSource {
	if d.bt == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()
	devices, err := d.bt.PairedDevices(ctx)
	if err != nil {
		return nil
	}
	restored := make([]restoredBluetoothSource, 0, len(devices))
	for _, dev := range devices {
		if dev.Connected {
			// No connect event is emitted for state that already existed, so
			// say it explicitly: otherwise a source that reconnected before
			// the daemon started appears with no "source connected" log while
			// its audio route is being restored.
			d.logs.Infof("bluetooth", "", "source already connected at startup name=%q address=%s; restoring its audio route",
				dev.Name, dev.Address)
			// No connect event follows, so nothing else re-asserts the source's
			// display of the level; the transport event does it again once the
			// stream starts.
			d.mirrorBluetoothVolume(d.user.Get().Values.MasterVolume)
			restored = append(restored, restoredBluetoothSource{
				Address: dev.Address, AudioReady: d.connectBluetoothAudio(dev.Address),
			})
		}
	}
	return restored
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
	_, err := pulseCommand("unload-module", strings.TrimSpace(index))
	return err
}
