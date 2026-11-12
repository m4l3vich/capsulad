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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"capsula/sdk/capsula"
)

const (
	outputEndpoint = "audio.output.speaker"
	ringResource   = "light.ring.main"

	// The package ships the default reply beside the executable. A configured
	// reply_asset comes from the framework's shared read-only asset store
	// instead; CAPSULA_ASSETS names that store (/assets in the sandbox, the
	// host assets directory for unsandboxed runs).
	defaultReplyFile  = "reply.wav"
	assetStoreEnv     = "CAPSULA_ASSETS"
	defaultAssetStore = "/assets"
	assetManifestName = "manifest.json"

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

	// Speech is measured from real captured PCM. The thresholds are full-scale
	// normalized RMS; the "on" value must be sustained for speechOnChunks
	// chunks before a turn counts as speech, and the quieter "off" value must
	// persist for endSilence before the utterance is considered finished.
	speechOnRMS   = 0.022
	speechOffRMS  = 0.012
	speechOnCount = 2
	endSilence    = 700 * time.Millisecond
	minSpeech     = 140 * time.Millisecond
	maxSpeech     = 10 * time.Second
	noSpeechLimit = 3 * time.Second
	listenTick    = 50 * time.Millisecond
)

var (
	// teal is the listening state (#008080); blue is the delay/answer chase.
	teal = capsula.RGB{G: 128, B: 128}
	blue = capsula.RGB{G: 64, B: 255}
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
	format        capsula.AudioFormat
	pcm           []byte
	bytesPerFrame int
}

func (c *clip) frames() int { return len(c.pcm) / c.bytesPerFrame }

type reply struct {
	activationID string
	origin       capsula.ActivationOrigin
	originName   string
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
	client     *capsula.Client
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
	env := capsula.FromEnv()
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
	client, err := capsula.Connect(ctx, env)
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
	client.SetHandlers(capsula.Handlers{
		Ready: func() {
			log.Printf("dumbassistant: ready; reply %d frames at %d Hz/%d ch",
				clip.frames(), clip.format.SampleRateHz, clip.format.Channels)
		},
		Activation: func(_ context.Context, act *capsula.Activation) {
			a.enqueue(act)
		},
		TouchRelease: func(rel *capsula.InputReleased) {
			a.releaseInput(rel)
		},
		Settings: a.applySettings,
		Action: func(_ context.Context, act *capsula.ActionInvoked) {
			if act == nil || act.ActionID != dismissActionID {
				return
			}
			a.dismiss()
		},
		Invocation: func(_ context.Context, inv *capsula.LaunchInvocation) {
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

func (a *assistant) enqueue(act *capsula.Activation) {
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

func (a *assistant) releaseInput(rel *capsula.InputReleased) {
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

func (a *assistant) beginConversation(ctx context.Context, activationID string) (*capsula.Conversation, string) {
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
	holdToTalk := item.origin == capsula.ActivationOriginTouch && a.holdToTalk(ctx)

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
			if isPreroll(item, c) {
				// Pre-activation audio is the room, not this utterance: learn
				// the noise floor from it so a loud fan or traffic is already
				// characterized before the first speech window arrives. It must
				// never start a turn by itself.
				tracker.calibrate(rms)
				continue
			}
			frames := int(c.FrameCount)
			if frames <= 0 {
				frames = len(c.PCM) / 2
			}
			duration := time.Duration(frames) * time.Second / time.Duration(rate)
			startedNow, ended := tracker.feed(rms, duration)
			if startedNow || tracker.active() {
				lastVoice = time.Now()
			}
			if startedNow {
				log.Printf("dumbassistant: activation=%s speech detected", item.activationID)
			}
			if ended && !holdToTalk {
				log.Printf("dumbassistant: activation=%s speech ended", item.activationID)
				return true
			}
		}
	}
}

// holdToTalk asks the live device settings whether touch activations are
// press-and-hold turns. A failure falls back to speech-end detection, which is
// safe: a late release still ends the turn.
func (a *assistant) holdToTalk(ctx context.Context) bool {
	settings, _, err := a.client.UserSettings(ctx)
	if err != nil {
		log.Printf("dumbassistant: user settings: %v", err)
		return false
	}
	return settings.TouchHoldMode == capsula.TouchHoldToTalk
}

func isPreroll(item reply, c *capsula.AudioChunk) bool {
	return item.activationNS != 0 && c.MonotonicTimeNS != 0 && c.MonotonicTimeNS < item.activationNS
}

// speechTracker classifies consecutive PCM chunks against an adaptive noise
// floor. A fixed absolute threshold fails in a real room: a fan, a fridge or
// traffic keeps the level above the "silence" value, so the turn never ends and
// the listen cap cancels it. Silence is instead whatever the room is doing, and
// speech must rise clearly above it. The absolute values remain the floor for a
// quiet room; the relative values scale with measured background noise.
type speechTracker struct {
	onRun    int
	speaking bool
	speech   time.Duration
	silence  time.Duration
	// floor is the running background RMS. It follows quieter rooms quickly
	// and louder ones slowly, and only samples below the onset threshold move
	// it, so an utterance cannot raise its own detection threshold.
	floor float64
}

const (
	// Noise-floor adaptation. Downward tracking is fast enough to notice a
	// fan stopping. Upward tracking is slower so a short utterance cannot
	// become the background, but it still converges within about a second of
	// steady noise (5 % per 20 ms frame).
	noiseFloorDown = 0.25
	noiseFloorUp   = 0.05
	noiseFloorMin  = 0.0005
	noiseFloorMax  = 0.04
	speechNoiseOn  = 2.5 // speech onset must exceed background by this factor
	speechNoiseOff = 1.5 // the tail counts as silence near this factor
)

func (t *speechTracker) active() bool { return t.speaking }

// calibrate folds a pre-activation chunk into the noise floor. Unlike observe it
// does not refuse a level above the current onset threshold: pre-roll is by
// definition background, and following it quickly is what makes a loud room
// distinguishable from speech once the turn actually starts.
func (t *speechTracker) calibrate(rms float64) {
	target := math.Min(rms, noiseFloorMax)
	if t.floor <= 0 {
		t.floor = target
	} else {
		t.floor += (target - t.floor) * noiseFloorDown
	}
	if t.floor < noiseFloorMin {
		t.floor = noiseFloorMin
	}
	if t.floor > noiseFloorMax {
		t.floor = noiseFloorMax
	}
}

func (t *speechTracker) reset() { *t = speechTracker{} }

// thresholds returns the onset and tail thresholds for the current floor.
func (t *speechTracker) thresholds() (on, off float64) {
	on, off = speechOnRMS, speechOffRMS
	if scaled := t.floor * speechNoiseOn; scaled > on {
		on = scaled
	}
	if scaled := t.floor * speechNoiseOff; scaled > off {
		off = scaled
	}
	return on, off
}

// observe records a chunk that did not start speech. The first chunk seeds the
// floor; later chunks move it quickly toward a quieter room and slowly toward a
// louder one. A sample above the current onset threshold is likely speech and
// is not folded into the background.
func (t *speechTracker) observe(rms float64) {
	if t.floor <= 0 {
		// Seed low enough that the first chunk of a real utterance cannot
		// become the background it has to exceed. A noisy room then walks the
		// floor upward at the slow rate.
		t.floor = math.Min(rms, speechOffRMS)
	}
	if on, _ := t.thresholds(); rms > on {
		return
	}
	rate := noiseFloorUp
	if rms < t.floor {
		rate = noiseFloorDown
	}
	t.floor += (rms - t.floor) * rate
	if t.floor < noiseFloorMin {
		t.floor = noiseFloorMin
	}
	if t.floor > noiseFloorMax {
		t.floor = noiseFloorMax
	}
}

func (t *speechTracker) feed(rms float64, duration time.Duration) (started bool, ended bool) {
	if duration <= 0 {
		duration = 10 * time.Millisecond
	}
	t.observe(rms)
	if !t.speaking {
		on, _ := t.thresholds()
		if rms > on {
			t.onRun++
		} else {
			t.onRun = 0
		}
		if t.onRun >= speechOnCount {
			t.speaking = true
			t.onRun = 0
			t.speech = duration
			t.silence = 0
			return true, false
		}
		return false, false
	}

	_, off := t.thresholds()
	if rms <= off {
		t.silence += duration
	} else {
		// Only voiced time counts toward minSpeech; a click that survives
		// two chunks must not look like a complete utterance.
		t.speech += duration
		t.silence = 0
	}
	if t.silence >= endSilence {
		longEnough := t.speech >= minSpeech
		t.reset()
		if !longEnough {
			return false, false
		}
		return false, true
	}
	return false, false
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
	pres    *capsula.Presentation
	capture *capsula.Capture
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
	pixels      []capsula.RGB
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
		pixels: make([]capsula.RGB, ringPixels),
	}
	if err := v.writeFrame(); err != nil {
		_ = pres.Close()
		return nil, fmt.Errorf("ring frame: %w", err)
	}
	capture, err := a.client.OpenCapture(ctx, ringResource, pres.ID(), capsula.CaptureModeUnspecified)
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
				capsula.RGB{
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

func scaleRGB(c capsula.RGB, brightness float64) capsula.RGB {
	brightness = clamp01(brightness)
	return capsula.RGB{
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
func (a *assistant) play(ctx context.Context, playback *capsula.Playback, clip *clip) error {
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

func (a *assistant) applySettings(change *capsula.SettingsChanged) {
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

// sharedAsset mirrors the standard read-only asset catalog entry. The store
// manifest is the same file the daemon's asset catalog consumes.
type sharedAsset struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
}

type sharedAssetManifest struct {
	Assets []sharedAsset `json:"assets"`
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

func loadStoreReply(storeRoot, value string) (*clip, string, error) {
	rel, canonical, err := normalizeStoreAsset(value)
	if err != nil {
		return nil, "", err
	}
	assets, err := readAssetManifest(storeRoot)
	if err != nil {
		return nil, "", err
	}
	declared, ok := declaredAsset(assets, canonical)
	if !ok {
		return nil, "", fmt.Errorf("asset %q is not declared in %s", canonical, filepath.Join(storeRoot, assetManifestName))
	}
	if !wavMediaType(declared.MediaType) {
		return nil, "", fmt.Errorf("asset %q declares unsupported media type %q", canonical, declared.MediaType)
	}
	path, err := resolveContainedPath(storeRoot, rel)
	if err != nil {
		return nil, "", fmt.Errorf("asset %q: %w", canonical, err)
	}
	return loadClipFile(path)
}

func readAssetManifest(storeRoot string) ([]sharedAsset, error) {
	path := filepath.Join(storeRoot, assetManifestName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("asset store manifest: %w", err)
	}
	var doc sharedAssetManifest
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse asset store manifest: %w", err)
	}
	return doc.Assets, nil
}

// declaredAsset matches the catalog's normalization exactly: an empty path
// falls back to the asset id, and a missing /assets/ prefix is added.
func declaredAsset(assets []sharedAsset, canonical string) (sharedAsset, bool) {
	for _, asset := range assets {
		path := strings.TrimSpace(asset.Path)
		if path == "" {
			path = strings.TrimSpace(asset.ID)
		}
		if path == "" {
			continue
		}
		if !strings.HasPrefix(path, "/assets/") {
			path = "/assets/" + strings.TrimPrefix(path, "/")
		}
		if path == canonical {
			return asset, true
		}
	}
	return sharedAsset{}, false
}

func wavMediaType(mediaType string) bool {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "", "audio/wav", "audio/x-wav", "audio/wave", "audio/vnd.wave":
		return true
	default:
		return false
	}
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
		format: capsula.AudioFormat{
			SampleFormat: capsula.SampleFormatPCM16LE,
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
