package daemon

import (
	"context"
	"math"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/bluetooth"
	"capsulad/internal/grpcpeer"
	"capsulad/internal/presentations"
	"capsulad/internal/presets"
	"capsulad/internal/routing"
	"capsulad/internal/settings"
)

// applyBluetoothSettings powers the adapter and applies the persisted pairable
// switch. The A2DP sink profile is advertised before pairing is enabled: a
// source that connects first finds no audio-source profile and drops the link.
// It runs at startup and on every settings change; only a real change reaches
// D-Bus. Failures are logged, never claimed as success.
func (d *Daemon) applyBluetoothSettings(v settings.UserSettings) {
	if d.bt == nil {
		return
	}
	d.btMu.Lock()
	initial := !d.btInit
	changed := d.btPairable != v.BluetoothPairable
	d.btInit = true
	d.btMu.Unlock()
	if !initial && !changed {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
		defer cancel()
		if v.BluetoothPairable {
			// Refuse to accept a source before the profile it needs exists.
			if err := d.ensureBluetoothAdvertisement(); err != nil {
				d.logs.Warnf("bluetooth", "", "audio profile advertisement: %v; pairing stays off", err)
				return
			}
		}
		if err := d.bt.EnsurePowered(ctx); err != nil {
			d.logs.Warnf("bluetooth", "", "power on: %v", err)
			return
		}
		if err := d.bt.SetPairable(ctx, v.BluetoothPairable); err != nil {
			// Leave the recorded state unchanged so the next settings change
			// retries instead of claiming the switch was applied.
			d.logs.Warnf("bluetooth", "", "pairable=%v: %v", v.BluetoothPairable, err)
			return
		}
		d.btMu.Lock()
		d.btPairable = v.BluetoothPairable
		d.btMu.Unlock()
	}()
}

// applyBluetoothVolume mirrors the framework master onto the source's AVRCP
// volume, which the local gain stages complement. BlueZ suppresses the echo of
// a level it just wrote, so the source does not report it back as a change.
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
	if d.btNative {
		d.setBluetoothStreamTrim(level)
	} else {
		d.setBluetoothPlaybackGain(level)
	}
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

// applyBluetoothSourceVolume mirrors an AVRCP volume change made on the source
// into the framework master through the canonical master-volume path. That path
// persists the level, applies the local mixer and re-applies the Bluetooth
// gain; BlueZ does not republish a level it just wrote, so the mirror cannot
// loop. No OSD or cue is involved: those belong to input handling only.
func (d *Daemon) applyBluetoothSourceVolume(volume uint16) {
	if volume > 127 {
		volume = 127
	}
	level := float64(volume) / 127
	if math.Abs(level-d.user.Get().Values.MasterVolume) < 0.001 {
		return
	}
	if err := d.applyMasterVolume(level); err != nil {
		d.logs.Debugf("bluetooth", "", "sync master volume from source: %v", err)
		return
	}
	d.logs.Infof("bluetooth", "", "master volume synced from the source: %d%%", int(math.Round(level*100)))
}

// watchBluetooth owns the connect/disconnect notifications, the audio route,
// and volume mirroring. Everything is driven by manager events; there is no
// polling ticker, so a source-side change reaches the framework as soon as
// BlueZ reports it.
func (d *Daemon) watchBluetooth() {
	if d.bt == nil {
		return
	}
	// Subscribe before reading existing state: a source that connects between
	// the state read and the subscription would otherwise be invisible until
	// its next event, leaving it connected with no audio route. Seeding is
	// idempotent with the connect event that may also arrive.
	events := d.bt.Watch(d.ctx)
	// A source may already be connected when the daemon starts; the manager
	// seeds its state without emitting a connect event.
	d.ensureConnectedBluetoothRoutes()
	ring := &bluetoothNotice{}
	display := &bluetoothNotice{}
	defer func() {
		ring.close(d)
		display.close(d)
	}()
	for {
		var ringDone, displayDone <-chan time.Time
		if ring.timer != nil {
			ringDone = ring.timer.C
		}
		if display.timer != nil {
			displayDone = display.timer.C
		}
		select {
		case <-d.ctx.Done():
			return
		case <-ringDone:
			ring.close(d)
		case <-displayDone:
			display.close(d)
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Kind == bluetooth.EventResync {
				// Collapse a backlog of markers: only the newest snapshot is
				// worth one reconciliation. A real event that follows is
				// handled normally after the session is back in sync. The
				// drain is capped so a sustained flood cannot pin this loop.
				newest := ev
				var following *bluetooth.Event
				for range 64 {
					select {
					case next, ok := <-events:
						if !ok {
							return
						}
						if next.Kind == bluetooth.EventResync {
							newest = next
							continue
						}
						following = &next
					default:
					}
					break
				}
				d.reconcileBluetoothSession(newest.Media)
				if following == nil {
					continue
				}
				ev = *following
			}
			switch ev.Kind {
			case bluetooth.EventConnected:
				d.logs.Infof("bluetooth", "", "source connected name=%q address=%s", ev.DeviceName, ev.DeviceAddress)
				d.showBluetoothPulse(ring, bluetoothPulseCount)
				d.showBluetoothIndicator(display)
				d.connectBluetoothAudio(ev.DeviceAddress)
			case bluetooth.EventDisconnected:
				d.logs.Infof("bluetooth", "", "source disconnected name=%q address=%s", ev.DeviceName, ev.DeviceAddress)
				// Disconnect is a single pulse and no glyph: any connect
				// glyph still on screen is cleared at once, the display shows
				// the selected baseline, and only the ring acknowledges the
				// loss once.
				display.close(d)
				d.showBluetoothPulse(ring, 1)
				d.stopBluetoothStreamFor(ev.DeviceAddress)
				d.removeBluetoothAudioRoute(ev.DeviceAddress)
			case bluetooth.EventVolume:
				d.applyBluetoothSourceVolume(ev.Volume)
			case bluetooth.EventTransport:
				// BlueZ configured a usable transport; acquire it now instead
				// of retrying on a timer. The transport exposes AVRCP absolute
				// volume at this point, so the framework level can reach the
				// source before the first media packet. PulseAudio owns the
				// transport in fallback mode, so only the native sink acquires.
				if !d.btNative {
					continue
				}
				d.forceBluetoothVolume()
				d.startBluetoothStream(ev.DeviceAddress)
			case bluetooth.EventMedia:
				if ev.Media.Present {
					if !d.btNative {
						d.ensureBluetoothAudioRoute(ev.Media.DeviceAddress)
					} else if ev.Media.Playing {
						// A source can connect while paused; the transport is
						// only acquirable once playback starts.
						d.startBluetoothStream(ev.Media.DeviceAddress)
					}
				}
			}
		}
	}
}

// connectBluetoothAudio brings up the source's A2DP/AVRCP profiles before
// loading the audio route. Sources often connect at ACL level only.
func (d *Daemon) connectBluetoothAudio(address string) {
	if d.bt == nil || strings.TrimSpace(address) == "" {
		return
	}
	if d.btNative {
		go func() {
			start := time.Now()
			ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
			defer cancel()
			// The sink endpoint is registered at startup. If BlueZ restarted or
			// power-cycled the adapter since then it forgot the registration,
			// and the source would connect without an audio profile. Re-arm it
			// now; PulseAudio's discovery modules are unloaded only in this
			// repair path, never on a healthy connect, because unloading them
			// while a source is connected can tear the card down.
			if registrar, ok := d.bt.(bluetooth.A2DPRegistrar); ok && !registrar.SinkRegistered() {
				d.unloadPulseBluetoothModules()
				if sink, ok := d.bt.(bluetooth.A2DPSink); ok {
					if err := sink.RegisterA2DPSink(ctx); err != nil {
						d.logs.Warnf("bluetooth", "", "re-register A2DP sink before connecting %s: %v", address, err)
					} else {
						d.logs.Infof("bluetooth", "", "A2DP sink re-registered before connecting %s", address)
					}
				}
			}
			if trust, ok := d.bt.(bluetooth.TrustSetter); ok {
				if err := trust.SetTrusted(ctx, address, true); err != nil {
					d.logs.Debugf("bluetooth", "", "trust %s: %v", address, err)
				}
			}
			if err := d.bt.ConnectAudio(ctx, address); err != nil {
				d.logs.Warnf("bluetooth", "", "audio profiles for %s failed after %s: %v",
					address, time.Since(start).Round(time.Millisecond), err)
			} else {
				d.logs.Infof("bluetooth", "", "A2DP source profile connected for %s in %s",
					address, time.Since(start).Round(time.Millisecond))
			}
			d.forceBluetoothVolume()
			d.startBluetoothStream(address)
		}()
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

// --- connect/disconnect notifications --------------------------------------
//
// Both notifications are ordinary system presentations with captures, created
// through the same presentation manager and router that applications use. A
// presentation with a capture wins routing while it is held, and the app or
// internal baseline underneath resumes the moment the capture is released.

// The pulse is one fade-in/fade-out per second, so three complete pulses take
// exactly three seconds. The guard keeps the presentation alive just past the
// last fade so the final frame at zero intensity is rendered before release.
const (
	bluetoothPulseSpeed  = 1.0
	bluetoothPulseCount  = 3
	bluetoothPulseGuard  = 20 * time.Millisecond
	bluetoothIndicatorOn = 2 * time.Second
)

// Seven-segment masks for the lowercase "bt": bit 0 = a (top) ... bit 6 = g
// (middle). b lights c,d,e,f,g; t lights d,e,f,g.
var bluetoothGlyphs = [2]uint16{0b1111100, 0b1111000}

// bluetoothIndicatorCells puts "bt" on the two rightmost segment cells.
func bluetoothIndicatorCells(digitCount int) []uint16 {
	cells := make([]uint16, digitCount)
	if digitCount >= len(bluetoothGlyphs) {
		copy(cells[digitCount-len(bluetoothGlyphs):], bluetoothGlyphs[:])
	}
	return cells
}

// bluetoothPulseContent returns the blue whole-ring pulse and how long its
// presentation must live to complete exactly count fade-in/out cycles.
func bluetoothPulseContent(count int) (presentations.Content, time.Duration, error) {
	if count < 1 {
		count = 1
	}
	params, err := presets.ValidateParams(mustPreset("system.ring.pulse"), presets.Params{
		"speed": bluetoothPulseSpeed, "color": rgb(0, 136, 255), "min_intensity": 0.0,
	})
	if err != nil {
		return presentations.Content{}, 0, err
	}
	period := time.Duration(float64(time.Second) / bluetoothPulseSpeed)
	return presentations.RingEffect("system.ring.pulse", params),
		time.Duration(count)*period + bluetoothPulseGuard, nil
}

// bluetoothNotice is one transient system presentation plus its capture and
// lifetime timer. It is only touched from the watchBluetooth goroutine.
type bluetoothNotice struct {
	pres    *presentations.Presentation
	capture string
	timer   *time.Timer
}

func (n *bluetoothNotice) show(d *Daemon, resourceID string, content presentations.Content, life time.Duration) {
	n.close(d)
	if resourceID == "" {
		return
	}
	// Same open/capture/update entrypoints as any application: the facade
	// validates the resource and reserves SystemAppID for framework cues.
	p, err := d.openAppPresentation(routing.SystemAppID, "", resourceID)
	if err != nil {
		d.logs.Warnf("bluetooth", "", "notification presentation: %v", err)
		return
	}
	if err := d.updateAppPresentation(routing.SystemAppID, "", p, content); err != nil {
		d.logs.Warnf("bluetooth", "", "notification presentation: %v", err)
		d.closeAppPresentation(routing.SystemAppID, "", p)
		return
	}
	cap, err := d.acquireAppCapture(routing.SystemAppID, "", resourceID, p.ID, routing.TouchNone)
	if err != nil {
		d.logs.Warnf("bluetooth", "", "notification capture: %v", err)
		d.closeAppPresentation(routing.SystemAppID, "", p)
		return
	}
	n.pres, n.capture, n.timer = p, cap.ID, time.NewTimer(life)
}

// close releases the capture and destroys the presentation so the previous
// owner becomes visible again and no stale system baseline is left Ready.
func (n *bluetoothNotice) close(d *Daemon) {
	if n.timer != nil {
		n.timer.Stop()
		n.timer = nil
	}
	if n.capture != "" {
		// Owner-scoped release through the same helper RoutingService uses.
		d.releaseAppCapture(routing.SystemAppID, "", n.capture)
		n.capture = ""
	}
	if n.pres != nil {
		d.closeAppPresentation(routing.SystemAppID, "", n.pres)
		n.pres = nil
	}
}

// showBluetoothPulse runs the connect (three pulses) or disconnect (one
// pulse) animation.
func (d *Daemon) showBluetoothPulse(n *bluetoothNotice, count int) {
	content, life, err := bluetoothPulseContent(count)
	if err != nil {
		d.logs.Warnf("bluetooth", "", "pulse cue: %v", err)
		return
	}
	n.show(d, d.ringResourceID(), content, life)
}

// bluetoothIndicatorContent is the segment frame plus its exact display time.
func bluetoothIndicatorContent(digitCount int) (presentations.Content, time.Duration) {
	return presentations.SegmentFrame(bluetoothIndicatorCells(digitCount), false), bluetoothIndicatorOn
}

// showBluetoothIndicator shows "bt" on the two rightmost cells for two seconds.
func (d *Daemon) showBluetoothIndicator(n *bluetoothNotice) {
	resourceID := d.segmentDisplayResourceID()
	if resourceID == "" {
		return
	}
	digitCount := 4
	if info := d.resources[resourceID]; info != nil && info.GetSegment() != nil {
		digitCount = int(info.GetSegment().GetDigitCount())
	}
	content, life := bluetoothIndicatorContent(digitCount)
	n.show(d, resourceID, content, life)
}

func (d *Daemon) segmentDisplayResourceID() string {
	for _, info := range d.resources {
		if info.Kind == frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY {
			return info.ResourceId
		}
	}
	return ""
}

// reconcileBluetoothSession converges the audio session after the manager
// coalesced at least one watcher event (EventResync). The connected-device list,
// not a single transition, is authoritative: a source that disconnected while
// the watcher was behind is gone from it, and a source that appeared is in it.
// This is what makes a coalesced-away EventDisconnected harmless: the native
// stream and the fallback route are cleaned from state, not from the lost
// transition.
func (d *Daemon) reconcileBluetoothSession(media bluetooth.MediaState) {
	if d.bt == nil {
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	devices, err := d.bt.PairedDevices(ctx)
	cancel()
	if err != nil {
		// Without the device list, only "no source at all" is provable: a
		// single selected address cannot prove another source's stream is
		// gone. Clean everything only when the snapshot reports nothing left.
		if !media.Present {
			d.stopBluetoothStream()
			d.resetBluetoothRoutes()
		}
	} else {
		connected := make(map[string]bool, len(devices))
		for _, dev := range devices {
			if dev.Connected {
				connected[strings.ToUpper(dev.Address)] = true
			}
		}

		d.btStreamMu.Lock()
		stream := d.btStream
		d.btStreamMu.Unlock()
		if stream != nil && stream.address != "" && !connected[strings.ToUpper(stream.address)] {
			d.logs.Infof("bluetooth", "", "resync: stream for %s has no connected source", stream.address)
			d.stopBluetoothStreamFor(stream.address)
		}

		d.btRouteMu.Lock()
		stale := make([]string, 0, len(d.btRoutes))
		for address := range d.btRoutes {
			if !connected[strings.ToUpper(address)] {
				stale = append(stale, address)
			}
		}
		d.btRouteMu.Unlock()
		for _, address := range stale {
			d.removeBluetoothAudioRoute(address)
		}
	}

	// A source-side AVRCP volume change can be coalesced away too. The
	// transport is the same authority an EventVolume would have applied; the
	// mirror is a no-op when the framework level is already current.
	volCtx, volCancel := context.WithTimeout(d.ctx, 5*time.Second)
	volume, volErr := d.bt.TransportVolume(volCtx)
	volCancel()
	if volErr == nil && d.user != nil {
		d.applyBluetoothSourceVolume(volume)
	}

	// A present, audible source with nothing carrying it lost its
	// connect/transport event to the queue. Both acquisition paths are already
	// guarded against a second start, so a burst of markers starts audio once.
	if media.Present {
		if d.btNative {
			if media.Playing {
				d.startBluetoothStream(media.DeviceAddress)
			}
		} else {
			d.ensureBluetoothAudioRoute(media.DeviceAddress)
		}
	}
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
