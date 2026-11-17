package amlogic

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/backend/linuxdev"
)

// audioPinTimeout bounds each mixer invocation used to pin or verify gain, so a
// wedged sound server cannot stall a caller.
const audioPinTimeout = 3 * time.Second

// newAudio selects the userland audio path and reports it. Stock runs
// PulseAudio system mode; Alpine runs ALSA directly. Both are driven through
// the distribution's own raw-PCM client, so the framework never links an audio
// library the target image may not ship.
func newAudio(cfg Config) (backend.AudioDevice, backend.SubsystemStatus) {
	switch cfg.AudioBackend {
	case "alsa":
		if !linuxdev.HasCommand(cfg.RootPrefix, "aplay") || !linuxdev.HasCommand(cfg.RootPrefix, "arecord") {
			return nil, backend.SubsystemStatus{Subsystem: "audio", Status: backend.Unavailable, Detail: "aplay/arecord not found"}
		}
		return newALSA(cfg), backend.SubsystemStatus{
			Subsystem: "audio", BackendID: "amlogic.alsa", Status: backend.Partial,
			Detail: fmt.Sprintf("ALSA client pipes (device=%s); BlueALSA carries Bluetooth sources outside this path, and latency must be validated on hardware", cfg.PCMDevice),
		}
	default:
		if !linuxdev.HasCommand(cfg.RootPrefix, "pacat") || !linuxdev.HasCommand(cfg.RootPrefix, "parec") {
			return nil, backend.SubsystemStatus{Subsystem: "audio", Status: backend.Unavailable, Detail: "pacat/parec not found"}
		}
		return newPulse(cfg), backend.SubsystemStatus{
			Subsystem: "audio", BackendID: "amlogic.pulseaudio", Status: backend.Partial,
			Detail: fmt.Sprintf("PulseAudio client pipes (sink=%s, source=%s, from the image's device.pa); latency and format behavior must be validated on hardware", audioName(cfg.AudioSink), audioName(cfg.AudioSource)),
		}
	}
}

func audioName(name string) string {
	if name == "" {
		return "(default)"
	}
	return name
}

// --- PulseAudio ------------------------------------------------------------

// pulseAudio bridges the framework to the stock PulseAudio system instance via
// pacat/parec. The profile names the image's `master` sink and `speex` source;
// an empty name still falls back to the PulseAudio default.
type pulseAudio struct {
	cfg Config

	mu        sync.Mutex
	closed    bool
	pinning   bool
	pinDone   bool
	pinWarned bool
}

func newPulse(cfg Config) *pulseAudio { return &pulseAudio{cfg: cfg} }

func (p *pulseAudio) PlaybackFormat() backend.AudioFormat { return p.cfg.AudioPlaybackFormat }
func (p *pulseAudio) CaptureFormat() backend.AudioFormat  { return p.cfg.AudioCaptureFormat }

func (p *pulseAudio) OpenOutput() (backend.Sink, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, backend.ErrUnavailable
	}
	args := []string{"--playback"}
	if p.cfg.AudioSink != "" {
		args = append(args, "--device="+p.cfg.AudioSink)
	}
	args = append(args,
		"--format=s16le",
		fmt.Sprintf("--rate=%d", p.cfg.AudioPlaybackFormat.RateHz),
		fmt.Sprintf("--channels=%d", p.cfg.AudioPlaybackFormat.Channels),
		"--raw", "--latency-msec=120")
	sink, err := linuxdev.NewPipeSink(exec.Command("pacat", args...))
	if err != nil {
		return nil, fmt.Errorf("pacat: %w", err)
	}
	return sink, nil
}

func (p *pulseAudio) OpenInput() (backend.Source, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, backend.ErrUnavailable
	}
	args := []string{}
	if p.cfg.AudioSource != "" {
		args = append(args, "--device="+p.cfg.AudioSource)
	}
	args = append(args,
		"--format=s16le",
		fmt.Sprintf("--rate=%d", p.cfg.AudioCaptureFormat.RateHz),
		fmt.Sprintf("--channels=%d", p.cfg.AudioCaptureFormat.Channels),
		"--raw", "--latency-msec=20")
	src, err := linuxdev.NewPipeSource(exec.Command("parec", args...))
	if err != nil {
		return nil, fmt.Errorf("parec: %w", err)
	}
	return src, nil
}

// SetMasterVolume pins the PulseAudio sink gain at unity in the background: the
// framework mixer owns the only master gain, so a sink gain would be a second,
// hidden volume. A missing pactl degrades to the previous behaviour with one
// warning and never fails startup.
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
	sink := p.cfg.AudioSink
	if sink == "" {
		sink = "@DEFAULT_SINK@"
	}
	if err := runPactl("set-sink-volume", sink, "100%"); err != nil {
		p.warnOnce(fmt.Sprintf("PulseAudio sink gain could not be pinned to unity: %v", err))
		return
	}
	out, err := pactlOutput("get-sink-volume", sink)
	if err != nil {
		p.warnOnce(fmt.Sprintf("PulseAudio sink gain could not be verified: %v", err))
		return
	}
	for _, v := range volumePercentages(out) {
		if v != 100 {
			p.warnOnce(fmt.Sprintf("PulseAudio sink %q did not read back at unity", sink))
			return
		}
	}
	p.mu.Lock()
	p.pinDone = true
	p.mu.Unlock()
	p.infof("PulseAudio sink %q pinned at unity", audioName(p.cfg.AudioSink))
}

// SetHardwareMute mutes the capture source and, when the board exposes one, the
// microphone gate GPIO. Only the GPIO is a physical gate: a source mute stops
// the framework's capture stream and is reported as software muting otherwise.
func (p *pulseAudio) SetHardwareMute(muted bool) (bool, error) {
	var errs []string
	if err := setMicGate(p.cfg.MicEnableGPIO, muted); err != nil {
		errs = append(errs, "gpio: "+err.Error())
	}
	source := p.cfg.AudioSource
	if source == "" {
		source = "@DEFAULT_SOURCE@"
	}
	if out, err := exec.Command("pactl", "set-source-mute", source, boolWord(muted)).CombinedOutput(); err != nil {
		errs = append(errs, "pactl: "+strings.TrimSpace(string(out)))
	}
	hw := p.cfg.MicEnableGPIO > 0 && gateExists(p.cfg.MicEnableGPIO)
	if len(errs) > 0 {
		return hw, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return hw, nil
}

func (p *pulseAudio) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
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

func (p *pulseAudio) infof(format string, args ...any) {
	if p.cfg.Infof != nil {
		p.cfg.Infof(format, args...)
	}
}

func runPactl(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), audioPinTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pactl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("pactl: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func pactlOutput(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), audioPinTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pactl", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("pactl: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
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

// --- ALSA ------------------------------------------------------------------

// alsaAudio bridges the framework to ALSA through aplay/arecord. Alpine uses
// ALSA directly (dmix for local playback, BlueALSA for Bluetooth sources).
type alsaAudio struct {
	cfg Config

	mu        sync.Mutex
	closed    bool
	pinning   bool
	pinDone   bool
	pinWarned bool
}

func newALSA(cfg Config) *alsaAudio { return &alsaAudio{cfg: cfg} }

func (a *alsaAudio) PlaybackFormat() backend.AudioFormat { return a.cfg.AudioPlaybackFormat }
func (a *alsaAudio) CaptureFormat() backend.AudioFormat  { return a.cfg.AudioCaptureFormat }

func (a *alsaAudio) OpenOutput() (backend.Sink, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, backend.ErrUnavailable
	}
	sink, err := linuxdev.NewPipeSink(exec.Command("aplay", a.pcmArgs(a.cfg.AudioPlaybackFormat)...))
	if err != nil {
		return nil, fmt.Errorf("aplay: %w", err)
	}
	return sink, nil
}

func (a *alsaAudio) OpenInput() (backend.Source, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, backend.ErrUnavailable
	}
	src, err := linuxdev.NewPipeSource(exec.Command("arecord", a.pcmArgs(a.cfg.AudioCaptureFormat)...))
	if err != nil {
		return nil, fmt.Errorf("arecord: %w", err)
	}
	return src, nil
}

func (a *alsaAudio) pcmArgs(format backend.AudioFormat) []string {
	args := []string{"-q", "-t", "raw", "-f", "S16_LE",
		"-r", strconv.Itoa(format.RateHz), "-c", strconv.Itoa(format.Channels)}
	if a.cfg.PCMDevice != "" {
		args = append(args, "-D", a.cfg.PCMDevice)
	}
	return args
}

// SetMasterVolume pins the configured mixer control to 100% so the framework
// mixer remains the only gain stage. Without a published control name the
// platform keeps its own level and the software mixer is the only gain.
func (a *alsaAudio) SetMasterVolume(float64) error {
	a.mu.Lock()
	if a.closed || a.pinDone || a.pinning || a.cfg.MixerVolume == "" {
		a.mu.Unlock()
		return nil
	}
	a.pinning = true
	a.mu.Unlock()
	go a.pinMixer()
	return nil
}

func (a *alsaAudio) pinMixer() {
	defer func() {
		a.mu.Lock()
		a.pinning = false
		a.mu.Unlock()
	}()
	if err := a.amixer("sset", a.cfg.MixerVolume, "100%"); err != nil {
		a.warnOnce(fmt.Sprintf("ALSA mixer %q could not be pinned: %v", a.cfg.MixerVolume, err))
		return
	}
	a.mu.Lock()
	a.pinDone = true
	a.mu.Unlock()
	a.infof("ALSA mixer %q pinned at 100%%", a.cfg.MixerVolume)
}

func (a *alsaAudio) SetHardwareMute(muted bool) (bool, error) {
	if a.cfg.MixerMute == "" {
		return false, nil
	}
	if err := a.amixer("sset", a.cfg.MixerMute, map[bool]string{true: "mute", false: "unmute"}[muted]); err != nil {
		return false, err
	}
	return true, nil
}

func (a *alsaAudio) amixer(args ...string) error {
	base := []string{}
	if a.cfg.MixerCard != "" {
		base = append(base, "-c", a.cfg.MixerCard)
	}
	ctx, cancel := context.WithTimeout(context.Background(), audioPinTimeout)
	defer cancel()
	cmdArgs := append(base, args...)
	out, err := exec.CommandContext(ctx, "amixer", cmdArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("amixer %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (a *alsaAudio) Close() error {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	return nil
}

func (a *alsaAudio) warnOnce(msg string) {
	a.mu.Lock()
	first := !a.pinWarned
	a.pinWarned = true
	a.mu.Unlock()
	if first && a.cfg.Warnf != nil {
		a.cfg.Warnf("%s", msg)
	}
}

func (a *alsaAudio) infof(format string, args ...any) {
	if a.cfg.Infof != nil {
		a.cfg.Infof(format, args...)
	}
}

// --- microphone gate -------------------------------------------------------

func boolWord(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func gatePath(gpio int) string {
	return fmt.Sprintf("/sys/class/gpio/gpio%d/value", gpio)
}

func gateExists(gpio int) bool {
	if gpio <= 0 {
		return false
	}
	_, err := os.Stat(gatePath(gpio))
	return err == nil
}

// setMicGate drives the microphone-enable GPIO when the profile names one.
// Boards without a gate return nil: the caller still mutes the capture source.
func setMicGate(gpio int, muted bool) error {
	if gpio <= 0 {
		return nil
	}
	path := gatePath(gpio)
	if _, err := os.Stat(path); err != nil {
		if e := os.WriteFile("/sys/class/gpio/export", []byte(strconv.Itoa(gpio)), 0o644); e != nil {
			return err
		}
		_ = os.WriteFile(fmt.Sprintf("/sys/class/gpio/gpio%d/direction", gpio), []byte("out"), 0o644)
	}
	value := "1"
	if muted {
		value = "0"
	}
	return os.WriteFile(path, []byte(value), 0o644)
}
