package daemon

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/presentations"
	"capsulad/internal/presets"
	"capsulad/internal/routing"
)

// The wakeword wizard uses the ring as a recording indicator and, for takes
// that contain speech, an audible countdown. The cue is generated here instead
// of shipping a sound file: the PCM format follows the playback device.

const (
	recordPulsePreset = "system.ring.pulse"

	cueShortMS = 120
	cueGapMS   = 880
	cueLongMS  = 1000
	cueShortHz = 660
	cueLongHz  = 880
	// Full-scale tone: the audio mixer applies the global volume, so the cue
	// tracks the selected volume instead of carrying a second fixed gain.
	cueAmplitude = 1.0
)

// beginRecordingIndicator pulses the ring red for the duration of a take. It
// is a system capture like the muted-microphone indication, so it overlays
// whatever an app was showing and the previous presentation resumes on release.
// It returns an error when a visible indication cannot be opened: an
// enrollment take must not record with only a log line as feedback.
func (d *Daemon) beginRecordingIndicator() error {
	if d.pres == nil || d.router == nil {
		return fmt.Errorf("presentation manager is unavailable")
	}
	ringID := d.ringResourceID()
	if ringID == "" {
		return fmt.Errorf("no ring resource for the recording indication")
	}
	params, err := presets.ValidateParams(mustPreset(recordPulsePreset), presets.Params{
		"speed": 1.0, "color": rgb(255, 0, 0), "min_intensity": 0.08,
	})
	if err != nil {
		return fmt.Errorf("recording indicator preset: %w", err)
	}

	d.recIndMu.Lock()
	defer d.recIndMu.Unlock()
	if d.recCaptureID != "" {
		return nil
	}
	if d.recIndicator == nil {
		pres, err := d.openAppPresentation(routing.SystemAppID, "", ringID)
		if err != nil {
			return fmt.Errorf("open recording indicator: %w", err)
		}
		d.recIndicator = pres
	}
	if err := d.updateAppPresentation(routing.SystemAppID, "", d.recIndicator, presentations.RingEffect(recordPulsePreset, params)); err != nil {
		return fmt.Errorf("draw recording indicator: %w", err)
	}
	cap, err := d.acquireAppCapture(routing.SystemAppID, "", ringID, d.recIndicator.ID, routing.TouchNone)
	if err != nil {
		return fmt.Errorf("select recording indicator: %w", err)
	}
	d.recCaptureID = cap.ID
	return nil
}

func (d *Daemon) endRecordingIndicator() {
	d.recIndMu.Lock()
	defer d.recIndMu.Unlock()
	if d.recCaptureID != "" && d.router != nil {
		d.releaseAppCapture(routing.SystemAppID, "", d.recCaptureID)
		d.recCaptureID = ""
	}
	if d.recIndicator != nil && d.pres != nil {
		_ = d.updateAppPresentation(routing.SystemAppID, "", d.recIndicator, presentations.Blank())
		d.closeAppPresentation(routing.SystemAppID, "", d.recIndicator)
		d.recIndicator = nil
	}
}

func (d *Daemon) logf(format string, args ...any) {
	if d.logs != nil {
		d.logs.Warnf("wakeword", "", format, args...)
	}
}

// playRecordingCue plays a three-second countdown — short, short, long — and
// returns once the mixer has drained it. Recording must not start before this
// returns or the microphone would capture the countdown itself.
func (d *Daemon) playRecordingCue(ctx context.Context) error {
	if d.audio == nil || d.dev == nil {
		return fmt.Errorf("no audio device for the start tones")
	}
	dev := d.dev.Audio()
	if dev == nil {
		return fmt.Errorf("no audio device for the start tones")
	}
	format := dev.PlaybackFormat()
	p, err := d.openAppPlayback(routing.SystemAppID, "", "audio.output.speaker", format, "")
	if err != nil {
		return fmt.Errorf("play start tones: %w", err)
	}
	defer p.Close()
	if d.audio != nil {
		d.audio.BeginIndicationCue(p.ID())
		defer d.audio.EndIndicationCue(p.ID())
	}
	if err := p.Write(countdownCuePCM(format)); err != nil {
		return fmt.Errorf("play start tones: %w", err)
	}
	p.CloseInput()
	// The countdown is three seconds. A context without its own deadline must
	// not leave enrollment waiting forever, and a volume/mute change while the
	// cue drains means the take can no longer prove audible feedback.
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	health := time.NewTicker(25 * time.Millisecond)
	defer health.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("the recording cue was not consumed by the mixer")
		case <-health.C:
			if d.audio.MasterVolume() <= 0 {
				return fmt.Errorf("master volume dropped to zero during the recording cue")
			}
			if d.audio.Muted() {
				return fmt.Errorf("the microphone was muted during the recording cue")
			}
		case ev, ok := <-p.Events():
			if !ok {
				return nil
			}
			if ev.Notice != nil && ev.Notice.Code != "STREAM_CLOSED" {
				return fmt.Errorf("the recording cue stopped: %s: %s", ev.Notice.Code, ev.Notice.Detail)
			}
		}
	}
}

// countdownCuePCM renders one tone per second: short at 0 s, short at 1 s and
// a one-second long tone at 2 s that ends exactly when capture may start. The
// 6 ms fade avoids clicks at each tone edge.
func countdownCuePCM(format backend.AudioFormat) []byte {
	rate := format.RateHz
	if rate <= 0 {
		rate = 48000
	}
	channels := format.Channels
	if channels <= 0 {
		channels = 2
	}
	type tone struct {
		freq   float64
		ms     int
		silent bool
	}
	sequence := []tone{
		{freq: cueShortHz, ms: cueShortMS},
		{silent: true, ms: cueGapMS},
		{freq: cueShortHz, ms: cueShortMS},
		{silent: true, ms: cueGapMS},
		{freq: cueLongHz, ms: cueLongMS},
	}
	fadeFrames := rate * 6 / 1000
	var frames int
	for _, t := range sequence {
		frames += rate * t.ms / 1000
	}
	out := make([]byte, frames*channels*2)
	frame := 0
	for _, t := range sequence {
		n := rate * t.ms / 1000
		for i := range n {
			var v float64
			if !t.silent {
				env := 1.0
				if i < fadeFrames {
					env = float64(i) / float64(fadeFrames)
				}
				if rem := n - 1 - i; rem < fadeFrames {
					env = math.Min(env, float64(rem)/float64(fadeFrames))
				}
				v = cueAmplitude * env * math.Sin(2*math.Pi*t.freq*float64(i)/float64(rate))
			}
			sample := uint16(int16(v * 32767))
			for c := range channels {
				binary.LittleEndian.PutUint16(out[(frame*channels+c)*2:], sample)
			}
			frame++
		}
	}
	return out
}
