package daemon

import (
	"testing"

	"capsulad/internal/backend"
	"capsulad/internal/presentations"
	"capsulad/internal/presets"
	"capsulad/internal/routing"
)

func TestMicrophoneIndicationRequiresLitContent(t *testing.T) {
	d := startEmptyDaemon(t, shortTempDir(t))
	waitForAppSelection(t, d, "light.ring.main", ringAppID)
	for _, tc := range []struct {
		name     string
		resource string
		kind     presentations.Kind
		content  presentations.Content
		visible  bool
	}{
		{"blank ring", "light.ring.main", presentations.Ring, presentations.Blank(), false},
		{"black ring", "light.ring.main", presentations.Ring, presentations.RingFrame([]backend.RGB{{}}), false},
		{"lit ring", "light.ring.main", presentations.Ring, presentations.RingFrame([]backend.RGB{{G: 100}}), true},
		{"invisible effect", "light.ring.main", presentations.Ring, presentations.RingEffect("system.ring.pulse", mustParams("system.ring.pulse", presets.Params{"speed": 1.0, "color": backend.RGB{}, "min_intensity": 0.0})), false},
		{"pulse dark phase", "light.ring.main", presentations.Ring, presentations.RingEffect("system.ring.pulse", mustParams("system.ring.pulse", presets.Params{"speed": 1.0, "color": backend.RGB{B: 255}, "min_intensity": 0.0})), true},
		{"blank segment", "display.segment.front", presentations.Segment, presentations.Blank(), false},
		{"black segment", "display.segment.front", presentations.Segment, presentations.SegmentFrame([]uint16{0, 0, 0, 0}, false), false},
		{"lit segment", "display.segment.front", presentations.Segment, presentations.SegmentFrame([]uint16{1, 0, 0, 0}, false), true},
		{"lit colon", "display.segment.front", presentations.Segment, presentations.SegmentFrame([]uint16{0, 0, 0, 0}, true), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := d.pres.Open(tc.resource, "test.indication", "indication-run", tc.kind)
			defer d.pres.Close(p)
			if err := d.pres.Update(p, tc.content); err != nil {
				t.Fatal(err)
			}
			if _, err := d.router.Acquire(tc.resource, p.AppID, p.RunID, p.ID, routing.TouchNone); err != nil {
				t.Fatal(err)
			}
			if got := d.indicationActive(p.AppID); got != tc.visible {
				t.Fatalf("indication active = %v, want %v", got, tc.visible)
			}
		})
	}
}
