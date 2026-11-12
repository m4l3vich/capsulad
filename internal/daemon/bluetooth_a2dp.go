package daemon

import (
	"context"
	"errors"
	"math"
	"os/exec"
	"strings"
	"sync"
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
// microphones. If endpoint registration fails, the PA route remains available
// as a fallback so Bluetooth still works.
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

func (d *Daemon) bluetoothAudioReady() bool {
	if d.btNative {
		return true
	}
	if registrar, ok := d.bt.(bluetooth.A2DPRegistrar); ok && registrar.SinkRegistered() {
		return true
	}
	return pulseBluetoothModulesLoaded()
}

// prepareBluetoothAudio registers the A2DP sink endpoint or, when that is not
// possible, loads PulseAudio's bluez5 discovery so the adapter still advertises
// an audio sink. It reports whether some audio-source profile is advertised.
func (d *Daemon) prepareBluetoothAudio() error {
	if d.bt == nil || d.dev.Platform() == backend.PlatformSimulator {
		return nil
	}
	if d.btNative {
		return nil
	}
	sink, ok := d.bt.(bluetooth.A2DPSink)
	if !ok {
		return d.ensurePulseBluetoothModules()
	}
	if registrar, ok := d.bt.(bluetooth.A2DPRegistrar); ok && registrar.SinkRegistered() {
		d.btNative = true
		return nil
	}
	// PulseAudio's discovery module registers its own A2DP endpoints and would
	// compete with ours. Unload it (and any device modules) before registering.
	d.unloadPulseBluetoothModules()
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()
	if err := sink.RegisterA2DPSink(ctx); err != nil {
		d.logs.Warnf("bluetooth", "", "native A2DP endpoint unavailable: %v; using PulseAudio routing", err)
		return d.ensurePulseBluetoothModules()
	}
	d.btNative = true
	d.logs.Infof("bluetooth", "", "native A2DP SBC sink endpoint registered")
	return nil
}

func (d *Daemon) unloadPulseBluetoothModules() {
	out, err := exec.Command("pactl", "list", "short", "modules").Output()
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
	d.btStreamMu.Lock()
	stream := d.btStream
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

	playbackMu sync.Mutex
	playback   *audio.Playback

	stopOnce sync.Once
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
