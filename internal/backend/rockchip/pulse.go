package rockchip

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"capsulad/internal/backend"
)

// sinkPinTimeout bounds each pactl invocation used to pin and verify the
// physical sink gain, so a missing or wedged PulseAudio cannot stall a caller.
const sinkPinTimeout = 3 * time.Second

// pulseAudio bridges the framework to the stock PulseAudio system instance via
// the standard pacat/parec clients. Output goes to the `master` sink that
// /etc/pulse/device.pa declares as the default; capture uses `speex`, the
// processed microphone source, or `mics` for raw channels.
type pulseAudio struct {
	cfg Config

	mu        sync.Mutex
	closed    bool
	hwMute    bool
	pinning   bool
	pinDone   bool
	pinWarned bool
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
		"--latency-msec=120")
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

// SetMasterVolume pins the PulseAudio sink gain at unity. The mixer owns the
// only software master gain, so a sink gain other than unity would apply a
// second, hidden volume on top of the framework's master ceiling. The pin runs
// in the background and is bounded; a missing pactl degrades to the previous
// behaviour with one warning and never fails startup.
func (p *pulseAudio) SetMasterVolume(float64) error {
	p.mu.Lock()
	if p.closed || p.pinDone || p.pinning {
		p.mu.Unlock()
		return nil
	}
	p.pinning = true
	p.mu.Unlock()
	go p.pinSinkUnity()
	return nil
}

func (p *pulseAudio) pinSinkUnity() {
	defer func() {
		p.mu.Lock()
		p.pinning = false
		p.mu.Unlock()
	}()
	if _, err := exec.LookPath("pactl"); err != nil {
		p.warnOnce("pactl is unavailable; the PulseAudio sink gain stays unpinned")
		p.mu.Lock()
		p.pinDone = true
		p.mu.Unlock()
		return
	}
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		if err := p.setSinkUnity(); err != nil {
			last = err
			continue
		}
		unity, err := p.sinkIsUnity()
		switch {
		case err == nil && unity:
			p.mu.Lock()
			p.pinDone = true
			p.mu.Unlock()
			p.infof("PulseAudio sink %q pinned at unity", p.cfg.AudioSink)
			return
		case err != nil:
			last = err
		default:
			last = fmt.Errorf("sink %q did not read back at unity", p.cfg.AudioSink)
		}
	}
	// Leave pinDone false so a later volume change retries, but keep the
	// warning single-shot: a warning per volume step would flood the log.
	p.warnOnce(fmt.Sprintf("PulseAudio sink gain could not be pinned to unity: %v", last))
}

func (p *pulseAudio) setSinkUnity() error {
	ctx, cancel := context.WithTimeout(context.Background(), sinkPinTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pactl", "set-sink-volume", p.cfg.AudioSink, "100%").CombinedOutput()
	if err != nil {
		return fmt.Errorf("pactl set-sink-volume: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sinkIsUnity reads the sink volume back. Every reported channel percentage
// must be 100; a sink with one channel lowered is not unity.
func (p *pulseAudio) sinkIsUnity() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sinkPinTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pactl", "get-sink-volume", p.cfg.AudioSink).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("pactl get-sink-volume: %v: %s", err, strings.TrimSpace(string(out)))
	}
	vals := volumePercentages(string(out))
	if len(vals) == 0 {
		return false, fmt.Errorf("pactl get-sink-volume reported no channel volume")
	}
	for _, v := range vals {
		if v != 100 {
			return false, nil
		}
	}
	return true, nil
}

// volumePercentages extracts the integers from the `NN%` tokens pactl prints
// for each channel, e.g. "front-left: 65536 / 100% / 0.00 dB".
func volumePercentages(out string) []int {
	var vals []int
	for _, field := range strings.Fields(out) {
		if !strings.HasSuffix(field, "%") {
			continue
		}
		if v, err := strconv.Atoi(strings.TrimSuffix(field, "%")); err == nil {
			vals = append(vals, v)
		}
	}
	return vals
}

func (p *pulseAudio) warnOnce(msg string) {
	p.mu.Lock()
	first := !p.pinWarned
	p.pinWarned = true
	p.mu.Unlock()
	if first && p.cfg.Warnf != nil {
		p.cfg.Warnf("%s", msg)
	}
}

// infof reports the successful pin. The config hook is set once at
// construction and never mutated, so reading it here needs no lock.
func (p *pulseAudio) infof(format string, args ...any) {
	if p.cfg.Infof != nil {
		p.cfg.Infof(format, args...)
	}
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
