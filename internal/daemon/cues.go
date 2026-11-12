package daemon

import (
	"encoding/binary"
	"math"
	"sync"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/presentations"
	"capsulad/internal/routing"
)

// --- volume cue -------------------------------------------------------------

// volumeCueOnce caches the synthesized cue; it is deterministic so only the
// sample format decides the buffer.
var (
	volumeCueOnce sync.Once
	volumeCueData []byte
)

// volumeCuePCM returns a short percussive tick: a noise transient, two fast
// decaying partials and a low thump. It is generated here rather than taken
// from the stock sound resources. The synthesized waveform is deliberately
// quiet: the mixer still applies the master gain, but a near-full-scale
// transient became painful at mid volume on the Mini's amplifier.
func volumeCuePCM(format backend.AudioFormat) []byte {
	volumeCueOnce.Do(func() {
		rate := format.RateHz
		if rate <= 0 {
			rate = 48000
		}
		channels := format.Channels
		if channels <= 0 {
			channels = 2
		}
		const seconds = 0.09
		frames := int(float64(rate) * seconds)
		out := make([]byte, frames*channels*2)
		noise := uint32(0x2545F491)
		for i := 0; i < frames; i++ {
			t := float64(i) / float64(rate)
			noise = noise*1664525 + 1013904223
			n := float64(int32(noise)>>8) / float64(1<<23) // [-1,1)
			click := n * 0.55 * math.Exp(-t/0.0025)
			body := 0.42 * math.Exp(-t/0.018) * math.Sin(2*math.Pi*420*t)
			top := 0.22 * math.Exp(-t/0.010) * math.Sin(2*math.Pi*1050*t)
			thump := 0.30 * math.Exp(-t/0.032) * math.Sin(2*math.Pi*180*t)
			// A UI cue, not content: keep the peak around -13 dBFS before the
			// mixer's master gain. At 50 % that is roughly -19 dBFS, audible
			// without the harsh full-scale click the old 0.95 peak produced.
			v := (click + body + top + thump) * 0.22
			if v > 0.22 {
				v = 0.22
			} else if v < -0.22 {
				v = -0.22
			}
			s := int16(v * 32767)
			for c := 0; c < channels; c++ {
				binary.LittleEndian.PutUint16(out[(i*channels+c)*2:], uint16(s))
			}
		}
		volumeCueData = out
	})
	return volumeCueData
}

// playVolumeCue plays the tick through the mixer, so its loudness follows the
// volume that was just selected.
func (d *Daemon) playVolumeCue() {
	dev := d.dev.Audio()
	if dev == nil {
		return
	}
	format := dev.PlaybackFormat()
	p, err := d.openAppPlayback(routing.SystemAppID, "", "audio.output.speaker", format, "")
	if err != nil {
		d.logs.Warnf("audio", "", "volume cue: %v", err)
		return
	}
	if err := p.Write(volumeCuePCM(format)); err != nil {
		d.logs.Warnf("audio", "", "volume cue: %v", err)
	}
	p.CloseInput()
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
	d.osdTimer = time.AfterFunc(900*time.Millisecond, func() {
		d.osdMu.Lock()
		defer d.osdMu.Unlock()
		if d.osdCapture != "" {
			d.releaseAppCapture(routing.SystemAppID, "", d.osdCapture)
			d.osdCapture = ""
		}
		if d.osdPres != nil {
			_ = d.updateAppPresentation(routing.SystemAppID, "", d.osdPres, presentations.Blank())
			d.closeAppPresentation(routing.SystemAppID, "", d.osdPres)
			d.osdPres = nil
		}
		d.osdTimer = nil
	})
}
