// Package audio implements the framework-owned PCM mixer, capture fan-out,
// automatic ducking and the app microphone watchdog.
//
// One physical source is captured once and fanned out into bounded per-client
// queues; slow consumers receive discontinuity information instead of stalling
// hardware capture. Playback streams are independent and mixed at 48 kHz
// stereo S16LE.
package audio

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

const (
	outRate         = 48000
	outChannels     = 2
	mixFrameMS      = 10
	playbackQueue   = 32
	captureQueue    = 16
	progressPeriod  = 250 * time.Millisecond
	prerollMS       = 1000
	maxPlaybackItem = 1 << 20
)

type Options struct {
	// IndicationActive reports whether an app currently shows a qualifying
	// visible ring/segment indication.
	IndicationActive func(appID string) bool
	WatchdogGraceMS  int
	WatchdogPollMS   int
	DuckGain         float64
	DuckFadeMS       int
}

type Started struct {
	StreamID   string
	Format     backend.AudioFormat
	WatchdogMS uint32
}

type Chunk struct {
	Sequence      uint64
	FirstFrame    uint64
	MonotonicNS   uint64
	Frames        uint32
	PCM           []byte
	Discontinuity bool
	DroppedFrames uint64
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
	preroll       []Chunk
	sink          backend.Sink
	source        backend.Source
	lastProgress  uint64
	frameSinks    map[int]chan Frame
	nextFrameSink int
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
	}
}

// Frame is one chunk of the processed capture stream. It is shared with
// framework consumers (the wakeword detector) so they observe exactly what an
// application capture receives without opening a second microphone.
type Frame struct {
	PCM           []byte
	RateHz        int
	Sequence      uint64 // capture chunk ID; a gap means the subscriber lost audio
	Discontinuity bool   // capture resumed after a source read error
}

// SubscribeFrames registers a framework consumer on the capture stream. Frames
// are delivered with a bounded queue and dropped when the consumer falls
// behind: the audio path must never block on analysis.
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
	muted := m.muted
	m.mu.Unlock()

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
	_ = m.dev.SetMasterVolume(1)
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
	mu           sync.Mutex
	closed       bool
	queuedBytes  int
	framesPlayed uint64
	gain         float64
	paused       bool
	inputClosed  bool
}

func (p *Playback) ID() string                   { return p.id }
func (p *Playback) Events() <-chan PlaybackEvent { return p.events }
func (p *Playback) AppID() string                { return p.appID }

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
	p.mu.Unlock()
	select {
	case <-p.done:
		return fmt.Errorf("playback stream is closed")
	case p.in <- pcm:
		p.mu.Lock()
		p.queuedBytes += len(pcm)
		p.mu.Unlock()
		return nil
	}
}

func (p *Playback) CloseInput() {
	p.mu.Lock()
	p.inputClosed = true
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
		format: format,
		events: make(chan PlaybackEvent, 16), in: make(chan []byte, playbackQueue),
		ctl: make(chan string, 4), done: make(chan struct{}),
		conv: newResampler(format, outFmt()), gain: 1, mgr: m,
	}
	m.mu.Lock()
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

func (m *Manager) mixLoop() {
	frames := outRate * mixFrameMS / 1000
	ticker := time.NewTicker(time.Duration(mixFrameMS) * time.Millisecond)
	defer ticker.Stop()
	pcm := make([]byte, frames*outChannels*2)
	mix := make([]float32, frames*outChannels)
	fadeStep := float64(mixFrameMS) / float64(m.opts.DuckFadeMS)
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
		playbacks := make([]*Playback, 0, len(m.playbacks))
		for _, p := range m.playbacks {
			playbacks = append(playbacks, p)
		}
		volume := m.volume
		anyConversation := len(m.conversations) > 0
		convs := make(map[string]struct{}, len(m.conversations))
		for id := range m.conversations {
			convs[id] = struct{}{}
		}
		sink := m.sink
		m.mu.Unlock()

		for _, p := range playbacks {
			p.drainControl()
			p.feed()
			p.mu.Lock()
			paused := p.paused
			closedInput := p.inputClosed
			queued := p.queuedBytes
			_, isActiveConv := convs[p.conversationID]
			p.mu.Unlock()

			target := 1.0
			if anyConversation && (p.conversationID == "" || !isActiveConv) {
				target = m.opts.DuckGain
			}
			p.mu.Lock()
			if p.gain < target {
				p.gain = math.Min(target, p.gain+fadeStep)
			} else if p.gain > target {
				p.gain = math.Max(target, p.gain-fadeStep)
			}
			gain := p.gain
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
			for i := range mix {
				mix[i] += out[i] * float32(gain)
			}
		}

		for i, v := range mix {
			v *= float32(volume)
			if v > 1 {
				v = 1
			} else if v < -1 {
				v = -1
			}
			binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(v*32767)))
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

func (p *Playback) drainControl() {
	for {
		select {
		case cmd := <-p.ctl:
			switch cmd {
			case "FLUSH":
				for {
					select {
					case <-p.in:
						continue
					default:
					}
					break
				}
				p.mu.Lock()
				p.queuedBytes = 0
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

// feed moves queued chunks into the resampler. Called from the mixer loop.
func (p *Playback) feed() {
	for {
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
	mgr                                                      *Manager
	events                                                   chan CaptureEvent
	done                                                     chan struct{}

	mu             sync.Mutex
	closed         bool
	droppedFrames  uint64
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
	if endpointID != "" && endpointID != "audio.input.microphone" {
		return nil, fmt.Errorf("unknown capture endpoint %q", endpointID)
	}
	m.mu.Lock()
	if m.muted {
		m.mu.Unlock()
		return nil, fmt.Errorf("microphone is globally muted")
	}
	m.mu.Unlock()
	format := m.dev.CaptureFormat()
	c := &capture{
		id: ids.New("mic"), appID: appID, runID: runID, endpoint: "audio.input.microphone",
		activationID: activationID, conversationID: conversationID, format: format,
		mgr: m, events: make(chan CaptureEvent, captureQueue), done: make(chan struct{}),
		// The initial grace period runs from the start, so a capture that never
		// shows an indication is still terminated on time.
		lastQualifying: m.clock.NowNS(),
	}
	c.emit(CaptureEvent{Started: &Started{StreamID: c.id, Format: format, WatchdogMS: uint32(m.opts.WatchdogGraceMS)}})
	if activationID != "" {
		m.mu.Lock()
		pre := make([]Chunk, len(m.preroll))
		copy(pre, m.preroll)
		m.mu.Unlock()
		for _, ch := range pre {
			cp := ch
			c.emit(CaptureEvent{Chunk: &cp})
		}
		m.logs.Infof("audio", appID, "capture stream=%s attached %d pre-roll chunks for activation=%s",
			c.id, len(pre), activationID)
	}
	m.mu.Lock()
	m.captures[c.id] = c
	m.mu.Unlock()
	m.logs.Infof("audio", appID, "capture stream=%s opened endpoint=%s conversation=%s",
		c.id, c.endpoint, conversationID)
	go m.watchdog(c)
	return c, nil
}

func (m *Manager) StopCapture(streamID string) bool {
	m.mu.Lock()
	c := m.captures[streamID]
	delete(m.captures, streamID)
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
	seq := uint64(0)
	firstFrame := uint64(0)
	discontinuity := false
	for {
		select {
		case <-m.ctx.Done():
			return
		default:
		}
		n, err := m.source.ReadPCM(buf)
		if err != nil {
			discontinuity = true
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
		firstFrame += uint64(frameCount)
		discontinuity = false
		// Dispatch under the same lock as cancellation, so a subscriber cannot
		// close its channel between a snapshot and the nonblocking send.
		m.mu.Lock()
		frame := Frame{PCM: chunk.PCM, RateHz: format.RateHz, Sequence: seq,
			Discontinuity: chunk.Discontinuity}
		for _, ch := range m.frameSinks {
			select {
			case ch <- frame:
			default: // consumer is behind; sequence gap exposes the dropped audio
			}
		}
		m.addPrerollLocked(chunk)
		subs := make([]*capture, 0, len(m.captures))
		for _, c := range m.captures {
			subs = append(subs, c)
		}
		m.mu.Unlock()
		for _, c := range subs {
			c.mu.Lock()
			discon := c.pendingDiscont
			dropped := c.droppedFrames
			c.pendingDiscont = false
			c.droppedFrames = 0
			c.mu.Unlock()
			out := chunk
			out.Discontinuity = out.Discontinuity || discon
			out.DroppedFrames = dropped
			if !c.emit(CaptureEvent{Chunk: &out}) {
				c.mu.Lock()
				if !c.closed {
					c.droppedFrames += uint64(frameCount)
					c.pendingDiscont = true
				}
				c.mu.Unlock()
				c.emit(CaptureEvent{Notice: &Notice{Code: "CLIENT_TOO_SLOW", Detail: "audio consumer is not draining chunks"}})
			}
		}
	}
}

func (m *Manager) addPrerollLocked(c Chunk) {
	m.preroll = append(m.preroll, c)
	limit := prerollMS * m.dev.CaptureFormat().RateHz / 1000 * m.dev.CaptureFormat().BytesPerFrame()
	total := 0
	for _, item := range m.preroll {
		total += len(item.PCM)
	}
	for total > limit && len(m.preroll) > 1 {
		total -= len(m.preroll[0].PCM)
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

func (m *Manager) CaptureFormat() backend.AudioFormat  { return m.dev.CaptureFormat() }
func (m *Manager) PlaybackFormat() backend.AudioFormat { return m.dev.PlaybackFormat() }

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
