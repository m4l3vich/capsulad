package assistant

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"capsulad/apps/opencapsula/internal/earcon"
	"capsulad/apps/opencapsula/internal/protocol"
	"capsulad/apps/opencapsula/internal/ring"
	"capsulad/apps/opencapsula/internal/vad"
	"capsulad/sdk/capsulad"
)

const (
	// listenTimeout bounds the first wait for speech. A follow-up uses the
	// configured follow_up_timeout_ms instead.
	listenTimeout = 6 * time.Second
	// responseTimeout bounds how long the server may stay silent after the
	// utterance ended.
	responseTimeout = 30 * time.Second
	// maxTurn bounds a single reply end to end, including playback.
	maxTurn = 120 * time.Second
	// preRollMS is the audio kept before the detected onset.
	preRollMS = 300
	// turnCompleteDrain bounds the graceful playback drain. The writer paces
	// the reply at real time before the half-close, so a long answer must be
	// allowed to finish; a barge-in or link loss cancels it sooner.
	turnCompleteDrain = 120 * time.Second
	// cueTimeout bounds one earcon.
	cueTimeout = 4 * time.Second
)

const (
	inputRate     = 16000
	inputChannels = 1
)

var inputFormat = capsulad.AudioFormat{
	SampleFormat: capsulad.SampleFormatPCM16LE,
	SampleRateHz: inputRate,
	Channels:     inputChannels,
}

var earconFormat = inputFormat

type turnState int32

const (
	turnStateListening turnState = iota
	turnStateThinking
	turnStateSpeaking
	turnStateEnded
)

func (s turnState) String() string {
	switch s {
	case turnStateListening:
		return "listening"
	case turnStateThinking:
		return "thinking"
	case turnStateSpeaking:
		return "speaking"
	default:
		return "ended"
	}
}

type controlKind int

const (
	controlListenEnd controlKind = iota
	controlInterrupted
	controlComplete
	controlError
)

type control struct {
	kind      controlKind
	turnID    string
	serverErr *protocol.Error
}

type turn struct {
	session    *session
	activation *activationItem
	ready      protocol.Ready

	ctx    context.Context
	cancel context.CancelFunc
	state  atomic.Int32

	controls chan control
	audio    chan []byte

	mu             sync.Mutex
	id             string
	conversation   *capsulad.Conversation
	conversationID string
	started        bool
	ended          bool
	completed      bool
	cancelSent     bool
	failed         bool
	replied        bool
	interrupted    bool
	ending         bool
	sink           *playbackSink
	lastDrop       time.Time
}

func newTurn(s *session, activation *activationItem) *turn {
	ctx, cancel := context.WithCancel(s.ctx)
	t := &turn{
		session:    s,
		activation: activation,
		ready:      s.link.Ready(),
		ctx:        ctx,
		cancel:     cancel,
		controls:   make(chan control, 32),
		audio:      make(chan []byte, 256),
	}
	t.state.Store(int32(turnStateListening))
	cfg := s.a.Config()
	t.id = s.a.nextTurnID(cfg)
	return t
}

// ID is the current server turn identifier. It changes at a follow-up.
func (t *turn) ID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.id
}

// State is safe for the session's barge-in decision.
func (t *turn) State() turnState { return turnState(t.state.Load()) }

// Cancel aborts the turn; cleanup sends turn_cancel when a server turn was
// already open.
func (t *turn) Cancel() { t.cancel() }

// SetEnding makes the current reply the last one.
func (t *turn) SetEnding() {
	t.mu.Lock()
	t.ending = true
	t.mu.Unlock()
}

// Failed reports a terminal turn failure for the session's error indication.
func (t *turn) Failed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failed
}

// PushControl routes one server message, filtering a frame that belongs to a
// turn the client already abandoned.
func (t *turn) PushControl(message control) {
	if message.turnID != "" && message.turnID != t.ID() {
		return
	}
	select {
	case t.controls <- message:
	default:
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case t.controls <- message:
		case <-t.ctx.Done():
		case <-timer.C:
			t.session.log.Printf("opencapsula: turn %s control queue full; dropping %d", t.ID(), message.kind)
		}
	}
}

// PushAudio queues one server PCM frame for the reply. The session never
// blocks on a slow playback sink; the queue is bounded and drops are logged.
func (t *turn) PushAudio(pcm []byte) {
	state := t.State()
	if state == turnStateEnded {
		return
	}
	if state == turnStateListening && !t.hasStarted() {
		return
	}
	select {
	case t.audio <- pcm:
	default:
		t.session.log.Printf("opencapsula: turn %s audio queue full; dropping %d bytes", t.ID(), len(pcm))
	}
}

func (t *turn) setState(state turnState) { t.state.Store(int32(state)) }

func (t *turn) run() {
	defer t.cancel()
	defer t.cleanup()
	defer func() {
		if t.Failed() && t.session.a.Config().Earcons && t.ctx.Err() == nil {
			t.playCue(earcon.Error())
		}
	}()
	cfg := t.session.a.Config()

	if err := t.session.a.ring.Show(t.ctx, ring.StateListening); err != nil {
		t.fail("ring unavailable: %v", err)
		return
	}
	t.beginConversation()
	activationID := t.activation.activation.ID
	noSpeech := listenTimeout
	for {
		if !t.listenOnce(activationID, noSpeech) {
			break
		}
		if !t.awaitReply() {
			break
		}
		if !t.followUpAllowed(cfg.FollowUp, cfg.FollowUpTimeout) {
			break
		}
		activationID = ""
		noSpeech = cfg.FollowUpTimeout
		t.beginFollowUp()
		cfg = t.session.a.Config()
	}
}

func (t *turn) beginConversation() {
	conv, err := t.session.a.client.BeginConversation(t.ctx, t.activation.activation.ID)
	if err != nil {
		t.session.log.Printf("opencapsula: conversation for activation=%s: %v",
			t.activation.activation.ID, err)
		return
	}
	t.mu.Lock()
	t.conversation = conv
	t.conversationID = conv.ID()
	t.mu.Unlock()
}

func (t *turn) conversationKey() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.conversationID
}

func (t *turn) followUpAllowed(followUp bool, timeout time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return followUp && timeout > 0 && t.replied && !t.failed && !t.ending
}

func (t *turn) beginFollowUp() {
	t.mu.Lock()
	cfg := t.session.a.Config()
	t.id = t.session.a.nextTurnID(cfg)
	t.started, t.ended, t.completed = false, false, false
	t.cancelSent, t.failed, t.replied, t.interrupted = false, false, false, false
	t.mu.Unlock()
	t.setState(turnStateListening)
	t.session.a.ring.Set(ring.StateListening)
	t.session.a.ring.Observe(0)
}

// listenOnce records one request. It returns true when the utterance ended and
// a server turn was sent, false when the turn must be abandoned.
func (t *turn) listenOnce(activationID string, noSpeech time.Duration) bool {
	cfg := t.session.a.Config()
	if cfg.Earcons {
		t.playCue(earcon.Listen())
	}
	t.session.a.ring.Set(ring.StateListening)
	t.setState(turnStateListening)

	micCtx, micCancel := context.WithCancel(t.ctx)
	mic, err := t.session.a.client.OpenMicWithFormat(micCtx, activationID, t.conversationKey(), inputFormat)
	if err != nil {
		micCancel()
		if t.ctx.Err() != nil {
			return false
		}
		t.fail("microphone: %v", err)
		return false
	}
	defer func() {
		micCancel()
		_ = mic.Close()
		select {
		case <-mic.Done():
		case <-time.After(time.Second):
		}
	}()

	vadConfig := vad.DefaultConfig()
	endpointer := vad.New(vadConfig)
	endpointer.Reset(noSpeech)
	pre := newPreroll(preRollMS)
	var lastGap time.Time

	for {
		select {
		case <-t.ctx.Done():
			return false

		case control := <-t.controls:
			switch control.kind {
			case controlListenEnd:
				if t.hasStarted() {
					if !t.hasEnded() {
						if !t.send(protocol.TypeTurnEnd) {
							return false
						}
						t.markEnded()
					}
					t.setState(turnStateThinking)
					t.session.a.ring.Set(ring.StateThinking)
					return true
				}
				return false
			case controlComplete:
				t.markCompleted()
				t.markEnded()
				return t.hasStarted()
			case controlInterrupted:
				// Nothing is playing yet.
			case controlError:
				t.session.log.Printf("opencapsula: server error during listening: %v", control.serverErr)
			}

		case chunk, ok := <-mic.Chunks():
			if !ok {
				if t.ctx.Err() == nil {
					t.fail("microphone stream ended")
				}
				return false
			}
			if chunk.Format != nil {
				if err := validateInputFormat(*chunk.Format); err != nil {
					t.fail("%v", err)
					return false
				}
			}
			if chunk.Notice != nil {
				switch chunk.Notice.Code {
				case "CLIENT_TOO_SLOW":
					if time.Since(lastGap) > 500*time.Millisecond {
						lastGap = time.Now()
						t.session.log.Printf("opencapsula: microphone is behind: %s", chunk.Notice.Detail)
					}
					if !t.hasStarted() {
						pre.reset()
					}
					continue
				default:
					t.fail("microphone notice %s: %s", chunk.Notice.Code, chunk.Notice.Detail)
					return false
				}
			}
			if chunk.Chunk == nil {
				continue
			}
			pcm := chunk.Chunk.PCM
			if chunk.Chunk.Discontinuity {
				if time.Since(lastGap) > 500*time.Millisecond {
					lastGap = time.Now()
					t.session.log.Printf("opencapsula: microphone gap dropped=%d",
						chunk.Chunk.DroppedFramesBefore)
				}
				if !t.hasStarted() {
					pre.reset()
				}
			}
			t.session.a.ring.Observe(pcmLevel(pcm))

			switch endpointer.Process(pcm) {
			case vad.Waiting:
				pre.add(pcm)
			case vad.SpeechStarted:
				if !t.startServerTurn() {
					return false
				}
				if !pre.flush(t) {
					return false
				}
				if !t.sendAudio(pcm) {
					return false
				}
			case vad.Speaking:
				if !t.sendAudio(pcm) {
					return false
				}
			case vad.EndOfUtterance:
				if !t.sendAudio(pcm) {
					return false
				}
				if !t.send(protocol.TypeTurnEnd) {
					return false
				}
				t.markEnded()
				t.setState(turnStateThinking)
				t.session.a.ring.Set(ring.StateThinking)
				return true
			case vad.NoSpeech:
				t.session.log.Printf("opencapsula: no speech before the listening window ended (floor %.0f dBFS)",
					endpointer.NoiseFloor())
				return false
			}
		}
	}
}

func (t *turn) startServerTurn() bool {
	t.mu.Lock()
	id := t.id
	t.started = true
	t.mu.Unlock()
	if !t.send(protocol.TypeTurnStart) {
		return false
	}
	t.session.log.Printf("opencapsula: turn %s speech started", id)
	return true
}

func (t *turn) awaitReply() bool {
	t.mu.Lock()
	completed := t.completed
	t.mu.Unlock()
	if completed {
		t.flushAudio()
		t.drainPlayback()
		return true
	}
	t.setState(turnStateThinking)
	t.session.a.ring.Set(ring.StateThinking)
	start := time.Now()
	last := start
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-t.ctx.Done():
			return false

		case pcm := <-t.audio:
			last = time.Now()
			if !t.writeAudio(pcm) {
				return false
			}

		case control := <-t.controls:
			last = time.Now()
			switch control.kind {
			case controlComplete:
				t.markCompleted()
				t.flushAudio()
				t.drainPlayback()
				return true
			case controlInterrupted:
				t.interrupt()
				t.dropAudio()
				t.setState(turnStateThinking)
				t.session.a.ring.Set(ring.StateThinking)
			case controlListenEnd:
				// The utterance already ended locally.
			case controlError:
				t.mu.Lock()
				t.failed = true
				t.mu.Unlock()
			}

		case <-ticker.C:
			if time.Since(last) > responseTimeout || time.Since(start) > maxTurn {
				t.fail("server did not finish the turn in time")
				t.sendCancel()
				return false
			}
		}
	}
}

// writeAudio opens the reply stream lazily and paces it through the sink.
func (t *turn) writeAudio(pcm []byte) bool {
	t.mu.Lock()
	interrupted := t.interrupted
	sink := t.sink
	t.mu.Unlock()
	if interrupted {
		return true
	}
	if sink == nil {
		var err error
		sink, err = newPlaybackSink(t.ctx, t.session.a.client, t.ready.OutputFormat, t.conversationKey())
		if err != nil {
			t.fail("playback open: %v", err)
			return false
		}
		t.mu.Lock()
		t.sink = sink
		t.replied = true
		t.mu.Unlock()
		t.setState(turnStateSpeaking)
		t.session.a.ring.Set(ring.StateSpeaking)
	} else {
		t.mu.Lock()
		t.replied = true
		t.mu.Unlock()
	}
	if err := sink.Write(pcm); err != nil {
		switch {
		case errors.Is(err, errPlaybackBackpressure):
			t.mu.Lock()
			logNow := time.Since(t.lastDrop) > 500*time.Millisecond
			if logNow {
				t.lastDrop = time.Now()
			}
			t.mu.Unlock()
			if logNow {
				t.session.log.Printf("opencapsula: playback is behind; dropping %d bytes", len(pcm))
			}
		case errors.Is(err, errPlaybackClosed):
			// A concurrent interrupt/abort already stopped this stream.
		default:
			t.fail("playback: %v", err)
			return false
		}
	}
	t.session.a.ring.Observe(pcmLevel(pcm))
	return true
}

func (t *turn) flushAudio() {
	for {
		select {
		case pcm := <-t.audio:
			if !t.writeAudio(pcm) {
				return
			}
		default:
			return
		}
	}
}

func (t *turn) dropAudio() {
	for {
		select {
		case <-t.audio:
		default:
			return
		}
	}
}

func (t *turn) drainPlayback() {
	t.mu.Lock()
	sink := t.sink
	t.mu.Unlock()
	if sink == nil {
		return
	}
	ctx, cancel := context.WithTimeout(t.ctx, turnCompleteDrain)
	defer cancel()
	if err := sink.Drain(ctx); err != nil {
		t.session.log.Printf("opencapsula: playback drain: %v", err)
	}
	t.mu.Lock()
	if t.sink == sink {
		t.sink = nil
	}
	t.mu.Unlock()
}

func (t *turn) interrupt() {
	t.mu.Lock()
	t.interrupted = true
	sink := t.sink
	t.sink = nil
	t.mu.Unlock()
	if sink != nil {
		sink.Abort()
	}
}

func (t *turn) send(kind protocol.Type) bool {
	t.mu.Lock()
	id := t.id
	t.mu.Unlock()
	err := t.session.link.SendMessage(t.ctx, &protocol.Message{Type: kind, TurnID: id})
	if err != nil {
		if t.ctx.Err() == nil {
			t.fail("send %s: %v", kind, err)
		}
		return false
	}
	return true
}

func (t *turn) sendAudio(pcm []byte) bool {
	if err := t.session.link.SendAudio(t.ctx, pcm); err != nil {
		if t.ctx.Err() == nil {
			t.fail("send audio: %v", err)
		}
		return false
	}
	return true
}

func (t *turn) sendCancel() {
	t.mu.Lock()
	if !t.started || t.completed || t.cancelSent {
		t.mu.Unlock()
		return
	}
	t.cancelSent = true
	id := t.id
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = t.session.link.SendMessage(ctx, &protocol.Message{Type: protocol.TypeTurnCancel, TurnID: id})
}

func (t *turn) playCue(cue earcon.Cue) {
	if len(cue) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(t.ctx, cueTimeout)
	defer cancel()
	playback, err := t.session.a.client.OpenPlayback(ctx, "audio.output.speaker", earconFormat, t.conversationKey())
	if err != nil {
		t.session.log.Printf("opencapsula: earcon: %v", err)
		return
	}
	if err := playback.Write(cue); err != nil {
		_ = playback.Abort()
		return
	}
	_ = playback.Close()
}

func (t *turn) cleanup() {
	t.sendCancel()
	t.interrupt()
	t.setState(turnStateEnded)
	t.mu.Lock()
	conv := t.conversation
	t.mu.Unlock()
	if conv != nil {
		conv.Close()
	}
}

func (t *turn) fail(format string, args ...any) {
	t.mu.Lock()
	t.failed = true
	t.mu.Unlock()
	t.session.log.Printf("opencapsula: "+format, args...)
}

func (t *turn) hasStarted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.started
}

func (t *turn) hasEnded() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ended
}

func (t *turn) markEnded() {
	t.mu.Lock()
	t.ended = true
	t.mu.Unlock()
}

func (t *turn) markCompleted() {
	t.mu.Lock()
	t.completed = true
	t.mu.Unlock()
}

func validateInputFormat(format capsulad.AudioFormat) error {
	if format.SampleFormat != capsulad.SampleFormatUnspecified &&
		format.SampleFormat != capsulad.SampleFormatPCM16LE {
		return fmt.Errorf("microphone format %s is not PCM16; OpenCapsule sends LINEAR16", format.SampleFormat)
	}
	if format.SampleRateHz != 0 && format.SampleRateHz != inputRate {
		return fmt.Errorf("microphone format is %d Hz; OpenCapsule sends %d Hz", format.SampleRateHz, inputRate)
	}
	if format.Channels != 0 && format.Channels != inputChannels {
		return fmt.Errorf("microphone format has %d channels; OpenCapsule sends mono", format.Channels)
	}
	return nil
}

// preroll keeps a bounded tail of pre-onset audio. Frames are not copied: the
// SDK hands each chunk a fresh slice owned by this consumer.
type preroll struct {
	frames [][]byte
	bytes  int
	limit  int
}

func newPreroll(ms int) *preroll {
	return &preroll{limit: inputRate * ms / 1000 * 2}
}

func (p *preroll) add(pcm []byte) {
	p.frames = append(p.frames, pcm)
	p.bytes += len(pcm)
	for p.bytes > p.limit && len(p.frames) > 1 {
		p.bytes -= len(p.frames[0])
		p.frames = p.frames[1:]
	}
	if p.bytes > p.limit && len(p.frames) == 1 {
		p.reset()
	}
}

func (p *preroll) flush(t *turn) bool {
	for _, frame := range p.frames {
		if !t.sendAudio(frame) {
			return false
		}
	}
	p.reset()
	return true
}

func (p *preroll) reset() {
	p.frames = p.frames[:0]
	p.bytes = 0
}
