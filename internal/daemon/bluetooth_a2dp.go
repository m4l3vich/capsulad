package daemon

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"capsulad/internal/audio"
	"capsulad/internal/backend"
	"capsulad/internal/bluetooth"
	"capsulad/internal/routing"
)

var a2dpFIFOSeq uint64

// initBluetoothAudio chooses the audio owner for A2DP. The native endpoint
// gives the framework the stream fd and bypasses PulseAudio's bluez5 source,
// loopback and volume coupling. PulseAudio keeps owning app playback and the
// microphones. If endpoint registration fails, the PA route remains available
// as a fallback so Bluetooth still works.
func (d *Daemon) initBluetoothAudio() {
	if d.bt == nil || d.dev.Platform() == backend.PlatformSimulator {
		return
	}
	sink, ok := d.bt.(bluetooth.A2DPSink)
	if !ok {
		d.ensurePulseBluetoothModules()
		return
	}
	// PulseAudio's discovery module registers its own A2DP endpoints and would
	// compete with ours. Unload it (and any device modules) before registering.
	d.unloadPulseBluetoothModules()
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()
	if err := sink.RegisterA2DPSink(ctx); err != nil {
		d.logs.Warnf("bluetooth", "", "native A2DP endpoint unavailable: %v; using PulseAudio routing", err)
		d.ensurePulseBluetoothModules()
		return
	}
	d.btNative = true
	d.logs.Infof("bluetooth", "", "native A2DP SBC sink endpoint registered")
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

// startBluetoothStream waits for BlueZ to configure the transport and then
// owns it: RTP -> SBC decode -> framework mixer. The wait covers the profile
// handshake after a source connects.
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
		ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
		defer cancel()
		var transport *bluetooth.A2DPTransport
		var err error
		for i := 0; i < 75; i++ {
			transport, err = sink.AcquireA2DP(ctx)
			if err == nil {
				break
			}
			if !errors.Is(err, bluetooth.ErrNoTransport) {
				d.logs.Debugf("bluetooth", "", "A2DP acquire: %v", err)
			}
			select {
			case <-d.ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
		if transport == nil {
			d.btStreamMu.Lock()
			if d.btStreamGen == gen {
				d.btStreamStart = false
			}
			d.btStreamMu.Unlock()
			d.logs.Warnf("bluetooth", "", "A2DP transport for %s: %v", address, err)
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
			stream.stop()
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
	if stream != nil && stream.playback != nil {
		stream.playback.SetTrim(math.Sqrt(level))
	}
}

// a2dpStream is one acquired transport plus its decoder and mixer stream.
type a2dpStream struct {
	d         *Daemon
	address   string
	transport *bluetooth.A2DPTransport

	playback  *audio.Playback
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	pcm       *os.File
	fifo      string
	frameByte int

	stopOnce sync.Once
}

func (s *a2dpStream) start() error {
	fifo := filepath.Join(os.TempDir(), fmt.Sprintf("capsulad-a2dp-%d-%d.pcm", os.Getpid(), atomic.AddUint64(&a2dpFIFOSeq, 1)))
	_ = os.Remove(fifo)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		return fmt.Errorf("create decoder fifo: %w", err)
	}
	s.fifo = fifo
	cmd := exec.Command("sbcdec", "-f", fifo)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = os.Remove(fifo)
		return fmt.Errorf("sbcdec stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = os.Remove(fifo)
		return fmt.Errorf("start sbcdec: %w", err)
	}
	s.cmd, s.stdin = cmd, stdin

	// sbcdec opens the FIFO for writing; this open blocks until it does.
	pcm, err := os.Open(fifo)
	if err != nil {
		s.stop()
		return fmt.Errorf("open decoder fifo: %w", err)
	}
	s.pcm = pcm

	// Start the transport reader before waiting for the decoder header: the
	// decoder only emits the AU header after it has seen the first frames.
	go s.readTransport()

	header := make([]byte, 24)
	if _, err := io.ReadFull(pcm, header); err != nil {
		s.stop()
		return fmt.Errorf("read decoder header: %w", err)
	}
	if string(header[:4]) != ".snd" {
		s.stop()
		return fmt.Errorf("decoder returned %q, not an AU stream", header[:4])
	}
	offset := int(binary.BigEndian.Uint32(header[4:8]))
	if offset < 24 {
		offset = 24
	}
	if offset > 24 {
		if _, err := io.CopyN(io.Discard, pcm, int64(offset-24)); err != nil {
			s.stop()
			return fmt.Errorf("skip decoder header: %w", err)
		}
	}
	encoding := binary.BigEndian.Uint32(header[12:16])
	rate := int(binary.BigEndian.Uint32(header[16:20]))
	channels := int(binary.BigEndian.Uint32(header[20:24]))
	if encoding != 3 {
		s.stop()
		return fmt.Errorf("decoder AU encoding %d is not 16-bit linear PCM", encoding)
	}
	if rate <= 0 || channels <= 0 {
		s.stop()
		return fmt.Errorf("decoder reported %d Hz/%d channels", rate, channels)
	}
	s.frameByte = channels * 2
	playback, err := s.d.audio.OpenPlayback(routing.SystemAppID, "", "audio.output.speaker",
		backend.AudioFormat{Format: backend.S16LE, RateHz: rate, Channels: channels}, "")
	if err != nil {
		s.stop()
		return fmt.Errorf("open mixer stream: %w", err)
	}
	s.playback = playback
	playback.SetTrim(math.Sqrt(s.d.user.Get().Values.MasterVolume))
	go s.readPCM()
	return nil
}

func (s *a2dpStream) readTransport() {
	buf := make([]byte, 4096)
	for {
		n, err := s.transport.File.Read(buf)
		if err != nil {
			s.stop()
			return
		}
		payload := bluetooth.A2DPSBCPayload(buf[:n])
		if len(payload) == 0 {
			continue
		}
		if _, err := s.stdin.Write(payload); err != nil {
			s.stop()
			return
		}
	}
}

func (s *a2dpStream) readPCM() {
	buf := make([]byte, 8192)
	var carry []byte
	for {
		n, err := s.pcm.Read(buf)
		if n > 0 {
			data := append(carry, buf[:n]...)
			usable := len(data) - len(data)%s.frameByte
			if usable > 0 {
				// sbcdec emits AU S16 big-endian; the mixer consumes LE.
				le := make([]byte, usable)
				for i := 0; i < usable; i += 2 {
					le[i] = data[i+1]
					le[i+1] = data[i]
				}
				if err := s.playback.Write(le); err != nil {
					s.stop()
					return
				}
				data = data[usable:]
			}
			carry = append(carry[:0], data...)
		}
		if err != nil {
			s.stop()
			return
		}
	}
}

func (s *a2dpStream) stop() {
	s.stopOnce.Do(func() {
		if s.stdin != nil {
			_ = s.stdin.Close()
		}
		if s.transport != nil {
			_ = s.transport.File.Close()
		}
		if s.pcm != nil {
			_ = s.pcm.Close()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		if s.cmd != nil {
			_ = s.cmd.Wait()
		}
		if s.playback != nil {
			s.playback.CloseInput()
		}
		if s.fifo != "" {
			_ = os.Remove(s.fifo)
		}
		if sink, ok := s.d.bt.(bluetooth.A2DPSink); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = sink.ReleaseA2DP(ctx)
			cancel()
		}
		s.d.btStreamMu.Lock()
		if s.d.btStream == s {
			s.d.btStream = nil
		}
		s.d.btStreamMu.Unlock()
	})
}
