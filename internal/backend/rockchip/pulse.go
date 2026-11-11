package rockchip

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"capsulad/internal/backend"
)

// pulseAudio bridges the framework to the stock PulseAudio system instance via
// the standard pacat/parec clients. Output goes to the `master` sink that
// /etc/pulse/device.pa declares as the default; capture uses `speex`, the
// processed microphone source, or `mics` for raw channels.
type pulseAudio struct {
	cfg Config

	mu     sync.Mutex
	closed bool
	hwMute bool
}

func newPulseAudio(cfg Config) *pulseAudio { return &pulseAudio{cfg: cfg} }

func (p *pulseAudio) PlaybackFormat() backend.AudioFormat {
	return backend.AudioFormat{Format: backend.S16LE, RateHz: 48000, Channels: 2}
}

func (p *pulseAudio) CaptureFormat() backend.AudioFormat {
	return backend.AudioFormat{Format: backend.S16LE, RateHz: 16000, Channels: 1}
}

func (p *pulseAudio) OpenOutput() (backend.Sink, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, backend.ErrUnavailable
	}
	cmd := exec.Command("pacat", "--playback",
		"--device="+p.cfg.AudioSink,
		"--format=s16le", "--rate=48000", "--channels=2", "--raw",
		"--latency-msec=60")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("pacat: %w", err)
	}
	return &pulseSink{cmd: cmd, w: stdin}, nil
}

func (p *pulseAudio) OpenInput() (backend.Source, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, backend.ErrUnavailable
	}
	cmd := exec.Command("parec",
		"--device="+p.cfg.AudioSource,
		"--format=s16le", "--rate=16000", "--channels=1", "--raw",
		"--latency-msec=20")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("parec: %w", err)
	}
	return &pulseSource{cmd: cmd, r: stdout}, nil
}

func (p *pulseAudio) SetMasterVolume(v float64) error {
	// The mixer owns the software master gain. Keeping the PulseAudio sink at
	// unity avoids applying the same volume twice.
	return nil
}

func (p *pulseAudio) SetHardwareMute(muted bool) (bool, error) {
	p.mu.Lock()
	p.hwMute = muted
	p.mu.Unlock()
	var errs []string
	if err := p.setGPIO(muted); err != nil {
		errs = append(errs, "gpio: "+err.Error())
	}
	if out, err := exec.Command("pactl", "set-source-mute", p.cfg.AudioSource, boolWord(muted)).CombinedOutput(); err != nil {
		errs = append(errs, "pactl: "+strings.TrimSpace(string(out)))
	}
	// The mic-enable GPIO physically gates the microphone when available.
	hw := p.gpioExists()
	if len(errs) > 0 {
		return hw, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return hw, nil
}

func boolWord(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func (p *pulseAudio) gpioExists() bool {
	_, err := os.Stat(gpioValuePath(p.cfg.MicEnableGPIO))
	return err == nil
}

func gpioValuePath(gpio int) string {
	return fmt.Sprintf("/sys/class/gpio/gpio%d/value", gpio)
}

func (p *pulseAudio) setGPIO(muted bool) error {
	path := gpioValuePath(p.cfg.MicEnableGPIO)
	if _, err := os.Stat(path); err != nil {
		export := fmt.Sprintf("/sys/class/gpio/export")
		if e := os.WriteFile(export, []byte(strconv.Itoa(p.cfg.MicEnableGPIO)), 0o644); e != nil {
			return err
		}
		dir := fmt.Sprintf("/sys/class/gpio/gpio%d", p.cfg.MicEnableGPIO)
		_ = os.WriteFile(filepath.Join(dir, "direction"), []byte("out"), 0o644)
	}
	v := "1"
	if muted {
		v = "0"
	}
	return os.WriteFile(path, []byte(v), 0o644)
}

func (p *pulseAudio) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

type pulseSink struct {
	cmd *exec.Cmd
	w   io.WriteCloser
	mu  sync.Mutex
}

func (s *pulseSink) WritePCM(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.w.Write(p)
	return err
}

func (s *pulseSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.w.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	return s.cmd.Wait()
}

type pulseSource struct {
	cmd *exec.Cmd
	r   io.ReadCloser
	mu  sync.Mutex
}

func (s *pulseSource) ReadPCM(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := io.ReadFull(s.r, p)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		return n, nil
	}
	return n, err
}

func (s *pulseSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.r.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	return s.cmd.Wait()
}
