// Dumb assistant is a first-party optional voice assistant. It claims the
// voice_assistant role, but performs no recognition: after the user finishes
// speaking it plays the same packaged WAV reply after a configurable delay.
//
// The microphone is consumed through the framework's normal PCM capture API.
// While listening, the whole ring glows teal with brightness tracking the real
// input level. When speech ends, the ring smoothly becomes a blue chase for the
// delay and the reply. A hold-to-talk release ends the turn, exactly like the
// assistant providers shipped with the framework.
//
// It is deliberately not a system app. The package is installed and selected
// like any other supervised application; the daemon and its bundled providers
// are unchanged.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"os"
	"os/signal"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"capsulad/sdk/capsulad"
)

const (
	outputEndpoint = "audio.output.speaker"
	ringResource   = "light.ring.main"

	// The package ships the default reply beside the executable. A configured
	// reply_asset comes from the framework's shared read-only asset store
	// instead; CAPSULAD_ASSETS names that store (/assets in the sandbox, the
	// host assets directory for unsandboxed runs).
	defaultReplyFile  = "reply.wav"
	assetStoreEnv     = "CAPSULAD_ASSETS"
	defaultAssetStore = "/assets"

	chunkMS    = 20
	queueDepth = 8
	maxDelayMS = 60000

	ringPixels       = 12
	ringFramePeriod  = 33 * time.Millisecond
	ringFadePeriod   = 350 * time.Millisecond
	chasePeriod      = 1080 * time.Millisecond
	chaseHeadPixels  = 3.0
	levelFloor       = 0.04
	levelRiseSeconds = 0.06
	levelFallSeconds = 0.22
	minLevelDB       = -60.0

	// Speech is measured from real captured PCM in the 300-3400 Hz band, where a
	// voice lives and a room's rumble does not. An onset needs speechOnHold of
	// modulated band time; an utterance ends when the band level has fallen
	// clearly away from the voice for endSilence. Floors are learned, so these
	// values are the digital-silence boundaries.
	speechOnHold  = 60 * time.Millisecond
	endSilence    = 700 * time.Millisecond
	minSpeech     = 140 * time.Millisecond
	maxSpeech     = 10 * time.Second
	noSpeechLimit = 3 * time.Second
	listenTick    = 50 * time.Millisecond
)

var (
	// teal is the listening state (#008080); blue is the delay/answer chase.
	teal = capsulad.RGB{G: 128, B: 128}
	blue = capsulad.RGB{G: 64, B: 255}
)

// delaySpec is one configured reply delay. A zero spec means no wait.
type delaySpec struct {
	minMS int64
	maxMS int64
}

func (d delaySpec) isZero() bool { return d.minMS == 0 && d.maxMS == 0 }

func (d delaySpec) String() string {
	switch {
	case d.isZero():
		return "none"
	case d.minMS == d.maxMS:
		return fmt.Sprintf("%dms", d.minMS)
	default:
		return fmt.Sprintf("%d-%dms", d.minMS, d.maxMS)
	}
}

// delay returns the wait before one reply. The caller serializes responses, so
// a single rand.Rand is safe here. The range is inclusive at both bounds.
func (d delaySpec) delay(rng *rand.Rand) time.Duration {
	if d.maxMS <= 0 {
		return 0
	}
	if d.maxMS == d.minMS {
		return time.Duration(d.minMS) * time.Millisecond
	}
	return time.Duration(d.minMS+rng.Int64N(d.maxMS-d.minMS+1)) * time.Millisecond
}

// parseDelay accepts the single delay field grammar:
//
//	""              no delay
//	"100"           100 ms constant
//	"200-1500"      uniformly random 200..1500 ms, inclusive
//
// Bounds are decimal milliseconds, never negative, never above maxDelayMS, and
// an explicit range must ascend. Everything else is rejected with a reason.
func parseDelay(raw string) (delaySpec, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return delaySpec{}, nil
	}
	parts := strings.Split(text, "-")
	switch len(parts) {
	case 1:
		ms, err := parseDelayMS(parts[0])
		if err != nil {
			return delaySpec{}, err
		}
		return delaySpec{minMS: ms, maxMS: ms}, nil
	case 2:
		minMS, err := parseDelayMS(strings.TrimSpace(parts[0]))
		if err != nil {
			return delaySpec{}, err
		}
		maxMS, err := parseDelayMS(strings.TrimSpace(parts[1]))
		if err != nil {
			return delaySpec{}, err
		}
		if minMS > maxMS {
			return delaySpec{}, fmt.Errorf("minimum %d ms is greater than maximum %d ms", minMS, maxMS)
		}
		return delaySpec{minMS: minMS, maxMS: maxMS}, nil
	default:
		return delaySpec{}, fmt.Errorf("expected empty, one value, or an ascending minimum-maximum range")
	}
}

func parseDelayMS(text string) (int64, error) {
	if text == "" {
		return 0, fmt.Errorf("missing delay bound")
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("delay %q is not a whole number of milliseconds", text)
		}
	}
	ms, err := strconv.ParseInt(text, 10, 64)
	if err != nil || ms > maxDelayMS {
		return 0, fmt.Errorf("delay %q exceeds the %d ms limit", text, maxDelayMS)
	}
	return ms, nil
}

type clip struct {
	format        capsulad.AudioFormat
	pcm           []byte
	bytesPerFrame int
}

func (c *clip) frames() int { return len(c.pcm) / c.bytesPerFrame }

type reply struct {
	activationID string
	origin       capsulad.ActivationOrigin
	originName   string
	// holdToTalk is the per-activation rule from the framework: the release
	// event ends input instead of speech-end detection. It is carried by the
	// activation so the app never needs the administrator-only settings API.
	holdToTalk   bool
	activationNS uint64
	queuedAt     time.Time
	expiresAfter time.Duration
}

type assistantSettings struct {
	delay     delaySpec
	assetText string
	replyPath string
	clip      *clip
}

type turn struct {
	activationID string
	release      chan struct{}
	releaseOnce  sync.Once
	cancel       context.CancelFunc
}

func (t *turn) signalRelease() {
	t.releaseOnce.Do(func() { close(t.release) })
}

type assistant struct {
	ctx        context.Context
	client     *capsulad.Client
	rng        *rand.Rand
	pkgRoot    string
	assetStore string

	mu             sync.Mutex
	settings       assistantSettings
	active         *turn
	pendingRelease string

	queue chan reply
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	env := capsulad.FromEnv()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	root, err := packageRoot(os.Args[0])
	if err != nil {
		log.Fatalf("dumbassistant: %v", err)
	}
	clip, path, err := loadPackagedReply(root)
	if err != nil {
		log.Fatalf("dumbassistant: %v", err)
	}
	store := strings.TrimSpace(os.Getenv(assetStoreEnv))
	if store == "" || !filepath.IsAbs(store) {
		store = defaultAssetStore
	}
	store = filepath.Clean(store)
	client, err := capsulad.Connect(ctx, env)
	if err != nil {
		log.Fatalf("dumbassistant: connect: %v", err)
	}
	defer client.Close()

	a := &assistant{
		ctx: ctx, client: client, pkgRoot: root, assetStore: store,
		rng: rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(time.Now().UnixNano()>>32))),
		settings: assistantSettings{
			delay:     delaySpec{minMS: 1000, maxMS: 1000},
			replyPath: path, clip: clip,
		},
		queue: make(chan reply, queueDepth),
	}
	client.SetHandlers(capsulad.Handlers{
		Ready: func() {
			log.Printf("dumbassistant: ready; reply %d frames at %d Hz/%d ch",
				clip.frames(), clip.format.SampleRateHz, clip.format.Channels)
		},
		Activation: func(_ context.Context, act *capsulad.Activation) {
			a.enqueue(act)
		},
		TouchRelease: func(rel *capsulad.InputReleased) {
			a.releaseInput(rel)
		},
		Settings: a.applySettings,
		Action: func(_ context.Context, act *capsulad.ActionInvoked) {
			if act == nil || act.ActionID != dismissActionID {
				return
			}
			a.dismiss()
		},
		Invocation: func(_ context.Context, inv *capsulad.LaunchInvocation) {
			log.Printf("dumbassistant: invocation %s", inv.ID)
			_ = client.NotifyInvocationResult(inv.ID, true, "dumb assistant is always ready")
		},
	})

	go a.run()
	log.Printf("dumbassistant: starting bus")
	if err := client.Start(ctx); err != nil && ctx.Err() == nil {
		log.Printf("dumbassistant: bus stopped: %v", err)
	}
}

func (a *assistant) enqueue(act *capsulad.Activation) {
	if act == nil {
		return
	}
	if act.ID == "" {
		// Every turn is matched to the framework's release event by this ID.
		// An unidentified activation could otherwise collide with the empty
		// "no pending release" sentinel and be released before it listens.
		log.Printf("dumbassistant: activation without an id dropped origin=%s", act.Origin.String())
		return
	}
	item := reply{
		activationID: act.ID,
		origin:       act.Origin,
		originName:   act.Origin.String(),
		holdToTalk:   act.HoldToTalk,
		activationNS: act.MonotonicTimeNS,
		queuedAt:     time.Now(),
	}
	if act.ExpiresMonotonicNS > act.MonotonicTimeNS {
		item.expiresAfter = time.Duration(act.ExpiresMonotonicNS - act.MonotonicTimeNS)
	}
	select {
	case a.queue <- item:
		log.Printf("dumbassistant: activation=%s origin=%s queued", item.activationID, item.originName)
	default:
		log.Printf("dumbassistant: activation=%s origin=%s dropped; reply queue is full", item.activationID, item.originName)
	}
}

func (a *assistant) releaseInput(rel *capsulad.InputReleased) {
	if rel == nil || rel.ActivationID == "" {
		return
	}
	a.mu.Lock()
	if a.active != nil && a.active.activationID == rel.ActivationID {
		active := a.active
		a.mu.Unlock()
		active.signalRelease()
		return
	}
	// A quick hold-to-talk press can be released while an earlier reply is
	// still queued. Remember it so the turn does not fall back to a silence
	// window when it starts.
	a.pendingRelease = rel.ActivationID
	a.mu.Unlock()
}

func (a *assistant) setActive(t *turn) {
	a.mu.Lock()
	a.active = t
	if a.pendingRelease != "" && a.pendingRelease == t.activationID {
		a.pendingRelease = ""
		t.signalRelease()
	}
	a.mu.Unlock()
}

func (a *assistant) clearActive(t *turn) {
	a.mu.Lock()
	if a.active == t {
		a.active = nil
	}
	a.mu.Unlock()
}

func (a *assistant) run() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case item := <-a.queue:
			a.answer(item)
		}
	}
}

func (a *assistant) answer(item reply) {
	if item.expiresAfter > 0 && time.Since(item.queuedAt) >= item.expiresAfter {
		log.Printf("dumbassistant: activation=%s expired before its turn; dropped", item.activationID)
		return
	}

	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()

	t := &turn{activationID: item.activationID, release: make(chan struct{}), cancel: cancel}
	a.setActive(t)
	defer a.clearActive(t)

	ring, err := a.openRing(ctx, item)
	if err != nil {
		log.Printf("dumbassistant: activation=%s cancelled: %v", item.activationID, err)
		return
	}
	defer ring.close()

	conv, conversationID := a.beginConversation(ctx, item.activationID)
	if conv != nil {
		defer conv.Close()
	}

	if !a.listen(ctx, item, t, ring, conversationID) {
		log.Printf("dumbassistant: activation=%s cancelled before reply", item.activationID)
		return
	}

	spec, clip := a.nextReply()
	wait := spec.delay(a.rng)
	ring.beginDelay()
	log.Printf("dumbassistant: activation=%s reply in %s after speech end", item.activationID, wait)
	if !sleep(ctx, wait) {
		return
	}

	playback, err := a.client.OpenPlayback(ctx, outputEndpoint, clip.format, conversationID)
	if err != nil {
		log.Printf("dumbassistant: playback for activation=%s: %v", item.activationID, err)
		return
	}
	defer playback.Close()

	log.Printf("dumbassistant: playing reply for activation=%s conversation=%s", item.activationID, conversationID)
	if err := a.play(ctx, playback, clip); err != nil {
		_ = playback.Abort()
		log.Printf("dumbassistant: reply for activation=%s failed: %v", item.activationID, err)
		return
	}
	log.Printf("dumbassistant: reply for activation=%s finished", item.activationID)
}

// dismissActionID is the manifest-declared action the framework invokes when a
// physical key dismisses the current turn. It cancels listening, the reply
// delay and playback in one step; the deferred teardown releases the ring,
// microphone, conversation and playback streams.
const dismissActionID = "dismiss"

// dismiss cancels the active turn and drops activations that were queued but
// not started, so a dismissed assistant cannot answer a stale request later.
func (a *assistant) dismiss() {
	a.mu.Lock()
	active := a.active
	a.pendingRelease = ""
	a.mu.Unlock()
	if active == nil || active.cancel == nil {
		log.Printf("dumbassistant: dismiss ignored; no active turn")
		return
	}
	log.Printf("dumbassistant: activation=%s dismissed", active.activationID)
	active.cancel()
	for {
		select {
		case item := <-a.queue:
			log.Printf("dumbassistant: activation=%s dropped; dismissed", item.activationID)
		default:
			return
		}
	}
}

func (a *assistant) beginConversation(ctx context.Context, activationID string) (*capsulad.Conversation, string) {
	conv, err := a.client.BeginConversation(ctx, activationID)
	if err != nil {
		// Capture and playback still work; the mixer simply will not duck
		// other audio for this turn.
		log.Printf("dumbassistant: conversation for activation=%s: %v", activationID, err)
		return nil, ""
	}
	return conv, conv.ID()
}

// listen consumes real microphone PCM until the utterance ends. It returns
// false when the turn must be cancelled: mute, watchdog, daemon shutdown, a
// closed stream, or silence with no speech at all. Hold-to-talk turns end on
// the framework's release event instead of a silence window, but the no-speech
// watchdog still applies: a release that is never delivered must not leave the
// ring and microphone claimed for the full listen cap.
func (a *assistant) listen(ctx context.Context, item reply, t *turn, ring *ringView, conversationID string) bool {
	holdToTalk := item.origin == capsulad.ActivationOriginTouch && item.holdToTalk

	micCtx, micCancel := context.WithCancel(ctx)
	mic, err := a.client.OpenMic(micCtx, item.activationID, conversationID)
	if err != nil {
		micCancel()
		log.Printf("dumbassistant: microphone for activation=%s: %v", item.activationID, err)
		return false
	}
	defer func() {
		micCancel()
		_ = mic.Close()
		<-mic.Done()
	}()
	log.Printf("dumbassistant: activation=%s microphone listening", item.activationID)

	tracker := &speechTracker{}
	rate := 16000
	started := time.Now()
	lastVoice := started
	lastGapLog := time.Time{}
	ticker := time.NewTicker(listenTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("dumbassistant: activation=%s cancelled while listening", item.activationID)
			return false

		case <-t.release:
			log.Printf("dumbassistant: activation=%s talk button released", item.activationID)
			return true

		case <-ticker.C:
			// The no-speech window applies to hold-to-talk turns too: if the
			// release event is lost, an otherwise silent turn must not keep
			// the ring and microphone claimed for the full listen cap.
			if !tracker.active() && time.Since(lastVoice) >= noSpeechLimit {
				log.Printf("dumbassistant: activation=%s no speech within %s; cancelled", item.activationID, noSpeechLimit)
				return false
			}
			if time.Since(started) >= maxSpeech {
				log.Printf("dumbassistant: activation=%s listen window elapsed after %s; cancelled", item.activationID, maxSpeech)
				return false
			}

		case chunk, ok := <-mic.Chunks():
			if !ok {
				log.Printf("dumbassistant: activation=%s microphone stream ended; cancelled", item.activationID)
				return false
			}
			if chunk.Notice != nil {
				log.Printf("dumbassistant: activation=%s microphone notice %s: %s",
					item.activationID, chunk.Notice.Code, chunk.Notice.Detail)
				if chunk.Notice.Code == "CLIENT_TOO_SLOW" {
					tracker.reset()
					continue
				}
				return false
			}
			if chunk.Format != nil && chunk.Format.SampleRateHz > 0 {
				rate = int(chunk.Format.SampleRateHz)
			}
			if chunk.Chunk == nil {
				continue
			}
			c := chunk.Chunk
			if c.Discontinuity || c.DroppedFramesBefore > 0 {
				// A gap is not evidence that the utterance ended. Reset the
				// classifier and keep listening; the no-speech window and the
				// safety cap still bound the turn, so dropped audio can never
				// produce a premature reply. Log at most twice a second: a
				// backlog can carry hundreds of gap markers, and a line per
				// chunk slows this consumer enough to overflow the transport
				// buffer instead of draining it.
				if time.Since(lastGapLog) >= 500*time.Millisecond {
					lastGapLog = time.Now()
					log.Printf("dumbassistant: activation=%s audio gap dropped=%d; continuing", item.activationID, c.DroppedFramesBefore)
				}
				tracker.reset()
				continue
			}

			rms := pcmRMS(c.PCM)
			if ring != nil {
				ring.observe(levelFromRMS(rms))
			}
			feed := speechFeedLive
			if isPreroll(item, c) {
				// Pre-activation audio is the room, not this utterance. Only
				// the oldest part of the window is guaranteed to precede the
				// wakeword/summons, so a late phrase cannot be folded into the
				// noise floor it must exceed. The rest is still filtered so the
				// band state stays continuous across the activation.
				feed = speechFeedWarm
				if earlyPreroll(item, c) {
					feed = speechFeedCalibrate
				}
			}
			events := tracker.observe(c.PCM, rate, feed)
			if events.Rejected {
				log.Printf("dumbassistant: activation=%s steady room noise; speech hypothesis rejected", item.activationID)
			}
			// Only confirmed speech keeps the no-speech window open: a rejected
			// noise hypothesis must not extend the turn.
			if events.Started || tracker.confirmedSpeech() {
				lastVoice = time.Now()
			}
			if events.Started {
				log.Printf("dumbassistant: activation=%s speech detected", item.activationID)
			}
			if events.Ended && !holdToTalk {
				log.Printf("dumbassistant: activation=%s speech ended", item.activationID)
				return true
			}
		}
	}
}

func isPreroll(item reply, c *capsulad.AudioChunk) bool {
	return item.activationNS != 0 && c.MonotonicTimeNS != 0 && c.MonotonicTimeNS < item.activationNS
}

// earlyPreroll selects the part of the pre-roll that predates the wakeword or
// summons. For a wakeword activation the phrase itself is part of the last
// second, so folding the whole window into the noise floor would teach the
// tracker that speech is background. The guard also absorbs chunk duration.
func earlyPreroll(item reply, c *capsulad.AudioChunk) bool {
	const guard = uint64(400 * time.Millisecond)
	const chunk = uint64(20 * time.Millisecond)
	if item.activationNS == 0 || c.MonotonicTimeNS == 0 {
		return true
	}
	return c.MonotonicTimeNS+chunk+guard <= item.activationNS
}

// The framework's processed application tap already removes sub-80 Hz rumble,
// but a real room still carries most of its energy below 300 Hz: a fan, an air
// conditioner or traffic is a loud, slowly wandering low-frequency roar. A
// full-band energy or zero-crossing test cannot tell that room from a voice,
// and a "stationary" test that requires a flat broadband background never fires
// on it. One unannotated 10 s ambient capture from the device microphone (not a
// speech-absence guarantee) measured 76% of its energy below 300 Hz, with a
// 300-3400 Hz band level that swung by tens of percent and a zero-crossing rate
// far below any broadband threshold, which is exactly why the old escape never
// engaged.
//
// The assistant therefore measures only the speech band, 300-3400 Hz, and
// decides by modulation: speech alternates voiced syllables and consonants, so
// the band level moves by tens of percent within a few hundred milliseconds,
// while fans, hums, hisses and tones hold their band level steady however loud
// they are. On that ambient capture the band level's 160 ms relative deviation
// was 0.11 median and 0.19 at the 90th percentile; realistic speech mixed over
// the same capture measured 0.33-0.59.
//
// One band-pass, one learned floor and one modulation window drive onset, and
// the same recent-voice level decides the end: a voice that keeps its level
// (a sustained vowel) is preserved, while a room that returns is far below it.
// Everything is integrated in sample time over fixed 20 ms analysis frames
// assembled from the stream, never from transport chunk boundaries.
const (
	// Capture evidence boundaries. speechBandFloorMin is the digital-silence
	// floor, so quiet speech is still detected with no measurable background.
	// speechBandOnset is how far the band must rise above the learned room:
	// the ambient capture peaked at 2.2x its own 20th percentile, while a voice
	// near the device exceeded 2.7x.
	speechBandHighHz   = 300.0
	speechBandLowHz    = 3400.0
	speechBandFloorMin = 0.0012
	speechBandFloorMax = 0.6
	// Two gates. An onset only needs a modulated rise above the room
	// (speechBandOnset); utterance evidence needs a clearly voiced level
	// (speechBandVoice). Keeping them separate lets a quiet word open a
	// hypothesis without making the room's own fluctuation count as voiced
	// time, which is what would confirm a transient.
	speechBandOnset = 1.25
	speechBandVoice = 1.6

	// Floor adaptation is deliberately symmetric and slow. A fast downward
	// rate once ratcheted the floor toward the room's quietest frames until the
	// room's own level sat above the onset gate; a measured room then confirmed
	// itself on a transient and answered it. The floor follows the room, it
	// does not chase its minima.
	speechBandFloorDown = 0.08
	speechBandFloorUp   = 0.08
	speechBandCold      = 160 * time.Millisecond
	speechBandColdRate  = 0.4

	// Speech evidence: 160 ms of sample time, and the relative mean absolute
	// deviation of the band level that separates a speaking voice from the
	// loudest ambient capture we have.
	modulationWindow    = 8
	modulationDeviation = 0.30

	// End of speech: the band level must fall well below the recent voiced
	// level (7 dB) and hold there. Syllable dips inside a word are shallower
	// than that, so they cannot end a word; a room that returns after an
	// utterance is far below it. speechReferenceRate follows the voiced level
	// so a voice that rises or falls slowly does not look like an offset.
	tailDrop            = 0.72
	speechReferenceRate = 0.2

	// Two independent kinds of utterance evidence. Articulated speech supplies
	// minSpeech of modulated band time. A continuously voiced vowel supplies no
	// modulation but a steady level clearly above the room for a longer hold:
	// a short unmodulated room event is a bump, while a vowel is sustained by
	// definition.
	steadyVoiceFloor     = 2.5
	steadyVoiceDeviation = 0.10
	steadyVoiceHold      = 400 * time.Millisecond

	// A short voiced event only counts as a word when it was articulated: at
	// least this much of its voiced time moved the band level. A flat room bump
	// carries none, while a quiet word still carries syllable structure.
	speechModulationHold = 40 * time.Millisecond

	// speechAnalysisMS is the fixed analysis frame. The processed tap delivers
	// 20 ms chunks; assembling frames of the same duration makes onset and end
	// independent of how many chunks one transport read returned.
	speechAnalysisMS = 20
)

// biquad is one Direct Form I second-order section. Two sections per edge give
// a 4th-order Butterworth band edge, which removes the room roar before the
// modulation test sees it.
type biquad struct {
	b0, b1, b2, a1, a2 float64
	x1, x2, y1, y2     float64
}

func (b *biquad) process(x float64) float64 {
	y := b.b0*x + b.b1*b.x1 + b.b2*b.x2 - b.a1*b.y1 - b.a2*b.y2
	b.x2, b.x1 = b.x1, x
	b.y2, b.y1 = b.y1, y
	return y
}

func highPassSection(rate, freq, q float64) biquad {
	w0 := 2 * math.Pi * freq / rate
	alpha := math.Sin(w0) / (2 * q)
	cos := math.Cos(w0)
	a0 := 1 + alpha
	return biquad{
		b0: (1 + cos) / 2 / a0, b1: -(1 + cos) / a0, b2: (1 + cos) / 2 / a0,
		a1: -2 * cos / a0, a2: (1 - alpha) / a0,
	}
}

func lowPassSection(rate, freq, q float64) biquad {
	w0 := 2 * math.Pi * freq / rate
	alpha := math.Sin(w0) / (2 * q)
	cos := math.Cos(w0)
	a0 := 1 + alpha
	return biquad{
		b0: (1 - cos) / 2 / a0, b1: (1 - cos) / a0, b2: (1 - cos) / 2 / a0,
		a1: -2 * cos / a0, a2: (1 - alpha) / a0,
	}
}

// speechFeed says what to do with captured samples. Pre-activation audio still
// runs through the band filter so the filter state is continuous, but it may
// only calibrate the floor (the oldest part of the window) or be discarded (the
// wakeword tail).
type speechFeed uint8

const (
	speechFeedLive speechFeed = iota
	speechFeedCalibrate
	speechFeedWarm
)

// speechEvents reports the decisions produced while consuming one chunk. A
// chunk can complete several analysis frames, but a single frame starts or
// finishes at most one hypothesis.
type speechEvents struct {
	Started  bool
	Ended    bool
	Rejected bool
}

// speechTracker classifies the processed microphone stream in the speech band.
type speechTracker struct {
	rate  int
	frame int // samples per analysis frame
	fill  int
	sum   float64 // band energy accumulated in the current frame

	hp1, hp2, lp1, lp2 biquad

	// floor is the learned band level of the room.
	floor float64
	cold  time.Duration
	armed bool

	// Rolling band-level window for the modulation test.
	window     [modulationWindow]float64
	windowN    int
	windowNext int

	// ref is the recent articulated speech level; tailLevel is the level the
	// rejected tail settled at, folded into the floor on a noise rejection.
	ref       float64
	tailLevel float64

	// coldOnset permits a level-only onset when no pre-roll was ever seen: the
	// first sound of a turn must not be swallowed by its own cold floor.
	coldOnset bool

	onRun     time.Duration
	speaking  bool
	speech    time.Duration // voiced band time above the room
	speechMod time.Duration // modulated portion of it
	steady    time.Duration // steady voiced band time, for vowels
	tail      time.Duration
	confirmed bool

	// Pre-roll calibration ring: bounded and deliberately a low percentile so
	// a late word cannot dominate it.
	calValues [64]float64
	calCount  int
	calNext   int
}

// observe consumes captured PCM. It returns the decisions for every analysis
// frame the chunk completed. Sample rate changes (or a fresh tracker) rebuild
// the filters and forget the floor; a stream keeps a fixed format for a turn.
func (t *speechTracker) observe(pcm []byte, rate int, feed speechFeed) (ev speechEvents) {
	if rate <= 0 {
		rate = 16000
	}
	if rate != t.rate || t.frame == 0 {
		t.configure(rate)
	}
	for offset := 0; offset+1 < len(pcm); offset += 2 {
		sample := float64(int16(binary.LittleEndian.Uint16(pcm[offset:offset+2]))) / 32768
		band := t.lp2.process(t.lp1.process(t.hp2.process(t.hp1.process(sample))))
		t.sum += band * band
		t.fill++
		if t.fill < t.frame {
			continue
		}
		level := math.Sqrt(t.sum / float64(t.frame))
		t.sum, t.fill = 0, 0
		// The modulation window is sample-time state, so pre-activation audio
		// warms it: the first live frame is judged against the room, not
		// against an empty window.
		t.pushWindow(level)
		switch feed {
		case speechFeedCalibrate:
			t.calibrate(level)
		case speechFeedWarm:
		default:
			t.finishCalibration()
			t.step(level, &ev)
		}
	}
	return ev
}

func (t *speechTracker) configure(rate int) {
	*t = speechTracker{rate: rate}
	t.frame = rate * speechAnalysisMS / 1000
	if t.frame < 1 {
		t.frame = 1
	}
	// Butterworth 4th order as two cascaded sections per edge.
	t.hp1 = highPassSection(float64(rate), speechBandHighHz, 0.5412)
	t.hp2 = highPassSection(float64(rate), speechBandHighHz, 1.3066)
	t.lp1 = lowPassSection(float64(rate), speechBandLowHz, 0.5412)
	t.lp2 = lowPassSection(float64(rate), speechBandLowHz, 1.3066)
}

// calibrate records a pre-activation band level. finishCalibration turns the
// ring into a floor; no live frame is ever calibrated.
func (t *speechTracker) calibrate(level float64) {
	if math.IsNaN(level) || math.IsInf(level, 0) || level < 0 {
		return
	}
	t.calValues[t.calNext] = level
	t.calNext = (t.calNext + 1) % len(t.calValues)
	if t.calCount < len(t.calValues) {
		t.calCount++
	}
}

// finishCalibration freezes the pre-roll floor. The 30th percentile keeps the
// wakeword tail or a door slam from dominating the window; the factor restores
// roughly the mean of the room's band level.
func (t *speechTracker) finishCalibration() {
	if t.armed || t.calCount == 0 {
		return
	}
	sort.Float64s(t.calValues[:t.calCount])
	idx := t.calCount * 3 / 10
	t.floor = clampBandFloor(t.calValues[idx] * 1.1)
	t.armed = true
	t.cold = speechBandCold
	t.coldOnset = false
	t.calCount = 0
}

func clampBandFloor(level float64) float64 {
	if level < 0.0002 {
		return 0.0002
	}
	if level > speechBandFloorMax {
		return speechBandFloorMax
	}
	return level
}

func (t *speechTracker) active() bool { return t.speaking }

// confirmedSpeech reports whether enough modulated speech has been seen in this
// hypothesis to answer. Only confirmed speech keeps the turn's no-speech window
// from expiring: a rejected noise hypothesis must not extend a turn.
func (t *speechTracker) confirmedSpeech() bool { return t.confirmed }

// reset forgets the stream and any hypothesis but keeps the learned floor and
// an in-progress pre-roll calibration: a capture gap is not evidence that the
// room changed, and relearning the floor from the first 160 ms after a gap
// could absorb a voice already in progress. The filters are rebuilt at the
// same rate (a zero rate would make the next chunk cold-start and discard the
// pre-roll calibration) and the modulation window is cleared, so no audio is
// spliced across the gap.
func (t *speechTracker) reset() {
	rate := t.rate
	floor, armed, cold, coldOnset := t.floor, t.armed, t.cold, t.coldOnset
	calValues, calCount, calNext := t.calValues, t.calCount, t.calNext
	if rate > 0 {
		t.configure(rate)
	} else {
		*t = speechTracker{}
	}
	t.floor, t.armed, t.cold, t.coldOnset = floor, armed, cold, coldOnset
	t.calValues, t.calCount, t.calNext = calValues, calCount, calNext
}

// resetDetection ends the current hypothesis but keeps the learned floor and the
// recent spectral window for the rest of the turn.
func (t *speechTracker) resetDetection() {
	t.speaking = false
	t.onRun = 0
	t.speech = 0
	t.speechMod = 0
	t.steady = 0
	t.tail = 0
	t.confirmed = false
	t.ref = 0
	t.tailLevel = 0
}

// adaptFloor follows the room's band level. Upward tracking only runs on
// unmodulated audio, so a word is never learned as background.
func (t *speechTracker) adaptFloor(level float64) {
	rate := speechBandFloorUp
	switch {
	case t.floor <= 0:
		rate = 1
	case t.cold < speechBandCold:
		rate = speechBandColdRate
	case level < t.floor:
		rate = speechBandFloorDown
	}
	if t.floor <= 0 {
		t.floor = level
	} else {
		t.floor += (level - t.floor) * rate
	}
	t.floor = clampBandFloor(t.floor)
}

// pushWindow appends one analysis frame's band level to the modulation window.
func (t *speechTracker) pushWindow(level float64) {
	t.window[t.windowNext] = level
	t.windowNext = (t.windowNext + 1) % modulationWindow
	if t.windowN < modulationWindow {
		t.windowN++
	}
}

// step classifies one analysis frame of sample time.
func (t *speechTracker) step(level float64, ev *speechEvents) {
	frame := time.Duration(t.frame) * time.Second / time.Duration(t.rate)

	// Mean band level and its relative mean absolute deviation over the window.
	// Level and slow drift cancel out, so what remains is modulation.
	mean := 0.0
	for i := range t.windowN {
		mean += t.window[i]
	}
	mean /= float64(t.windowN)
	deviation := 0.0
	if mean > 0 {
		for i := range t.windowN {
			deviation += math.Abs(t.window[i] - mean)
		}
		deviation /= float64(t.windowN) * mean
	}

	if !t.armed {
		// No usable pre-roll: the first speechBandCold of audio establishes the
		// background from its quietest frame. A loud turn start is therefore not
		// learned, and the classifier never runs without a floor.
		t.cold = min(t.cold+frame, speechBandCold)
		// Do not arm a floor from a loud first 160 ms: a turn can open on a
		// voice already in progress, and a floor of 1.1x that voice would hide
		// it. The cold floor stays at the digital-silence value and a level-only
		// onset is allowed once it arms.
		t.floor = speechBandFloorMin
		t.coldOnset = true
		if t.cold >= speechBandCold {
			t.armed = true
			t.cold = 0
		}
		return
	}

	onsetGate := math.Max(speechBandFloorMin, t.floor*speechBandOnset)
	voiceGate := math.Max(speechBandFloorMin, t.floor*speechBandVoice)
	voiced := level > voiceGate
	modulated := level > onsetGate && t.windowN >= modulationWindow && deviation >= modulationDeviation

	// While listening, unmodulated audio is the room, even when it is loud: a
	// fan or an air conditioner must be learned so the onset gate can rise
	// above it. Articulated speech is never learned because its band level is
	// modulated, and during a hypothesis only the tail region is learned, so a
	// sustained vowel cannot be absorbed as background.
	// While listening, unmodulated audio is the room. A cold level onset is the
	// exception: do not learn the level that is about to become the utterance.
	if !t.speaking && !modulated && !(t.coldOnset && voiced) {
		t.adaptFloor(level)
		t.cold = min(t.cold+frame, speechBandCold)
	}

	if t.speaking {
		// Voiced band time above the learned room is the utterance evidence.
		// A quiet word over a loud room spends part of each syllable below the
		// onset gate; counting only modulated frames made its confirmation
		// budget too small, so any voiced frame counts.
		if voiced {
			t.speech += frame
			if modulated {
				t.speechMod += frame
			}
			// The reference is the recent voiced level, so a word that arrives
			// after a room hypothesis raised neither the tail nor an early end.
			// A one-frame room event (a door, a gust) is not the voice: a jump
			// above 1.6x the reference must not drag it up and turn the rest of
			// a word into a "drop".
			if level < t.ref*1.6 || t.ref <= 0 {
				t.ref += (level - t.ref) * speechReferenceRate
			}
		} else if t.speech -= frame / 2; t.speech < 0 {
			t.speech = 0
		}
		if !modulated {
			if t.speechMod -= frame / 2; t.speechMod < 0 {
				t.speechMod = 0
			}
		}
		// A room that starts mid-turn rises above the stale floor and can look
		// like a vowel, but its level keeps moving: one clear drop below the
		// recent voiced level restarts the steady hold, so the room can never
		// accumulate it. A genuinely steady vowel never drops.
		// A rise back to the recent voice level means the utterance resumed: a
		// dip accumulated by the room before a word arrived must not complete
		// inside that word. A jump far above the voice is a room event, not a
		// resumption, and must not restart the tail (a periodic room event would
		// otherwise postpone the end indefinitely).
		if t.ref > 0 && level > t.ref*1.05 && level < t.ref*1.6 {
			t.tail = 0
		}
		dropped := level < t.ref*tailDrop
		if voiced && !dropped && level > t.floor*steadyVoiceFloor && deviation < steadyVoiceDeviation {
			t.steady += frame
		} else {
			t.steady = 0
		}
		if (t.speech >= minSpeech && t.speechMod >= speechModulationHold) || t.steady >= steadyVoiceHold {
			t.confirmed = true
		}
		// End of speech is a sustained drop away from the recent voiced level,
		// not the absence of modulation: a returning fan/AC room is far below
		// the voice, while a sustained vowel keeps its level and is preserved.
		if dropped {
			t.tail += frame
			if t.tailLevel <= 0 {
				t.tailLevel = level
			} else {
				t.tailLevel += (level - t.tailLevel) * 0.2
			}
		} else if t.tail -= 2 * frame; t.tail < 0 {
			t.tail = 0
		}
		if t.tail < endSilence {
			return
		}
		confirmed := t.confirmed
		if confirmed {
			ev.Ended = true
		} else {
			// Positive noise evidence. Fold the level the tail settled at into
			// the floor so the same room does not immediately pass the onset
			// gate again.
			if t.tailLevel > t.floor {
				t.floor = clampBandFloor(t.tailLevel)
			}
			ev.Rejected = true
		}
		t.resetDetection()
		return
	}

	if !modulated && !(t.coldOnset && voiced) {
		t.onRun = 0
		return
	}
	t.onRun += frame
	if t.onRun < speechOnHold {
		return
	}
	t.coldOnset = false
	t.speaking = true
	// The window restarts at the onset: the modulation that opened the
	// hypothesis must not also confirm it, so a flat room bump that merely
	// started at a level step cannot become a word.
	t.windowN = 0
	t.windowNext = 0
	t.speech = t.onRun
	t.speechMod = 0
	t.steady = 0
	t.tail = 0
	t.confirmed = false
	t.ref = level
	t.tailLevel = 0
	t.onRun = 0
	ev.Started = true
}

// pcmRMS returns the full-scale RMS of 16-bit little-endian PCM. Slicing the
// input does not copy, and the accumulated sum is reused for every sample.
func pcmRMS(pcm []byte) float64 {
	if len(pcm) < 2 {
		return 0
	}
	var sum float64
	samples := 0
	for offset := 0; offset+1 < len(pcm); offset += 2 {
		value := float64(int16(binary.LittleEndian.Uint16(pcm[offset:offset+2]))) / 32768
		sum += value * value
		samples++
	}
	if samples == 0 {
		return 0
	}
	return math.Sqrt(sum / float64(samples))
}

func levelFromRMS(rms float64) float64 {
	if rms <= 0 {
		return 0
	}
	db := 20 * math.Log10(rms)
	return clamp01((db - minLevelDB) / -minLevelDB)
}

// --- ring ------------------------------------------------------------------

type ringMode uint8

const (
	ringListening ringMode = iota
	ringDelay
)

// ringView owns the ring presentation and capture for one turn. A single
// renderer goroutine writes time-based frames; captured audio only publishes
// the latest level, so frames never allocate per pixel.
type ringView struct {
	pres    *capsulad.Presentation
	capture *capsulad.Capture
	stop    chan struct{}
	done    chan struct{}

	closeOnce sync.Once

	mu          sync.Mutex
	mode        ringMode
	level       float64
	target      float64
	listenLevel float64
	lastFrame   time.Time
	delayAt     time.Time
	fade        time.Duration
	pixels      []capsulad.RGB
}

func (a *assistant) openRing(ctx context.Context, item reply) (*ringView, error) {
	pres, err := a.client.OpenPresentation(ctx, ringResource)
	if err != nil {
		return nil, fmt.Errorf("ring unavailable: %w", err)
	}
	v := &ringView{
		pres:   pres,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		pixels: make([]capsulad.RGB, ringPixels),
	}
	if err := v.writeFrame(); err != nil {
		_ = pres.Close()
		return nil, fmt.Errorf("ring frame: %w", err)
	}
	capture, err := a.client.OpenCapture(ctx, ringResource, pres.ID(), capsulad.CaptureModeUnspecified)
	if err != nil {
		_ = pres.Close()
		return nil, fmt.Errorf("ring capture: %w", err)
	}
	v.capture = capture
	log.Printf("dumbassistant: activation=%s ring listening", item.activationID)
	go v.render(ctx)
	return v, nil
}

func (v *ringView) observe(level float64) {
	v.mu.Lock()
	v.target = clamp01(level)
	v.mu.Unlock()
}

// beginDelay switches to the blue chase. The blend always runs for the full
// transition period and may continue into reply playback; an empty or short
// delay never skips it, and it never adds a wait to the reply.
func (v *ringView) beginDelay() {
	v.mu.Lock()
	v.mode = ringDelay
	v.listenLevel = v.level
	v.delayAt = time.Now()
	v.fade = ringFadePeriod
	v.mu.Unlock()
}

func (v *ringView) render(ctx context.Context) {
	defer close(v.done)
	ticker := time.NewTicker(ringFramePeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-v.stop:
			return
		case <-ticker.C:
			if err := v.writeFrame(); err != nil {
				log.Printf("dumbassistant: ring frame: %v", err)
				return
			}
		}
	}
}

func (v *ringView) writeFrame() error {
	now := time.Now()
	v.mu.Lock()
	dt := now.Sub(v.lastFrame)
	if dt <= 0 || dt > time.Second {
		dt = ringFramePeriod
	}
	v.lastFrame = now

	tau := levelFallSeconds
	if v.target > v.level {
		tau = levelRiseSeconds
	}
	alpha := 1 - math.Exp(-dt.Seconds()/tau)
	v.level = clamp01(v.level + (v.target-v.level)*alpha)
	level := v.level
	mode := v.mode
	listenLevel := v.listenLevel
	delayAt := v.delayAt
	fade := v.fade

	switch mode {
	case ringDelay:
		progress := transitionProgress(now, delayAt, fade)
		blend := smoothstep(progress)
		phase := math.Mod(float64(now.Sub(delayAt))/float64(chasePeriod), 1)
		for i := range v.pixels {
			distance := math.Abs(float64(i)/ringPixels - phase)
			if distance > 0.5 {
				distance = 1 - distance
			}
			head := 0.0
			if limit := chaseHeadPixels / ringPixels; distance < limit {
				head = 1 - distance/limit
			}
			chase := 0.12 + 0.88*head*head
			brightness := listenLevel + (chase-listenLevel)*blend
			v.pixels[i] = scaleRGB(
				capsulad.RGB{
					R: uint8(lerp(0, float64(blue.R), blend) + 0.5),
					G: uint8(lerp(float64(teal.G), float64(blue.G), blend) + 0.5),
					B: uint8(lerp(float64(teal.B), float64(blue.B), blend) + 0.5),
				}, brightness)
		}
	default:
		brightness := levelFloor + (1-levelFloor)*level
		pixel := scaleRGB(teal, brightness)
		for i := range v.pixels {
			v.pixels[i] = pixel
		}
	}
	v.mu.Unlock()
	return v.pres.Ring(v.pixels)
}

func (v *ringView) close() {
	v.closeOnce.Do(func() {
		close(v.stop)
		select {
		case <-v.done:
		case <-time.After(2 * time.Second):
			log.Printf("dumbassistant: ring renderer did not stop promptly")
		}
		if v.capture != nil {
			v.capture.Close()
		}
		if err := v.pres.Close(); err != nil {
			log.Printf("dumbassistant: ring close: %v", err)
			return
		}
		log.Printf("dumbassistant: ring released")
	})
}

func scaleRGB(c capsulad.RGB, brightness float64) capsulad.RGB {
	brightness = clamp01(brightness)
	return capsulad.RGB{
		R: uint8(float64(c.R)*brightness + 0.5),
		G: uint8(float64(c.G)*brightness + 0.5),
		B: uint8(float64(c.B)*brightness + 0.5),
	}
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

func lerp(from, to, t float64) float64 { return from + (to-from)*t }

// transitionProgress is the linear teal-to-blue blend fraction. The ring
// transition is time-based and independent of the reply delay, so it continues
// into playback when the delay is empty or shorter than the transition.
func transitionProgress(now, start time.Time, fade time.Duration) float64 {
	if fade <= 0 {
		return 1
	}
	return clamp01(float64(now.Sub(start)) / float64(fade))
}

func smoothstep(t float64) float64 {
	t = clamp01(t)
	return t * t * (3 - 2*t)
}

// --- playback --------------------------------------------------------------

// play paces chunks at real time so the daemon sees a normal streaming client
// instead of a burst that outlives its source. Closing the stream after the
// last chunk lets the mixer drain the queued tail normally.
func (a *assistant) play(ctx context.Context, playback *capsulad.Playback, clip *clip) error {
	framesPerChunk := int(clip.format.SampleRateHz) * chunkMS / 1000
	if framesPerChunk <= 0 {
		return fmt.Errorf("invalid clip sample rate %d", clip.format.SampleRateHz)
	}
	bytesPerChunk := framesPerChunk * clip.bytesPerFrame
	ticker := time.NewTicker(chunkMS * time.Millisecond)
	defer ticker.Stop()

	for offset := 0; offset < len(clip.pcm); {
		end := offset + bytesPerChunk
		if end > len(clip.pcm) {
			end = len(clip.pcm)
		}
		if err := playback.Write(clip.pcm[offset:end]); err != nil {
			return err
		}
		offset = end
		if offset >= len(clip.pcm) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}

// --- settings --------------------------------------------------------------

func (a *assistant) nextReply() (delaySpec, *clip) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.settings.delay, a.settings.clip
}

func (a *assistant) applySettings(change *capsulad.SettingsChanged) {
	if change == nil {
		return
	}
	a.mu.Lock()
	next := a.settings
	var rejected []string
	for _, value := range change.Values {
		switch value.Key {
		case "delay":
			var raw string
			if err := json.Unmarshal([]byte(value.ValueJSON), &raw); err != nil {
				rejected = append(rejected, fmt.Sprintf("delay: expected text, got %s", value.ValueJSON))
				continue
			}
			spec, err := parseDelay(raw)
			if err != nil {
				rejected = append(rejected, fmt.Sprintf("delay %q: %v", raw, err))
				continue
			}
			next.delay = spec
		case "reply_asset":
			var raw string
			if err := json.Unmarshal([]byte(value.ValueJSON), &raw); err != nil {
				rejected = append(rejected, fmt.Sprintf("reply_asset: expected text, got %s", value.ValueJSON))
				continue
			}
			if raw == next.assetText {
				continue
			}
			var loaded *clip
			var path string
			var err error
			if strings.TrimSpace(raw) == "" {
				loaded, path, err = loadPackagedReply(a.pkgRoot)
			} else {
				loaded, path, err = loadStoreReply(a.assetStore, raw)
			}
			if err != nil {
				rejected = append(rejected, fmt.Sprintf("reply_asset %q: %v", raw, err))
				continue
			}
			next.assetText = raw
			next.replyPath = path
			next.clip = loaded
		}
	}
	a.settings = next
	delay, assetText, replyPath := next.delay, next.assetText, next.replyPath
	a.mu.Unlock()

	for _, problem := range rejected {
		log.Printf("dumbassistant: setting rejected: %s", problem)
	}
	if assetText == "" {
		assetText = "packaged"
	}
	log.Printf("dumbassistant: settings revision=%d delay=%s asset=%s reply=%s",
		change.Revision, delay, assetText, replyPath)
}

// --- assets ----------------------------------------------------------------

func packageRoot(argv0 string) (string, error) {
	if argv0 == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("locate executable: %w", err)
		}
		argv0 = exe
	}
	abs, err := filepath.Abs(argv0)
	if err != nil {
		return "", fmt.Errorf("resolve executable path %q: %w", argv0, err)
	}
	return filepath.Dir(abs), nil
}

func loadPackagedReply(pkgRoot string) (*clip, string, error) {
	assetRoot := filepath.Join(pkgRoot, "assets")
	path, err := resolveContainedPath(assetRoot, defaultReplyFile)
	if err != nil {
		return nil, "", fmt.Errorf("packaged reply: %w", err)
	}
	return loadClipFile(path)
}

// normalizeStoreAsset converts the configured value to a path relative to the
// shared store and to the catalog's canonical /assets/... path.
func normalizeStoreAsset(value string) (string, string, error) {
	rel := strings.TrimSpace(value)
	if rel == "" {
		return "", "", fmt.Errorf("asset path is empty")
	}
	if strings.IndexByte(rel, 0) >= 0 {
		return "", "", fmt.Errorf("asset path contains a NUL byte")
	}
	rel = filepath.FromSlash(rel)
	if filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("asset path must not be absolute; use a path relative to the asset store, got %q", value)
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == ".." {
			return "", "", fmt.Errorf("asset path must not contain '..': %q", value)
		}
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." {
		return "", "", fmt.Errorf("asset path does not name a file: %q", value)
	}
	canonical := "/assets/" + pathpkg.Clean(filepath.ToSlash(clean))
	return clean, canonical, nil
}

// loadStoreReply resolves one configured reply from the shared read-only asset
// store. The store is a plain directory with no manifest: any regular,
// non-symlink file inside it whose name ends in .wav is a valid reply. The
// daemon performs the same containment and media-type checks when the setting
// is saved, so a rejected value here means the file changed or disappeared.
func loadStoreReply(storeRoot, value string) (*clip, string, error) {
	rel, canonical, err := normalizeStoreAsset(value)
	if err != nil {
		return nil, "", err
	}
	if !wavAssetName(rel) {
		return nil, "", fmt.Errorf("asset %q must be a .wav file", canonical)
	}
	path, err := resolveContainedPath(storeRoot, rel)
	if err != nil {
		return nil, "", fmt.Errorf("asset %q: %w", canonical, err)
	}
	return loadClipFile(path)
}

// wavAssetName is the app's supported reply format: the shared store carries no
// declared media type, so the extension is the contract.
func wavAssetName(rel string) bool {
	return strings.EqualFold(filepath.Ext(rel), ".wav")
}

func loadClipFile(path string) (*clip, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read reply asset: %w", err)
	}
	c, err := parseWAV(data)
	if err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", path, err)
	}
	return c, path, nil
}

// resolveContainedPath confines rel to root and rejects traversal, symlink
// components, missing targets and non-regular files.
func resolveContainedPath(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty asset path")
	}
	if strings.IndexByte(rel, 0) >= 0 {
		return "", fmt.Errorf("asset path contains a NUL byte")
	}
	rel = filepath.FromSlash(rel)
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("asset path must be relative")
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == ".." {
			return "", fmt.Errorf("asset path must not contain '..'")
		}
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." {
		return "", fmt.Errorf("asset path does not name a file")
	}
	full := filepath.Join(root, clean)
	inside, err := filepath.Rel(root, full)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) || filepath.IsAbs(inside) {
		return "", fmt.Errorf("asset path is outside the asset store")
	}
	current := root
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("asset path must not use a symlink")
		}
	}
	info, err := os.Stat(full)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("asset is not a regular file")
	}
	return full, nil
}

// parseWAV accepts the smallest useful subset for a packaged reply: PCM,
// 16-bit little-endian, any positive channel count and sample rate. Everything
// else is rejected explicitly instead of being mislabeled as audio.
func parseWAV(data []byte) (*clip, error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, fmt.Errorf("not a RIFF/WAVE file")
	}
	var formatChunk, pcm []byte
	for offset := 12; offset+8 <= len(data); {
		id := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		end := start + size
		if size < 0 || end > len(data) {
			return nil, fmt.Errorf("truncated %q chunk", id)
		}
		switch id {
		case "fmt ":
			formatChunk = data[start:end]
		case "data":
			pcm = data[start:end]
		}
		offset = end + size%2
	}
	if len(formatChunk) < 16 {
		return nil, fmt.Errorf("missing or short fmt chunk")
	}
	if pcm == nil {
		return nil, fmt.Errorf("missing data chunk")
	}
	audioFormat := binary.LittleEndian.Uint16(formatChunk[0:2])
	channels := int(binary.LittleEndian.Uint16(formatChunk[2:4]))
	sampleRate := int(binary.LittleEndian.Uint32(formatChunk[4:8]))
	bits := int(binary.LittleEndian.Uint16(formatChunk[14:16]))
	if audioFormat != 1 {
		return nil, fmt.Errorf("unsupported WAV format %d; only PCM is accepted", audioFormat)
	}
	if channels <= 0 || sampleRate <= 0 {
		return nil, fmt.Errorf("invalid WAV format: %d channels at %d Hz", channels, sampleRate)
	}
	if bits != 16 {
		return nil, fmt.Errorf("unsupported sample width %d; only 16-bit PCM is accepted", bits)
	}
	bytesPerFrame := channels * 2
	if len(pcm)%bytesPerFrame != 0 {
		return nil, fmt.Errorf("data chunk is not frame-aligned")
	}
	if len(pcm) == 0 {
		return nil, fmt.Errorf("data chunk is empty")
	}
	return &clip{
		format: capsulad.AudioFormat{
			SampleFormat: capsulad.SampleFormatPCM16LE,
			SampleRateHz: uint32(sampleRate),
			Channels:     uint32(channels),
		},
		pcm:           pcm,
		bytesPerFrame: bytesPerFrame,
	}, nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
