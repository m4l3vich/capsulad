package daemon

import (
	"slices"
	"testing"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/presentations"
	"capsulad/internal/routing"
	"capsulad/internal/settings"
)

func TestOutputClearsUnownedDisplayAndRing(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitForAppSelection(t, d, "display.segment.front", clockAppID)
	waitForAppSelection(t, d, "light.ring.main", ringAppID)
	if err := d.writeUserSettings([]string{"default_app_by_resource"}, func(v *settings.UserSettings) {
		v.DefaultAppByResource = map[string]string{}
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "no baseline providers", func() bool {
		return d.router.Current("display.segment.front").SelectedPresentation == "" && d.router.Current("light.ring.main").SelectedPresentation == ""
	})
	sim := d.dev.(*backend.Simulator)
	ring := d.pres.Open("light.ring.main", "test.output", "output-run", presentations.Ring)
	segment := d.pres.Open("display.segment.front", "test.output", "output-run", presentations.Segment)
	pixels := make([]backend.RGB, sim.Ring().Caps().PixelCount)
	for i := range pixels {
		pixels[i] = backend.RGB{R: 120, G: 40}
	}
	cells := []uint16{0x7f, 0x7f, 0x7f, 0x7f}
	for p, content := range map[*presentations.Presentation]presentations.Content{
		ring: presentations.RingFrame(pixels), segment: presentations.SegmentFrame(cells, true),
	} {
		if err := d.pres.Update(p, content); err != nil {
			t.Fatal(err)
		}
		if _, err := d.router.Acquire(p.ResourceID, p.AppID, p.RunID, p.ID, routing.TouchNone); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 3*time.Second, "captured frames reach backend", func() bool {
		return slices.Equal(sim.RingSnapshot().Pixels, pixels) && slices.Equal(sim.SegmentSnapshot().Cells, cells) && sim.SegmentSnapshot().Colon
	})
	d.pres.Close(ring)
	d.pres.Close(segment)
	blankPixels := make([]backend.RGB, len(pixels))
	waitFor(t, time.Second, "unowned outputs physically cleared", func() bool {
		r, s := sim.RingSnapshot(), sim.SegmentSnapshot()
		return slices.Equal(r.Pixels, blankPixels) && slices.Equal(s.Cells, []uint16{0, 0, 0, 0}) && !s.Colon
	})
}

func TestOutputAppliesBrightnessToStaticFrames(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitForAppSelection(t, d, "light.ring.main", ringAppID)
	sim := d.dev.(*backend.Simulator)
	p := d.pres.Open("light.ring.main", "test.output", "output-run", presentations.Ring)
	if err := d.pres.Update(p, presentations.RingFrame([]backend.RGB{{R: 100}})); err != nil {
		t.Fatal(err)
	}
	if _, err := d.router.Acquire(p.ResourceID, p.AppID, p.RunID, p.ID, routing.TouchNone); err != nil {
		t.Fatal(err)
	}
	defer d.pres.Close(p)
	for _, brightness := range []float64{0.8, 0.2, 0, 0.5} {
		if err := d.writeUserSettings([]string{"display_brightness"}, func(v *settings.UserSettings) {
			v.DisplayBrightness["light.ring.main"] = brightness
		}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, time.Second, "static ring brightness update", func() bool {
			frame := sim.RingSnapshot()
			return len(frame.Pixels) > 0 && frame.Pixels[0].R == 100 && frame.Brightness == brightness
		})
	}
}
