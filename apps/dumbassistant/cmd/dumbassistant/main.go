// Dumb assistant is a first-party optional voice assistant. It claims the
// voice_assistant role, but performs no recognition: every activation gets the
// same packaged WAV reply after a configurable delay. The delay is either a
// constant or a uniformly random value inside a configured range.
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
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"capsula/sdk/capsula"
)

const (
	outputEndpoint = "audio.output.speaker"
	ringResource   = "light.ring.main"
	replyAsset     = "assets/reply.wav"

	chunkMS      = 20
	queueDepth   = 8
	maxDelayMS   = 60000
	defaultDelay = 1000
	defaultMinMS = 500
	defaultMaxMS = 2500
)

// teal is the active-state ring colour (#008080).
var teal = capsula.RGB{R: 0, G: 128, B: 128}

type delaySettings struct {
	mode       string
	constantMS int64
	minMS      int64
	maxMS      int64
}

func defaultDelaySettings() delaySettings {
	return delaySettings{
		mode:       "constant",
		constantMS: defaultDelay,
		minMS:      defaultMinMS,
		maxMS:      defaultMaxMS,
	}
}

// delay returns the wait before one reply. The caller serializes responses, so
// a single rand.Rand is safe here. A reversed range is accepted and swapped;
// equal bounds degrade to a constant wait.
func (s delaySettings) delay(rng *rand.Rand) time.Duration {
	if s.mode != "random" {
		return time.Duration(s.constantMS) * time.Millisecond
	}
	lo, hi := s.minMS, s.maxMS
	if lo > hi {
		lo, hi = hi, lo
	}
	if hi <= lo {
		return time.Duration(lo) * time.Millisecond
	}
	return time.Duration(lo+rng.Int64N(hi-lo+1)) * time.Millisecond
}

type clip struct {
	format        capsula.AudioFormat
	pcm           []byte
	bytesPerFrame int
}

func (c *clip) frames() int { return len(c.pcm) / c.bytesPerFrame }

type reply struct {
	activationID string
	origin       string
}

type assistant struct {
	ctx    context.Context
	client *capsula.Client
	clip   *clip
	rng    *rand.Rand

	mu       sync.Mutex
	settings delaySettings

	queue chan reply
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	env := capsula.FromEnv()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	clip, err := loadClip()
	if err != nil {
		log.Fatalf("dumbassistant: %v", err)
	}
	client, err := capsula.Connect(ctx, env)
	if err != nil {
		log.Fatalf("dumbassistant: connect: %v", err)
	}
	defer client.Close()

	a := &assistant{
		ctx: ctx, client: client, clip: clip,
		rng:      rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(time.Now().UnixNano()>>32))),
		settings: defaultDelaySettings(),
		queue:    make(chan reply, queueDepth),
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
			// The reply is intentionally independent of the talk button
			// lifetime: the point of this assistant is a fixed answer.
			log.Printf("dumbassistant: input released activation=%s", rel.ActivationID)
		},
		Settings: a.applySettings,
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
	item := reply{activationID: act.ID, origin: act.Origin.String()}
	select {
	case a.queue <- item:
		log.Printf("dumbassistant: activation=%s origin=%s queued", item.activationID, item.origin)
	default:
		log.Printf("dumbassistant: activation=%s origin=%s dropped; reply queue is full", item.activationID, item.origin)
	}
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
	releaseRing := a.showActive()
	defer releaseRing()

	wait := a.nextDelay()
	log.Printf("dumbassistant: activation=%s reply in %s", item.activationID, wait)
	if !sleep(a.ctx, wait) {
		return
	}

	conv, err := a.client.BeginConversation(a.ctx, item.activationID)
	conversationID := ""
	if err != nil {
		// Play anyway; the mixer simply will not duck other audio for this
		// reply. A dropped activation is not a reason for silence.
		log.Printf("dumbassistant: conversation for activation=%s: %v", item.activationID, err)
	} else {
		conversationID = conv.ID()
		defer conv.Close()
	}

	playback, err := a.client.OpenPlayback(a.ctx, outputEndpoint, a.clip.format, conversationID)
	if err != nil {
		log.Printf("dumbassistant: playback for activation=%s: %v", item.activationID, err)
		return
	}
	defer playback.Close()

	log.Printf("dumbassistant: playing reply for activation=%s conversation=%s", item.activationID, conversationID)
	if err := a.play(a.ctx, playback); err != nil {
		log.Printf("dumbassistant: reply for activation=%s failed: %v", item.activationID, err)
		return
	}
	log.Printf("dumbassistant: reply for activation=%s finished", item.activationID)
}

// showActive lights the whole ring teal for the lifetime of one reply. The
// presentation exists only while the assistant is active, so closing it
// restores the previous ring selection instead of leaving a static claim.
func (a *assistant) showActive() func() {
	ring, err := a.client.OpenPresentation(a.ctx, ringResource)
	if err != nil {
		log.Printf("dumbassistant: ring unavailable: %v", err)
		return func() {}
	}
	if err := ring.Effect("system.ring.indicator", map[string]any{
		"color": teal, "start": 0, "count": 12, "intensity": 1.0, "fade_in": 0.4,
	}); err != nil {
		log.Printf("dumbassistant: ring effect: %v", err)
		_ = ring.Close()
		return func() {}
	}
	capture, err := a.client.OpenCapture(a.ctx, ringResource, ring.ID(), capsula.CaptureModeUnspecified)
	if err != nil {
		log.Printf("dumbassistant: ring capture: %v", err)
		_ = ring.Close()
		return func() {}
	}
	log.Printf("dumbassistant: ring glowing teal")
	return func() {
		capture.Close()
		if err := ring.Close(); err != nil {
			log.Printf("dumbassistant: ring close: %v", err)
			return
		}
		log.Printf("dumbassistant: ring released")
	}
}

// play paces chunks at real time so the daemon sees a normal streaming client
// instead of a burst that outlives its source. Closing the stream after the
// last chunk lets the mixer drain the queued tail normally.
func (a *assistant) play(ctx context.Context, playback *capsula.Playback) error {
	framesPerChunk := int(a.clip.format.SampleRateHz) * chunkMS / 1000
	if framesPerChunk <= 0 {
		return fmt.Errorf("invalid clip sample rate %d", a.clip.format.SampleRateHz)
	}
	bytesPerChunk := framesPerChunk * a.clip.bytesPerFrame
	ticker := time.NewTicker(chunkMS * time.Millisecond)
	defer ticker.Stop()

	for offset := 0; offset < len(a.clip.pcm); {
		end := offset + bytesPerChunk
		if end > len(a.clip.pcm) {
			end = len(a.clip.pcm)
		}
		if err := playback.Write(a.clip.pcm[offset:end]); err != nil {
			return err
		}
		offset = end
		if offset >= len(a.clip.pcm) {
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

func (a *assistant) nextDelay() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.settings.delay(a.rng)
}

func (a *assistant) applySettings(change *capsula.SettingsChanged) {
	a.mu.Lock()
	for _, value := range change.Values {
		switch value.Key {
		case "delay_mode":
			var mode string
			if json.Unmarshal([]byte(value.ValueJSON), &mode) == nil && (mode == "constant" || mode == "random") {
				a.settings.mode = mode
			}
		case "delay_ms":
			var ms int64
			if json.Unmarshal([]byte(value.ValueJSON), &ms) == nil {
				a.settings.constantMS = clampMS(ms)
			}
		case "delay_min_ms":
			var ms int64
			if json.Unmarshal([]byte(value.ValueJSON), &ms) == nil {
				a.settings.minMS = clampMS(ms)
			}
		case "delay_max_ms":
			var ms int64
			if json.Unmarshal([]byte(value.ValueJSON), &ms) == nil {
				a.settings.maxMS = clampMS(ms)
			}
		}
	}
	settings := a.settings
	a.mu.Unlock()
	log.Printf("dumbassistant: settings revision=%d mode=%s constant=%dms range=%d..%dms",
		change.Revision, settings.mode, settings.constantMS, settings.minMS, settings.maxMS)
}

func clampMS(ms int64) int64 {
	if ms < 0 {
		return 0
	}
	if ms > maxDelayMS {
		return maxDelayMS
	}
	return ms
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

func loadClip() (*clip, error) {
	path, err := assetPath(os.Args[0])
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read reply asset: %w", err)
	}
	c, err := parseWAV(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

// assetPath locates the package asset beside the executable. The Linux sandbox
// has no /proc/self/exe, so rely on the launcher-provided argv[0], which is the
// absolute in-namespace path /app/<entrypoint>.
func assetPath(argv0 string) (string, error) {
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
	return filepath.Join(filepath.Dir(abs), replyAsset), nil
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
