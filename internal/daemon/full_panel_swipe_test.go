package daemon

import (
	"math"
	"testing"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/bluetooth"
	"capsulad/internal/routing"
)

// A full-panel crossing is dispatched as a media skip and its leading volume
// step is reverted through the canonical master-volume path, so the persisted
// value is exactly the level the input manager captured before that step. A
// following lone tap still steps normally.
func TestFullPanelSwipeRevertsThroughMasterVolume(t *testing.T) {
	d, cancel, _ := newTestDaemon(t)
	defer cancel()

	if err := d.applyMasterVolume(0.5); err != nil {
		t.Fatal(err)
	}
	base := d.clock.NowNS()
	panelAt := func(offset time.Duration, key string, pressed bool) {
		d.input.Handle(backend.InputEvent{MonotonicNS: base + uint64(offset),
			Key: &backend.KeyEvent{Key: key, Pressed: pressed}})
	}

	panelAt(0, "volume_down", true)
	panelAt(60*time.Millisecond, "volume_down", false)
	if got := d.user.Get().Values.MasterVolume; math.Abs(got-0.45) > 1e-9 {
		t.Fatalf("leading edge persisted %v, want 0.45 before the revert", got)
	}
	panelAt(120*time.Millisecond, "volume_up", true)
	panelAt(180*time.Millisecond, "volume_up", false)
	if got := d.user.Get().Values.MasterVolume; math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("full-panel swipe persisted %v, want the pre-step 0.5", got)
	}

	// The source never sees the transient step: the mirror coalescer outlasts
	// the crossing window, so only the restored level is written. The field
	// report this pins is "a full-panel swipe triggers the volume control on both
	// sides" — the speaker's overlay and the source's level must both stay put.
	mem, ok := d.bt.(*bluetooth.Memory)
	if !ok {
		t.Fatalf("simulator Bluetooth manager is %T", d.bt)
	}
	waitFor(t, 3*time.Second, "mirror settles to the restored level", func() bool {
		level, written := mem.TransportVolumeWritten()
		return written && level == uint16(math.Round(0.5*127))
	})
	if sel := d.router.Current("display.segment.front"); sel.SelectedAppID == routing.SystemAppID {
		t.Fatalf("a reverted crossing left the volume overlay selected: %+v", sel)
	}

	// The gesture must not swallow a later, unrelated tap.
	panelAt(2*time.Second, "volume_up", true)
	panelAt(2*time.Second+60*time.Millisecond, "volume_up", false)
	if got := d.user.Get().Values.MasterVolume; math.Abs(got-0.55) > 1e-9 {
		t.Fatalf("lone tap after a full-panel swipe persisted %v, want 0.55", got)
	}
}
