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
// the call idempotent. volumeMu orders every canonical caller (physical keys,
// Bluetooth adoption, the Web UI's audio control and the AirPlay receiver), so
// two identical requests cannot race into divergent durable or live states.
func (d *Daemon) applyMasterVolume(v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
		return fmt.Errorf("volume must be a finite value in [0,1]")
	}
	d.volumeMu.Lock()
	defer d.volumeMu.Unlock()
	return d.applyMasterVolumeLocked(v)
}

// applyMasterVolumeLocked is the durable-write boundary. The caller holds
// volumeMu from the mutation through the live apply, and v is already validated
// as a finite [0,1] level. check runs under the settings store lock immediately
// before the commit, so a queued request cannot persist after its context or
// AirPlay run identity went away while it waited for an independent settings
// writer.
func (d *Daemon) applyMasterVolumeLocked(v float64) error {
	return d.applyMasterVolumeCheckedLocked(v, nil)
}

func (d *Daemon) applyMasterVolumeCheckedLocked(v float64, check func() error) error {
	if _, err := d.user.SetMasterVolume(v, check); err != nil {
		return err
	}
	d.applyLiveMasterVolumeLocked(v)
	return nil
}

// applyLiveMasterVolumeLocked applies one level to the mixer and then to the
// Bluetooth gain. Callers hold volumeMu so the durable value and both live
// consumers always observe the same order.
func (d *Daemon) applyLiveMasterVolumeLocked(v float64) {
	d.audio.SetMasterVolume(v)
	d.applyBluetoothGain(v)
}

// applyLatestMasterVolume applies the newest durable master level to the mixer
// and Bluetooth gain. The settings watcher can still be holding an older
// queued snapshot when a physical key, a Bluetooth report or the AirPlay
// receiver has already committed a newer level; reading the durable value
// under volumeMu keeps that queued snapshot from briefly restoring the older
// level. The watcher's unrelated settings reconciliation stays outside the
// lock.
func (d *Daemon) applyLatestMasterVolume() {
	d.volumeMu.Lock()
	defer d.volumeMu.Unlock()
	d.applyLiveMasterVolumeLocked(d.user.Get().Values.MasterVolume)
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
