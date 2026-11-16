package daemon

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/bluetooth"
	"capsulad/internal/grpcpeer"
	"capsulad/internal/presentations"
	"capsulad/internal/presets"
	"capsulad/internal/routing"
	"capsulad/internal/settings"
	"capsulad/internal/uichanges"
)

// applyBluetoothSettings applies the persisted incoming-connections and
// discoverability policy. The A2DP sink profile is made ready before any
// inbound path is opened: a source that connects first finds no audio-source
// profile and drops the link, and it does not retry just because the endpoint
// appears later. Incoming off still powers the adapter, because the radio is
// shared with BLE work; it only denies inbound links.
//
// It runs at startup and on every settings change. Only a real change reaches
// D-Bus, and applications are serialized so a burst of settings updates
// converges on the latest persisted values. Failures are logged and leave the
// affected switch off, never claimed as success.
func (d *Daemon) applyBluetoothSettings(v settings.UserSettings) {
	if d.bt == nil {
		return
	}
	d.btMu.Lock()
	initial := !d.btInit
	changed := d.btIncoming != v.BluetoothIncomingConnections ||
		d.btDiscoverable != v.BluetoothDiscoverable
	d.btInit = true
	if !initial && !changed {
		d.btMu.Unlock()
		return
	}
	d.btApplyGen++
	gen := d.btApplyGen
	d.btMu.Unlock()
	go func() {
		// Serialize the asynchronous applies. A newer settings edit bumps the
		// generation; an older queued apply must not overwrite it with stale
		// values, so it exits when it finally takes the lock.
		d.btApplyMu.Lock()
		defer d.btApplyMu.Unlock()
		d.btMu.Lock()
		stale := d.btApplyGen != gen
		d.btMu.Unlock()
		if stale {
			return
		}
		ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
		defer cancel()
		wantIncoming := v.BluetoothIncomingConnections
		wantDiscoverable := v.BluetoothDiscoverable
		if wantDiscoverable {
			wantIncoming = true
		}
		if !wantIncoming {
			wantDiscoverable = false
		}

		record := func(incoming, discoverable bool) {
			d.btMu.Lock()
			d.btIncoming, d.btDiscoverable = incoming, discoverable
			d.btMu.Unlock()
		}

		// Incoming connections need the A2DP sink profile; discoverability is
		// only meaningful when it does too. The initial settings pass follows
		// initBluetoothAudio's single registration attempt and reports that
		// readiness; a later user action may repair a lost registration.
		advertised := true
		if wantIncoming {
			if initial {
				advertised = d.bluetoothAudioReady()
			} else {
				advertised = d.ensureBluetoothAdvertisement() == nil
			}
			if !advertised {
				if initial {
					d.logs.Warnf("bluetooth", "", "audio profile is not advertised at startup; incoming connections stay off")
				} else {
					d.logs.Warnf("bluetooth", "", "audio profile advertisement failed; incoming connections stay off")
				}
				// Do not accept an inbound link that has no audio profile. The
				// deny is applied before power so an autopowered controller
				// cannot accept one, and the radio stays on for BLE.
				denyOK, discoveryOK := true, true
				if err := d.bt.SetIncomingConnections(ctx, false); err != nil {
					denyOK = false
					d.logs.Warnf("bluetooth", "", "incoming=false after a failed audio profile: %v", err)
				}
				if err := d.bt.EnsurePowered(ctx); err != nil {
					d.logs.Warnf("bluetooth", "", "power/connectability after a failed audio profile: %v", err)
				}
				if err := d.bt.SetIncomingConnections(ctx, false); err != nil {
					denyOK = false
					d.logs.Warnf("bluetooth", "", "incoming=false after a failed audio profile: %v", err)
				}
				if err := d.bt.SetDiscoverable(ctx, false); err != nil {
					discoveryOK = false
					d.logs.Warnf("bluetooth", "", "discoverable=false after a failed audio profile: %v", err)
				}
				if denyOK && discoveryOK {
					record(false, false)
				}
				return
			}
		}

		// Power the radio even when no inbound link is accepted: BLE work for
		// applications must remain possible. When incoming is denied, apply it
		// once before power so a controller that autopowers connectable cannot
		// accept a link during startup; the explicit call below is
		// authoritative. Failures here are retried after power.
		if !wantIncoming {
			_ = d.bt.SetIncomingConnections(ctx, false)
		}
		if err := d.bt.EnsurePowered(ctx); err != nil {
			d.logs.Warnf("bluetooth", "", "power/connectability: %v", err)
			return
		}
		if err := d.bt.SetIncomingConnections(ctx, wantIncoming); err != nil {
			// Leave the recorded state unchanged so the next settings change
			// retries instead of claiming the switch was applied.
			d.logs.Warnf("bluetooth", "", "incoming=%v: %v", wantIncoming, err)
			return
		}
		if err := d.bt.SetDiscoverable(ctx, wantDiscoverable); err != nil {
			d.logs.Warnf("bluetooth", "", "discoverable=%v: %v", wantDiscoverable, err)
			// Incoming may already be applied; reflect only that truth so a
			// later settings update retries the discovery window.
			record(wantIncoming, false)
			return
		}
		record(wantIncoming, wantDiscoverable)
	}()
}

// The framework master is the authoritative listening level, and the AVRCP
// absolute-volume register mirrors it so the source's own display follows a
// speaker-side change. The mirror is a display write, not a gain: the local
// attenuation stays the only stage that changes how loud the stream is, which is
// why a compensated build sounded far too loud and a doubly attenuated one
// near-silent on this unit.
//
// A peer's echo is not a usable authority either: it reports the level it thinks
// it applied, rounds it to its own step, and reports nothing at all when the
// register is unwritable. Adopting that echo moved the user's master to the
// peer's neighbour and made a volume press take seconds to be audible while the
// round trip settled, so the level we just wrote is remembered and its own
// report is ignored. The memory must stay narrow: a phone's slider drag is a run
// of reports, and a wide window or step neighbourhood swallowed the whole drag
// so the speaker lagged behind the phone.
const (
	// bluetoothVolumeForwardDeadline bounds one mirror write to the source. The
	// write is asynchronous and never blocks a key press or the render path.
	bluetoothVolumeForwardDeadline = 5 * time.Second
	// bluetoothVolumeEchoWindow is how long after a write the source may still
	// report exactly that level back. A peer's echo follows its write immediately;
	// a deliberate change must never wait for a window, so this is a fraction of a
	// second rather than the seconds an earlier build used.
	bluetoothVolumeEchoWindow = 900 * time.Millisecond
	// bluetoothVolumeEchoSlack is the AVRCP step distance a report may differ by
	// and still be the echo of our write, because a source rounds the level to its
	// own step count before reporting it. Anything further away is the user's
	// slider and is adopted at once, even while a write is in flight.
	bluetoothVolumeEchoSlack = 1
	// bluetoothVolumeMirrorSettle coalesces the intermediate levels of one
	// gesture: a key repeat, a slider drag or a full-panel swipe (whose revert
	// lands ~100 ms after the leading step) replaces the pending level inside this
	// window, so the peer sees one write per settled level. It is a fixed delay
	// from the first pending change, not a debounce a continuous gesture can
	// extend, and it runs on the mirror goroutine, never on the key path.
	bluetoothVolumeMirrorSettle = 450 * time.Millisecond
	// bluetoothVolumeEchoMemory bounds how many recent writes are remembered.
	// Writes are serialized and user-paced, so a handful is enough and the memory
	// can never grow.
	bluetoothVolumeEchoMemory = 4
)

// bluetoothVolumeStep converts a [0,1] level to the AVRCP absolute-volume step
// the manager writes, so a write and the peer's report are compared in the
// units the peer actually publishes.
func bluetoothVolumeStep(level float64) uint16 {
	if level < 0 {
		level = 0
	}
	if level > 1 {
		level = 1
	}
	return uint16(math.Round(level * 127))
}

// applyBluetoothGain applies the framework master locally and mirrors it to the
// source. The local stage is unchanged and remains the only gain: writing the
// AVRCP register used to attenuate the same signal twice (10 % on both ends
// sounded near-silent, because the product curve already scales the stream by the
// master), and compensating the local trim for the source's share made it far too
// loud — macOS reports the written value back without attenuating the A2DP
// stream, so the "confirmation" that the source applies it is not evidence. The
// write that remains is the display mirror: the source's own level indicator
// follows the speaker, and the peer's echo of it is filtered instead of adopted.
func (d *Daemon) applyBluetoothGain(level float64) {
	if d.bt == nil {
		return
	}
	d.btMu.Lock()
	if d.btGainSet && d.btGain == level {
		d.btMu.Unlock()
		return
	}
	d.btGain, d.btGainSet = level, true
	d.btMu.Unlock()
	if d.bluetoothNative() {
		d.setBluetoothStreamTrim(level)
	} else {
		d.setBluetoothPlaybackGain(level)
	}
	d.mirrorBluetoothVolume(level)
}

// forgetBluetoothGain drops the deduplication memory so the next apply is
// unconditional, after a new route or a new source appears. Because apply is the
// mirror's trigger, the same call re-asserts the mirror when a transport or a new
// connection appears.
func (d *Daemon) forgetBluetoothGain() {
	d.btMu.Lock()
	d.btGainSet = false
	d.btMu.Unlock()
}

// bluetoothVolumeEcho is one level this daemon wrote to the source's AVRCP
// register and when it did. It exists only so the peer's report of our own write
// is not mistaken for a slider move.
type bluetoothVolumeEcho struct {
	step uint16
	at   time.Time
}

// noteBluetoothVolumeEchoLocked remembers a level about to be written and
// returns the slot it took, so a refused write can drop exactly that record.
// Writes are serialized, so the slot cannot belong to another write.
func (d *Daemon) noteBluetoothVolumeEchoLocked(step uint16) int {
	slot := d.btEchoNext
	d.btEchoes[slot] = bluetoothVolumeEcho{step: step, at: time.Now()}
	d.btEchoNext = (slot + 1) % len(d.btEchoes)
	return slot
}

// bluetoothVolumeEchoedLocked reports whether a report from the source reproduces
// a level this daemon wrote inside the echo window. The comparison is in AVRCP
// steps because that is the unit the peer publishes and rounds to. Empty slots
// carry a zero time, so they fail the window check and never match.
func (d *Daemon) bluetoothVolumeEchoedLocked(volume uint16) bool {
	now := time.Now()
	for i := range d.btEchoes {
		echo := d.btEchoes[i]
		if echo.at.IsZero() || now.Sub(echo.at) > bluetoothVolumeEchoWindow {
			continue
		}
		diff := int(echo.step) - int(volume)
		if diff < 0 {
			diff = -diff
		}
		if diff <= bluetoothVolumeEchoSlack {
			return true
		}
	}
	return false
}

// forgetBluetoothVolumeEchoes clears the write memory and the once-per-session
// diagnosis. It runs on connect and disconnect: an echo belongs to the session
// that produced it, and a new source must not inherit the previous one's.
func (d *Daemon) forgetBluetoothVolumeEchoes() {
	d.btMu.Lock()
	d.btEchoes = [bluetoothVolumeEchoMemory]bluetoothVolumeEcho{}
	d.btEchoNext = 0
	d.btNoVolumeLogged = false
	d.btMu.Unlock()
}

// mirrorBluetoothVolume publishes the framework master to the source's AVRCP
// register so a speaker-side change is visible on the source. It is asynchronous
// and coalesced in a single slot: a key repeat or a slider drag leaves at most one
// pending write, the newest level wins, and neither the key path nor the render
// path ever waits for D-Bus.
func (d *Daemon) mirrorBluetoothVolume(level float64) {
	if d.bt == nil {
		return
	}
	d.btMu.Lock()
	d.btMirrorLevel, d.btMirrorGen = level, d.btMirrorGen+1
	start := !d.btMirrorBusy
	d.btMirrorBusy = true
	d.btMu.Unlock()
	if start {
		go d.runBluetoothVolumeMirror()
	}
}

// runBluetoothVolumeMirror drains the mirror slot through one coalescing window.
// The window is fixed from the first pending change, so the newest level inside
// it is the one written: the leading step of a swipe and its revert land together
// and the peer sees one write, while a continuous drag still writes at least once
// per window. It is the only writer, so the echo record it takes belongs to its own
// write; it exits as soon as no newer level arrived while writing.
func (d *Daemon) runBluetoothVolumeMirror() {
	for {
		timer := time.NewTimer(bluetoothVolumeMirrorSettle)
		select {
		case <-d.ctx.Done():
			// Shutting down: the busy flag stays set so no caller starts a new
			// runner against a cancelled context.
			timer.Stop()
			return
		case <-timer.C:
		}
		d.btMu.Lock()
		level, gen := d.btMirrorLevel, d.btMirrorGen
		d.btMu.Unlock()
		d.writeBluetoothVolume(level)
		d.btMu.Lock()
		if d.btMirrorGen == gen {
			d.btMirrorBusy = false
			d.btMu.Unlock()
			return
		}
		d.btMu.Unlock()
	}
}

// writeBluetoothVolume writes one mirror level and records it as an echo unless
// the write was refused. A source without a writable register is normal: the
// speaker still attenuates locally, a source-slider change still drives the
// master, and the diagnosis is said once per session at Debug so a volume press
// does not produce a repeated warning.
func (d *Daemon) writeBluetoothVolume(level float64) {
	step := bluetoothVolumeStep(level)
	d.btMu.Lock()
	slot := d.noteBluetoothVolumeEchoLocked(step)
	d.btMu.Unlock()

	ctx, cancel := context.WithTimeout(d.ctx, bluetoothVolumeForwardDeadline)
	err := d.bt.SetTransportVolume(ctx, level)
	cancel()
	if err == nil {
		// The field question "did we send it, and did the peer take it?" needs
		// this line: the write is asynchronous and otherwise silent.
		d.logs.Debugf("bluetooth", "", "transport volume mirrored to the source: %d%%", int(math.Round(level*100)))
		return
	}
	// The register did not take our level, so no report of it can follow: drop
	// the record instead of treating a later genuine report as its echo.
	d.btMu.Lock()
	d.btEchoes[slot] = bluetoothVolumeEcho{}
	d.btMu.Unlock()

	if errors.Is(err, bluetooth.ErrNoSource) || errors.Is(err, bluetooth.ErrNoTransport) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// No source, or its transport is not configured yet. The transport event
		// and the next master change re-assert the mirror; there is nothing for
		// the user to act on.
		return
	}
	if errors.Is(err, bluetooth.ErrNoAbsoluteVolume) {
		d.btMu.Lock()
		logged := d.btNoVolumeLogged
		d.btNoVolumeLogged = true
		d.btMu.Unlock()
		if !logged {
			d.logs.Debugf("bluetooth", "", "source does not accept a mirrored AVRCP volume, the local level stays authoritative: %v", err)
		}
		return
	}
	d.logs.Debugf("bluetooth", "", "mirror master volume to the source: %v", err)
}

// applyBluetoothSourceVolume applies a volume the source reports to the framework
// master. Volume is two-way for display, but a report that reproduces a level this
// daemon just wrote is the source's echo, not a slider move, and adopting it moved
// the master to the peer's rounded neighbour. A report outside that echo memory is
// the user moving the phone's slider, and the speaker follows it once.
func (d *Daemon) applyBluetoothSourceVolume(volume uint16) {
	if volume > 127 {
		volume = 127
	}
	d.btMu.Lock()
	echoed := d.bluetoothVolumeEchoedLocked(volume)
	d.btMu.Unlock()
	if echoed {
		d.logs.Debugf("bluetooth", "", "ignoring the source's echo of the level just written: %d/127", volume)
		return
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
	d.observeBluetoothCommands()
	// A source may already be connected when the daemon starts; the manager
	// seeds its state without emitting a connect event.
	//
	// Session verification is generation-scoped: a new connection bumps the
	// generation and cancels the pending verdict, a disconnect does the same, and
	// daemon shutdown cancels through d.ctx. The guard bounds the single A2DP
	// profile request per source.
	gen := &atomic.Uint64{}
	sessions := newBTSessionGuard()
	var verifyCancel context.CancelFunc
	stopVerify := func() {
		if verifyCancel != nil {
			verifyCancel()
			verifyCancel = nil
		}
	}
	startVerify := func(address string, audioReady <-chan struct{}) {
		if strings.TrimSpace(address) == "" {
			return
		}
		stopVerify()
		ctx, cancel := context.WithTimeout(d.ctx, btSessionVerifyDeadline+btSessionRequestDeadline+30*time.Second)
		verifyCancel = cancel
		current := gen.Add(1)
		go d.verifyBluetoothSession(ctx, address, audioReady, sessions, current, func() bool {
			return gen.Load() == current
		})
	}
	defer stopVerify()
	for _, restored := range d.ensureConnectedBluetoothRoutes() {
		startVerify(restored.Address, restored.AudioReady)
	}
	d.publishUI(uichanges.Bluetooth)
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
			// Adapter state has no D-Bus property stream for connectability;
			// this invalidation makes the Web UI re-read observed status after
			// every manager event instead of polling.
			d.publishUI(uichanges.Bluetooth)
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
				if newest.Media.Present {
					// A coalesced-away connect event also lost its verdict. The
					// session was just reconciled, so assert its audio link too.
					ready := make(chan struct{})
					close(ready)
					startVerify(newest.Media.DeviceAddress, ready)
				}
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
				startVerify(ev.DeviceAddress, d.connectBluetoothAudio(ev.DeviceAddress))
				// A new source session starts with no echo memory, and its own
				// display gets the current level as soon as its transport exists.
				d.forgetBluetoothVolumeEchoes()
				d.mirrorBluetoothVolume(d.user.Get().Values.MasterVolume)
			case bluetooth.EventDisconnected:
				d.logs.Infof("bluetooth", "", "source disconnected name=%q address=%s", ev.DeviceName, ev.DeviceAddress)
				// Every disconnect is a real lifecycle change: the daemon never
				// disconnects a source itself. It supersedes the pending verdict.
				stopVerify()
				gen.Add(1)
				d.forgetBluetoothVolumeEchoes()
				// Disconnect is a single pulse and no glyph: any connect
				// glyph still on screen is cleared at once, the display shows
				// the selected baseline, and only the ring acknowledges the
				// loss once.
				display.close(d)
				d.showBluetoothPulse(ring, 1)
				d.stopBluetoothStreamFor(ev.DeviceAddress)
				d.removeBluetoothAudioRoute(ev.DeviceAddress)
				// A native switch deferred at startup (PulseAudio owned the
				// audio of a source that was already connected) is completed
				// now that the source is gone. Registering with no active
				// stream cannot disturb a listening session, and the next
				// incoming connection finds the sink profile ready.
				if !d.bluetoothNative() {
					go func() {
						if err := d.prepareBluetoothAudio(); err != nil {
							d.logs.Debugf("bluetooth", "", "switch to the native A2DP sink: %v", err)
						}
					}()
				}
			case bluetooth.EventVolume:
				d.applyBluetoothSourceVolume(ev.Volume)
			case bluetooth.EventVolumeUnavailable:
				// Once per source session: the host has no writable absolute
				// volume, so the master level only moves the local trim. The
				// media detail says so as well.
				d.logs.Warnf("bluetooth", "", "%s", ev.Detail)
			case bluetooth.EventTransport:
				// BlueZ configured a usable transport; acquire it now instead
				// of retrying on a timer. The transport exposes AVRCP absolute
				// volume at this point, so the framework level can reach the
				// source before the first media packet. PulseAudio owns the
				// transport in fallback mode, so only the native sink acquires.
				if !d.bluetoothNative() {
					continue
				}
				d.forgetBluetoothGain()
				d.applyBluetoothGain(d.user.Get().Values.MasterVolume)
				d.startBluetoothStream(ev.DeviceAddress)
			case bluetooth.EventMedia:
				if ev.Media.Present {
					if !d.bluetoothNative() {
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

// observeBluetoothCommands reports how the manager resolved a transport command.
// A resolved toggle logs its direction and the evidence behind it at Debug; a
// failed AVRCP call is the honest failure surface for a media key, so it logs at
// Warn. It is read-only: the manager never lets an observer change a command.
func (d *Daemon) observeBluetoothCommands() {
	observer, ok := d.bt.(bluetooth.CommandObserver)
	if !ok {
		return
	}
	observer.SetCommandObserver(func(obs bluetooth.CommandObservation) {
		if obs.Err != nil {
			d.logs.Warnf("bluetooth", "", "avrcp %s for %s failed: %v", obs.Command, obs.Device, obs.Err)
			return
		}
		evidence := obs.Evidence
		if evidence == "" {
			evidence = "none"
		}
		d.logs.Debugf("bluetooth", "", "avrcp %s for %s -> %s (evidence=%s)",
			obs.Command, obs.Device, obs.Method, evidence)
	})
}

// connectBluetoothAudio brings up the source's A2DP/AVRCP profiles before
// loading the audio route. Sources often connect at ACL level only. The returned
// channel is closed when the profile request has finished (immediately when the
// daemon does not request one), so session verification can judge the link after
// the request instead of racing it.
func (d *Daemon) connectBluetoothAudio(address string) <-chan struct{} {
	done := make(chan struct{})
	if d.bt == nil || strings.TrimSpace(address) == "" {
		close(done)
		return done
	}
	if d.bluetoothNative() {
		go func() {
			defer close(done)
			d.connectBluetoothAudioNative(address)
		}()
		return done
	}
	go func() {
		defer close(done)
		// If no audio owner became ready at startup (BlueZ was still coming up,
		// or its registration was lost), resolve it before asking the source
		// for a profile. The owner selection is idempotent and serialized; it
		// also retires a loaded PulseAudio stack that is not carrying audio, so
		// the source cannot land on its Headset endpoint while the native sink
		// is armed.
		if err := d.prepareBluetoothAudio(); err != nil {
			d.logs.Warnf("bluetooth", "", "audio owner before connecting %s: %v", address, err)
		}
		if d.bluetoothNative() {
			d.connectBluetoothAudioNative(address)
			return
		}
		d.connectBluetoothAudioPulse(address)
	}()
	return done
}

// connectBluetoothAudioNative registers/verifies the native sink and then asks
// the source for its audio profiles. The sink must be armed first: a source
// that is asked for A2DP before the endpoint exists answers "profile
// unavailable". The request uses Device1.ConnectProfile on the already-connected
// device, so it does not open a second connection; live MacBook evidence shows
// that request brings up A2DP/AVRCP on an ACL-only link. Whether a source that
// already abandoned A2DP during the pre-registration window honors the request
// without reconnecting is device-dependent and remains a hardware gate.
func (d *Daemon) connectBluetoothAudioNative(address string) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
	defer cancel()
	// A healthy registration is never touched; only a lost or failed one is
	// repaired.
	if err := d.ensureBluetoothSinkRegistered(ctx); err != nil {
		d.logs.Warnf("bluetooth", "", "A2DP sink is not ready before connecting %s: %v", address, err)
	}
	if trust, ok := d.bt.(bluetooth.TrustSetter); ok {
		if err := trust.SetTrusted(ctx, address, true); err != nil {
			d.logs.Debugf("bluetooth", "", "trust %s: %v", address, err)
		}
	}
	if err := d.bt.ConnectAudio(ctx, address); err != nil {
		d.logs.Warnf("bluetooth", "", "audio profiles for %s failed after %s: %v",
			address, time.Since(start).Round(time.Millisecond), err)
	} else if verifier, ok := d.bt.(bluetooth.A2DPSession); ok {
		// The ConnectProfile calls not failing only means BlueZ accepted the
		// request. Only the observed transport says the session is A2DP, so log
		// what was actually observed; the session verification follows it.
		linkCtx, cancel := context.WithTimeout(d.ctx, 3*time.Second)
		link, linkErr := verifier.Session(linkCtx, address)
		cancel()
		if linkErr != nil {
			d.logs.Infof("bluetooth", "", "audio profiles accepted for %s in %s; audio link unreadable: %v",
				address, time.Since(start).Round(time.Millisecond), linkErr)
		} else {
			d.logs.Infof("bluetooth", "", "audio profiles accepted for %s in %s; audio link %s",
				address, time.Since(start).Round(time.Millisecond), link)
		}
	} else {
		d.logs.Infof("bluetooth", "", "audio profiles accepted for %s in %s",
			address, time.Since(start).Round(time.Millisecond))
	}
	d.startBluetoothStream(address)
}

// connectBluetoothAudioPulse is the PulseAudio fallback route. It is blocking
// and is always called from a goroutine.
func (d *Daemon) connectBluetoothAudioPulse(address string) {
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
		d.logs.Infof("bluetooth", "", "audio profiles accepted for %s; PulseAudio owns the route", address)
	}
	// With the device module decoupled from AVRCP, the loopback carries the local
	// gain (setBluetoothPlaybackGain) and the transport volume is only the
	// source's display mirror of the framework master.
	d.ensureBluetoothAudioRoute(address)
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
	if !media.Present {
		// A session lost to a coalesced event is over for volume as well: the
		// echo memory of its writes must not outlive it.
		d.forgetBluetoothVolumeEchoes()
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
		if d.bluetoothNative() {
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
	return d.requireAdministrativeCaller(ctx, "paired-device management is a local administrator operation")
}

// publishUI invalidates one Web UI topic after its authoritative store changed.
// The in-process hub carries no data; the browser watcher rebuilds the view.
func (d *Daemon) publishUI(topic uichanges.Topic) {
	if d.uiChanges != nil {
		d.uiChanges.Publish(topic)
	}
}

// requireAdministrativeCaller is the single local-administrator gate. A
// supervised application cannot qualify by omitting its run-id metadata:
// on Linux its sandbox UID or its supervised PID still identifies it. The
// daemon's own connections may omit a run ID (they carry the internal token
// for private app-bus targets); a compiled-in system app always sends its run
// ID and is therefore rejected like any other app.
func (d *Daemon) requireAdministrativeCaller(ctx context.Context, detail string) error {
	if md, ok := metadata.FromIncomingContext(ctx); ok && len(md.Get("capsulad-run-id")) > 0 {
		return status.Error(codes.PermissionDenied, detail)
	}
	if creds := grpcpeer.FromContext(ctx); creds.OK {
		if creds.UID >= d.cfg.SandboxUIDBase {
			return status.Error(codes.PermissionDenied, detail)
		}
		if d.isSupervisedPID(creds.PID) {
			return status.Error(codes.PermissionDenied, detail)
		}
	}
	return nil
}
