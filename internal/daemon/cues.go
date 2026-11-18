package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/presentations"
	"capsulad/internal/presets"
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
// The same decision shows the startup ring introduction, which owns its own
// bounded lifetime, so a missing or failed audio output cannot strand it.
//
// The installation marker is written only after the cue actually reaches the
// mixer (see the commit that introduced it): a boot without audio must not
// consume the one-shot cue. The visual mirrors that decision rather than
// inventing a second rule, so when the cue cannot be opened both the jingle and
// the introduction are retried on the next boot. Once the marker exists, a
// disabled startup sound produces neither.
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
	d.showStartupRing()
	d.playUISoundWhen(sounds.Startup(), delivered)
}

// --- startup ring introduction ----------------------------------------------

const (
	startupRingPreset = "system.ring.indicator"

	// The introduction is a short boot accent, not a second baseline effect:
	// the jingle is over ten seconds long and does not time the lights. The
	// preset itself fades the run in from dark over startupRingFadeIn seconds.
	// The hold is measured from that first frame, so the full-intensity
	// plateau after the fade-in lasts about 0.9 s and the whole introduction
	// lasts about 1.92 s. The steps below taper it back before ownership is
	// released, so the ring never snaps off.
	startupRingFadeIn  = 0.6
	startupRingHold    = 1500 * time.Millisecond
	startupRingFadeOut = 6
	startupRingStep    = 70 * time.Millisecond

	// startupRingMaxRun is the preset's largest countable run (the indicator
	// count parameter is bounded at 12). A ring larger than 36 pixels keeps a
	// 12-pixel top arc instead of silently losing the introduction.
	startupRingMaxRun = 12
)

// startupRingColor is the midpoint of the ring app's canonical aurora palette
// (#00DCB4 to #5082FF), so the boot accent is the speaker's own blue-teal
// rather than a new colour.
var startupRingColor = backend.RGB{R: 40, G: 175, B: 217}

// startupRingParams derives the run from the advertised geometry: pixel 0 is
// the top of the ring and indices advance clockwise, so a short arc from index
// 0 is the top of whatever ring the board profile fits.
func startupRingParams(pixelCount int) (presets.Params, bool) {
	if pixelCount <= 0 {
		return nil, false
	}
	count := pixelCount / 3
	if count < 1 {
		count = 1
	}
	if count > startupRingMaxRun {
		count = startupRingMaxRun
	}
	params, err := presets.ValidateParams(mustPreset(startupRingPreset), presets.Params{
		"color": startupRingColor, "start": int64(0), "count": int64(count),
		"intensity": 1.0, "fade_in": startupRingFadeIn,
	})
	if err != nil {
		return nil, false
	}
	return params, true
}

// showStartupRing marks the boot with a short blue-teal run that fades in,
// holds and fades out, then releases its capture so the selected baseline (or
// the explicit blank of a closed active window / no ring provider) is selected
// again. It is a temporary system capture like the muted-microphone
// indication, opened and released through the same facade apps use.
//
// Trigger: the same installation/startup_sound_enabled decision that plays the
// startup cue. Feedback: the lit run; the introduction has no audible half of
// its own. Ownership: one generation plus its own presentation and capture. A
// newer system indication or app capture simply takes selection priority;
// this presentation stays privately owned and its release never touches the
// newer owner. A replacement introduction, cancellation or shutdown bumps the
// generation, which is what makes a stale fade step inert. Dismissal: the
// bounded lifetime; it captures no input. Final state: capture released and
// presentation destroyed, never a forced black frame over a deliberately
// selected baseline.
func (d *Daemon) showStartupRing() {
	if d.dev == nil || d.router == nil || d.pres == nil || d.ctx == nil {
		return
	}
	ring := d.dev.Ring()
	if ring == nil {
		return
	}
	resourceID := d.ringResourceID()
	if resourceID == "" {
		return
	}
	// This is a transient framework indication, like the muted-microphone and
	// recording indications, so it does not require a selected ring provider:
	// an explicit no-provider baseline still stays dark once the introduction
	// releases. The selected baseline is never overwritten by a forced frame.
	params, ok := startupRingParams(ring.Caps().PixelCount)
	if !ok {
		return
	}

	d.startupMu.Lock()
	defer d.startupMu.Unlock()
	// An introduction that is still running is replaced, never stacked. The
	// generation bump above makes its fade a no-op against this presentation.
	d.startupGen++
	d.releaseStartupRingLocked("replaced")
	pres, err := d.openAppPresentation(routing.SystemAppID, "", resourceID)
	if err != nil {
		d.logs.Warnf("daemon", "", "startup ring presentation: %v", err)
		return
	}
	if err := d.updateAppPresentation(routing.SystemAppID, "", pres, presentations.RingEffect(startupRingPreset, params)); err != nil {
		d.logs.Warnf("daemon", "", "startup ring: %v", err)
		d.closeAppPresentation(routing.SystemAppID, "", pres)
		return
	}
	cap, err := d.acquireAppCapture(routing.SystemAppID, "", resourceID, pres.ID, routing.TouchNone)
	if err != nil {
		d.logs.Warnf("daemon", "", "startup ring capture: %v", err)
		_ = d.updateAppPresentation(routing.SystemAppID, "", pres, presentations.Blank())
		d.closeAppPresentation(routing.SystemAppID, "", pres)
		return
	}
	d.startupPres, d.startupCapture = pres, cap.ID
	gen := d.startupGen
	d.logs.Infof("daemon", "", "startup ring introduction on %s (capture=%s)", resourceID, cap.ID)
	go d.runStartupRing(gen, params)
}

// runStartupRing holds the lit run for its fade-in, then tapers it in steps
// that are each validated against the owning generation. The final dark step
// gets one frame interval before release, so the handover to the selected
// baseline is not a visible jump.
func (d *Daemon) runStartupRing(gen uint64, base presets.Params) {
	timer := time.NewTimer(startupRingHold)
	defer timer.Stop()
	select {
	case <-d.ctx.Done():
		d.finishStartupRing(gen, "cancelled")
		return
	case <-timer.C:
	}
	intensity, _ := base["intensity"].(float64)
	for i := 1; i <= startupRingFadeOut; i++ {
		params := cloneParams(base)
		params["intensity"] = intensity * (1 - float64(i)/startupRingFadeOut)
		params["fade_in"] = 0.0
		if !d.fadeStartupRing(gen, params) {
			d.finishStartupRing(gen, "failed")
			return
		}
		select {
		case <-d.ctx.Done():
			d.finishStartupRing(gen, "cancelled")
			return
		case <-time.After(startupRingStep):
		}
	}
	d.finishStartupRing(gen, "complete")
}

// fadeStartupRing writes one taper step only while gen still owns the
// introduction. The update happens under the ownership lock: a newer system
// capture may be selected over this presentation, but a stale step must never
// land after the presentation was released or replaced.
func (d *Daemon) fadeStartupRing(gen uint64, params presets.Params) bool {
	d.startupMu.Lock()
	defer d.startupMu.Unlock()
	if d.startupGen != gen || d.startupPres == nil {
		return false
	}
	if err := d.updateAppPresentation(routing.SystemAppID, "", d.startupPres, presentations.RingEffect(startupRingPreset, params)); err != nil {
		d.logs.Warnf("daemon", "", "startup ring fade: %v", err)
		return false
	}
	return true
}

// finishStartupRing releases the introduction only while gen still owns it: a
// newer indication or a shutdown has already bumped the generation, and its
// capture must be left alone.
func (d *Daemon) finishStartupRing(gen uint64, reason string) {
	d.startupMu.Lock()
	defer d.startupMu.Unlock()
	if d.startupGen != gen {
		return
	}
	d.startupGen++
	d.releaseStartupRingLocked(reason)
}

// stopStartupRing is the terminal path for shutdown and cancellation. It
// invalidates any in-flight fade before releasing through the router, so the
// selected baseline returns instead of a forced frame.
func (d *Daemon) stopStartupRing(reason string) {
	d.startupMu.Lock()
	defer d.startupMu.Unlock()
	d.startupGen++
	d.releaseStartupRingLocked(reason)
}

// releaseStartupRingLocked closes one introduction: release the capture (the
// router then selects the baseline again), blank and destroy the presentation.
// A presentation obscured by a newer capture is still ours to destroy; nothing
// here touches the newer owner.
func (d *Daemon) releaseStartupRingLocked(reason string) {
	if d.startupCapture == "" && d.startupPres == nil {
		return
	}
	selected := false
	if d.startupCapture != "" && d.router != nil {
		selected = d.router.Current(d.ringResourceID()).SelectedCaptureID == d.startupCapture
		d.releaseAppCapture(routing.SystemAppID, "", d.startupCapture)
		d.startupCapture = ""
	}
	if d.startupPres != nil {
		_ = d.updateAppPresentation(routing.SystemAppID, "", d.startupPres, presentations.Blank())
		d.closeAppPresentation(routing.SystemAppID, "", d.startupPres)
		d.startupPres = nil
	}
	if selected {
		d.logs.Infof("daemon", "", "startup ring %s; selected ring baseline restored", reason)
	} else {
		d.logs.Infof("daemon", "", "startup ring %s while another indication was selected", reason)
	}
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
