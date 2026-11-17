package turn

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"capsulad/sdk/capsulad"

	cadencev2 "capsulad/apps/cadence/internal/proto/cadencev2"
	"capsulad/apps/cadence/internal/settings"
)

// Controller runs assistant turns one at a time on its own goroutine. The
// framework queues activations into it, the link pushes envelopes into it, and
// nothing it calls blocks the link's reader for long.
type Controller struct {
	d Deps

	triggers chan Trigger
	releases chan string
	inbox    chan *cadencev2.Envelope
	dismiss  chan struct{}
	linkDown chan struct{}

	// originRefused logs the unusable-origin refusal once, not per activation.
	originRefused bool

	// pending holds releases that arrived before their queued activation
	// began (a quick hold-to-talk press while an earlier turn runs). Only the
	// Run goroutine touches it.
	pending map[string]struct{}

	// dropped counts envelopes the controller could not accept in time. A turn
	// that lost a frame is degraded: an audibly holed reply must not offer a
	// follow-up window.
	dropped atomic.Int64
}

// New builds a controller. Zero tuning uses the documented defaults.
func New(d Deps) *Controller {
	if d.Log == nil {
		d.Log = log.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Settings == nil {
		d.Settings = func() settings.Config {
			return settings.Config{HoldTail: time.Second, FollowUp: true}
		}
	}
	if d.NoSpeechTimeout <= 0 {
		d.NoSpeechTimeout = DefaultNoSpeechTimeout
	}
	if d.ListenTimeout <= 0 {
		d.ListenTimeout = DefaultListenTimeout
	}
	if d.StreamQueue <= 0 {
		d.StreamQueue = DefaultStreamQueue
	}
	return &Controller{
		d:        d,
		triggers: make(chan Trigger, 8),
		releases: make(chan string, 8),
		inbox:    make(chan *cadencev2.Envelope, 128),
		dismiss:  make(chan struct{}, 1),
		linkDown: make(chan struct{}, 1),
		pending:  make(map[string]struct{}),
	}
}

// Offer queues one activation. It reports false when the queue is full; the
// framework never blocks on an assistant.
func (c *Controller) Offer(t Trigger) bool {
	select {
	case c.triggers <- t:
		return true
	default:
		return false
	}
}

// Release reports a hold-to-talk release for an activation.
func (c *Controller) Release(activationID string) {
	select {
	case c.releases <- activationID:
	default:
	}
}

// Dismiss cancels the running turn and drops queued activations. It never
// blocks.
func (c *Controller) Dismiss() {
	select {
	case c.dismiss <- struct{}{}:
	default:
	}
}

// LinkDown tells the controller the link ended; the running turn is cancelled
// locally because its reply can no longer arrive.
func (c *Controller) LinkDown() {
	select {
	case c.linkDown <- struct{}{}:
	default:
	}
}

// Message is the link handler. It hands one envelope to the controller without
// blocking the reader for long: past a short wait the frame is dropped and
// logged, because a stalled link would also stop ping replies.
func (c *Controller) Message(env *cadencev2.Envelope) {
	select {
	case c.inbox <- env:
		return
	default:
	}
	t := time.NewTimer(250 * time.Millisecond)
	defer t.Stop()
	select {
	case c.inbox <- env:
	case <-t.C:
		c.dropped.Add(1)
		c.d.Log.Printf("controller_busy: dropped %s", envelopeName(env))
	}
}

// Run serves triggers, releases, envelopes and timers until ctx ends. Every
// terminal path releases the microphone, indication, conversation, playback
// and encoder.
func (c *Controller) Run(ctx context.Context) {
	var t *turn
	defer func() {
		if t != nil {
			t.cancel("shutdown")
		}
	}()
	for {
		var (
			mic      <-chan capsulad.MicChunk
			noSpeech <-chan time.Time
			listen   <-chan time.Time
			window   <-chan time.Time
			tail     <-chan time.Time
		)
		if t != nil {
			mic, noSpeech, listen, window, tail = t.mic, t.noSpeechC, t.listenC, t.windowC, t.tailC
		}
		select {
		case <-ctx.Done():
			return

		case trig := <-c.triggers:
			if t != nil {
				t.cancel("superseded")
				t = nil
			}
			t = c.begin(ctx, trig)

		case id := <-c.releases:
			if t != nil && id == t.activationID {
				t.onRelease(id)
			} else {
				// A quick hold-to-talk press can be released while its
				// activation is still queued; remember it for that turn. The
				// map is bounded so a bad peer cannot grow it.
				if len(c.pending) >= 8 {
					for k := range c.pending {
						delete(c.pending, k)
						break
					}
				}
				c.pending[id] = struct{}{}
			}

		case env := <-c.inbox:
			if t != nil {
				t.handle(env)
			} else {
				c.refuse(env)
			}

		case <-c.dismiss:
			c.drainTriggers()
			clear(c.pending)
			if t != nil {
				t.cancel("dismissed")
				t = nil
			}

		case <-c.linkDown:
			// Ignore a stale signal: only a link that is still down cancels.
			if t != nil && !c.d.Link.Connected() {
				t.cancel("link_down")
				t = nil
			}

		case chunk, ok := <-mic:
			if t == nil {
				continue
			}
			if !ok {
				t.onMicClosed()
			} else {
				t.onChunk(chunk)
			}

		case <-noSpeech:
			if t != nil {
				t.cancel("no_speech")
				t = nil
			}

		case <-listen:
			if t != nil {
				t.cancel("listen_timeout")
				t = nil
			}

		case <-window:
			if t != nil {
				t.cancel("follow_up_silence")
				t = nil
			}

		case <-tail:
			if t != nil {
				t.onTail()
			}
		}
		if t != nil && t.ended {
			t = nil
		}
	}
}

func (c *Controller) drainTriggers() {
	for {
		select {
		case tr := <-c.triggers:
			c.d.Log.Printf("activation=%s dropped; dismissed", tr.ActivationID)
		default:
			return
		}
	}
}

// refuse answers envelopes that need no turn: a reply stream for a session the
// server no longer runs, or an announcement this client has no path for.
func (c *Controller) refuse(env *cadencev2.Envelope) {
	if mo := env.GetMicOpen(); mo != nil {
		// A screen press in the room asks this speaker to open its microphone.
		// The request carries no physical local owner signal for this device,
		// so it is refused explicitly (the server forgets it after 15 s).
		c.d.Log.Printf("mic_open_refused session=%s origin=%s: no local owner signal", mo.GetSessionId(), mo.GetOriginDeviceId())
		return
	}
	if st := env.GetStreamStart(); st != nil {
		switch st.GetPurpose() {
		case cadencev2.StreamPurpose_STREAM_PURPOSE_ANNOUNCE:
			c.d.Log.Printf("announce_refused stream=%s: announcements play outside an activation", st.GetStreamId())
		case cadencev2.StreamPurpose_STREAM_PURPOSE_REPLY:
			c.d.Log.Printf("stream_refused stream=%s: no session", st.GetStreamId())
		}
	}
}

func envelopeName(env *cadencev2.Envelope) string {
	switch env.GetBody().(type) {
	case *cadencev2.Envelope_SessionEvent:
		return "session_event"
	case *cadencev2.Envelope_SessionClose:
		return "session_close"
	case *cadencev2.Envelope_StreamStart:
		return "stream_start"
	case *cadencev2.Envelope_AudioDown:
		return "audio_down"
	case *cadencev2.Envelope_StreamEnd:
		return "stream_end"
	case *cadencev2.Envelope_CapabilityCommand:
		return "capability_command"
	case *cadencev2.Envelope_Duck:
		return "duck"
	default:
		return "envelope"
	}
}

// --- one turn --------------------------------------------------------------

// session is one cadence.v2 voice session. A turn may span several: the server
// offers a follow-up window, and the next session reuses the microphone.
type turnPhase int

const (
	phaseListen turnPhase = iota
	phaseThink
	phaseSpeak
)

type session struct {
	id      string
	seq     uint32
	heard   bool
	frames  int
	streams map[string]*replyStream
}

type turn struct {
	c    *Controller
	ctx  context.Context
	stop context.CancelFunc

	activationID string
	origin       capsulad.ActivationOrigin
	holdToTalk   bool
	modelID      string

	ring      Indicator
	conv      Conversation
	enc       Encoder
	micStream MicStream
	mic       <-chan capsulad.MicChunk
	assembler frameAssembler
	inputDone bool
	formatErr string

	sess     *session
	followUp bool
	// phase separates the listening indication from the still-open microphone
	// during thinking/speaking.
	phase turnPhase
	// droppedAt is the controller's drop count when the turn began.
	droppedAt int64
	// draining are reply streams whose StreamEnd arrived; on a normal end they
	// are waited for so the conversation outlives the reply's tail.
	draining []*replyStream

	noSpeech  *time.Timer
	noSpeechC <-chan time.Time
	listen    *time.Timer
	listenC   <-chan time.Time
	window    *time.Timer
	windowC   <-chan time.Time
	tail      *time.Ticker
	tailC     <-chan time.Time
	tailLeft  int

	ended bool
	lost  bool
}

// begin starts a turn for one activation. It returns nil when the activation
// is expired, has no honest protocol trigger, or the indication cannot open.
func (c *Controller) begin(parent context.Context, trig Trigger) *turn {
	if trig.Expires > 0 && c.d.Now().Sub(trig.QueuedAt) >= trig.Expires {
		c.d.Log.Printf("activation=%s expired before its turn; dropped", trig.ActivationID)
		return nil
	}
	protoTrigger, ok := TriggerOrigin(trig.Origin)
	if !ok {
		if !c.originRefused {
			c.d.Log.Printf("activation refused: %s is not a wake word or a key press; cadence.v2 vouches owner privileges only for those", trig.Origin)
			c.originRefused = true
		}
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	t := &turn{
		c: c, ctx: ctx, stop: cancel,
		activationID: trig.ActivationID, origin: trig.Origin,
		holdToTalk: trig.Origin == capsulad.ActivationOriginTouch && trig.HoldToTalk,
		modelID:    trig.ModelID,
		droppedAt:  c.dropped.Load(),
	}

	// The visible indication must exist before the microphone opens: the
	// framework withholds capture without one.
	if err := c.d.Ring.Open(ctx); err != nil {
		c.d.Log.Printf("activation=%s cancelled: capture indication: %v", trig.ActivationID, err)
		cancel()
		return nil
	}
	t.ring = c.d.Ring

	conv, err := c.d.Audio.BeginConversation(ctx, trig.ActivationID)
	if err != nil {
		// Playback and capture still work; the mixer will not duck.
		c.d.Log.Printf("activation=%s conversation: %v", trig.ActivationID, err)
	} else {
		t.conv = conv
	}

	enc, err := c.d.Codecs.NewEncoder()
	if err != nil {
		c.d.Log.Printf("activation=%s cancelled: opus encoder: %v", trig.ActivationID, err)
		t.end("encoder")
		return nil
	}
	t.enc = enc

	if err := t.startSession(protoTrigger); err != nil {
		c.d.Log.Printf("activation=%s cancelled: session start: %v", trig.ActivationID, err)
		t.end("send_failed")
		return nil
	}
	t.listen = time.NewTimer(c.d.ListenTimeout)
	t.listenC = t.listen.C

	convID := ""
	if t.conv != nil {
		convID = t.conv.ID()
	}
	mic, err := c.d.Audio.OpenMic(ctx, trig.ActivationID, convID)
	if err != nil {
		c.d.Log.Printf("activation=%s cancelled: microphone: %v", trig.ActivationID, err)
		t.cancelSession("mic_failed")
		t.endNow("mic_failed")
		return nil
	}
	t.micStream = mic
	t.mic = mic.Chunks()
	t.phase = phaseListen
	t.ring.EnterListen()
	if _, early := c.pending[trig.ActivationID]; early {
		delete(c.pending, trig.ActivationID)
		t.onRelease(trig.ActivationID)
	}
	c.d.Log.Printf("turn_start activation=%s origin=%s session=%s", trig.ActivationID, trig.Origin, t.sess.id)
	return t
}

// startSession opens a new cadence session on the turn's microphone. The
// session id is fresh (a follow-up is a new session, not a continuation).
func (t *turn) startSession(trigger cadencev2.Trigger) error {
	id, err := t.c.d.NewID()
	if err != nil {
		return err
	}
	t.sess = &session{id: id, streams: make(map[string]*replyStream)}
	t.assembler.reset()
	start := &cadencev2.SessionStart{SessionId: id, Trigger: trigger}
	if trigger == cadencev2.Trigger_TRIGGER_WAKE && t.modelID != "" {
		start.Wake = &cadencev2.Wake{Engine: t.modelID}
	}
	return t.c.d.Link.Send(&cadencev2.Envelope{
		Body: &cadencev2.Envelope_SessionStart{SessionStart: start},
	})
}

// markHeard ends the no-speech countdown; a recognized utterance keeps the
// turn open until the server closes it.
func (t *turn) markHeard() {
	if t.noSpeech != nil {
		t.noSpeech.Stop()
		t.noSpeech = nil
		t.noSpeechC = nil
	}
	if t.sess != nil {
		t.sess.heard = true
	}
}

// armNoSpeech starts the no-speech deadline on the first captured packet that
// did not already count as speech.
func (t *turn) armNoSpeech() {
	if t.sess == nil || t.sess.heard || t.noSpeech != nil {
		return
	}
	t.noSpeech = time.NewTimer(t.c.d.NoSpeechTimeout)
	t.noSpeechC = t.noSpeech.C
}

// onChunk encodes one captured chunk and sends it. The same chunk may also
// carry the format announcement and a notice, so all three are checked.
func (t *turn) onChunk(mc capsulad.MicChunk) {
	if f := mc.Format; f != nil && f.SampleRateHz > 0 {
		if f.SampleFormat != capsulad.SampleFormatPCM16LE || f.SampleRateHz != uplinkRate || f.Channels != 1 {
			if t.formatErr == "" {
				t.formatErr = fmt.Sprintf("capture format %v %d Hz %d ch is not 16 kHz mono s16le",
					f.SampleFormat, f.SampleRateHz, f.Channels)
				t.c.d.Log.Printf("activation=%s cancelled: %s", t.activationID, t.formatErr)
				t.cancel("unsupported_audio")
			}
			return
		}
	}
	if n := mc.Notice; n != nil {
		switch n.Code {
		case "CLIENT_TOO_SLOW":
			t.assembler.reset()
		case "STREAM_CLOSED":
			t.onMicClosed()
		default:
			t.c.d.Log.Printf("activation=%s cancelled: capture %s", t.activationID, n.Code)
			t.cancel(noticeReason(n.Code))
		}
		return
	}
	chunk := mc.Chunk
	if chunk == nil || len(chunk.PCM) == 0 || t.formatErr != "" {
		return
	}
	if chunk.Discontinuity || chunk.DroppedFramesBefore > 0 {
		// A gap is not the end of the utterance and not a reason to splice
		// samples across it.
		t.assembler.reset()
	}
	if t.phase == phaseListen {
		t.ring.Level(levelFromRMS(pcmRMS(chunk.PCM)))
	}
	if t.sess == nil || t.inputDone {
		return
	}
	t.armNoSpeech()
	for _, frame := range t.assembler.push(chunk.PCM) {
		t.sendMic(frame)
	}
}

func (t *turn) sendMic(frame []byte) {
	if t.sess == nil || t.enc == nil {
		return
	}
	packet, err := t.enc.Encode(frame)
	if err != nil {
		t.lost = true
		t.c.d.Log.Printf("activation=%s cancelled: opus encode", t.activationID)
		t.cancel("encode_failed")
		return
	}
	env := &cadencev2.Envelope{Body: &cadencev2.Envelope_MicAudio{MicAudio: &cadencev2.MicAudio{
		SessionId: t.sess.id, Seq: t.sess.seq, Opus: packet,
	}}}
	if err := t.c.d.Link.Send(env); err != nil {
		t.lost = true
		t.c.d.Log.Printf("activation=%s cancelled: uplink: %v", t.activationID, err)
		t.cancel("send_failed")
		return
	}
	t.sess.seq++
}

// onMicClosed handles the capture stream ending. A deliberate stop sets
// inputDone, so only an unexpected end cancels the turn.
func (t *turn) onMicClosed() {
	if t.ended || t.inputDone {
		return
	}
	t.inputDone = true
	t.mic = nil
	t.c.d.Log.Printf("activation=%s cancelled: microphone stream ended", t.activationID)
	t.cancel("mic_closed")
}

// stopInput closes the capture and its visible indication. Used by a
// hold-to-talk release, which ends the user's input before the server answers.
func (t *turn) stopInput() {
	if t.inputDone {
		return
	}
	t.inputDone = true
	if t.micStream != nil {
		_ = t.micStream.Close()
		t.micStream = nil
	}
	t.mic = nil
	if t.noSpeech != nil {
		t.noSpeech.Stop()
		t.noSpeech = nil
		t.noSpeechC = nil
	}
}

// onRelease ends input for a hold-to-talk turn and streams a bounded silence
// tail so the server's recognizer closes the utterance; cadence.v2 has no
// device end-of-utterance message.
func (t *turn) onRelease(activationID string) {
	if t.ended || activationID != t.activationID || !t.holdToTalk || t.inputDone {
		return
	}
	t.stopInput()
	t.ring.Thinking()
	tail := t.c.d.Settings().HoldTail
	if tail <= 0 {
		return
	}
	t.tailLeft = int(tail / (20 * time.Millisecond))
	t.tail = time.NewTicker(20 * time.Millisecond)
	t.tailC = t.tail.C
	t.c.d.Log.Printf("activation=%s released; streaming %s of silence", t.activationID, tail)
}

// silenceFrame is one 20 ms frame of digital silence, reused by the release
// tail. sendMic's encoder copies it before returning.
var silenceFrame = make([]byte, uplinkFrameBytes)

// onTail streams one 20 ms silence frame. The packets are paced at real time:
// the server's microphone queue is 50 deep, so a burst would be dropped.
func (t *turn) onTail() {
	if t.ended || t.sess == nil {
		return
	}
	t.sendMic(silenceFrame)
	t.tailLeft--
	if t.tailLeft <= 0 {
		t.tail.Stop()
		t.tail = nil
		t.tailC = nil
	}
}

// handle dispatches one server envelope for the running turn.
func (t *turn) handle(env *cadencev2.Envelope) {
	switch {
	case env.GetSessionEvent() != nil:
		t.onSessionEvent(env.GetSessionEvent())
	case env.GetSessionClose() != nil:
		t.onSessionClose(env.GetSessionClose())
	case env.GetStreamStart() != nil:
		t.onStreamStart(env.GetStreamStart())
	case env.GetAudioDown() != nil:
		t.onAudioDown(env.GetAudioDown())
	case env.GetStreamEnd() != nil:
		t.onStreamEnd(env.GetStreamEnd())
	case env.GetMicOpen() != nil:
		mo := env.GetMicOpen()
		t.c.d.Log.Printf("mic_open_refused session=%s origin=%s: no local owner signal", mo.GetSessionId(), mo.GetOriginDeviceId())
	case env.GetDuck() != nil:
		// Duck is advisory for the device's own mix; the framework conversation
		// already ducks other playback for this turn.
	case env.GetCapabilityCommand() != nil:
		// This client advertises no capabilities, so it must never be asked.
		cmd := env.GetCapabilityCommand()
		t.c.d.Log.Printf("capability_command_refused capability=%s", cmd.GetCapability())
		_ = t.c.d.Link.Send(&cadencev2.Envelope{Body: &cadencev2.Envelope_CommandResult{
			CommandResult: &cadencev2.CommandResult{RequestId: cmd.GetRequestId(), Code: "unsupported"},
		}})
	}
}

func (t *turn) onSessionEvent(ev *cadencev2.SessionEvent) {
	if t.sess == nil || ev.GetSessionId() != t.sess.id {
		return
	}
	switch ev.GetKind() {
	case cadencev2.SessionEventKind_SESSION_EVENT_KIND_SPEECH_STARTED:
		t.markHeard()
		t.phase = phaseListen
		t.ring.EnterListen()
	case cadencev2.SessionEventKind_SESSION_EVENT_KIND_LISTENING:
		t.phase = phaseListen
		t.ring.EnterListen()
	case cadencev2.SessionEventKind_SESSION_EVENT_KIND_THINKING:
		t.markHeard()
		t.phase = phaseThink
		t.ring.Thinking()
	}
}

func (t *turn) onSessionClose(sc *cadencev2.SessionClose) {
	if t.sess == nil || sc.GetSessionId() != t.sess.id {
		return
	}
	code := sc.GetCode()
	if code == "" {
		code = "ok"
	}
	if t.c.dropped.Load() != t.droppedAt {
		// An envelope was dropped for this turn (a bounded queue can lose a
		// transition); the reply may be holed, so no follow-up window.
		t.lost = true
	}
	heardWhole := t.playedPackets() > 0 && !t.lost && len(t.sess.streams) == 0
	normal := code == "ok" && !t.lost
	t.closeOpenStreams(normal)
	if code != "ok" {
		t.ring.Failed()
		t.c.d.Log.Printf("turn_failed activation=%s code=%s stage=%s", t.activationID, code, sc.GetFailure().GetStage())
	}
	if t.canFollowUp(sc, code, heardWhole) {
		if err := t.startSession(cadencev2.Trigger_TRIGGER_FOLLOW_UP); err == nil {
			t.followUp = true
			window := time.Duration(sc.GetFollowUpWindowMs()) * time.Millisecond
			if window > maxFollowUpWindow {
				window = maxFollowUpWindow
			}
			t.window = time.NewTimer(window)
			t.windowC = t.window.C
			t.phase = phaseListen
			t.ring.EnterListen()
			t.c.d.Log.Printf("follow_up activation=%s session=%s window=%s", t.activationID, t.sess.id, window)
			return
		}
		t.c.d.Log.Printf("activation=%s follow-up could not start", t.activationID)
	}
	t.end(code)
}

// canFollowUp mirrors cadence-device's follow-up facts: the server allowed a
// window, the reply was heard whole, and the microphone is still open because
// the user did not release a hold-to-talk turn.
func (t *turn) canFollowUp(sc *cadencev2.SessionClose, code string, heardWhole bool) bool {
	if !t.c.d.Settings().FollowUp || t.inputDone || t.formatErr != "" {
		return false
	}
	if code != "ok" || !sc.GetFollowUpAllowed() || sc.GetFollowUpWindowMs() == 0 {
		return false
	}
	return heardWhole
}

// closeOpenStreams ends the session's still-open streams (the server cut the
// reply). Streams whose StreamEnd already arrived keep draining when normal.
func (t *turn) closeOpenStreams(normal bool) {
	if t.sess == nil {
		return
	}
	for id, rs := range t.sess.streams {
		delete(t.sess.streams, id)
		rs.finish(false)
	}
	if !normal {
		for _, rs := range t.draining {
			rs.finish(false)
		}
	}
}

func (t *turn) onStreamStart(st *cadencev2.StreamStart) {
	if t.sess == nil || st.GetSessionId() != t.sess.id ||
		st.GetPurpose() != cadencev2.StreamPurpose_STREAM_PURPOSE_REPLY {
		t.c.d.Log.Printf("stream_refused stream=%s: %s", st.GetStreamId(), refusalReason(st))
		return
	}
	channels := int(st.GetChannels())
	if channels != 1 && channels != 2 {
		channels = 1
	}
	dec, err := t.c.d.Codecs.NewDecoder(channels)
	if err != nil {
		t.c.d.Log.Printf("stream_refused stream=%s: opus decoder", st.GetStreamId())
		return
	}
	convID := ""
	if t.conv != nil {
		convID = t.conv.ID()
	}
	pb, err := t.c.d.Audio.OpenPlayback(t.ctx, "audio.output.speaker", capsulad.AudioFormat{
		SampleFormat: capsulad.SampleFormatPCM16LE,
		SampleRateHz: 48000,
		Channels:     uint32(channels),
	}, convID)
	if err != nil {
		dec.Close()
		t.c.d.Log.Printf("stream_refused stream=%s: playback", st.GetStreamId())
		return
	}
	if old, ok := t.sess.streams[st.GetStreamId()]; ok {
		old.finish(false)
	}
	// Detach the stream from the turn context: a successful half-close must
	// drain its tail even as the turn itself winds down.
	t.sess.streams[st.GetStreamId()] = newReplyStream(context.WithoutCancel(t.ctx), st.GetStreamId(), dec, pb, t.c.d.StreamQueue)
	t.phase = phaseSpeak
	t.ring.Speaking()
}

func refusalReason(st *cadencev2.StreamStart) string {
	switch {
	case st.GetPurpose() == cadencev2.StreamPurpose_STREAM_PURPOSE_ANNOUNCE:
		return "announcement"
	case st.GetPurpose() != cadencev2.StreamPurpose_STREAM_PURPOSE_REPLY:
		return "purpose"
	default:
		return "session"
	}
}

func (t *turn) onAudioDown(a *cadencev2.AudioDown) {
	if t.sess == nil {
		return
	}
	rs, ok := t.sess.streams[a.GetStreamId()]
	if !ok {
		return
	}
	if err := rs.push(packet{seq: a.GetSeq(), opus: a.GetOpus()}); err != nil {
		t.lost = true
		t.c.d.Log.Printf("stream_gap stream=%s seq=%d", a.GetStreamId(), a.GetSeq())
		delete(t.sess.streams, a.GetStreamId())
		rs.finish(false)
		return
	}
	t.sess.frames++
}

func (t *turn) onStreamEnd(end *cadencev2.StreamEnd) {
	if t.sess == nil {
		return
	}
	rs, ok := t.sess.streams[end.GetStreamId()]
	if !ok {
		return
	}
	delete(t.sess.streams, end.GetStreamId())
	// The stream is complete: half-close it so its queued tail drains while the
	// session close is still arriving.
	rs.finish(true)
	t.draining = append(t.draining, rs)
}

// cancelSession tells the server this device ended the session. The server
// answers a cancel with nothing.
func (t *turn) cancelSession(reason string) {
	if t.sess == nil {
		return
	}
	_ = t.c.d.Link.Send(&cadencev2.Envelope{Body: &cadencev2.Envelope_SessionCancel{
		SessionCancel: &cadencev2.SessionCancel{SessionId: t.sess.id, Reason: reason},
	}})
}

// cancel is the local terminal path: tell the server, discard audio, release
// everything now.
func (t *turn) cancel(reason string) {
	if t.ended {
		return
	}
	t.cancelSession(reason)
	t.endNow(reason)
}

// end is the normal terminal path: the server already closed the session, so
// the reply tail drains before the conversation (and its duck) ends.
func (t *turn) end(code string) {
	if t.ended {
		return
	}
	normal := code == "ok" && !t.lost
	t.finish(code, normal)
}

// endNow is end without a drain wait: every stream aborts and the conversation
// closes immediately.
func (t *turn) endNow(code string) {
	if t.ended {
		return
	}
	t.finish(code, false)
}

func (t *turn) finish(code string, normal bool) {
	t.ended = true
	t.stopTimers()
	t.stopInput()
	if t.sess != nil {
		for id, rs := range t.sess.streams {
			delete(t.sess.streams, id)
			rs.finish(false)
		}
	}
	if !normal {
		for _, rs := range t.draining {
			rs.finish(false)
		}
	}
	if t.enc != nil {
		t.enc.Close()
		t.enc = nil
	}
	if t.ring != nil {
		if !normal && failureVisible(code) {
			// A distinct error state, held briefly so it is seen. Mute and a
			// lost indication are the framework's own states: overriding them
			// would hide the mute.
			t.ring.Failed()
			time.Sleep(failureHold)
		}
		t.ring.Close()
	}

	conv := t.conv
	t.conv = nil
	draining := t.draining
	t.draining = nil
	if conv == nil {
		t.logEnd(code)
		t.stop()
		return
	}
	if !normal || len(draining) == 0 {
		conv.Close()
		t.logEnd(code)
		t.stop()
		return
	}
	// Keep the conversation (and its duck) until the reply tail has played, so
	// the tail is not un-ducked mid-word. It is bounded and never blocks the
	// controller.
	go func() {
		deadline := time.Now().Add(maxTailDrain)
		for _, rs := range draining {
			if !rs.waitFor(time.Until(deadline)) {
				break
			}
		}
		conv.Close()
	}()
	t.logEnd(code)
	t.stop()
}

// playedPackets counts reply packets the stream writers actually handed to the
// playback, across open and draining streams.
func (t *turn) playedPackets() int {
	n := 0
	for _, rs := range t.draining {
		n += int(rs.sent.Load())
	}
	if t.sess != nil {
		for _, rs := range t.sess.streams {
			n += int(rs.sent.Load())
		}
	}
	return n
}

func (t *turn) logEnd(code string) {
	t.c.d.Log.Printf("turn_end activation=%s session=%s code=%s", t.activationID, sessionID(t.sess), code)
}

func (t *turn) stopTimers() {
	for _, timer := range []*time.Timer{t.noSpeech, t.listen, t.window} {
		if timer != nil {
			timer.Stop()
		}
	}
	t.noSpeech, t.noSpeechC = nil, nil
	t.listen, t.listenC = nil, nil
	t.window, t.windowC = nil, nil
	if t.tail != nil {
		t.tail.Stop()
		t.tail, t.tailC = nil, nil
	}
}

func sessionID(s *session) string {
	if s == nil {
		return ""
	}
	return s.id
}

// failureHold bounds the red error state so it cannot run into the next turn.
const failureHold = 300 * time.Millisecond

// failureVisible says whether a terminal code deserves the error indication.
// User dismissal, a superseding activation, shutdown and an expired follow-up
// window are not errors, and a mute or lost indication must leave the
// framework's own indication in place.
func failureVisible(code string) bool {
	switch code {
	case "ok", "dismissed", "superseded", "shutdown", "muted", "indication_lost", "follow_up_silence":
		return false
	}
	return true
}

// noticeReason maps a capture notice to a bounded, loggable cancel reason.
func noticeReason(code string) string {
	switch code {
	case "MICROPHONE_MUTED":
		return "muted"
	case "WATCHDOG_EXPIRED":
		return "indication_lost"
	case "DAEMON_SHUTDOWN", "APP_EXITED":
		return "shutdown"
	default:
		return "mic_notice"
	}
}
