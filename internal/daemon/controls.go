package daemon

import (
	"fmt"
	"math"

	"capsulad/internal/settings"
)

// Canonical system control paths.
//
// App-facing RPCs, the physical volume/microphone keys and Bluetooth volume
// synchronization all mutate the same durable setting through these methods and
// then let the settings watcher drive the backend. Keeping one writer prevents
// a key press, an app call and an AVRCP event from racing into divergent
// hardware states or bypassing the muted-indicator policy.

// applyMasterVolume persists one validated master-volume level and applies it
// to the local mixer immediately. The settings watcher also applies it, making
// the call idempotent.
func (d *Daemon) applyMasterVolume(v float64) error {
	if v < 0 || v > 1 {
		return fmt.Errorf("volume must be in [0,1]")
	}
	if err := d.writeUserSettings([]string{"master_volume"}, func(s *settings.UserSettings) {
		s.MasterVolume = v
	}); err != nil {
		return err
	}
	d.audio.SetMasterVolume(v)
	d.applyBluetoothGain(v)
	return nil
}

// restoreVolumeAfterFullSwipe undoes the leading volume step of a full-panel
// swipe through the same canonical master path as the step itself, so the
// persisted value, the local mixer and the Bluetooth gain stay consistent. The
// media command is dispatched separately; this only removes the audible step.
func (d *Daemon) restoreVolumeAfterFullSwipe(level float64) {
	if err := d.applyMasterVolume(level); err != nil {
		d.logs.Warnf("daemon", "", "could not revert the full-panel swipe volume step: %v", err)
		return
	}
	// The step being undone was never the user's level, so it does not deserve
	// an overlay: a crossing must not flash the volume number on the display for
	// a step it immediately takes back. A pending overlay is closed instead.
	d.hideVolumeOSD()
	d.logs.Infof("daemon", "", "full-panel swipe reverted the leading volume step to %d%%", int(math.Round(level*100)))
}

// applyMicrophoneMute persists the microphone mute state. The settings watcher
// owns the muted-indicator lifetime and microphone watchdog transitions, so
// this path never manipulates captures itself.
func (d *Daemon) applyMicrophoneMute(muted bool) error {
	if err := d.writeUserSettings([]string{"microphone_muted"}, func(s *settings.UserSettings) {
		s.MicrophoneMuted = muted
	}); err != nil {
		return err
	}
	d.audio.SetMuted(muted)
	d.wake.SetMuted(muted)
	return nil
}
