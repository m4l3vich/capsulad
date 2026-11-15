package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"capsula/sdk/capsula"
)

const (
	micWindow   = 1800 * time.Millisecond
	micTarget   = time.Second
	toneChunkMS = 20
)

// stepAudio requires a selected, visible, non-blank ring presentation: the
// daemon releases microphone audio only while the physical indication is live.
// The ring hold, capture and microphone are released before return; playback
// is drained with Close or aborted.
func (a *devApp) stepAudio(ctx context.Context, gen uint64) stepResult {
	resources, err := a.resources(ctx)
	if err != nil {
		return classify("list resources", err)
	}
	ring := findResource(resources, ringResource, "RESOURCE_LED_RING")
	if ring == nil || !ring.Available || ring.Ring == nil || ring.Ring.PixelCount == 0 {
		return unsupported("resource %s is unavailable; microphone indication cannot be shown", ringResource)
	}
	pres, err := a.client.OpenPresentation(ctx, ring.ResourceID)
	if err != nil {
		return classify("open indication presentation", err)
	}
	defer pres.Close()
	indication := indicationPattern(int(ring.Ring.PixelCount))
	if err := pres.Ring(indication); err != nil {
		return classify("write indication frame", err)
	}
	capture, err := a.client.OpenCapture(ctx, ring.ResourceID, pres.ID(), capsula.CaptureModeUnspecified)
	if err != nil {
		return classify("open indication capture", err)
	}
	defer capture.Close()
	// Selection and visibility follow acquisition; give the daemon one frame.
	if err := wait(ctx, 250*time.Millisecond); err != nil {
		return classify("hold indication", err)
	}
	log.Printf("devtools: audio gen=%d indication presentation=%s capture=%s grant_selected=%v frame=%s",
		gen, pres.ID(), capture.ID(), capture.Info().Selected, rgbSummary(indication))

	micDetail, micStatus := a.captureMic(ctx, gen)
	state, err := a.client.AudioState(ctx)
	if err != nil {
		return classify("audio state", err)
	}
	log.Printf("devtools: audio gen=%d mixer volume=%.2f muted=%v",
		gen, state.MasterVolume, state.MicrophoneMuted)
	toneDetail, err := a.playTone(ctx, gen)
	if err != nil {
		return classify("play tone", err)
	}
	log.Printf("devtools: audio gen=%d playback %s", gen, toneDetail)
	if err := pres.Blank(); err != nil {
		log.Printf("devtools: audio gen=%d blank indication: %v", gen, err)
	}
	if err := pres.Close(); err != nil {
		log.Printf("devtools: audio gen=%d close presentation: %v", gen, err)
	}

	detail := fmt.Sprintf("microphone[%s] %s; playback[ok] %s; volume=%.2f muted=%v",
		micStatus, micDetail, toneDetail, state.MasterVolume, state.MicrophoneMuted)
	switch micStatus {
	case statusOK:
		return okResult("%s", detail)
	case statusUnsupported:
		return unsupported("%s", detail)
	}
	return errorResult("%s", detail)
}

// captureMic reads at most ~1 s of delivered PCM; the wall-clock bound keeps a
// stalled backend from holding the indication. Closing Mic unblocks the SDK
// reader and Done confirms the stream ended.
func (a *devApp) captureMic(ctx context.Context, gen uint64) (string, string) {
	mic, err := a.client.OpenMic(ctx, "", "")
	if err != nil {
		if peerUnavailable(err) {
			return "open failed: " + statusDetail(err), statusUnsupported
		}
		return "open failed: " + statusDetail(err), statusError
	}
	defer func() {
		_ = mic.Close()
		select {
		case <-mic.Done():
		case <-time.After(time.Second):
			log.Printf("devtools: audio gen=%d microphone did not end within 1s of close", gen)
		}
	}()

	var chunks int
	var frames, rate = uint64(0), uint64(16000)
	var peak int
	format := "unannounced"
	var notices []string
	timer := time.NewTimer(micWindow)
	defer timer.Stop()
read:
	for {
		select {
		case <-ctx.Done():
			return "cancelled while capturing", statusError
		case <-timer.C:
			break read
		case chunk, ok := <-mic.Chunks():
			if !ok {
				break read
			}
			if chunk.Format != nil && chunk.StreamID != "" {
				if chunk.Format.SampleRateHz > 0 {
					rate = uint64(chunk.Format.SampleRateHz)
				}
				format = fmt.Sprintf("%s/%dHz/%dch", chunk.Format.SampleFormat.String(),
					chunk.Format.SampleRateHz, chunk.Format.Channels)
				log.Printf("devtools: audio gen=%d microphone stream=%s format=%s",
					gen, chunk.StreamID, format)
			}
			if chunk.Notice != nil {
				notices = append(notices, chunk.Notice.Code+": "+chunk.Notice.Detail)
				log.Printf("devtools: audio gen=%d microphone notice code=%s detail=%q",
					gen, chunk.Notice.Code, chunk.Notice.Detail)
			}
			if chunk.Chunk == nil {
				continue
			}
			chunks++
			frames += uint64(chunk.Chunk.FrameCount)
			peak = max(peak, pcmPeak(chunk.Chunk.PCM))
			if frames >= uint64(micTarget.Seconds()*float64(rate)) {
				break read
			}
		}
	}
	detail := fmt.Sprintf("%s chunks=%d frames=%d captured_ms=%d peak=%d",
		format, chunks, frames, frames*1000/rate, peak)
	if len(notices) > 0 {
		detail += " notices=" + strings.Join(notices, "|")
	}
	switch {
	case chunks == 0:
		return detail, statusUnsupported
	case peak == 0:
		return detail + " signal=silence", statusOK
	}
	return detail, statusOK
}

// playTone writes a bounded 440 Hz burst in 20 ms chunks and drains it. Any
// error or cancellation aborts the stream, discarding queued audio.
func (a *devApp) playTone(ctx context.Context, gen uint64) (string, error) {
	const (
		rate     = 48000
		channels = 2
	)
	pcm := tonePCM(rate, channels, 300*time.Millisecond, 440, 0.2)
	chunkBytes := rate * channels * 2 * toneChunkMS / 1000
	playback, err := a.client.OpenPlayback(ctx, outputEndpoint, capsula.DefaultAudioFormat(), "")
	if err != nil {
		return "", err
	}
	closed := false
	defer func() {
		if !closed {
			_ = playback.Abort()
		}
	}()
	for off := 0; off < len(pcm); off += chunkBytes {
		if err := ctx.Err(); err != nil {
			_ = playback.Abort()
			return "", err
		}
		end := min(off+chunkBytes, len(pcm))
		if err := playback.Write(pcm[off:end]); err != nil {
			_ = playback.Abort()
			return "", err
		}
	}
	log.Printf("devtools: audio gen=%d playback queued bytes=%d endpoint=%s", gen, len(pcm), outputEndpoint)
	if err := playback.Close(); err != nil {
		return "", err
	}
	closed = true
	return fmt.Sprintf("tone=440Hz/300ms bytes=%d drained=true", len(pcm)), nil
}

func tonePCM(rate, channels int, duration time.Duration, frequency, amplitude float64) []byte {
	frames := int(float64(rate) * duration.Seconds())
	pcm := make([]byte, frames*channels*2)
	fadeFrames := rate / 100 // 10 ms at either edge avoids a click
	for i := range frames {
		envelope := 1.0
		if i < fadeFrames {
			envelope = float64(i) / float64(fadeFrames)
		} else if remaining := frames - i; remaining < fadeFrames {
			envelope = float64(remaining) / float64(fadeFrames)
		}
		sample := amplitude * envelope * math.Sin(2*math.Pi*frequency*float64(i)/float64(rate))
		value := uint16(int16(sample * math.MaxInt16))
		for channel := range channels {
			binary.LittleEndian.PutUint16(pcm[(i*channels+channel)*2:], value)
		}
	}
	return pcm
}

func pcmPeak(pcm []byte) int {
	peak := 0
	for i := 0; i+1 < len(pcm); i += 2 {
		sample := int(int16(binary.LittleEndian.Uint16(pcm[i:])))
		peak = max(peak, max(sample, -sample))
	}
	return peak
}

func indicationPattern(pixels int) []capsula.RGB {
	frame := make([]capsula.RGB, pixels)
	for i := range frame {
		frame[i] = capsula.RGB{G: 32, B: 32}
	}
	if pixels > 0 {
		frame[0] = capsula.RGB{G: 64, B: 64}
	}
	return frame
}

// --- input -------------------------------------------------------------

type inputMode struct {
	name   string
	mode   capsula.CaptureMode
	window time.Duration
}

// stepInput opens partial and full touch captures in turn. Nothing is inferred
// when no event arrives: an empty window is reported as zero events, because
// the simulator panel only emits through the development admin socket.
func (a *devApp) stepInput(ctx context.Context, gen uint64) stepResult {
	resources, err := a.resources(ctx)
	if err != nil {
		return classify("list resources", err)
	}
	touch := findResource(resources, touchResource, "RESOURCE_TOUCH")
	if touch == nil || !touch.Available {
		return unsupported("resource %s is not available on this backend", touchResource)
	}
	modes := []inputMode{{"partial", capsula.CaptureModePartialGestures, 2500 * time.Millisecond}}
	if touch.Touch == nil || touch.Touch.FullPanel || touch.Touch.FullRaw {
		modes = append(modes, inputMode{"full", capsula.CaptureModeFullRaw, 2500 * time.Millisecond})
	} else {
		log.Printf("devtools: input gen=%d mode=full status=unsupported detail=%q",
			gen, "backend does not advertise a full-panel stream")
	}
	var parts []string
	status := statusOK
	for _, im := range modes {
		detail, state := a.captureInput(ctx, gen, im)
		parts = append(parts, im.name+"["+state+"] "+detail)
		if state == statusError || (state == statusUnsupported && status == statusOK) {
			status = state
		}
		if ctx.Err() != nil {
			break
		}
	}
	return stepResult{status: status, detail: strings.Join(parts, "; ")}
}

func (a *devApp) captureInput(ctx context.Context, gen uint64, im inputMode) (string, string) {
	capture, err := a.client.OpenCapture(ctx, touchResource, "", im.mode)
	if err != nil {
		if peerUnavailable(err) {
			return "open capture failed: " + statusDetail(err), statusUnsupported
		}
		return "open capture failed: " + statusDetail(err), statusError
	}
	defer capture.Close()
	input, err := a.client.WatchInput(ctx, capture.ID())
	if err != nil {
		return "watch failed: " + statusDetail(err), statusUnsupported
	}
	defer input.Close()
	log.Printf("devtools: input gen=%d mode=%s capture=%s selected=%v ready=%v",
		gen, im.name, capture.ID(), capture.Info().Selected, capture.Info().Ready)

	var keys, gestures, raw, coordinates int
	timer := time.NewTimer(im.window)
	defer timer.Stop()
collect:
	for {
		select {
		case <-ctx.Done():
			return fmt.Sprintf("cancelled keys=%d gestures=%d raw=%d", keys, gestures, raw), statusError
		case <-timer.C:
			break collect
		case event, ok := <-input.Events():
			if !ok {
				break collect
			}
			switch {
			case event.Key != nil:
				keys++
				log.Printf("devtools: input gen=%d mode=%s kind=key key=%s pressed=%v",
					gen, im.name, event.Key.Key, event.Key.Pressed)
			case event.Gesture != nil:
				gestures++
				log.Printf("devtools: input gen=%d mode=%s kind=gesture gesture=%s phase=%s",
					gen, im.name, event.Gesture.GestureID, event.Gesture.Phase)
			case event.Raw != nil:
				raw++
				if event.Raw.CoordinatesValid {
					coordinates++
				}
				log.Printf("devtools: input gen=%d mode=%s kind=raw contacts=%d coordinates_valid=%v",
					gen, im.name, len(event.Raw.Contacts), event.Raw.CoordinatesValid)
			}
		}
	}
	_ = input.Close()
	capture.Close()
	detail := fmt.Sprintf("capture=%s selected=%v keys=%d gestures=%d raw=%d",
		capture.ID(), capture.Info().Selected, keys, gestures, raw)
	if raw > 0 {
		detail += fmt.Sprintf(" raw_with_coordinates=%d", coordinates)
	}
	if keys+gestures+raw == 0 {
		detail += fmt.Sprintf(" events=0 window_ms=%d", im.window.Milliseconds())
	}
	log.Printf("devtools: input gen=%d mode=%s window_ms=%d keys=%d gestures=%d raw=%d",
		gen, im.name, im.window.Milliseconds(), keys, gestures, raw)
	return detail, statusOK
}

// --- patterns ----------------------------------------------------------

// Segment bits 0-5 are the six outer segments; bit 6 is the middle dash.
var digitMasks = map[byte]uint16{
	'0': 1<<0 | 1<<1 | 1<<2 | 1<<3 | 1<<4 | 1<<5,
	'1': 1<<1 | 1<<2,
	'2': 1<<0 | 1<<1 | 1<<6 | 1<<4 | 1<<3,
	'3': 1<<0 | 1<<1 | 1<<2 | 1<<3 | 1<<6,
	'4': 1<<5 | 1<<6 | 1<<1 | 1<<2,
	'5': 1<<0 | 1<<5 | 1<<6 | 1<<2 | 1<<3,
	'6': 1<<0 | 1<<5 | 1<<6 | 1<<2 | 1<<3 | 1<<4,
	'7': 1<<0 | 1<<1 | 1<<2,
	'8': 1<<0 | 1<<1 | 1<<2 | 1<<3 | 1<<4 | 1<<5 | 1<<6,
	'9': 1<<0 | 1<<1 | 1<<2 | 1<<3 | 1<<5 | 1<<6,
}

func segmentMasks(pattern string, digits int) []uint16 {
	if digits <= 0 || pattern == "" {
		return nil
	}
	mask := make([]uint16, digits)
	for i := range digits {
		mask[i] = digitMasks[pattern[i%len(pattern)]]
	}
	return mask
}

func maskHex(cells []uint16) string {
	parts := make([]string, len(cells))
	for i, cell := range cells {
		parts[i] = fmt.Sprintf("0x%04x", cell)
	}
	return strings.Join(parts, ",")
}

func ringPattern(pixels int) []capsula.RGB {
	frame := make([]capsula.RGB, pixels)
	last := float64(max(pixels-1, 1))
	for i := range frame {
		position := float64(i) / last
		frame[i] = capsula.RGB{
			R: uint8(16 * (position + 1) / 2),
			G: uint8(64 + 128*position),
			B: uint8(128 + 96*position),
		}
	}
	return frame
}

func rgbSummary(pixels []capsula.RGB) string {
	if len(pixels) == 0 {
		return "(none)"
	}
	first, last := pixels[0], pixels[len(pixels)-1]
	return fmt.Sprintf("first=%d,%d,%d last=%d,%d,%d",
		first.R, first.G, first.B, last.R, last.G, last.B)
}
