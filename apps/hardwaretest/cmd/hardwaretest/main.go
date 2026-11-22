// Command hardwaretest is the foreground Hardware Test app for Capsula Mini.
//
// One run owns the segment display, the LED ring and a full-panel touch capture
// for its whole lifetime. It shows 12:34 with the upper colon dot blinking at
// 1 Hz for ten seconds, then the lower dot blinking at 2 Hz, repeating; paints a
// clockwise rainbow chase; and three seconds after the chase starts plays a
// soft 20% 440 Hz tone while recording the microphone. If the recording does not
// contain the tone the whole ring blinks red and the chase resumes. Every held
// panel key replaces the clock with its number (north 1, east 2, south 3, west
// 4, centre 5).
//
// The app never writes hardware directly: all output goes through the
// framework's presentation and capture services, and the daemon's output loop
// is the physical writer. Stopping the run releases the presentations, the
// touch input subscription and the capture, restoring the configured clock and
// panel behaviour.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"capsulad/apps/hardwaretest/internal/testseq"
	"capsulad/sdk/capsulad"
)

const (
	deviceService       = "capsulad.framework.v1.DeviceService"
	segmentResource     = "display.segment.front"
	ringResource        = "light.ring.main"
	touchResource       = "touch.panel.main"
	speakerEndpoint     = "audio.output.speaker"
	frameInterval       = 33 * time.Millisecond
	toneStartDelay      = 3 * time.Second
	toneFrequencyHz     = 440.0
	toneAmplitude       = 0.2 // 20% of full scale, not the device master volume
	toneDuration        = 800 * time.Millisecond
	baselineDuration    = 250 * time.Millisecond
	recordingDuration   = 1500 * time.Millisecond
	redFlashDuration    = 3 * time.Second
	callTimeout         = 5 * time.Second
)

type segmentCaps struct {
	DigitCount    uint32 `json:"digitCount"`
	SupportsColon bool   `json:"supportsColon"`
}

type ringCaps struct {
	PixelCount  uint32 `json:"pixelCount"`
	SupportsRGB bool   `json:"supportsRgb"`
}

type touchCaps struct {
	PartialGestures bool `json:"partialGesturesAvailable"`
	FullRaw         bool `json:"fullRawAvailable"`
	FullPanel       bool `json:"fullPanelAvailable"`
}

type resourceInfo struct {
	ResourceID string       `json:"resourceId"`
	Kind       string       `json:"kind"`
	Available  bool         `json:"available"`
	Segment    *segmentCaps `json:"segment,omitempty"`
	Ring       *ringCaps    `json:"ring,omitempty"`
	Touch      *touchCaps   `json:"touch,omitempty"`
}

type resourceList struct {
	Resources []resourceInfo `json:"resources"`
}

type app struct {
	client *capsulad.Client

	segmentPres *capsulad.Presentation
	segmentCap  *capsulad.Capture
	ringPres    *capsulad.Presentation
	ringCap     *capsulad.Capture
	touchCap    *capsulad.Capture
	input       *capsulad.InputStream

	ringPixels int

	mu       sync.Mutex
	redStart time.Time
	redUntil time.Time
	frameSig segmentSignature
}

type segmentSignature struct {
	cells       [4]uint16
	colonTop    bool
	colonBottom bool
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	env := capsulad.FromEnv()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	client, err := capsulad.Connect(ctx, env)
	if err != nil {
		log.Fatalf("hardwaretest: connect: %v", err)
	}
	defer client.Close()

	a := &app{client: client}
	client.SetHandlers(capsulad.Handlers{
		Ready: func() {
			log.Printf("hardwaretest: ready app=%s run=%s", env.AppID, env.RunID)
			go a.run(ctx)
		},
		Disconnected: func(err error) {
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("hardwaretest: disconnected: %v", err)
			}
		},
	})
	if err := client.Start(ctx); err != nil && ctx.Err() == nil {
		log.Printf("hardwaretest: bus stopped: %v", err)
	}
}

func (a *app) run(ctx context.Context) {
	defer a.client.Close()
	if err := a.setup(ctx); err != nil {
		log.Printf("hardwaretest: unavailable: %v", err)
		a.shutdown()
		return
	}
	defer a.shutdown()

	log.Printf("hardwaretest: controls: hold a panel key to show its number; stop the app to leave")
	a.loop(ctx)
}

func (a *app) setup(ctx context.Context) error {
	resources, err := a.listResources(ctx)
	if err != nil {
		return fmt.Errorf("list resources: %w", err)
	}
	segment := findResource(resources, segmentResource, "RESOURCE_SEGMENT_DISPLAY")
	if segment == nil || !segment.Available || segment.Segment == nil ||
		segment.Segment.DigitCount < 4 {
		return fmt.Errorf("%s with four digits is not available", segmentResource)
	}
	ring := findResource(resources, ringResource, "RESOURCE_LED_RING")
	if ring == nil || !ring.Available || ring.Ring == nil || ring.Ring.PixelCount == 0 {
		return fmt.Errorf("%s is not available", ringResource)
	}
	a.ringPixels = int(ring.Ring.PixelCount)
	touch := findResource(resources, touchResource, "RESOURCE_TOUCH")
	if touch == nil || !touch.Available || touch.Touch == nil || !touch.Touch.FullPanel {
		return fmt.Errorf("%s does not offer a full-panel capture", touchResource)
	}

	if err := a.openSegment(ctx); err != nil {
		return err
	}
	if err := a.openRing(ctx); err != nil {
		return err
	}
	if err := a.openTouch(ctx); err != nil {
		return err
	}
	return nil
}

func (a *app) openSegment(ctx context.Context) error {
	pres, err := a.client.OpenPresentation(ctx, segmentResource)
	if err != nil {
		return fmt.Errorf("open segment presentation: %w", err)
	}
	a.segmentPres = pres
	a.frameSig = segmentSignature{}
	if err := a.writeSegment(testseq.Clock, false, false); err != nil {
		return fmt.Errorf("write segment frame: %w", err)
	}
	cap, err := a.client.OpenCapture(ctx, segmentResource, pres.ID(), capsulad.CaptureModeUnspecified)
	if err != nil {
		return fmt.Errorf("acquire segment capture: %w", err)
	}
	a.segmentCap = cap
	return nil
}

func (a *app) openRing(ctx context.Context) error {
	pres, err := a.client.OpenPresentation(ctx, ringResource)
	if err != nil {
		return fmt.Errorf("open ring presentation: %w", err)
	}
	a.ringPres = pres
	if err := pres.Ring(testseq.RainbowChase(a.ringPixels, 0)); err != nil {
		return fmt.Errorf("write ring frame: %w", err)
	}
	cap, err := a.client.OpenCapture(ctx, ringResource, pres.ID(), capsulad.CaptureModeUnspecified)
	if err != nil {
		return fmt.Errorf("acquire ring capture: %w", err)
	}
	a.ringCap = cap
	return nil
}

func (a *app) openTouch(ctx context.Context) error {
	cap, err := a.client.OpenCapture(ctx, touchResource, "", capsulad.CaptureModeFullRaw)
	if err != nil {
		return fmt.Errorf("acquire full touch capture: %w", err)
	}
	a.touchCap = cap
	input, err := a.client.WatchInput(ctx, cap.ID())
	if err != nil {
		return fmt.Errorf("watch touch input: %w", err)
	}
	a.input = input
	return nil
}

func (a *app) loop(ctx context.Context) {
	started := time.Now()
	ticker := time.NewTicker(frameInterval)
	defer ticker.Stop()

	held := map[string]bool{}
	activeKey := ""
	toneStarted := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-a.input.Done():
			log.Printf("hardwaretest: touch input ended; stopping")
			return
		case <-a.touchCap.Done():
			log.Printf("hardwaretest: full touch capture ended; stopping")
			return
		case <-a.segmentCap.Done():
			log.Printf("hardwaretest: segment capture ended; stopping")
			return
		case <-a.ringCap.Done():
			log.Printf("hardwaretest: ring capture ended; stopping")
			return
		case <-a.segmentPres.Done():
			log.Printf("hardwaretest: segment presentation ended; stopping")
			return
		case <-a.ringPres.Done():
			log.Printf("hardwaretest: ring presentation ended; stopping")
			return
		case event, ok := <-a.input.Events():
			if !ok {
				log.Printf("hardwaretest: touch input subscription closed; stopping")
				return
			}
			if event.Key == nil {
				continue
			}
			if event.Key.Pressed {
				held[event.Key.Key] = true
				activeKey = event.Key.Key
			} else {
				delete(held, event.Key.Key)
				if event.Key.Key == activeKey {
					activeKey = anyHeld(held)
				}
			}
		case event := <-a.touchCap.Events():
			if !event.Selected {
				log.Printf("hardwaretest: full touch capture released (%s); stopping", event.Reason)
				return
			}
		case now := <-ticker.C:
			elapsed := now.Sub(started)
			a.renderSegment(elapsed, activeKey)
			a.renderRing(elapsed, now)
			if !toneStarted && elapsed >= toneStartDelay {
				toneStarted = true
				go a.runToneTest(ctx)
			}
		}
	}
}

func anyHeld(held map[string]bool) string {
	for key := range held {
		return key
	}
	return ""
}

func (a *app) renderSegment(elapsed time.Duration, activeKey string) {
	var cells [4]uint16
	copy(cells[:], testseq.Clock)
	var colonTop, colonBottom bool
	if digit := testseq.KeyDigit(activeKey); digit > 0 {
		copy(cells[:], testseq.DigitFrame(digit))
	} else {
		colonTop, colonBottom = testseq.ColonDots(uint64(elapsed))
	}
	sig := segmentSignature{cells: cells, colonTop: colonTop, colonBottom: colonBottom}
	a.mu.Lock()
	changed := sig != a.frameSig
	if changed {
		a.frameSig = sig
	}
	a.mu.Unlock()
	if !changed {
		return
	}
	if err := a.writeSegment(cells[:], colonTop, colonBottom); err != nil {
		log.Printf("hardwaretest: write segment: %v", err)
	}
}

func (a *app) writeSegment(cells []uint16, colonTop, colonBottom bool) error {
	if a.segmentPres == nil {
		return errors.New("segment presentation is not open")
	}
	return a.segmentPres.SegmentDots(cells, colonTop, colonBottom)
}

func (a *app) renderRing(elapsed time.Duration, now time.Time) {
	a.mu.Lock()
	redStart, redUntil := a.redStart, a.redUntil
	a.mu.Unlock()

	var frame []capsulad.RGB
	if !redStart.IsZero() && now.Before(redUntil) {
		frame = testseq.RedFlash(a.ringPixels, uint64(now.Sub(redStart)))
	} else {
		frame = testseq.RainbowChase(a.ringPixels, uint64(elapsed))
	}
	if err := a.ringPres.Ring(frame); err != nil {
		log.Printf("hardwaretest: write ring: %v", err)
	}
}

func (a *app) runToneTest(ctx context.Context) {
	log.Printf("hardwaretest: tone test starting: %dHz amplitude=%.2f", int(toneFrequencyHz), toneAmplitude)
	mic, err := a.client.OpenMic(ctx, "", "")
	if err != nil {
		a.reportToneFailure("microphone capture unavailable: %v", err)
		return
	}
	defer func() {
		_ = mic.Close()
		select {
		case <-mic.Done():
		case <-time.After(time.Second):
		}
	}()

	format := capsulad.AudioFormat{
		SampleFormat: capsulad.SampleFormatPCM16LE,
		SampleRateHz: 16000,
		Channels:     1,
	}
	baseline := a.recordWindow(ctx, mic, &format, baselineDuration)
	baselineAmp, _ := testseq.ToneAmplitude(baseline, format, toneFrequencyHz)
	log.Printf("hardwaretest: microphone baseline rate=%d channels=%d amplitude=%.5f samples=%d",
		format.SampleRateHz, format.Channels, baselineAmp, len(baseline))

	tone := testseq.TonePCM(48000, 2, uint64(toneDuration), toneFrequencyHz, toneAmplitude)
	playbackDone := make(chan error, 1)
	go func() {
		playback, err := a.client.OpenPlayback(ctx, speakerEndpoint, capsulad.DefaultAudioFormat(), "")
		if err != nil {
			playbackDone <- err
			return
		}
		const chunkMS = 20
		chunkBytes := 48000 * 2 * 2 * chunkMS / 1000
		for off := 0; off < len(tone); off += chunkBytes {
			end := min(off+chunkBytes, len(tone))
			if err := playback.Write(tone[off:end]); err != nil {
				_ = playback.Abort()
				playbackDone <- err
				return
			}
		}
		playbackDone <- playback.Close()
	}()

	recorded := a.recordWindow(ctx, mic, &format, recordingDuration)
	toneAmp, rms := testseq.ToneAmplitude(recorded, format, toneFrequencyHz)
	log.Printf("hardwaretest: microphone tone amplitude=%.5f rms=%.5f samples=%d", toneAmp, rms, len(recorded))

	select {
	case err := <-playbackDone:
		if err != nil {
			a.reportToneFailure("tone playback failed: %v", err)
			return
		}
	case <-time.After(toneDuration + 2*time.Second):
		a.reportToneFailure("tone playback did not drain")
		return
	}

	if testseq.ToneHeard(toneAmp, baselineAmp) {
		log.Printf("hardwaretest: tone detected; ring stays in the rainbow chase")
		return
	}
	a.reportToneFailure("tone was not heard in the recording (tone=%.5f baseline=%.5f)", toneAmp, baselineAmp)
}

func (a *app) reportToneFailure(format string, args ...any) {
	log.Printf("hardwaretest: tone test FAILED: "+format+"; blinking the ring red", args...)
	now := time.Now()
	a.mu.Lock()
	a.redStart = now
	a.redUntil = now.Add(redFlashDuration)
	a.mu.Unlock()
}

func (a *app) recordWindow(ctx context.Context, mic *capsulad.Mic, format *capsulad.AudioFormat, window time.Duration) []byte {
	var pcm []byte
	deadline := time.NewTimer(window)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return pcm
		case <-deadline.C:
			return pcm
		case <-mic.Done():
			return pcm
		case chunk, ok := <-mic.Chunks():
			if !ok {
				return pcm
			}
			if chunk.Format != nil {
				*format = *chunk.Format
			}
			if chunk.Notice != nil {
				log.Printf("hardwaretest: microphone notice code=%s detail=%q",
					chunk.Notice.Code, chunk.Notice.Detail)
			}
			if chunk.Chunk != nil {
				pcm = append(pcm, chunk.Chunk.PCM...)
			}
		}
	}
}

func (a *app) shutdown() {
	log.Printf("hardwaretest: releasing outputs and captures")
	if a.input != nil {
		_ = a.input.Close()
	}
	if a.touchCap != nil {
		a.touchCap.Close()
	}
	if a.segmentCap != nil {
		a.segmentCap.Close()
	}
	if a.ringCap != nil {
		a.ringCap.Close()
	}
	if a.segmentPres != nil {
		_ = a.segmentPres.Blank()
		_ = a.segmentPres.Close()
	}
	if a.ringPres != nil {
		_ = a.ringPres.Blank()
		_ = a.ringPres.Close()
	}
}

func (a *app) listResources(ctx context.Context) ([]resourceInfo, error) {
	var list resourceList
	if err := a.client.CallJSON(ctx, "", deviceService, "ListResources", struct{}{}, &list, callTimeout); err != nil {
		return nil, err
	}
	return list.Resources, nil
}

func findResource(resources []resourceInfo, resourceID, kind string) *resourceInfo {
	for i := range resources {
		if resources[i].ResourceID == resourceID && resources[i].Kind == kind {
			return &resources[i]
		}
	}
	return nil
}


