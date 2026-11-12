package daemon

import (
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
	var timer *time.Timer
	timer = time.AfterFunc(volumeOSDTimeout, func() { d.expireVolumeOSD(timer) })
	d.osdTimer = timer
}

// expireVolumeOSD releases the overlay only while timer still owns it: the
// callback may already have been running when the next volume step replaced
// the timer, and it must not blank the fresh percentage.
func (d *Daemon) expireVolumeOSD(timer *time.Timer) {
	d.osdMu.Lock()
	defer d.osdMu.Unlock()
	if d.osdTimer != timer {
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
