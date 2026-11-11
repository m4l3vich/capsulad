package daemon

import (
	"context"
	"errors"
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

// applyBluetoothVolume forwards the framework master volume to the connected
// source through AVRCP absolute volume, when the source supports it. The last
// sent value is remembered so unrelated settings changes do not spam D-Bus.
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
	d.btMu.Unlock()
	d.pushBluetoothVolume(level)
}

// forceBluetoothVolume re-sends the current level after a transport appears.
func (d *Daemon) forceBluetoothVolume() {
	if d.bt == nil {
		return
	}
	d.btMu.Lock()
	d.btVolSet = false
	level := d.btVolume
	d.btMu.Unlock()
	if level == 0 {
		level = d.user.Get().Values.MasterVolume
	}
	d.applyBluetoothVolume(level)
}

func (d *Daemon) pushBluetoothVolume(level float64) {
	go func() {
		ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
		defer cancel()
		err := d.bt.SetAbsoluteVolume(ctx, level)
		switch {
		case err == nil:
		case errors.Is(err, bluetooth.ErrUnsupported), errors.Is(err, bluetooth.ErrNoSource):
			// The source has no absolute-volume control; master volume still
			// applies inside the mixer.
		default:
			d.logs.Debugf("bluetooth", "", "absolute volume: %v", err)
		}
	}()
}

// watchBluetooth owns the connect/disconnect ring cue and volume resync.
func (d *Daemon) watchBluetooth() {
	if d.bt == nil {
		return
	}
	// A source may already be connected when the daemon starts; the manager
	// seeds its state without emitting a connect event.
	d.ensureConnectedBluetoothRoutes()
	events := d.bt.Watch(d.ctx)
	for {
		select {
		case <-d.ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			switch ev.Kind {
			case bluetooth.EventConnected:
				d.logs.Infof("bluetooth", "", "source connected name=%q address=%s", ev.DeviceName, ev.DeviceAddress)
				d.pulseBluetoothRing()
				d.forceBluetoothVolume()
				d.ensureBluetoothAudioRoute(ev.DeviceAddress)
			case bluetooth.EventDisconnected:
				d.logs.Infof("bluetooth", "", "source disconnected name=%q address=%s", ev.DeviceName, ev.DeviceAddress)
				d.pulseBluetoothRing()
				d.removeBluetoothAudioRoute(ev.DeviceAddress)
			case bluetooth.EventMedia:
				if ev.Media.Present {
					d.forceBluetoothVolume()
				}
			}
		}
	}
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
