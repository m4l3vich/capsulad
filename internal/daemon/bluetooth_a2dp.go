package daemon

import (
	"context"
	"errors"
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
// point of the switch. A source PulseAudio is actively streaming keeps that
// route; a loaded PulseAudio stack that is not carrying audio is retired as soon
// as our endpoint is armed, so the next connection cannot land on its Headset
// endpoint. Either way the next incoming connection finds the registered sink.
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
		// The sink is advertised. Do not take ownership from a source
		// PulseAudio is actively streaming; otherwise retire its idle module
		// set and own the sink, so the next connection is native.
		if d.bluetoothSourceConnected() {
			// A source's audio route belongs to the source for as long as it is
			// connected, paused included. Retiring the PulseAudio route of a
			// paused source tears down its transport, and the resume then has to
			// renegotiate A2DP mid-playback, which is heard as a freeze. The
			// switch completes on disconnect, when no route is in use.
			return nil
		}
		if pulseBluetoothModulesLoaded() {
			d.logs.Infof("bluetooth", "", "retiring PulseAudio Bluetooth discovery: the native sink is armed and no source is connected")
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

// pulseKeepsBluetoothSource reports whether PulseAudio is actively carrying a
// connected source's audio right now. Only that state is worth preserving: a
// loaded module set with nothing running is the narrowband trap, because its
// Headset endpoint stays advertised next to our sink and the next connection can
// land on HFP even though the native sink is armed and idle. Unloading it makes
// the following connection use the native sink.
func (d *Daemon) pulseKeepsBluetoothSource() bool {
	return pulseBluetoothModulesLoaded() && d.bluetoothSourceConnected() && d.pulseStreamsBluetoothSource()
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

// a2dpStream is one acquired transport plus its persistent decoder and mixer
// stream. The decoder keeps its synthesis history across RTP packets, so there
// are no chunk boundaries and no decoder subprocess. Its mixer stream stays at
// unity trim: the framework mixer owns the single master taper, so a second
// level-dependent gain here would make Bluetooth quieter than every other
// source at the same persisted level.
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
// receiving audio for address right now. While this daemon owns the acquired
// transport that receipt is the authoritative playback evidence for a media
// toggle: a source that paused keeps the transport open but stops sending, and
// its MediaPlayer1.Status lags our own command. It is read-only: it never
// publishes or invents playback state, and a failed command leaves it untouched.
// An empty address accepts the current stream.
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
	s.playback = p
	s.d.logs.Infof("bluetooth", "", "A2DP mixer stream opened at %d Hz/%d channels", rate, channels)
	return p, nil
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

// --- session verification ----------------------------------------------------
//
// Connecting the audio profiles is a request, not a result: BlueZ answers
// ConnectProfile for a profile the peer merely has registered, and the source
// can still end up on a Headset-only or ACL-only link. The session is therefore
// asserted after the request. When A2DP is missing the daemon asks for that one
// profile, once per source, and never disconnects the peer: dropping a user's
// phone off the adapter stops their audio and cannot be undone by the daemon.

var (
	// btSessionVerifyDeadline is how long a source has to produce an A2DP
	// transport, counted from the end of the profile request. Healthy setups on
	// this unit measured 156-496 ms, so two seconds leaves room for a slow SBC
	// negotiation without making the user wait five seconds for a link the peer
	// is not going to open — the field report is "it recovered a couple of
	// seconds later", which is this deadline. Asking for the profile while AVDTP
	// is already starting is harmless: BlueZ answers InProgress and the transport
	// arrives anyway.
	btSessionVerifyDeadline = 2 * time.Second
	// btSessionRequestDeadline bounds the one A2DP profile request per source.
	btSessionRequestDeadline = 15 * time.Second
	// btRequestWindow is how long a source is left alone after a profile
	// request. Requesting it creates a new connection generation, so without
	// this window a source that keeps landing on a non-A2DP profile would be
	// asked again in a loop.
	btRequestWindow = 2 * time.Minute
)

// isProfileInProgress recognizes BlueZ's answer when the profile the daemon asked
// for is already connecting: the request is redundant, not failed, and the
// transport arrives on its own.
func isProfileInProgress(err error) bool {
	if err == nil {
		return false
	}
	detail := strings.ToLower(err.Error())
	return strings.Contains(detail, "inprogress") || strings.Contains(detail, "in progress") ||
		strings.Contains(detail, "already connected")
}

// btLinkWait is the outcome of waiting for a source's A2DP transport.
type btLinkWait int

const (
	// btLinkVerified: an A2DP transport is present.
	btLinkVerified btLinkWait = iota
	// btLinkAbsent: the deadline passed without one.
	btLinkAbsent
	// btLinkGone: the source disconnected, or it is not an audio source at all.
	btLinkGone
)

// btSessionGuard bounds the A2DP profile request to one per connection, not one
// per source. A fresh connection is a fresh negotiation and must be allowed to
// ask again — the third reconnect in the field report was refused because the
// previous connection's request was still inside the window, and the source was
// left connected with AVRCP only, which the host then reports as "no audio
// output". Within one connection the attempt stays bounded, so a source that
// keeps refusing A2DP cannot be asked in a loop.
type btSessionGuard struct {
	mu        sync.Mutex
	pending   map[string]bool
	requested map[string]btProfileRequest
}

// btProfileRequest is the last request made for one source and the connection
// generation it belonged to.
type btProfileRequest struct {
	generation uint64
	at         time.Time
}

func newBTSessionGuard() *btSessionGuard {
	return &btSessionGuard{
		pending:   map[string]bool{},
		requested: map[string]btProfileRequest{},
	}
}

// claimRequest reports whether this connection may ask the source for its A2DP
// profile. A new connection generation always may; within one generation the
// request is allowed once per btRequestWindow.
func (g *btSessionGuard) claimRequest(address string, generation uint64, now time.Time) bool {
	key := strings.ToUpper(strings.TrimSpace(address))
	if key == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending[key] {
		return false
	}
	if prev, ok := g.requested[key]; ok {
		if prev.generation == generation && now.Sub(prev.at) < btRequestWindow {
			return false
		}
	}
	g.pending[key] = true
	return true
}

func (g *btSessionGuard) finishRequest(address string, generation uint64, now time.Time) {
	key := strings.ToUpper(strings.TrimSpace(address))
	if key == "" {
		return
	}
	g.mu.Lock()
	delete(g.pending, key)
	g.requested[key] = btProfileRequest{generation: generation, at: now}
	g.mu.Unlock()
}

// awaitBluetoothA2DP waits for the source's audio link to become A2DP until it
// does, the source leaves, or the deadline passes.
//
// The wait is signal-driven: the manager's own event stream is the wake-up, and
// each event is one reason to re-read the link. There is no observation
// interval and no periodic object-tree read; the deadline is the only timer.
func (d *Daemon) awaitBluetoothA2DP(ctx context.Context, verifier bluetooth.A2DPSession, address string) (bluetooth.AudioLink, btLinkWait) {
	var last bluetooth.AudioLink
	// Subscribe before the first read: a transport that appears between the
	// read and the wait is then queued on the watcher instead of being missed.
	// The subscription is scoped to this wait, not to the whole verdict.
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	var events <-chan bluetooth.Event
	if d.bt != nil {
		events = d.bt.Watch(watchCtx)
	}
	deadline := time.NewTimer(btSessionVerifyDeadline)
	defer deadline.Stop()
	for {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		link, err := verifier.Session(callCtx, address)
		cancel()
		if err != nil {
			d.logs.Debugf("bluetooth", "", "audio link for %s: %v", address, err)
		} else {
			last = link
			switch {
			case !link.Connected, !link.AudioSource:
				return link, btLinkGone
			case link.A2DP():
				return link, btLinkVerified
			}
		}
		select {
		case <-ctx.Done():
			return last, btLinkGone
		case <-deadline.C:
			return last, btLinkAbsent
		case _, ok := <-events:
			if !ok {
				// The manager closed its stream (shutdown): the link cannot
				// change anymore, so the current verdict stands.
				if ctx.Err() != nil {
					return last, btLinkGone
				}
				return last, btLinkAbsent
			}
		}
	}
}

// verifyBluetoothSession asserts that a newly connected source got an A2DP
// transport and repairs the link once when it did not. current reports whether
// this verdict still belongs to the newest connection generation; a newer
// connection, a disconnect and daemon shutdown all invalidate it. The repair is
// additionally capped per source by the guard, so it can never loop.
func (d *Daemon) verifyBluetoothSession(ctx context.Context, address string, audioReady <-chan struct{}, guard *btSessionGuard, generation uint64, current func() bool) {
	verifier, ok := d.bt.(bluetooth.A2DPSession)
	if !ok || strings.TrimSpace(address) == "" {
		return
	}
	// Judge the link after the profile request finished: a setup that is still
	// in flight is not a missing transport.
	select {
	case <-audioReady:
	case <-ctx.Done():
		return
	}
	link, state := d.awaitBluetoothA2DP(ctx, verifier, address)
	switch state {
	case btLinkVerified:
		d.logs.Debugf("bluetooth", "", "audio link for %s is A2DP (%s)", address, link)
		return
	case btLinkGone:
		return
	}
	profiles := strings.Join(link.ProfilesUp(), ",")
	advertised := strings.Join(link.Advertised, ",")
	if advertised == "" {
		advertised = "none"
	}
	if !current() {
		return
	}
	now := time.Now()
	if !guard.claimRequest(address, generation, now) {
		d.logs.Warnf("bluetooth", "", "%s is connected without an A2DP transport (profiles up: %s; peer advertises: %s); the A2DP profile was already requested for this connection, not repeating",
			address, profiles, advertised)
		return
	}
	d.logs.Warnf("bluetooth", "", "%s is connected without an A2DP transport (profiles up: %s; peer advertises: %s); requesting its A2DP source profile",
		address, profiles, advertised)
	// Derived from the verification context, not the daemon's: a newer
	// connection generation (stopVerify) or daemon shutdown must abort the
	// request instead of asking a source the user just disconnected.
	requestCtx, cancel := context.WithTimeout(ctx, btSessionRequestDeadline)
	err := verifier.RequestA2DPProfile(requestCtx, address)
	cancel()
	guard.finishRequest(address, generation, time.Now())
	if err != nil {
		if isProfileInProgress(err) {
			d.logs.Debugf("bluetooth", "", "A2DP profile for %s is already being set up: %v", address, err)
			return
		}
		d.logs.Warnf("bluetooth", "", "A2DP profile request for %s failed: %v; the source stays connected", address, err)
		return
	}
	d.logs.Infof("bluetooth", "", "A2DP profile requested for %s; the source stays connected", address)
}
