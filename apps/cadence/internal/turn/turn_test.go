package turn_test

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"capsulad/sdk/capsulad"

	cadencev2 "capsulad/apps/cadence/internal/proto/cadencev2"
	"capsulad/apps/cadence/internal/settings"
	"capsulad/apps/cadence/internal/turn"
)

// --- fakes -----------------------------------------------------------------

type fakeLink struct {
	mu   sync.Mutex
	up   bool
	sent []*cadencev2.Envelope
}

func (l *fakeLink) Send(env *cadencev2.Envelope) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.up {
		return errors.New("offline")
	}
	l.sent = append(l.sent, env)
	return nil
}

func (l *fakeLink) Connected() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.up
}

func (l *fakeLink) setUp(up bool) {
	l.mu.Lock()
	l.up = up
	l.mu.Unlock()
}

func (l *fakeLink) snapshot() []*cadencev2.Envelope {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*cadencev2.Envelope, len(l.sent))
	copy(out, l.sent)
	return out
}

func (l *fakeLink) count(pred func(*cadencev2.Envelope) bool) int {
	n := 0
	for _, env := range l.snapshot() {
		if pred(env) {
			n++
		}
	}
	return n
}

type fakePlayback struct {
	mu      sync.Mutex
	writes  [][]byte
	closed  bool
	aborted bool
}

func (p *fakePlayback) Write(b []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.aborted {
		return errors.New("aborted")
	}
	p.writes = append(p.writes, append([]byte(nil), b...))
	return nil
}

func (p *fakePlayback) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

func (p *fakePlayback) Abort() error {
	p.mu.Lock()
	p.aborted = true
	p.mu.Unlock()
	return nil
}

func (p *fakePlayback) state() (writes int, closed, aborted bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.writes), p.closed, p.aborted
}

type fakeMic struct {
	ch     chan capsulad.MicChunk
	once   sync.Once
	closed atomic.Bool
}

func newFakeMic() *fakeMic {
	return &fakeMic{ch: make(chan capsulad.MicChunk, 64)}
}

func (m *fakeMic) Chunks() <-chan capsulad.MicChunk { return m.ch }

func (m *fakeMic) Close() error {
	m.closed.Store(true)
	m.once.Do(func() { close(m.ch) })
	return nil
}

func (m *fakeMic) send(c capsulad.MicChunk) bool {
	if m.closed.Load() {
		return false
	}
	select {
	case m.ch <- c:
		return true
	default:
		return false
	}
}

type fakeConv struct{ closed atomic.Bool }

func (c *fakeConv) ID() string { return "conv-1" }
func (c *fakeConv) Close()     { c.closed.Store(true) }

type fakeAudio struct {
	mu        sync.Mutex
	mic       *fakeMic
	micErr    error
	playbacks []*fakePlayback
	conv      *fakeConv
	openOrder *atomic.Int32
}

func (a *fakeAudio) OpenMic(context.Context, string, string) (turn.MicStream, error) {
	if a.micErr != nil {
		return nil, a.micErr
	}
	if a.openOrder != nil {
		a.openOrder.Store(2)
	}
	return a.mic, nil
}

func (a *fakeAudio) OpenPlayback(context.Context, string, capsulad.AudioFormat, string) (turn.Playback, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := &fakePlayback{}
	a.playbacks = append(a.playbacks, p)
	return p, nil
}

func (a *fakeAudio) BeginConversation(context.Context, string) (turn.Conversation, error) {
	return a.conv, nil
}

func (a *fakeAudio) playback(i int) *fakePlayback {
	a.mu.Lock()
	defer a.mu.Unlock()
	if i >= len(a.playbacks) {
		return nil
	}
	return a.playbacks[i]
}

type fakeIndicator struct {
	mu     sync.Mutex
	opens  atomic.Int32
	closes atomic.Int32
	states []string
	level  float64
	order  *atomic.Int32
}

func (r *fakeIndicator) Open(context.Context) error {
	r.opens.Add(1)
	if r.order != nil {
		r.order.Store(1)
	}
	return nil
}

func (r *fakeIndicator) openCount() int  { return int(r.opens.Load()) }
func (r *fakeIndicator) closeCount() int { return int(r.closes.Load()) }

func (r *fakeIndicator) EnterListen() { r.record("listening") }

func (r *fakeIndicator) Level(level float64) {
	r.mu.Lock()
	r.level = level
	r.mu.Unlock()
}

func (r *fakeIndicator) Thinking() { r.record("thinking") }
func (r *fakeIndicator) Speaking() { r.record("speaking") }
func (r *fakeIndicator) Failed()   { r.record("failed") }

func (r *fakeIndicator) record(s string) {
	r.mu.Lock()
	r.states = append(r.states, s)
	r.mu.Unlock()
}

func (r *fakeIndicator) Close() { r.closes.Add(1) }

func (r *fakeIndicator) lastState() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.states) == 0 {
		return ""
	}
	return r.states[len(r.states)-1]
}

func (r *fakeIndicator) saw(state string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.states {
		if s == state {
			return true
		}
	}
	return false
}

type passthroughCodec struct{}

func (passthroughCodec) NewEncoder() (turn.Encoder, error)    { return passthroughEnc{}, nil }
func (passthroughCodec) NewDecoder(int) (turn.Decoder, error) { return passthroughDec{}, nil }

type passthroughEnc struct{}

func (passthroughEnc) Encode(pcm []byte) ([]byte, error) { return append([]byte(nil), pcm...), nil }
func (passthroughEnc) Close()                            {}

type passthroughDec struct{}

func (passthroughDec) Decode(p []byte) ([]byte, error) { return append([]byte(nil), p...), nil }
func (passthroughDec) Close()                          {}

// --- harness ---------------------------------------------------------------

type harness struct {
	t     *testing.T
	ctrl  *turn.Controller
	link  *fakeLink
	audio *fakeAudio
	ring  *fakeIndicator
	order atomic.Int32
	cfg   settings.Config

	mu     sync.Mutex
	nextID int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, link: &fakeLink{up: true}, audio: &fakeAudio{conv: &fakeConv{}},
		ring: &fakeIndicator{}, cfg: settings.Config{HoldTail: 100 * time.Millisecond, FollowUp: true}}
	h.audio.mic = newFakeMic()
	h.ring.order = &h.order
	h.audio.openOrder = &h.order
	h.ctrl = turn.New(turn.Deps{
		Link: h.link, Audio: h.audio, Ring: h.ring, Codecs: passthroughCodec{},
		Log:      log.New(io.Discard, "", 0),
		Settings: func() settings.Config { return h.cfg },
		NewID:    func() (string, error) { return h.id(), nil },
		Now:      time.Now,
	})
	return h
}

func (h *harness) id() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	return "s" + string(rune('0'+h.nextID))
}

func (h *harness) run(t *testing.T) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go h.ctrl.Run(ctx)
	return cancel
}

// waitFor polls until cond is true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func frame(fill byte) capsulad.MicChunk {
	pcm := make([]byte, 640)
	for i := range pcm {
		pcm[i] = fill
	}
	return capsulad.MicChunk{
		Format: &capsulad.AudioFormat{SampleFormat: capsulad.SampleFormatPCM16LE, SampleRateHz: 16000, Channels: 1},
		Chunk:  &capsulad.AudioChunk{PCM: pcm},
	}
}

func sessionStarts(l *fakeLink) []*cadencev2.SessionStart {
	var out []*cadencev2.SessionStart
	for _, env := range l.snapshot() {
		if s := env.GetSessionStart(); s != nil {
			out = append(out, s)
		}
	}
	return out
}

func micSent(l *fakeLink) []*cadencev2.MicAudio {
	var out []*cadencev2.MicAudio
	for _, env := range l.snapshot() {
		if m := env.GetMicAudio(); m != nil {
			out = append(out, m)
		}
	}
	return out
}

// --- tests -----------------------------------------------------------------

func TestWakeTurnPlaysReplyAndCleansUp(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	if !h.ctrl.Offer(turn.Trigger{ActivationID: "a1", Origin: capsulad.ActivationOriginWakeword,
		ModelID: "hey-capsula", QueuedAt: time.Now()}) {
		t.Fatal("activation refused")
	}
	waitFor(t, "session start", func() bool { return len(sessionStarts(h.link)) == 1 })
	st := sessionStarts(h.link)[0]
	if st.GetTrigger() != cadencev2.Trigger_TRIGGER_WAKE || st.GetWake().GetEngine() != "hey-capsula" {
		t.Fatalf("session start = %v", st)
	}
	waitFor(t, "microphone", func() bool { return h.audio.openOrder.Load() == 2 })
	if h.order.Load() != 2 || h.ring.openCount() != 1 {
		t.Fatalf("indication must open before the microphone: ring=%d mic=%d", h.ring.openCount(), h.order.Load())
	}
	// Give the controller a moment to arm the mic.
	time.Sleep(5 * time.Millisecond)
	h.audio.mic.send(frame(0x11))
	waitFor(t, "mic uplink", func() bool { return len(micSent(h.link)) >= 1 })
	m := micSent(h.link)[0]
	if m.GetSessionId() != st.GetSessionId() || m.GetSeq() != 0 {
		t.Fatalf("mic audio = %v", m)
	}
	if len(m.GetOpus()) != 640 {
		t.Fatalf("uplink packet = %d bytes", len(m.GetOpus()))
	}

	h.ctrl.Message(&cadencev2.Envelope{Body: &cadencev2.Envelope_StreamStart{StreamStart: &cadencev2.StreamStart{
		StreamId: "reply-1", SessionId: st.GetSessionId(),
		Purpose: cadencev2.StreamPurpose_STREAM_PURPOSE_REPLY, Codec: cadencev2.Codec_CODEC_OPUS, Channels: 1,
	}}})
	waitFor(t, "playback", func() bool { return h.audio.playback(0) != nil })
	reply := []byte{1, 2, 3, 4}
	h.ctrl.Message(&cadencev2.Envelope{Body: &cadencev2.Envelope_AudioDown{AudioDown: &cadencev2.AudioDown{
		StreamId: "reply-1", Seq: 0, Opus: reply,
	}}})
	h.ctrl.Message(&cadencev2.Envelope{Body: &cadencev2.Envelope_StreamEnd{StreamEnd: &cadencev2.StreamEnd{StreamId: "reply-1"}}})
	waitFor(t, "playback drained", func() bool {
		_, closed, _ := h.audio.playback(0).state()
		return closed
	})
	if _, _, aborted := h.audio.playback(0).state(); aborted {
		t.Fatal("a successful stream was aborted")
	}
	if !h.ring.saw("speaking") {
		t.Fatal("speaking indication missing")
	}

	h.ctrl.Message(&cadencev2.Envelope{Body: &cadencev2.Envelope_SessionClose{SessionClose: &cadencev2.SessionClose{
		SessionId: st.GetSessionId(), Code: "ok", FollowUpAllowed: false,
	}}})
	waitFor(t, "cleanup", func() bool {
		return h.audio.mic.closed.Load() && h.ring.closeCount() == 1 && h.audio.conv.closed.Load()
	})
	if n := len(sessionStarts(h.link)); n != 1 {
		t.Fatalf("follow-up started without permission: %d sessions", n)
	}
}

// While the reply plays the microphone stays open for a follow-up; its room
// level must not repaint the speaking indication as listening.
func TestSpeakingStateSurvivesOpenMicrophone(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.ctrl.Offer(turn.Trigger{ActivationID: "a1", Origin: capsulad.ActivationOriginWakeword, QueuedAt: time.Now()})
	waitFor(t, "session start", func() bool { return len(sessionStarts(h.link)) == 1 })
	id := sessionStarts(h.link)[0].GetSessionId()
	waitFor(t, "mic", func() bool { return h.audio.openOrder.Load() == 2 })

	h.ctrl.Message(&cadencev2.Envelope{Body: &cadencev2.Envelope_StreamStart{StreamStart: &cadencev2.StreamStart{
		StreamId: "r", SessionId: id, Purpose: cadencev2.StreamPurpose_STREAM_PURPOSE_REPLY, Channels: 1,
	}}})
	waitFor(t, "speaking", func() bool { return h.ring.lastState() == "speaking" })

	h.audio.mic.send(frame(0x33))
	time.Sleep(30 * time.Millisecond)
	if got := h.ring.lastState(); got != "speaking" {
		t.Fatalf("a capture chunk repainted the indication as %q", got)
	}
}

func TestDismissCancelsTurn(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.ctrl.Offer(turn.Trigger{ActivationID: "a1", Origin: capsulad.ActivationOriginWakeword, QueuedAt: time.Now()})
	waitFor(t, "session start", func() bool { return len(sessionStarts(h.link)) == 1 })
	h.ctrl.Dismiss()
	waitFor(t, "session cancel", func() bool {
		return h.link.count(func(e *cadencev2.Envelope) bool {
			return e.GetSessionCancel().GetReason() == "dismissed"
		}) == 1
	})
	waitFor(t, "cleanup", func() bool { return h.audio.mic.closed.Load() && h.ring.closeCount() == 1 })
}

func TestErrorCloseFailsWithoutFollowUp(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.ctrl.Offer(turn.Trigger{ActivationID: "a1", Origin: capsulad.ActivationOriginTouch, QueuedAt: time.Now()})
	waitFor(t, "session start", func() bool { return len(sessionStarts(h.link)) == 1 })
	id := sessionStarts(h.link)[0].GetSessionId()
	h.ctrl.Message(&cadencev2.Envelope{Body: &cadencev2.Envelope_SessionClose{SessionClose: &cadencev2.SessionClose{
		SessionId: id, Code: "stt_failed", FollowUpAllowed: true, FollowUpWindowMs: 3000,
		Failure: &cadencev2.TurnFailure{Code: "stt_failed", Stage: "stt"},
	}}})
	waitFor(t, "failed indication and cleanup", func() bool { return h.ring.saw("failed") && h.ring.closeCount() == 1 })
	if n := len(sessionStarts(h.link)); n != 1 {
		t.Fatalf("follow-up after a failed turn: %d sessions", n)
	}
}

func TestFollowUpReusesMicrophone(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.ctrl.Offer(turn.Trigger{ActivationID: "a1", Origin: capsulad.ActivationOriginWakeword, QueuedAt: time.Now()})
	waitFor(t, "session start", func() bool { return len(sessionStarts(h.link)) == 1 })
	first := sessionStarts(h.link)[0].GetSessionId()

	// A complete reply makes the turn follow-up eligible.
	h.ctrl.Message(&cadencev2.Envelope{Body: &cadencev2.Envelope_StreamStart{StreamStart: &cadencev2.StreamStart{
		StreamId: "r", SessionId: first, Purpose: cadencev2.StreamPurpose_STREAM_PURPOSE_REPLY, Channels: 1,
	}}})
	waitFor(t, "playback", func() bool { return h.audio.playback(0) != nil })
	h.ctrl.Message(&cadencev2.Envelope{Body: &cadencev2.Envelope_AudioDown{AudioDown: &cadencev2.AudioDown{
		StreamId: "r", Seq: 0, Opus: []byte{1},
	}}})
	h.ctrl.Message(&cadencev2.Envelope{Body: &cadencev2.Envelope_StreamEnd{StreamEnd: &cadencev2.StreamEnd{StreamId: "r"}}})
	waitFor(t, "drain", func() bool { _, closed, _ := h.audio.playback(0).state(); return closed })

	h.ctrl.Message(&cadencev2.Envelope{Body: &cadencev2.Envelope_SessionClose{SessionClose: &cadencev2.SessionClose{
		SessionId: first, Code: "ok", FollowUpAllowed: true, FollowUpWindowMs: 2000,
	}}})
	waitFor(t, "follow-up session", func() bool { return len(sessionStarts(h.link)) == 2 })
	second := sessionStarts(h.link)[1]
	if second.GetTrigger() != cadencev2.Trigger_TRIGGER_FOLLOW_UP {
		t.Fatalf("follow-up trigger = %v", second.GetTrigger())
	}
	if second.GetSessionId() == first {
		t.Fatal("follow-up reused the session id")
	}
	if h.audio.mic.closed.Load() {
		t.Fatal("follow-up closed the microphone")
	}
	if h.ring.closeCount() != 0 {
		t.Fatal("follow-up closed the indication")
	}

	// The follow-up window expiring ends the turn and tells the server.
	h.ctrl.Message(&cadencev2.Envelope{Body: &cadencev2.Envelope_SessionClose{SessionClose: &cadencev2.SessionClose{
		SessionId: second.GetSessionId(), Code: "ok",
	}}})
	waitFor(t, "cleanup", func() bool { return h.audio.mic.closed.Load() && h.ring.closeCount() == 1 })
}

func TestHoldToTalkReleaseSendsSilenceTail(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.ctrl.Offer(turn.Trigger{ActivationID: "a1", Origin: capsulad.ActivationOriginTouch, HoldToTalk: true,
		QueuedAt: time.Now()})
	waitFor(t, "mic", func() bool { return h.audio.openOrder.Load() == 2 })
	if len(sessionStarts(h.link)) != 1 || sessionStarts(h.link)[0].GetTrigger() != cadencev2.Trigger_TRIGGER_BUTTON {
		t.Fatalf("touch trigger = %v", sessionStarts(h.link))
	}
	h.audio.mic.send(frame(0x22))
	waitFor(t, "first mic packet", func() bool { return len(micSent(h.link)) >= 1 })

	before := len(micSent(h.link))
	h.ctrl.Release("a1")
	waitFor(t, "microphone closed", func() bool { return h.audio.mic.closed.Load() })
	waitFor(t, "silence tail", func() bool { return len(micSent(h.link)) > before+2 })
	for _, m := range micSent(h.link)[before:] {
		if len(m.GetOpus()) != 640 || !allZero(m.GetOpus()) {
			t.Fatalf("tail packet is not digital silence: %d bytes", len(m.GetOpus()))
		}
	}
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// An API activation is an automation trigger, not a physical owner signal:
// cadence.v2 vouches owner privileges only for a wake word or a press, so this
// client refuses it rather than fabricating a press.
func TestAPIActivationRefused(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.ctrl.Offer(turn.Trigger{ActivationID: "a1", Origin: capsulad.ActivationOriginAPI, QueuedAt: time.Now()})
	time.Sleep(50 * time.Millisecond)
	if n := len(sessionStarts(h.link)); n != 0 {
		t.Fatalf("API activation opened %d sessions", n)
	}
	if h.ring.openCount() != 0 {
		t.Fatal("API activation opened an indication")
	}
}

func TestExpiredActivationDropped(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.ctrl.Offer(turn.Trigger{ActivationID: "a1", Origin: capsulad.ActivationOriginWakeword,
		QueuedAt: time.Now().Add(-time.Second), Expires: 100 * time.Millisecond})
	time.Sleep(50 * time.Millisecond)
	if n := len(sessionStarts(h.link)); n != 0 {
		t.Fatalf("expired activation opened %d sessions", n)
	}
}

func TestLinkDownCancelsLocalTurn(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.ctrl.Offer(turn.Trigger{ActivationID: "a1", Origin: capsulad.ActivationOriginWakeword, QueuedAt: time.Now()})
	waitFor(t, "session start", func() bool { return len(sessionStarts(h.link)) == 1 })
	h.link.setUp(false)
	h.ctrl.LinkDown()
	waitFor(t, "cleanup", func() bool { return h.audio.mic.closed.Load() && h.ring.closeCount() == 1 })
}

func TestQueuedReleaseEndsInputWhenTheTurnBegins(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	// A quick hold-to-talk press released before its queued activation began.
	h.ctrl.Release("a1")
	time.Sleep(10 * time.Millisecond)
	h.ctrl.Offer(turn.Trigger{ActivationID: "a1", Origin: capsulad.ActivationOriginTouch, HoldToTalk: true,
		QueuedAt: time.Now()})
	waitFor(t, "microphone", func() bool { return h.audio.openOrder.Load() == 2 })
	waitFor(t, "release applied", func() bool { return h.audio.mic.closed.Load() && len(micSent(h.link)) > 0 })
}

func TestMicOpenFailureCancelsSession(t *testing.T) {
	h := newHarness(t)
	h.audio.micErr = errors.New("FAILED_PRECONDITION: microphone capture requires a selected visible ring or segment indication")
	cancel := h.run(t)
	defer cancel()

	h.ctrl.Offer(turn.Trigger{ActivationID: "a1", Origin: capsulad.ActivationOriginWakeword, QueuedAt: time.Now()})
	waitFor(t, "session cancel", func() bool {
		return h.link.count(func(e *cadencev2.Envelope) bool {
			return e.GetSessionCancel().GetReason() == "mic_failed"
		}) == 1
	})
	waitFor(t, "cleanup", func() bool { return h.ring.closeCount() == 1 })
}
