package daemon

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"capsulad/internal/audio"
	"capsulad/internal/backend"
	"capsulad/internal/bluetooth"
	"capsulad/internal/routing"
	"capsulad/internal/sbc"
)

// initBluetoothAudio chooses the audio owner for A2DP. The native endpoint
// gives the framework the stream fd and bypasses PulseAudio's bluez5 source,
// loopback and volume coupling. PulseAudio keeps owning app playback and the
// microphones. It runs before adapter power/pairability settings so the sink
// profile exists before anything can accept a connection, and falls back to
// the PA route so Bluetooth still works when registration fails.
func (d *Daemon) initBluetoothAudio() {
	if err := d.prepareBluetoothAudio(); err != nil {
		d.logs.Warnf("bluetooth", "", "audio owner: %v", err)
	}
}

// ensureBluetoothAdvertisement makes the sink profile available before a
// source is allowed to connect. It is idempotent: an already registered
// endpoint or a running PulseAudio owner is left untouched so applying the
// pairing setting never tears down a working audio path.
func (d *Daemon) ensureBluetoothAdvertisement() error {
	if d.bt == nil || d.dev.Platform() == backend.PlatformSimulator {
		return nil
	}
	if d.bluetoothAudioReady() {
		return nil
	}
	return d.prepareBluetoothAudio()
}

// bluetoothAudioReady reports whether some audio-source profile is actually
// advertised right now. A native claim alone is not enough: after a BlueZ
// restart or adapter power cycle the endpoint registration is gone, so the
// manager's generation-scoped SinkRegistered answer decides. A failed
// registration therefore never looks ready.
func (d *Daemon) bluetoothAudioReady() bool {
	if d.bt == nil {
		return false
	}
	if d.dev != nil && d.dev.Platform() == backend.PlatformSimulator {
		// The simulator's in-memory adapter has no audio-profile owner to
		// prepare; do not probe the host's PulseAudio for it.
		return true
	}
	if registrar, ok := d.bt.(bluetooth.A2DPRegistrar); ok && registrar.SinkRegistered() {
		return true
	}
	if d.bluetoothNative() {
		return false
	}
	return pulseBluetoothModulesLoaded()
}

// prepareBluetoothAudio registers the A2DP sink endpoint or, when that is not
// possible, loads PulseAudio's bluez5 discovery so the adapter still advertises
// an audio sink. It reports whether some audio-source profile is advertised.
//
// Ordering matters: the endpoint is registered before PulseAudio's competing
// endpoints are removed, so the adapter advertises an Audio Sink at every
// point of the switch. A source that is already connected through PulseAudio
// keeps that route and is never torn down; the native owner takes over after it
// disconnects, and the next incoming connection finds the registered sink.
func (d *Daemon) prepareBluetoothAudio() error {
	if d.bt == nil || d.dev.Platform() == backend.PlatformSimulator {
		return nil
	}
	btOwnerMu.Lock()
	defer btOwnerMu.Unlock()
	sink, ok := d.bt.(bluetooth.A2DPSink)
	if !ok {
		return d.ensurePulseBluetoothModules()
	}
	registrar, hasRegistrar := d.bt.(bluetooth.A2DPRegistrar)
	if hasRegistrar && registrar.SinkRegistered() {
		// The sink is advertised. Do not take ownership from a connected
		// source PulseAudio is routing; otherwise settle the switch now that
		// no active stream exists.
		if d.pulseKeepsBluetoothSource() {
			return nil
		}
		if pulseBluetoothModulesLoaded() {
			d.unloadPulseBluetoothModules()
		}
		d.setBluetoothNative(true)
		return nil
	}
	// An endpoint that was armed before but is no longer is not ready; drop
	// the stale native claim instead of advertising a profile BlueZ lost.
	d.setBluetoothNative(false)

	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()
	if err := sink.RegisterA2DPSink(ctx); err != nil {
		d.logs.Warnf("bluetooth", "", "native A2DP endpoint unavailable: %v; using PulseAudio routing", err)
		return d.ensurePulseBluetoothModules()
	}
	if hasRegistrar && !registrar.SinkRegistered() {
		// Registration returned without leaving the endpoint armed for the
		// current adapter generation; the sink is not advertised.
		d.logs.Warnf("bluetooth", "", "A2DP endpoint registration did not arm; using PulseAudio routing")
		return d.ensurePulseBluetoothModules()
	}
	if d.pulseKeepsBluetoothSource() {
		// The endpoint is now advertised for future connections, but the
		// connected source keeps its working PulseAudio route.
		d.logs.Infof("bluetooth", "", "native A2DP sink registered; PulseAudio keeps the connected source until it disconnects")
		return nil
	}
	d.unloadPulseBluetoothModules()
	d.setBluetoothNative(true)
	d.logs.Infof("bluetooth", "", "native A2DP SBC sink endpoint registered")
	return nil
}

// pulseKeepsBluetoothSource reports whether PulseAudio owns the audio profiles
// and is routing a connected source right now. Unloading its modules in that
// state tears the source's card down, and the source's reconnection can race a
// registration that has not completed yet.
func (d *Daemon) pulseKeepsBluetoothSource() bool {
	return pulseBluetoothModulesLoaded() && d.bluetoothSourceConnected()
}

// btOwnerMu serializes audio-owner selection. Settings, the watcher and
// connection repair can request it concurrently (for example a source that is
// already connected when the daemon starts); serializing prevents a second
// switch from unloading PulseAudio between registration and owning the sink,
// and prevents the native flag from flapping. It is process-wide because the
// daemon is the single owner of the adapter.
var btOwnerMu sync.Mutex

// bluetoothSourceConnected reports whether a paired device is connected now.
// It is used only to avoid tearing down an existing PulseAudio audio session;
// it is not a readiness poll.
func (d *Daemon) bluetoothSourceConnected() bool {
	if d.bt == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(d.ctx, 3*time.Second)
	defer cancel()
	devices, err := d.bt.PairedDevices(ctx)
	if err != nil {
		return false
	}
	for _, device := range devices {
		if device.Connected {
			return true
		}
	}
	return false
}

// ensureBluetoothSinkRegistered verifies that the native sink is armed for the
// current adapter before an audio profile is requested. A registration BlueZ
// dropped (service restart, adapter power cycle, or a failed startup attempt)
// is repaired here; a healthy registration is left untouched. PulseAudio's
// endpoints are removed only in this repair path, never on a healthy connect.
func (d *Daemon) ensureBluetoothSinkRegistered(ctx context.Context) error {
	btOwnerMu.Lock()
	defer btOwnerMu.Unlock()
	registrar, hasRegistrar := d.bt.(bluetooth.A2DPRegistrar)
	if hasRegistrar && registrar.SinkRegistered() {
		return nil
	}
	sink, ok := d.bt.(bluetooth.A2DPSink)
	if !ok {
		return nil
	}
	d.logs.Infof("bluetooth", "", "A2DP sink is not registered with the current adapter; re-registering")
	if err := sink.RegisterA2DPSink(ctx); err != nil {
		return err
	}
	if hasRegistrar && !registrar.SinkRegistered() {
		return errors.New("A2DP endpoint did not arm after registration")
	}
	// Remove PulseAudio's competing endpoints only after the sink is armed,
	// and never while it is routing the connected source.
	if !d.pulseKeepsBluetoothSource() {
		d.unloadPulseBluetoothModules()
	}
	d.setBluetoothNative(true)
	return nil
}

// bluetoothNative reports whether the framework owns the A2DP sink rather than
// PulseAudio. It is guarded because settings, watcher and connect paths read
// and update the owner concurrently.
func (d *Daemon) bluetoothNative() bool {
	d.btMu.Lock()
	defer d.btMu.Unlock()
	return d.btNative
}

func (d *Daemon) setBluetoothNative(native bool) {
	d.btMu.Lock()
	d.btNative = native
	d.btMu.Unlock()
}

func (d *Daemon) unloadPulseBluetoothModules() {
	out, err := pulseCommand("list", "short", "modules")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[1]
		if strings.Contains(name, "bluez5") || strings.Contains(name, "bluetooth") {
			_ = unloadPulseModule(fields[0])
		}
	}
}

// startBluetoothStream acquires the A2DP transport BlueZ just made usable and
// hands it to the framework mixer: RTP -> SBC decode -> mixer. A single
// attempt is made per transport event; ErrNoTransport means the source is not
// streaming yet and the next transport event will try again, so no timer or
// retry loop is involved.
func (d *Daemon) startBluetoothStream(address string) {
	sink, ok := d.bt.(bluetooth.A2DPSink)
	if !ok {
		return
	}
	d.btStreamMu.Lock()
	if d.btStream != nil || d.btStreamStart {
		d.btStreamMu.Unlock()
		return
	}
	d.btStreamStart = true
	d.btStreamGen++
	gen := d.btStreamGen
	d.btStreamMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
		defer cancel()
		transport, err := sink.AcquireA2DP(ctx)
		if err != nil {
			if !errors.Is(err, bluetooth.ErrNoTransport) {
				d.logs.Debugf("bluetooth", "", "A2DP acquire: %v", err)
			}
			d.btStreamMu.Lock()
			if d.btStreamGen == gen {
				d.btStreamStart = false
			}
			d.btStreamMu.Unlock()
			return
		}
		if !d.bluetoothGen(gen) {
			_ = transport.File.Close()
			return
		}
		stream := &a2dpStream{d: d, address: address, transport: transport}
		if err := stream.start(); err != nil {
			transport.File.Close()
			relCtx, relCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = sink.ReleaseA2DP(relCtx)
			relCancel()
			d.btStreamMu.Lock()
			if d.btStreamGen == gen {
				d.btStreamStart = false
			}
			d.btStreamMu.Unlock()
			d.logs.Warnf("bluetooth", "", "A2DP stream for %s: %v", address, err)
			return
		}
		d.btStreamMu.Lock()
		if d.btStreamGen != gen {
			d.btStreamMu.Unlock()
			// This acquire belongs to an older connection; close it without
			// releasing whatever transport the current stream owns.
			_ = transport.File.Close()
			stream.stopWithoutRelease()
			return
		}
		if d.btStream != nil {
			d.btStreamMu.Unlock()
			stream.stop()
			return
		}
		d.btStream = stream
		d.btStreamStart = false
		d.btStreamMu.Unlock()
		d.logs.Infof("bluetooth", "", "A2DP stream started for %s (fd mtu=%d config=%x)", address, transport.ReadMTU, transport.Config)
	}()
}

func (d *Daemon) bluetoothGen(gen uint64) bool {
	d.btStreamMu.Lock()
	defer d.btStreamMu.Unlock()
	return d.btStreamGen == gen
}

func (d *Daemon) stopBluetoothStream() {
	d.stopBluetoothStreamFor("")
}

// stopBluetoothStreamFor stops the acquired stream only when it belongs to the
// disconnecting source. A second paired device (a phone that connects for the
// call profile, or the user's laptop) leaving must not silence the source that
// is actually streaming: the single-stream sink has no event to reacquire its
// transport until that source pauses or resumes.
func (d *Daemon) stopBluetoothStreamFor(address string) {
	d.btStreamMu.Lock()
	stream := d.btStream
	if stream != nil && address != "" && stream.address != "" && !strings.EqualFold(stream.address, address) {
		d.btStreamMu.Unlock()
		return
	}
	d.btStream = nil
	d.btStreamGen++
	d.btStreamStart = false
	d.btStreamMu.Unlock()
	if stream != nil {
		stream.stop()
	}
}

// setBluetoothStreamTrim applies the product volume curve to the Bluetooth
// stream only. The mixer already multiplies the master once, so the trim is
// sqrt(master) and the stream's effective gain is master^1.5.
func (d *Daemon) setBluetoothStreamTrim(level float64) {
	if level < 0 {
		level = 0
	}
	if level > 1 {
		level = 1
	}
	d.btStreamMu.Lock()
	stream := d.btStream
	d.btStreamMu.Unlock()
	if stream != nil {
		stream.setTrim(math.Sqrt(level))
	}
}

// a2dpStream is one acquired transport plus its persistent decoder and mixer
// stream. The decoder keeps its synthesis history across RTP packets, so there
// are no chunk boundaries and no decoder subprocess.
type a2dpStream struct {
	d         *Daemon
	address   string
	transport *bluetooth.A2DPTransport
	decoder   *sbc.Decoder
	done      chan struct{}

	// lastPacketNS is the monotonic receipt time of the newest transport read.
	// It is evidence that this source is actually sending audio (a source that
	// paused keeps the transport open but stops sending). It belongs to this
	// stream and therefore to this connection: a new source gets a new stream
	// and stop() clears the daemon's current-stream slot, so a stale source's
	// receipts can never answer for another device. Nothing acts on this value
	// by itself; only a media toggle consults it.
	lastPacketNS atomic.Int64

	playbackMu sync.Mutex
	playback   *audio.Playback

	stopOnce sync.Once
}

// a2dpReceiptGrace is how recently the acquired transport must have delivered a
// packet for the sink to treat the source as playing. A playing A2DP source
// sends an RTP packet every 20-30 ms, so half a second absorbs scheduler and
// retransmission jitter while still resolving a pause long before the next
// physical tap. It is evidence freshness, not a playback timeout.
const a2dpReceiptGrace = 500 * time.Millisecond

// notePacket records that the transport delivered audio just now.
func (s *a2dpStream) notePacket() {
	s.lastPacketNS.Store(int64(s.d.clock.NowNS()))
}

// receivingWithin reports whether this stream delivered a packet inside grace.
func (s *a2dpStream) receivingWithin(nowNS uint64, grace time.Duration) bool {
	last := s.lastPacketNS.Load()
	if last == 0 {
		return false
	}
	now := int64(nowNS)
	return now >= last && now-last <= int64(grace)
}

// bluetoothReceivingAudio reports whether the framework-owned sink is actually
// receiving audio for address right now. An acquired transport disappears from
// BlueZ's object tree and a source may expose no MediaPlayer1.Status, so this
// sink-side receipt state is the only honest playback evidence for a media
// toggle. It is read-only: it never publishes or invents playback state, and a
// failed command leaves it untouched. An empty address accepts the current
// stream.
func (d *Daemon) bluetoothReceivingAudio(address string) bool {
	d.btStreamMu.Lock()
	stream := d.btStream
	d.btStreamMu.Unlock()
	if stream == nil {
		return false
	}
	if address != "" && !strings.EqualFold(stream.address, address) {
		return false
	}
	return stream.receivingWithin(d.clock.NowNS(), a2dpReceiptGrace)
}

func (s *a2dpStream) start() error {
	s.done = make(chan struct{})
	s.decoder = sbc.NewDecoder()
	go s.readTransport()
	return nil
}

func (s *a2dpStream) readTransport() {
	buf := make([]byte, 4096)
	logged := false
	for {
		n, err := s.transport.File.Read(buf)
		if n > 0 {
			s.notePacket()
		}
		if n > 0 && !logged {
			logged = true
			s.d.logs.Infof("bluetooth", "", "A2DP first transport packet: %d bytes head=%x", n, buf[:min(n, 16)])
		}
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				select {
				case <-s.done:
					return
				case <-time.After(10 * time.Millisecond):
				}
				continue
			}
			s.d.logs.Warnf("bluetooth", "", "A2DP transport read for %s: %v", s.address, err)
			s.stop()
			return
		}
		data, _ := bluetooth.A2DPSBCPayloadFrames(buf[:n])
		if len(data) == 0 && n > 4 && buf[0] == 0x9c {
			// Raw SBC frames: no RTP/A2DP headers.
			data = buf[:n]
		}
		if len(data) == 0 {
			continue
		}
		pcm, rate, channels, derr := s.decoder.Decode(data)
		if len(pcm) > 0 {
			s.play(pcm, rate, channels)
		}
		if derr != nil {
			s.d.logs.Debugf("bluetooth", "", "SBC decode: %v", derr)
		}
	}
}

func (s *a2dpStream) play(pcm []byte, rate, channels int) {
	if len(pcm) == 0 {
		return
	}
	p, err := s.ensurePlayback(rate, channels)
	if err != nil {
		s.d.logs.Warnf("bluetooth", "", "A2DP playback: %v", err)
		return
	}
	if err := p.Write(pcm); err != nil {
		s.stop()
	}
}

func (s *a2dpStream) ensurePlayback(rate, channels int) (*audio.Playback, error) {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	if s.playback != nil {
		return s.playback, nil
	}
	p, err := s.d.openAppPlayback(routing.SystemAppID, "", "audio.output.speaker",
		backend.AudioFormat{Format: backend.S16LE, RateHz: rate, Channels: channels}, "")
	if err != nil {
		return nil, err
	}
	p.SetTrim(math.Sqrt(s.d.user.Get().Values.MasterVolume))
	s.playback = p
	s.d.logs.Infof("bluetooth", "", "A2DP mixer stream opened at %d Hz/%d channels", rate, channels)
	return p, nil
}

func (s *a2dpStream) setTrim(v float64) {
	s.playbackMu.Lock()
	p := s.playback
	s.playbackMu.Unlock()
	if p != nil {
		p.SetTrim(v)
	}
}

func (s *a2dpStream) stop() {
	s.stopWithRelease(true)
}

func (s *a2dpStream) stopWithoutRelease() {
	s.stopWithRelease(false)
}

func (s *a2dpStream) stopWithRelease(release bool) {
	s.stopOnce.Do(func() {
		// A stopped or failed stream has no receipt evidence; a later stream
		// must not inherit it.
		s.lastPacketNS.Store(0)
		close(s.done)
		if s.transport != nil {
			_ = s.transport.File.Close()
		}
		s.playbackMu.Lock()
		p := s.playback
		s.playback = nil
		s.playbackMu.Unlock()
		if p != nil {
			p.CloseInput()
		}
		if release {
			if sink, ok := s.d.bt.(bluetooth.A2DPSink); ok {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = sink.ReleaseA2DP(ctx)
				cancel()
			}
		}
		s.d.btStreamMu.Lock()
		if s.d.btStream == s {
			s.d.btStream = nil
		}
		s.d.btStreamMu.Unlock()
	})
}
