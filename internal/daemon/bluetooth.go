package daemon

import (
	"context"
	"math"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"capsulad/internal/bluetooth"
	"capsulad/internal/grpcpeer"
	"capsulad/internal/presentations"
	"capsulad/internal/presets"
	"capsulad/internal/routing"
	"capsulad/internal/settings"
)

// applyBluetoothSettings powers the adapter and applies the persisted pairable
// switch. It runs at startup and on every settings change; only a real change
// reaches D-Bus. Failures are logged, never claimed as success.
func (d *Daemon) applyBluetoothSettings(v settings.UserSettings) {
	if d.bt == nil {
		return
	}
	d.btMu.Lock()
	initial := !d.btInit
	changed := d.btPairable != v.BluetoothPairable
	d.btPairable = v.BluetoothPairable
	d.btInit = true
	d.btMu.Unlock()
	if !initial && !changed {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(d.ctx, 8*time.Second)
		defer cancel()
		if err := d.bt.EnsurePowered(ctx); err != nil {
			d.logs.Warnf("bluetooth", "", "power on: %v", err)
			return
		}
		if err := d.bt.SetPairable(ctx, v.BluetoothPairable); err != nil {
			d.logs.Warnf("bluetooth", "", "pairable=%v: %v", v.BluetoothPairable, err)
		}
	}()
}

// applyBluetoothVolume mirrors the framework master onto the Mac's AVRCP
// volume. This is the single Bluetooth listening gain: the local source and
// loopback stay neutral, so nothing is attenuated twice. The write timestamp
// lets the sync loop ignore the echo of its own write.
func (d *Daemon) applyBluetoothVolume(level float64) {
	if d.bt == nil {
		return
	}
	d.btMu.Lock()
	if d.btVolSet && d.btVolume == level {
		d.btMu.Unlock()
		return
	}
	d.btVolume = level
	d.btVolSet = true
	d.btVolumeAt = time.Now()
	d.btMu.Unlock()
	d.setBluetoothPlaybackGain(level)
	go func() {
		ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
		defer cancel()
		if err := d.bt.SetTransportVolume(ctx, level); err != nil {
			d.logs.Debugf("bluetooth", "", "transport volume: %v", err)
		}
	}()
}

// forceBluetoothVolume re-applies the current level after a loopback appears.
func (d *Daemon) forceBluetoothVolume() {
	if d.bt == nil {
		return
	}
	d.btMu.Lock()
	d.btVolSet = false
	d.btMu.Unlock()
	d.applyBluetoothVolume(d.user.Get().Values.MasterVolume)
}

// watchBluetooth owns the connect/disconnect ring cue and volume resync.
// currentBluetoothAddress returns the connected source reported by the last
// media state or the paired-device list, if any.
func (d *Daemon) currentBluetoothAddress() string {
	if d.bt == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(d.ctx, 2*time.Second)
	defer cancel()
	if state, err := d.bt.Media(ctx); err == nil && state.DeviceAddress != "" {
		return state.DeviceAddress
	}
	if devices, err := d.bt.PairedDevices(ctx); err == nil {
		for _, dev := range devices {
			if dev.Connected {
				return dev.Address
			}
		}
	}
	return ""
}

func (d *Daemon) watchBluetooth() {
	if d.bt == nil {
		return
	}
	// A source may already be connected when the daemon starts; the manager
	// seeds its state without emitting a connect event.
	d.ensureConnectedBluetoothRoutes()
	events := d.bt.Watch(d.ctx)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.syncBluetoothVolume()
		case ev, ok := <-events:
			if !ok {
				return
			}
			switch ev.Kind {
			case bluetooth.EventConnected:
				d.logs.Infof("bluetooth", "", "source connected name=%q address=%s", ev.DeviceName, ev.DeviceAddress)
				d.pulseBluetoothRing()
				d.connectBluetoothAudio(ev.DeviceAddress)
			case bluetooth.EventDisconnected:
				d.logs.Infof("bluetooth", "", "source disconnected name=%q address=%s", ev.DeviceName, ev.DeviceAddress)
				d.pulseBluetoothRing()
				d.removeBluetoothAudioRoute(ev.DeviceAddress)
			case bluetooth.EventMedia:
				if ev.Media.Present {
					d.ensureBluetoothAudioRoute(ev.Media.DeviceAddress)
					d.forceBluetoothVolume()
				}
			}
		}
	}
}

// syncBluetoothVolume keeps the Mac and the framework master in step. It also
// self-heals PulseAudio recreating the device module with the default
// avrcp_absolute_volume=1, which would otherwise mirror the remote volume onto
// the local source and re-introduce the double/cubic attenuation.
func (d *Daemon) syncBluetoothVolume() {
	if d.bt == nil {
		return
	}
	address := d.currentBluetoothAddress()
	if address == "" {
		return
	}
	if d.ensureDecoupledBluetoothModule(address) {
		d.removeBluetoothAudioRoute(address)
		d.ensureBluetoothAudioRoute(address)
	}
	// Re-assert the local stages: PulseAudio can recreate/reset them on
	// profile or source changes, and a lost loopback gain means full scale.
	d.setBluetoothSourceFixed(address)
	d.setBluetoothPlaybackGain(d.user.Get().Values.MasterVolume)
	d.btMu.Lock()
	wrote := d.btVolumeAt
	d.btMu.Unlock()
	if time.Since(wrote) < 3*time.Second {
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, 3*time.Second)
	volume, err := d.bt.TransportVolume(ctx)
	cancel()
	if err != nil {
		return
	}
	level := float64(volume) / 127
	if level > 1 {
		level = 1
	}
	current := d.user.Get().Values.MasterVolume
	diff := level - current
	if diff < 0 {
		diff = -diff
	}
	if diff <= 0.01 {
		return
	}
	if err := d.writeUserSettings([]string{"master_volume"}, func(v *settings.UserSettings) {
		v.MasterVolume = level
	}); err != nil {
		d.logs.Debugf("bluetooth", "", "sync master volume: %v", err)
		return
	}
	d.logs.Infof("bluetooth", "", "master volume synced from the source: %d%%", int(math.Round(level*100)))
}

// connectBluetoothAudio brings up the source's A2DP/AVRCP profiles before
// loading the audio route. Sources often connect at ACL level only.
func (d *Daemon) connectBluetoothAudio(address string) {
	if d.bt == nil || strings.TrimSpace(address) == "" {
		return
	}
	go func() {
		// Reloading the PulseAudio device module recreates the source/card,
		// so drop the route before it and let the loader wait for the new
		// source.
		d.removeBluetoothAudioRoute(address)
		d.ensureDecoupledBluetoothModule(address)
		ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
		defer cancel()
		if err := d.bt.ConnectAudio(ctx, address); err != nil {
			d.logs.Debugf("bluetooth", "", "audio profiles for %s: %v", address, err)
		} else {
			d.logs.Infof("bluetooth", "", "audio profiles connected for %s", address)
		}
		// The Mac's AVRCP volume is the single Bluetooth gain and mirrors the
		// framework master.
		d.forceBluetoothVolume()
		d.ensureBluetoothAudioRoute(address)
	}()
}

// pulseBluetoothRing runs a short blue pulse on the ring for a source
// connect/disconnect. It uses the shared safe system presentation path: an
// app-presented ring baseline is restored when the pulse releases.
func (d *Daemon) pulseBluetoothRing() {
	ringID := d.ringResourceID()
	if ringID == "" {
		return
	}
	params, err := presets.ValidateParams(mustPreset("system.ring.pulse"), presets.Params{
		"speed": 1.1, "color": rgb(0, 136, 255), "min_intensity": 0.0,
	})
	if err != nil {
		d.logs.Warnf("bluetooth", "", "pulse cue: %v", err)
		return
	}
	// Reuse the framework's ring fallback presentation. Opening a second
	// system presentation would create two eligible baselines and make the
	// fallback selection depend on creation order.
	pres := d.fallback[ringID]
	if pres == nil {
		return
	}
	d.btPulseMu.Lock()
	if d.btPulseCap == "" {
		cap, err := d.router.Acquire(ringID, routing.SystemAppID, "system", pres.ID, routing.TouchNone)
		if err != nil {
			d.btPulseMu.Unlock()
			d.logs.Warnf("bluetooth", "", "pulse capture: %v", err)
			return
		}
		d.btPulseCap = cap.ID
	}
	if err := d.pres.Update(pres, presentations.RingEffect("system.ring.pulse", params)); err != nil {
		d.logs.Warnf("bluetooth", "", "pulse effect: %v", err)
	}
	d.btPulseGen++
	gen := d.btPulseGen
	if d.btPulseTmr != nil {
		d.btPulseTmr.Stop()
	}
	d.btPulseTmr = time.AfterFunc(2*time.Second, func() { d.endBluetoothPulse(gen) })
	d.btPulseMu.Unlock()
}

func (d *Daemon) endBluetoothPulse(gen uint64) {
	d.btPulseMu.Lock()
	defer d.btPulseMu.Unlock()
	if gen != d.btPulseGen {
		return
	}
	ringID := d.ringResourceID()
	capture := d.btPulseCap
	d.btPulseCap = ""
	if capture != "" {
		selected := d.router.Current(ringID).SelectedCaptureID == capture
		d.router.ForceRelease(capture)
		if !selected {
			d.logs.Infof("bluetooth", "", "connection pulse was obscured by another ring capture")
		}
	}
	// Restore the fallback effect the settings-driven state machine wants
	// (blank, or the microphone-active pulse).
	d.sysRingMu.Lock()
	d.sysRingState = ""
	d.sysRingMu.Unlock()
	d.updateSystemRing()
}

// requireLocalAdmin rejects paired-device management from supervised apps.
// The Web UI and the local CLI run outside the app sandbox; apps identify
// themselves through the bus run ID and their assigned UID.
func (d *Daemon) requireLocalAdmin(ctx context.Context) error {
	const detail = "paired-device management is a local administrator operation"
	if md, ok := metadata.FromIncomingContext(ctx); ok && len(md.Get("capsula-run-id")) > 0 {
		return status.Error(codes.PermissionDenied, detail)
	}
	if creds := grpcpeer.FromContext(ctx); creds.OK && creds.UID >= d.cfg.SandboxUIDBase {
		return status.Error(codes.PermissionDenied, detail)
	}
	return nil
}
