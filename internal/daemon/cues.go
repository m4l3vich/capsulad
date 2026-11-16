package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/presentations"
	"capsulad/internal/routing"
	"capsulad/internal/sounds"
)

// --- framework UI cues ------------------------------------------------------

// uiSoundTrim attenuates the authored cues before the mixer's master gain.
// The ubuntu-sounds set peaks around -3 dBFS (the startup jingle at -1 dBFS);
// at full master that is a near-full-scale transient, which was painful on the
// Mini's amplifier. -8 dB keeps every cue clearly audible at mid volume without
// making a volume step or a mute toggle startling.
const uiSoundTrim = 0.4

// uiSoundChunk bounds one write so a long clip (the startup jingle is over ten
// seconds) streams through the mixer's bounded queue instead of being copied
// whole into the resampler.
const uiSoundChunk = 16 << 10

// playUISound plays one embedded cue through the mixer, so its loudness follows
// the selected master volume. It owns no presentation and releases the
// playback stream on every path; the caller must not block on the clip.
func (d *Daemon) playUISound(clip sounds.Clip) { d.playUISoundWhen(clip, nil) }

// playUISoundWhen is playUISound with an optional acknowledgement that runs
// once the playback stream is open, so a one-shot cue is recorded as delivered
// only after it actually reached the mixer.
func (d *Daemon) playUISoundWhen(clip sounds.Clip, onStart func()) {
	if len(clip.PCM) == 0 || d.dev.Audio() == nil {
		return
	}
	format := backend.AudioFormat{Format: backend.S16LE, RateHz: clip.RateHz, Channels: clip.Channels}
	go func() {
		p, err := d.openAppPlayback(routing.SystemAppID, "", "audio.output.speaker", format, "")
		if err != nil {
			d.logs.Warnf("audio", "", "ui sound: %v", err)
			return
		}
		if onStart != nil {
			onStart()
		}
		p.SetTrim(uiSoundTrim)
		defer p.CloseInput()
		frameBytes := clip.Channels * 2
		for off := 0; off < len(clip.PCM); off += uiSoundChunk {
			end := off + uiSoundChunk
			if end > len(clip.PCM) {
				end = len(clip.PCM)
			}
			end -= (end - off) % frameBytes
			if err := p.Write(clip.PCM[off:end]); err != nil {
				d.logs.Warnf("audio", "", "ui sound: %v", err)
				return
			}
		}
	}()
}

// playVolumeCue plays the volume-step cue, so its loudness follows the volume
// that was just selected.
func (d *Daemon) playVolumeCue() { d.playUISound(sounds.Volume()) }

// audibleMicIndication is the audible half of the live-microphone indication
// for supervised application captures. It returns an error instead of letting
// a capture proceed silently: no output device, an explicit zero volume or
// mute, a cue the mixer cannot open, or a cue that never drains all deny the
// capture. The mixer makes this playback the exclusive source while it runs,
// and the caller waits for the stream to drain to the output sink rather than
// accepting queue admission as proof.
func (d *Daemon) audibleMicIndication(appID string) error {
	if d.dev == nil || d.dev.Audio() == nil {
		return fmt.Errorf("no audio output for the microphone indication")
	}
	if d.audio == nil || d.audio.MasterVolume() <= 0 {
		return fmt.Errorf("master volume is zero; raise it to enable microphone capture")
	}
	if d.audio.Muted() {
		return fmt.Errorf("microphone is globally muted")
	}
	if err := d.playMicCue(); err != nil {
		d.logs.Warnf("audio", appID, "microphone capture refused: %v", err)
		return err
	}
	d.logs.Infof("audio", appID, "microphone live cue consumed by the mixer")
	return nil
}

// playMicCue plays the microphone-live cue as the exclusive mixer source and
// waits until the mixer has drained it. Exclusivity freezes application
// playbacks for the cue's duration, so a flood cannot mask the indication.
func (d *Daemon) playMicCue() error {
	clip := sounds.MicrophoneLive()
	if len(clip.PCM) == 0 {
		return fmt.Errorf("the microphone cue is unavailable")
	}
	format := backend.AudioFormat{Format: backend.S16LE, RateHz: clip.RateHz, Channels: clip.Channels}
	p, err := d.openAppPlayback(routing.SystemAppID, "", "audio.output.speaker", format, "")
	if err != nil {
		return fmt.Errorf("the microphone cue could not be opened: %w", err)
	}
	defer p.Close()
	d.audio.BeginIndicationCue(p.ID())
	defer d.audio.EndIndicationCue(p.ID())
	p.SetTrim(uiSoundTrim)
	frameBytes := clip.Channels * 2
	for off := 0; off < len(clip.PCM); off += uiSoundChunk {
		end := off + uiSoundChunk
		if end > len(clip.PCM) {
			end = len(clip.PCM)
		}
		end -= (end - off) % frameBytes
		if err := p.Write(clip.PCM[off:end]); err != nil {
			return fmt.Errorf("the microphone cue could not be queued: %w", err)
		}
	}
	p.CloseInput()

	// The mixer closes the stream only after it has pulled the remaining
	// frames and written them to the output sink. A notice other than normal
	// completion, or a volume/mute change during the cue, is a hard failure.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	health := time.NewTicker(25 * time.Millisecond)
	defer health.Stop()
	for {
		select {
		case <-deadline.C:
			return fmt.Errorf("the microphone cue was not consumed by the mixer")
		case <-health.C:
			if d.audio.MasterVolume() <= 0 {
				return fmt.Errorf("master volume dropped to zero during the microphone cue")
			}
			if d.audio.Muted() {
				return fmt.Errorf("the microphone was muted during the cue")
			}
		case ev, ok := <-p.Events():
			if !ok {
				return nil
			}
			if ev.Notice != nil && ev.Notice.Code != "STREAM_CLOSED" {
				return fmt.Errorf("the microphone cue stopped: %s: %s", ev.Notice.Code, ev.Notice.Detail)
			}
		}
	}
}

// startupSoundMarker records that the installation startup cue has already
// played. Its absence is what defines the first boot of this daemon on a data
// directory, so an upgraded device plays the cue once and then follows the
// persisted setting.
const startupSoundMarker = "startup-sound.played"

// maybePlayStartupCue plays the jingle on the first boot after installation,
// then only when the user enabled the startup sound. The cue never blocks
// startup: the clip is over ten seconds long and streams through the mixer.
func (d *Daemon) maybePlayStartupCue() {
	due, installation, err := d.startupCueDue()
	if err != nil {
		d.logs.Warnf("audio", "", "startup cue marker: %v", err)
		return
	}
	if !due {
		return
	}
	var delivered func()
	if installation {
		marker := filepath.Join(d.cfg.DataDir, startupSoundMarker)
		delivered = func() {
			if err := os.WriteFile(marker, []byte("startup cue played\n"), 0o644); err != nil {
				d.logs.Warnf("audio", "", "startup cue marker: %v", err)
			}
		}
	}
	d.playUISoundWhen(sounds.Startup(), delivered)
}

// startupCueDue reports whether the boot cue should play and whether this boot
// is the installation boot that records the marker. A missing marker is the
// installation definition: the cue plays once and is then governed by the
// persisted setting.
func (d *Daemon) startupCueDue() (due, installation bool, err error) {
	_, err = os.Stat(filepath.Join(d.cfg.DataDir, startupSoundMarker))
	switch {
	case err == nil:
		return d.user.Get().Values.StartupSoundEnabled, false, nil
	case os.IsNotExist(err):
		return true, true, nil
	default:
		return false, false, err
	}
}

// --- volume percentage on the segment display -------------------------------

// segment digit masks: bit 0 = a (top) ... bit 6 = g (middle), matching the
// bundled clock app.
var volumeDigits = [10]uint16{
	0b0111111, 0b0000110, 0b1011011, 0b1001111, 0b1100110,
	0b1101101, 0b1111101, 0b0000111, 0b1111111, 0b1101111,
}

// volumeCells right-aligns the percentage into the four cells, so 35 reads as
// "  35" and 100 as " 100".
func volumeCells(pct int) []uint16 {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	cells := make([]uint16, 4)
	cells[3] = volumeDigits[pct%10]
	if pct >= 10 {
		cells[2] = volumeDigits[(pct/10)%10]
	}
	if pct >= 100 {
		cells[1] = volumeDigits[1]
	}
	return cells
}

// volumeOSDTimeout is how long the percentage stays on the segment display
// after the last volume step.
const volumeOSDTimeout = 900 * time.Millisecond

// showVolumeOSD overlays the new percentage on the segment display for a
// moment. It is a system capture like the muted-microphone indication, so the
// clock resumes on release.
func (d *Daemon) showVolumeOSD(pct int) {
	resourceID := ""
	for _, info := range d.resources {
		if info.Kind == frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY {
			resourceID = info.ResourceId
			break
		}
	}
	if resourceID == "" {
		return
	}
	d.osdMu.Lock()
	defer d.osdMu.Unlock()
	if d.osdPres == nil {
		pres, err := d.openAppPresentation(routing.SystemAppID, "", resourceID)
		if err != nil {
			return
		}
		d.osdPres = pres
	}
	if err := d.updateAppPresentation(routing.SystemAppID, "", d.osdPres, presentations.SegmentFrame(volumeCells(pct), false)); err != nil {
		return
	}
	if d.osdCapture == "" {
		if cap, err := d.acquireAppCapture(routing.SystemAppID, "", resourceID, d.osdPres.ID, routing.TouchNone); err == nil {
			d.osdCapture = cap.ID
		}
	}
	if d.osdTimer != nil {
		d.osdTimer.Stop()
	}
	// Ownership is a generation, not the timer pointer: the callback must never
	// read the variable the caller is still assigning to (a data race the race
	// detector caught through the full-panel swipe revert), and a stale callback
	// must not blank a newer percentage.
	d.osdGen++
	gen := d.osdGen
	d.osdTimer = time.AfterFunc(volumeOSDTimeout, func() { d.expireVolumeOSD(gen) })
}

// hideVolumeOSD closes a volume overlay without showing a level. It is used when
// the step that opened it was taken back, so the display returns to its selected
// baseline instead of flashing a number the user never chose.
func (d *Daemon) hideVolumeOSD() {
	d.osdMu.Lock()
	defer d.osdMu.Unlock()
	d.osdGen++
	if d.osdTimer != nil {
		d.osdTimer.Stop()
		d.osdTimer = nil
	}
	if d.osdCapture != "" {
		d.releaseAppCapture(routing.SystemAppID, "", d.osdCapture)
		d.osdCapture = ""
	}
	if d.osdPres != nil {
		_ = d.updateAppPresentation(routing.SystemAppID, "", d.osdPres, presentations.Blank())
		d.closeAppPresentation(routing.SystemAppID, "", d.osdPres)
		d.osdPres = nil
	}
}

// expireVolumeOSD releases the overlay only while gen still owns it: the
// callback may already have been running when the next volume step replaced the
// timer, and it must not blank the fresh percentage.
func (d *Daemon) expireVolumeOSD(gen uint64) {
	d.osdMu.Lock()
	defer d.osdMu.Unlock()
	if d.osdGen != gen {
		return
	}
	d.osdTimer = nil
	if d.osdCapture != "" {
		d.releaseAppCapture(routing.SystemAppID, "", d.osdCapture)
		d.osdCapture = ""
	}
	if d.osdPres != nil {
		_ = d.updateAppPresentation(routing.SystemAppID, "", d.osdPres, presentations.Blank())
		d.closeAppPresentation(routing.SystemAppID, "", d.osdPres)
		d.osdPres = nil
	}
}
