// Package audio implements the framework-owned PCM mixer, capture fan-out,
// automatic ducking and the app microphone watchdog.
//
// One physical source is captured once and fanned out into bounded per-client
// queues; slow consumers receive discontinuity information instead of stalling
// hardware capture. Playback streams are independent and mixed at 48 kHz
// stereo S16LE.
//
// Application capture additionally passes through a bounded 16 kHz mono DSP
// chain (rumble high-pass, conservative spectral noise suppression, peak
// limiter). The same declared processed tap feeds framework wakeword
// enrollment and inference, so training and detection share one policy; an
// explicit raw app capture remains available separately. See dsp.go.
package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

const (
	outRate       = 48000
	outChannels   = 2
	mixFrameMS    = 10
	playbackQueue = 32
	// The capture event channel must hold the one-second pre-roll, which is
	// emitted synchronously before the RPC reader starts: captureLoop produces
	// 20 ms chunks, so the window is at most 50 chunks plus the Started event.
	// A smaller queue would silently truncate the pre-roll; live overflow is
	// still bounded and reported as CLIENT_TOO_SLOW.
	captureQueue    = 64
	progressPeriod  = 250 * time.Millisecond
	prerollMS       = 1000
	maxPlaybackItem = 1 << 20

	// maxPlaybackStreamsPerApp bounds one app's concurrent playback width.
	// Together with the per-app ceiling it makes the master volume a structural
	// limit on what a single app can contribute, and it bounds mixer cost per
	// tick on the A35.
	maxPlaybackStreamsPerApp = 4

	// Soft limiter on the master bus. The threshold leaves a little headroom
	// before full scale; the attack window holds the detected peak, and the
	// release is an envelope time constant.
	limiterThreshold = 0.95
	limiterAttackMS  = 5
	limiterReleaseMS = 150
)

// ErrPlaybackLimit reports that the app already holds
// maxPlaybackStreamsPerApp concurrent playback streams. The media service maps
// it to RESOURCE_EXHAUSTED; other open errors remain precondition failures.
var ErrPlaybackLimit = errors.New("concurrent playback stream limit reached")

type Options struct {
	// IndicationActive reports whether an app currently shows a qualifying
	// visible ring/segment indication.
	IndicationActive func(appID string) bool
	// IndicationCue delivers the audible half of the microphone indication. It
	// is called once when a supervised application opens a capture and must
	// return only after the mixer has consumed and drained the cue to the
	// output sink. It returns an error when an audible cue cannot be
	// guaranteed (no output, zero master volume, mute, or a failed stream). A
	// nil hook denies app captures: silence is never an implicit microphone
	// indication.
	IndicationCue   func(appID string) error
	WatchdogGraceMS int
	WatchdogPollMS  int
	DuckGain        float64
	DuckFadeMS      int
	// CaptureDSP applies the bounded framework capture cleanup to application
	// capture, wakeword inference and enrollment. The zero value is
	// deliberately conservative because the Rockchip capture source (`speex`)
	// is already processed by the vendor firmware.
	CaptureDSP CaptureDSPOptions
}

type Started struct {
	StreamID      string
	Format        backend.AudioFormat
	WatchdogMS    uint32
	ProcessingTap string // empty for the physical/raw tap
}

type Chunk struct {
	Sequence      uint64
	FirstFrame    uint64
	MonotonicNS   uint64
	Frames        uint32
	PCM           []byte
	Discontinuity bool
	DroppedFrames uint64
	// ProcessingTap identifies the producer of PCM. Empty means the physical
	// (vendor-processed `speex`) capture is passed through unchanged.
	ProcessingTap string
}

type Notice struct{ Code, Detail string }

type CaptureEvent struct {
	Started *Started
	Chunk   *Chunk
	Notice  *Notice
}

type Progress struct {
	FramesPlayed uint64
	FramesQueued uint64
}

type PlaybackEvent struct {
	Started  *Started
	Progress *Progress
	Notice   *Notice
}

type Manager struct {
	ctx    context.Context
	cancel context.CancelFunc
	clock  *ids.Clock
	logs   *logbus.Bus
	dev    backend.AudioDevice
	opts   Options

	mu            sync.Mutex
	started       bool
	volume        float64
	muted         bool
	hardwareMute  bool
	playbacks     map[string]*Playback
	captures      map[string]*capture
	conversations map[string]conversation
	preroll       []prerollEntry
	sink          backend.Sink
	source        backend.Source
	lastProgress  uint64
	frameSinks    map[int]chan Frame
	nextFrameSink int
	dspEnabled    bool
	resetDSP      bool
	// cueID is the playback stream that owns the microphone indication. While
	// it is set, only that stream is mixed; application playbacks cannot mask
	// or delay the audible indication with concurrent audio.
	cueID string
	// Retained terminal drop snapshot: enough for a caller that samples after
	// EndCapture without an unbounded per-app history map.
	closedDropApp   string
	closedDropTotal uint64
}

// prerollEntry keeps the bounded pre-activation history in both provenances:
// app captures request the processed tap, while an explicit raw capture and
// compatibility diagnostics can still see the physical stream.
type prerollEntry struct {
	raw       Chunk
	processed *Chunk
}

type conversation struct {
	appID string
	runID string
}

func NewManager(parent context.Context, clock *ids.Clock, logs *logbus.Bus, dev backend.AudioDevice, opts Options) *Manager {
	ctx, cancel := context.WithCancel(parent)
	if opts.DuckGain <= 0 || opts.DuckGain >= 1 {
		opts.DuckGain = 0.25
	}
	if opts.DuckFadeMS <= 0 {
		opts.DuckFadeMS = 150
	}
	if opts.WatchdogGraceMS <= 0 {
		opts.WatchdogGraceMS = 5000
	}
	if opts.WatchdogPollMS <= 0 {
		opts.WatchdogPollMS = 1000
	}
	return &Manager{
		ctx: ctx, cancel: cancel, clock: clock, logs: logs, dev: dev, opts: opts,
		volume: 0.6, playbacks: map[string]*Playback{}, captures: map[string]*capture{},
		conversations: map[string]conversation{},
		dspEnabled:    !opts.CaptureDSP.Disabled && captureDSPSupported(dev.CaptureFormat()),
	}
}

// Frame is one chunk of the declared capture stream shared with framework
// consumers (the wakeword detector and enrollment recorder), so they observe
// exactly the samples detection runs on without opening a second microphone.
// When the framework cleanup is active the PCM is its output and ProcessingTap
// is CaptureProcessingTap; when the profile is disabled or the backend format
// is unsupported the PCM is the physical vendor `speex` tap and the tap is
// empty. Consumers must validate ProcessingTap rather than assume either.
type Frame struct {
	PCM           []byte
	RateHz        int
	Sequence      uint64 // capture chunk ID; a gap means the subscriber lost audio
	Discontinuity bool   // capture resumed after a source read error
	ProcessingTap string // CaptureProcessingTap, or empty for the physical raw tap
}

// SubscribeFrames registers a framework consumer on the declared capture
// stream. Frames carry the same processed samples application capture and
// wakeword inference use, with their provenance in ProcessingTap. They are
// delivered with a bounded queue and dropped when the consumer falls behind:
// the audio path must never block on analysis. The explicit raw endpoint stays
// available to application capture through RawCaptureEndpoint.
func (m *Manager) SubscribeFrames(buffer int) (<-chan Frame, func()) {
	if buffer <= 0 {
		buffer = 8
	}
	ch := make(chan Frame, buffer)
	m.mu.Lock()
	id := m.nextFrameSink
	m.nextFrameSink++
	if m.frameSinks == nil {
		m.frameSinks = map[int]chan Frame{}
	}
	m.frameSinks[id] = ch
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		if _, ok := m.frameSinks[id]; ok {
			delete(m.frameSinks, id)
			close(ch)
		}
		m.mu.Unlock()
	}
}

func (m *Manager) Start() error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return nil
	}
	sink, err := m.dev.OpenOutput()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	source, err := m.dev.OpenInput()
	if err != nil {
		sink.Close()
		m.mu.Unlock()
		return err
	}
	m.sink, m.source, m.started = sink, source, true
	m.resetDSP = true
	muted := m.muted
	m.mu.Unlock()

	// The mixer owns the only software master gain. Pin the backend/sink gain
	// at unity on every start so a vendor agent or an external mixer change
	// cannot rescale framework output behind the master ceiling. Backends
	// implement this asynchronously and boundedly; a missing mixer client
	// degrades to the previous state instead of failing startup.
	if err := m.dev.SetMasterVolume(1); err != nil {
		m.logs.Warnf("audio", "", "startup sink gain pin incomplete: %v", err)
	}

	// Reconcile the hardware gate on every start: the previous owner (vendor
	// agent or an earlier daemon) may have left it in the opposite state, and
	// SetMuted would skip a matching transition.
	if hw, err := m.dev.SetHardwareMute(muted); err != nil {
		m.logs.Warnf("audio", "", "startup hardware mute (muted=%v) incomplete: %v", muted, err)
	} else {
		m.mu.Lock()
		m.hardwareMute = hw
		m.mu.Unlock()
	}

	go m.mixLoop()
	go m.captureLoop()
	return nil
}

func (m *Manager) Close() {
	m.cancel()
	m.mu.Lock()
	playbacks := make([]*Playback, 0, len(m.playbacks))
	for _, p := range m.playbacks {
		playbacks = append(playbacks, p)
	}
	captures := make([]*capture, 0, len(m.captures))
	for _, c := range m.captures {
		captures = append(captures, c)
	}
	sink, source := m.sink, m.source
	m.mu.Unlock()
	for _, p := range playbacks {
		p.close(Notice{Code: "DAEMON_SHUTDOWN", Detail: "audio service stopping"})
	}
	for _, c := range captures {
		c.close(Notice{Code: "DAEMON_SHUTDOWN", Detail: "audio service stopping"})
	}
	if sink != nil {
		sink.Close()
	}
	if source != nil {
		source.Close()
	}
}

func (m *Manager) SetMasterVolume(v float64) {
	v = clamp01(v)
	m.mu.Lock()
	m.volume = v
	m.mu.Unlock()
	if err := m.dev.SetMasterVolume(1); err != nil {
		m.logs.Warnf("audio", "", "sink gain pin incomplete: %v", err)
	}
}

// BeginIndicationCue makes playback id the exclusive mixer source for the
// microphone indication. Other playbacks are muted, not stopped, so the cue
// cannot be masked by an application flooding the mixer.
func (m *Manager) BeginIndicationCue(id string) {
	if id == "" {
		return
	}
	m.mu.Lock()
	m.cueID = id
	m.mu.Unlock()
}

// EndIndicationCue releases exclusivity only if id still owns it, so a stale
// completion cannot unmute a newer cue.
func (m *Manager) EndIndicationCue(id string) {
	m.mu.Lock()
	if m.cueID == id {
		m.cueID = ""
	}
	m.mu.Unlock()
}

func (m *Manager) MasterVolume() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.volume
}

// SetMuted applies the global microphone mute. Existing app captures end with
// an explicit notice; the mute state itself persists until explicitly cleared.
func (m *Manager) SetMuted(muted bool) {
	m.mu.Lock()
	if m.muted == muted {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	hw, err := m.dev.SetHardwareMute(muted)
	if err != nil {
		// The GPIO gate and the PulseAudio source mute are independent: losing
		// one silently would leave the microphone in an unexpected state.
		m.logs.Warnf("audio", "", "hardware mute (muted=%v) incomplete: %v", muted, err)
	}
	m.mu.Lock()
	m.muted = muted
	m.hardwareMute = hw
	// Mute boundaries are discontinuities: never hand pre-mute audio or DSP
	// state to a capture opened after unmute.
	m.resetDSP = true
	m.preroll = m.preroll[:0]
	captures := make([]*capture, 0, len(m.captures))
	if muted {
		for _, c := range m.captures {
			captures = append(captures, c)
		}
	}
	m.mu.Unlock()
	for _, c := range captures {
		m.logs.Infof("audio", c.appID, "capture stream=%s ended: global microphone mute", c.id)
		c.close(Notice{Code: "MICROPHONE_MUTED", Detail: "global microphone mute"})
	}
}

func (m *Manager) Muted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.muted
}

func (m *Manager) HardwareMute() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hardwareMute
}

// --- playback --------------------------------------------------------------

type Playback struct {
	id, appID, runID, conversationID string
	format                           backend.AudioFormat
	events                           chan PlaybackEvent
	in                               chan []byte
	ctl                              chan string
	done                             chan struct{}
	conv                             *resampler

	mgr          *Manager
	feedFrames   int
	mu           sync.Mutex
	closed       bool
	queuedBytes  int
	framesPlayed uint64
	gain         float64
	trim         float64
	paused       bool
	inputClosed  bool
}

func (p *Playback) ID() string                   { return p.id }
func (p *Playback) Events() <-chan PlaybackEvent { return p.events }
func (p *Playback) AppID() string                { return p.appID }

// Close stops playback immediately and discards queued audio. CloseInput,
// unlike Close, lets a successfully completed stream drain its queued tail.
func (p *Playback) Close() { p.mgr.closePlayback(p.id) }

func (p *Playback) emit(e PlaybackEvent) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	select {
	case p.events <- e:
		return true
	default:
		return false
	}
}

func (p *Playback) close(n Notice) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	select {
	case p.events <- PlaybackEvent{Notice: &n}:
	default:
	}
	close(p.events)
	p.mu.Unlock()
	close(p.done)
}

// Write queues PCM with real backpressure: a stalled client blocks here rather
// than growing daemon memory without bound.
func (p *Playback) Write(pcm []byte) error {
	if len(pcm) > maxPlaybackItem {
		return fmt.Errorf("playback chunk exceeds %d bytes", maxPlaybackItem)
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return fmt.Errorf("playback stream is closed")
	}
	// Account before publishing: the mixer may consume immediately after the
	// send, before this goroutine is scheduled again.
	p.queuedBytes += len(pcm)
	p.mu.Unlock()
	select {
	case <-p.done:
		p.mu.Lock()
		p.queuedBytes -= len(pcm)
		p.mu.Unlock()
		return fmt.Errorf("playback stream is closed")
	case p.in <- pcm:
		return nil
	}
}

func (p *Playback) CloseInput() {
	p.mu.Lock()
	p.inputClosed = true
	p.mu.Unlock()
}

// SetTrim applies a stream-specific linear gain on top of the mixer master.
// It is not touched by ducking, so a caller can implement a product volume
// curve for one stream while the master remains the single UI value.
func (p *Playback) SetTrim(v float64) {
	v = clamp01(v)
	p.mu.Lock()
	p.trim = v
	p.mu.Unlock()
}

func (p *Playback) Control(cmd string) {
	select {
	case p.ctl <- cmd:
	default:
	}
}

func (m *Manager) OpenPlayback(appID, runID, endpointID string, format backend.AudioFormat, conversationID string) (*Playback, error) {
	if endpointID != "" && endpointID != "audio.output.speaker" {
		return nil, fmt.Errorf("unknown playback endpoint %q", endpointID)
	}
	if format.Format != backend.S16LE && format.Format != backend.F32LE {
		return nil, fmt.Errorf("unsupported sample format %s", format.Format)
	}
	if format.RateHz <= 0 || format.Channels <= 0 {
		format = m.dev.PlaybackFormat()
	}
	p := &Playback{
		id: ids.New("pbs"), appID: appID, runID: runID, conversationID: conversationID,
		format:     format,
		feedFrames: (format.RateHz*mixFrameMS+999)/1000 + 2,
		events:     make(chan PlaybackEvent, 16), in: make(chan []byte, playbackQueue),
		ctl: make(chan string, 4), done: make(chan struct{}),
		conv: newResampler(format, outFmt()), gain: 1, trim: 1, mgr: m,
	}
	m.mu.Lock()
	open := 0
	for _, existing := range m.playbacks {
		if existing.appID == appID {
			open++
		}
	}
	if open >= maxPlaybackStreamsPerApp {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w (%d per app)", ErrPlaybackLimit, maxPlaybackStreamsPerApp)
	}
	m.playbacks[p.id] = p
	m.mu.Unlock()
	p.emit(PlaybackEvent{Started: &Started{StreamID: p.id, Format: outFmt()}})
	m.logs.Infof("audio", appID, "playback stream=%s opened input=%s/%d/%d conversation=%s",
		p.id, format.Format, format.RateHz, format.Channels, conversationID)
	return p, nil
}

func (m *Manager) closePlayback(id string) {
	m.mu.Lock()
	p := m.playbacks[id]
	delete(m.playbacks, id)
	m.mu.Unlock()
	if p != nil {
		p.close(Notice{Code: "STREAM_CLOSED", Detail: "playback finished"})
		m.logs.Infof("audio", p.appID, "playback stream=%s closed", id)
	}
}

func outFmt() backend.AudioFormat {
	return backend.AudioFormat{Format: backend.S16LE, RateHz: outRate, Channels: outChannels}
}

// masterLimiter is the soft limiter on the master bus. It is one small value
// state machine rather than inline locals so its contract is testable without a
// live mixer: a rising peak is caught immediately and held for the attack
// window, and the gain releases at the release rate in both directions.
//
// The gain never exceeds limiterThreshold/heldPeak, which is the largest gain
// whose product with the held peak stays at the threshold, so releasing upward
// cannot push the bus above the threshold. A lower gain latched by an earlier
// transient is therefore released toward the current target instead of sticking
// until the signal falls below the threshold entirely.
type masterLimiter struct {
	gain     float32
	holdPeak float32
	holdLeft int
	attack   int
	release  float32
}

func newMasterLimiter(rate int) masterLimiter {
	return masterLimiter{
		gain:    1,
		attack:  limiterAttackMS * rate / 1000,
		release: float32(1 - math.Exp(-math.Ln10/(float64(limiterReleaseMS)*float64(rate)/1000))),
	}
}

// next advances one output frame by its peak magnitude and returns the gain to
// apply to both channels of that frame. Allocation-free and O(1).
func (l *masterLimiter) next(peak float32) float32 {
	if peak >= l.holdPeak {
		l.holdPeak = peak
		l.holdLeft = l.attack
	} else if l.holdLeft > 0 {
		l.holdLeft--
	} else {
		l.holdPeak = peak
	}
	if l.holdPeak > limiterThreshold {
		target := float32(limiterThreshold) / l.holdPeak
		switch {
		case target < l.gain:
			// An immediate catch cannot overshoot: the held peak keeps the
			// reduced gain engaged for the whole attack window.
			l.gain = target
		case l.gain < target:
			l.gain += (target - l.gain) * l.release
			if target-l.gain < 1e-3 {
				l.gain = target
			}
		}
	} else if l.gain < 1 {
		l.gain += (1 - l.gain) * l.release
		if 1-l.gain < 1e-3 {
			l.gain = 1
		}
	}
	return l.gain
}

func (m *Manager) mixLoop() {
	frames := outRate * mixFrameMS / 1000
	ticker := time.NewTicker(time.Duration(mixFrameMS) * time.Millisecond)
	defer ticker.Stop()
	pcm := make([]byte, frames*outChannels*2)
	mix := make([]float32, frames*outChannels)
	// Per-app accumulation scratch: each app's streams are summed here and the
	// sum is clamped before it reaches the master mix, so the master volume is
	// a ceiling on every app's contribution rather than a gain applied after
	// all apps have already added themselves into one buffer.
	appMix := make([]float32, frames*outChannels)
	fadeStep := float64(mixFrameMS) / float64(m.opts.DuckFadeMS)
	lim := newMasterLimiter(outRate)
	// Reused across ticks: the steady-state mixer path allocates nothing.
	playbacks := make([]*Playback, 0, 16)
	apps := make([]string, 0, 8)
	convs := make([]string, 0, 4)
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
		}
		for i := range mix {
			mix[i] = 0
		}
		m.mu.Lock()
		playbacks = playbacks[:0]
		for _, p := range m.playbacks {
			playbacks = append(playbacks, p)
		}
		volume := m.volume
		cueID := m.cueID
		anyConversation := len(m.conversations) > 0
		convs = convs[:0]
		for id := range m.conversations {
			convs = append(convs, id)
		}
		sink := m.sink
		m.mu.Unlock()

		apps = apps[:0]
		if cueID != "" {
			// The microphone indication owns the mixer. Only the cue stream's
			// app is grouped, so every other playback is frozen without being
			// fed and cannot mask or delay the cue.
			for _, p := range playbacks {
				if p.id == cueID {
					apps = append(apps, p.appID)
					break
				}
			}
		} else {
			for _, p := range playbacks {
				if indexOf(apps, p.appID) < 0 {
					apps = append(apps, p.appID)
				}
			}
		}

		for _, appID := range apps {
			for i := range appMix {
				appMix[i] = 0
			}
			mixed := false
			for _, p := range playbacks {
				if p.appID != appID {
					continue
				}
				isCue := cueID != "" && p.id == cueID
				if cueID != "" && !isCue {
					continue
				}
				p.drainControl()
				p.feed()
				p.mu.Lock()
				if p.closed {
					p.mu.Unlock()
					continue
				}
				paused := p.paused
				closedInput := p.inputClosed
				queued := p.queuedBytes
				isActiveConv := indexOf(convs, p.conversationID) >= 0
				p.mu.Unlock()

				target := 1.0
				if !isCue && anyConversation && (p.conversationID == "" || !isActiveConv) {
					target = m.opts.DuckGain
				}
				p.mu.Lock()
				if p.gain < target {
					p.gain = math.Min(target, p.gain+fadeStep)
				} else if p.gain > target {
					p.gain = math.Max(target, p.gain-fadeStep)
				}
				gain := p.gain
				trim := p.trim
				p.mu.Unlock()

				starved := queued == 0 && closedInput
				if starved && p.conv.srcFrames() == 0 {
					m.closePlayback(p.id)
					continue
				}
				if paused {
					continue
				}
				out := p.conv.pull(frames)
				p.mu.Lock()
				p.framesPlayed += uint64(frames)
				p.mu.Unlock()
				scale := float32(gain * trim)
				for i := range appMix {
					appMix[i] += out[i] * scale
				}
				mixed = true
			}
			if !mixed {
				continue
			}
			for i, v := range appMix {
				// One app cannot exceed full scale no matter how many streams
				// it opened; non-finite values cannot leave the app bus.
				switch {
				case v != v:
					v = 0
				case v > 1:
					v = 1
				case v < -1:
					v = -1
				}
				mix[i] += v
			}
		}

		vol := float32(volume)
		for i := 0; i < len(mix); i += 2 {
			l := finite32(mix[i] * vol)
			r := finite32(mix[i+1] * vol)
			// The master volume is applied once, here, after every app has
			// contributed at most its ceiling.
			peak := abs32(l)
			if a := abs32(r); a > peak {
				peak = a
			}
			gain := lim.next(peak)
			l *= gain
			r *= gain
			// Existing full-scale hard clamp, retained as the final safety net.
			// The limiter holds the bus at its threshold, so it should be a
			// no-op; non-finite samples were already replaced with silence.
			if l > 1 {
				l = 1
			} else if l < -1 {
				l = -1
			}
			if r > 1 {
				r = 1
			} else if r < -1 {
				r = -1
			}
			binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(l*32767)))
			binary.LittleEndian.PutUint16(pcm[(i+1)*2:], uint16(int16(r*32767)))
		}
		if sink != nil {
			if err := sink.WritePCM(pcm); err != nil {
				m.logs.Warnf("audio", "", "hardware playback write failed: %v", err)
			}
		}

		now := m.clock.NowNS()
		if now-m.lastProgress >= uint64(progressPeriod) {
			m.lastProgress = now
			for _, p := range playbacks {
				p.mu.Lock()
				prog := Progress{FramesPlayed: p.framesPlayed, FramesQueued: uint64(p.queuedBytes / p.format.BytesPerFrame())}
				p.mu.Unlock()
				p.emit(PlaybackEvent{Progress: &prog})
			}
		}
	}
}

// indexOf reports the position of want in list, or -1. The lists it serves are
// the per-tick app and conversation snapshots: a handful of short strings, so a
// linear scan avoids a per-tick map allocation.
func indexOf(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
}

func abs32(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

// finite32 maps NaN and infinities to silence. A non-finite sample must not
// reach the int16 conversion, where the result is implementation-defined and
// audible as a click at any volume.
func finite32(v float32) float32 {
	if v != v || math.IsInf(float64(v), 0) {
		return 0
	}
	return v
}

func (p *Playback) drainControl() {
	for {
		select {
		case cmd := <-p.ctl:
			switch cmd {
			case "FLUSH":
				discarded := 0
			drain:
				for {
					select {
					case chunk := <-p.in:
						discarded += len(chunk)
					default:
						break drain
					}
				}
				p.mu.Lock()
				p.queuedBytes -= discarded
				p.mu.Unlock()
				p.conv.reset()
			case "PAUSE":
				p.mu.Lock()
				p.paused = true
				p.mu.Unlock()
			case "RESUME":
				p.mu.Lock()
				p.paused = false
				p.mu.Unlock()
			}
		default:
			return
		}
	}
}

// feed moves only the next mixer quantum into the resampler. Draining the
// entire channel on every tick would turn the resampler into an unbounded
// second queue and defeat playback backpressure, especially while paused.
func (p *Playback) feed() {
	for p.conv.srcFrames() < p.feedFrames {
		select {
		case chunk := <-p.in:
			p.mu.Lock()
			p.queuedBytes -= len(chunk)
			if p.queuedBytes < 0 {
				p.queuedBytes = 0
			}
			p.mu.Unlock()
			p.conv.feed(chunk)
		default:
			return
		}
	}
}

// --- capture ---------------------------------------------------------------

type capture struct {
	id, appID, runID, endpoint, activationID, conversationID string
	format                                                   backend.AudioFormat
	processed                                                bool
	mgr                                                      *Manager
	events                                                   chan CaptureEvent
	done                                                     chan struct{}

	mu             sync.Mutex
	closed         bool
	droppedFrames  uint64 // pending count, reset when the next chunk is delivered
	dropTotal      uint64 // cumulative for diagnostics/tests, never reset
	pendingDiscont bool
	lastQualifying uint64
}

func (c *capture) Events() <-chan CaptureEvent { return c.events }
func (c *capture) ID() string                  { return c.id }

func (c *capture) emit(e CaptureEvent) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	select {
	case c.events <- e:
		return true
	default:
		return false
	}
}

func (c *capture) close(n Notice) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	select {
	case c.events <- CaptureEvent{Notice: &n}:
	default:
	}
	close(c.events)
	c.mu.Unlock()
	close(c.done)
}

func (m *Manager) StartCapture(appID, runID, endpointID string, _ backend.AudioFormat, _ uint32, activationID, conversationID string) (*capture, error) {
	endpoint := CaptureEndpoint
	processed := true
	switch endpointID {
	case "", CaptureEndpoint:
	case RawCaptureEndpoint:
		endpoint, processed = RawCaptureEndpoint, false
	default:
		return nil, fmt.Errorf("unknown capture endpoint %q", endpointID)
	}
	m.mu.Lock()
	if m.muted {
		m.mu.Unlock()
		return nil, fmt.Errorf("microphone is globally muted")
	}
	if processed && !m.dspEnabled {
		processed = false
	}
	m.mu.Unlock()
	// A supervised application may only receive microphone audio while a
	// selected, visible, non-blank presentation and an audible framework cue
	// both prove the microphone is live. Checks run without the manager lock:
	// the visible hook reads routing state and the cue hook opens a playback
	// stream through this same manager.
	if appID != "" {
		if m.opts.IndicationActive == nil || m.opts.IndicationCue == nil {
			return nil, fmt.Errorf("microphone capture requires visible and audible indication")
		}
		if !m.opts.IndicationActive(appID) {
			return nil, fmt.Errorf("microphone capture requires a selected visible ring or segment indication")
		}
		if err := m.opts.IndicationCue(appID); err != nil {
			return nil, fmt.Errorf("microphone indication is not audible: %w", err)
		}
		// The cue takes time to drain; the presentation must still be visible
		// when capture actually starts.
		if !m.opts.IndicationActive(appID) {
			return nil, fmt.Errorf("microphone capture requires a selected visible ring or segment indication")
		}
	}
	format := m.dev.CaptureFormat()
	tap := CaptureProcessingTap
	if !processed {
		tap = ""
	}
	c := &capture{
		id: ids.New("mic"), appID: appID, runID: runID, endpoint: endpoint,
		processed:    processed,
		activationID: activationID, conversationID: conversationID, format: format,
		mgr: m, events: make(chan CaptureEvent, captureQueue), done: make(chan struct{}),
		// The initial grace period runs from the start, so a capture that never
		// shows an indication is still terminated on time. Frames are withheld
		// whenever an indication is absent, so the grace cannot leak audio.
		lastQualifying: m.clock.NowNS(),
	}
	c.emit(CaptureEvent{Started: &Started{StreamID: c.id, Format: format,
		WatchdogMS: uint32(m.opts.WatchdogGraceMS), ProcessingTap: tap}})
	if activationID != "" {
		m.mu.Lock()
		pre := make([]prerollEntry, len(m.preroll))
		copy(pre, m.preroll)
		m.mu.Unlock()
		frameBytes := format.BytesPerFrame()
		var lost uint64
		for _, entry := range pre {
			ch := entry.raw
			if c.processed && entry.processed != nil {
				ch = *entry.processed
			}
			cp := ch
			if !c.emit(CaptureEvent{Chunk: &cp}) {
				if frameBytes > 0 {
					lost += uint64(len(ch.PCM) / frameBytes)
				}
				continue
			}
		}
		if lost > 0 {
			// Pre-roll never blocks the caller, so a saturated queue is
			// disclosed on the next live chunk instead of pretending the full
			// history was attached.
			c.mu.Lock()
			c.droppedFrames += lost
			c.dropTotal += lost
			c.pendingDiscont = true
			c.mu.Unlock()
			m.logs.Warnf("audio", appID, "capture stream=%s pre-roll truncated: %d frames not queued", c.id, lost)
		}
		m.logs.Infof("audio", appID, "capture stream=%s attached %d pre-roll chunks for activation=%s tap=%s",
			c.id, len(pre), activationID, tapName(tap))
	}
	m.mu.Lock()
	m.captures[c.id] = c
	m.mu.Unlock()
	m.logs.Infof("audio", appID, "capture stream=%s opened endpoint=%s tap=%s conversation=%s",
		c.id, c.endpoint, tapName(tap), conversationID)
	go m.watchdog(c)
	return c, nil
}

func tapName(tap string) string {
	if tap == "" {
		return "raw"
	}
	return tap
}

func (m *Manager) StopCapture(streamID string) bool {
	m.mu.Lock()
	c := m.captures[streamID]
	delete(m.captures, streamID)
	m.retireCaptureLocked(c)
	m.mu.Unlock()
	if c == nil {
		return false
	}
	c.close(Notice{Code: "STREAM_CLOSED", Detail: "capture finished"})
	return true
}

func (m *Manager) watchdog(c *capture) {
	if c.appID == "" || m.opts.IndicationActive == nil {
		return
	}
	// Poll from the start: the initial grace is expressed by lastQualifying
	// being seeded with the capture start, so a capture that never shows an
	// indication still terminates on time, and one that shows it and loses it
	// gets the full grace measured from the last qualifying poll.
	ticker := time.NewTicker(time.Duration(m.opts.WatchdogPollMS) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			now := m.clock.NowNS()
			if m.opts.IndicationActive(c.appID) {
				c.mu.Lock()
				c.lastQualifying = now
				c.mu.Unlock()
				continue
			}
			c.mu.Lock()
			last := c.lastQualifying
			dropped := c.droppedFrames
			c.mu.Unlock()
			// Loss of the indication is tolerated for the grace period, measured
			// from the last qualifying frame, not from capture start.
			if now-last < uint64(m.opts.WatchdogGraceMS)*uint64(time.Millisecond)/uint64(time.Nanosecond) {
				continue
			}
			since := "never"
			if last > 0 {
				since = fmt.Sprintf("%.1fs", float64(m.clock.NowNS()-last)/1e9)
			}
			m.logs.Warnf("audio", c.appID,
				"microphone watchdog expired stream=%s: no qualifying visible indication (last=%s dropped_frames=%d)",
				c.id, since, dropped)
			m.mu.Lock()
			delete(m.captures, c.id)
			m.retireCaptureLocked(c)
			m.mu.Unlock()
			c.close(Notice{Code: "WATCHDOG_EXPIRED",
				Detail: "no qualifying visible ring/segment indication; capture terminated"})
			return
		}
	}
}

func (m *Manager) captureLoop() {
	format := m.dev.CaptureFormat()
	frames := format.RateHz * 20 / 1000
	if frames < 1 {
		frames = 320
	}
	buf := make([]byte, frames*format.BytesPerFrame())
	proc := newCaptureProcessor(format, m.opts.CaptureDSP)
	seq := uint64(0)
	firstFrame := uint64(0)
	discontinuity := false
	for {
		select {
		case <-m.ctx.Done():
			return
		default:
		}
		if m.consumeDSPReset() && proc != nil {
			proc.reset()
		}
		n, err := m.source.ReadPCM(buf)
		if err != nil {
			discontinuity = true
			if proc != nil {
				proc.reset()
			}
			m.mu.Lock()
			m.preroll = m.preroll[:0]
			m.mu.Unlock()
			m.logs.Warnf("audio", "", "capture read failed: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if n <= 0 {
			continue
		}
		seq++
		frameCount := uint32(n / format.BytesPerFrame())
		now := m.clock.NowNS()
		chunk := Chunk{Sequence: seq, FirstFrame: firstFrame,
			MonotonicNS: now, Frames: frameCount, PCM: append([]byte(nil), buf[:n]...),
			Discontinuity: discontinuity}
		// The processed tap is a separate bounded copy: the raw chunk remains
		// available to an explicit raw application capture, while framework
		// frame subscribers receive the processed frame below.
		var processed *Chunk
		if proc != nil {
			pcm := make([]byte, n)
			proc.process(pcm, buf[:n])
			p := chunk
			p.PCM = pcm
			p.ProcessingTap = CaptureProcessingTap
			processed = &p
		}
		firstFrame += uint64(frameCount)
		discontinuity = false
		// Dispatch under the same lock as cancellation, so a subscriber cannot
		// close its channel between a snapshot and the nonblocking send. A
		// subscriber observes the same processed samples as application
		// capture and wakeword inference; the raw chunk is not offered here.
		var frame Frame
		if processed != nil {
			frame = Frame{PCM: processed.PCM, RateHz: format.RateHz, Sequence: seq,
				Discontinuity: processed.Discontinuity, ProcessingTap: CaptureProcessingTap}
		} else {
			frame = Frame{PCM: chunk.PCM, RateHz: format.RateHz, Sequence: seq,
				Discontinuity: chunk.Discontinuity}
		}
		m.mu.Lock()
		for _, ch := range m.frameSinks {
			select {
			case ch <- frame:
			default: // consumer is behind; sequence gap exposes the dropped audio
			}
		}
		m.addPrerollLocked(chunk, processed)
		volume := m.volume
		muted := m.muted
		subs := make([]*capture, 0, len(m.captures))
		for _, c := range m.captures {
			subs = append(subs, c)
		}
		m.mu.Unlock()
		for _, c := range subs {
			deliver := true
			if c.appID != "" {
				// Both halves are re-evaluated for every chunk: zero volume or
				// a lost visible indication withholds audio immediately rather
				// than waiting for the termination watchdog.
				deliver = !muted && volume > 0 && m.opts.IndicationActive != nil && m.opts.IndicationActive(c.appID)
			}
			c.mu.Lock()
			delivering := deliver
			if !delivering && !c.closed {
				// Count the withheld audio as dropped so the app sees a
				// discontinuity instead of a silent gap, and so the watchdog's
				// evidence matches what was actually delivered.
				c.droppedFrames += uint64(frameCount)
				c.dropTotal += uint64(frameCount)
				c.pendingDiscont = true
			}
			discon := c.pendingDiscont
			dropped := c.droppedFrames
			if delivering {
				c.pendingDiscont = false
				c.droppedFrames = 0
			}
			c.mu.Unlock()
			if !delivering {
				continue
			}
			out := chunk
			if c.processed && processed != nil {
				out = *processed
			}
			out.Discontinuity = out.Discontinuity || discon
			out.DroppedFrames = dropped
			if !c.emit(CaptureEvent{Chunk: &out}) {
				c.mu.Lock()
				if !c.closed {
					c.droppedFrames += uint64(frameCount)
					c.dropTotal += uint64(frameCount)
					c.pendingDiscont = true
				}
				c.mu.Unlock()
				c.emit(CaptureEvent{Notice: &Notice{Code: "CLIENT_TOO_SLOW", Detail: "audio consumer is not draining chunks"}})
			}
		}
	}
}

func (m *Manager) consumeDSPReset() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	reset := m.resetDSP
	m.resetDSP = false
	return reset
}

func (m *Manager) addPrerollLocked(c Chunk, processed *Chunk) {
	m.preroll = append(m.preroll, prerollEntry{raw: c, processed: processed})
	limit := prerollMS * m.dev.CaptureFormat().RateHz / 1000 * m.dev.CaptureFormat().BytesPerFrame()
	total := 0
	for _, item := range m.preroll {
		total += len(item.raw.PCM)
	}
	for total > limit && len(m.preroll) > 1 {
		total -= len(m.preroll[0].raw.PCM)
		m.preroll = m.preroll[1:]
	}
}

// BeginConversation marks a ducking lifetime.
func (m *Manager) BeginConversation(conversationID, appID, runID string) {
	m.mu.Lock()
	m.conversations[conversationID] = conversation{appID: appID, runID: runID}
	m.mu.Unlock()
	m.logs.Infof("audio", appID, "conversation=%s begun; other playback is ducked", conversationID)
}

func (m *Manager) EndConversation(conversationID string) {
	m.mu.Lock()
	if conv, ok := m.conversations[conversationID]; ok {
		delete(m.conversations, conversationID)
		m.logs.Infof("audio", conv.appID, "conversation=%s ended", conversationID)
	}
	m.mu.Unlock()
}

// RemoveRun closes playback and capture owned by a terminated process.
func (m *Manager) RemoveRun(runID string) {
	m.mu.Lock()
	var playbackIDs []string
	for id, p := range m.playbacks {
		if p.runID == runID {
			playbackIDs = append(playbackIDs, id)
		}
	}
	var caps []*capture
	for id, c := range m.captures {
		if c.runID == runID {
			caps = append(caps, c)
			delete(m.captures, id)
			m.retireCaptureLocked(c)
		}
	}
	for id, conv := range m.conversations {
		if conv.runID == runID {
			delete(m.conversations, id)
		}
	}
	m.mu.Unlock()
	for _, id := range playbackIDs {
		m.closePlayback(id)
	}
	for _, c := range caps {
		c.close(Notice{Code: "APP_EXITED", Detail: "owning process exited"})
	}
}

type StreamInfo struct {
	StreamID   string
	AppID      string
	EndpointID string
	Input      bool
	OpenedNS   uint64
}

func (m *Manager) ActiveStreams() []StreamInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]StreamInfo, 0, len(m.playbacks)+len(m.captures))
	for _, p := range m.playbacks {
		out = append(out, StreamInfo{StreamID: p.id, AppID: p.appID, EndpointID: "audio.output.speaker"})
	}
	for _, c := range m.captures {
		out = append(out, StreamInfo{StreamID: c.id, AppID: c.appID, EndpointID: c.endpoint, Input: true})
	}
	return out
}

// retireCaptureLocked records the last removed capture's cumulative drop count
// so a caller sampling after EndCapture/removal does not see zero. m.mu must be
// held. The snapshot is manager-global and bounded to one entry: it holds the
// most recently removed capture, whatever its app. An intervening removal can
// overwrite it; it is not a per-app history.
func (m *Manager) retireCaptureLocked(c *capture) {
	if c == nil {
		return
	}
	c.mu.Lock()
	total := c.dropTotal
	c.mu.Unlock()
	m.closedDropApp, m.closedDropTotal = c.appID, total
}

// CaptureDroppedFrames reports cumulative frames dropped by appID's active
// capture(s) because the consumer did not drain its queue. If no matching
// capture is active, it reports the retained snapshot of the most recently
// removed capture, and only when that capture belonged to appID. The snapshot
// is manager-global and one entry, so an intervening other-app removal can
// overwrite it. It is a read-only diagnostic/test accessor, not a wire surface.
// It is not the pending count that resets when a later chunk is delivered, and
// it does not become zero merely because EndCapture removed the stream.
func (m *Manager) CaptureDroppedFrames(appID string) uint64 {
	m.mu.Lock()
	caps := make([]*capture, 0, len(m.captures))
	for _, c := range m.captures {
		if c.appID == appID {
			caps = append(caps, c)
		}
	}
	closedApp, closedTotal := m.closedDropApp, m.closedDropTotal
	m.mu.Unlock()
	if len(caps) > 0 {
		var total uint64
		for _, c := range caps {
			c.mu.Lock()
			total += c.dropTotal
			c.mu.Unlock()
		}
		return total
	}
	if closedApp == appID {
		return closedTotal
	}
	return 0
}

func (m *Manager) CaptureFormat() backend.AudioFormat  { return m.dev.CaptureFormat() }
func (m *Manager) PlaybackFormat() backend.AudioFormat { return m.dev.PlaybackFormat() }

// CaptureProcessingTap reports the processing tap an application capture
// actually receives on CaptureEndpoint. An empty result means the physical
// vendor stream: DSP is disabled or the backend format is not the tuned
// 16 kHz mono S16LE profile. Endpoint advertisement must use this value
// instead of assuming processing is active.
func (m *Manager) CaptureProcessingTap() string {
	if m.dspEnabled {
		return CaptureProcessingTap
	}
	return ""
}

// FrameProcessingTap reports the provenance SubscribeFrames delivers. It is
// the same chain as CaptureProcessingTap because wakeword inference, enrollment
// and application capture must share one declared preprocessing policy.
func (m *Manager) FrameProcessingTap() string { return m.CaptureProcessingTap() }

// ProcessCaptureS16 applies this manager's configured capture cleanup to a
// complete recording, starting from reset state. Enrollment uses it to convert
// legacy raw vendor-tap takes onto the declared runtime profile without
// rewriting the file. The returned tap is empty when the configured profile is
// disabled or unsupported and the input was copied unchanged.
func (m *Manager) ProcessCaptureS16(samples []int16) ([]int16, string) {
	return ProcessCaptureS16(samples, m.opts.CaptureDSP)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
