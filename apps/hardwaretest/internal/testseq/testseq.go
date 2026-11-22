// Package testseq holds the deterministic parts of the Hardware Test app:
// seven-segment masks, the two-colon blink schedule, the clockwise rainbow
// chase, the touch-key map and the microphone tone check. The app owns the
// framework streams; this package owns only the values those streams carry so
// the schedule can be tested without a daemon.
package testseq

import (
	"encoding/binary"
	"math"

	"capsulad/sdk/capsulad"
)

// Four-digit segment masks: bit 0 is segment a through bit 6 = segment g.
var digitMasks = [...]uint16{
	0x3f, 0x06, 0x5b, 0x4f, 0x66, 0x6d, 0x7d, 0x07, 0x7f, 0x6f,
}

// Clock is the fixed "12:34" glyph set.
var Clock = []uint16{digitMasks[1], digitMasks[2], digitMasks[3], digitMasks[4]}

// Digit returns the mask for a single ASCII digit, or zero when it is not one.
func Digit(d byte) uint16 {
	if d < '0' || d > '9' {
		return 0
	}
	return digitMasks[d-'0']
}

// phaseDurationNS is how long one colon-dot phase runs before the other dot
// takes over. The sequence repeats for as long as the app runs.
const phaseDurationNS = 10 * 1_000_000_000

// ColonDots returns the two colon dots for the elapsed run time. The first
// phase blinks the upper dot at 1 Hz; the second blinks the lower dot at 2 Hz.
// The inactive dot is always dark. The phase is absolute, so changing segment
// content does not restart it.
func ColonDots(elapsedNS uint64) (top, bottom bool) {
	if (elapsedNS/phaseDurationNS)%2 == 0 {
		return elapsedNS%1_000_000_000 < 500_000_000, false
	}
	return false, elapsedNS%500_000_000 < 250_000_000
}

// KeyDigit maps a panel key to the digit shown while it is held.
func KeyDigit(key string) int {
	switch key {
	case "mic_toggle":
		return 1 // north
	case "volume_up":
		return 2 // east
	case "play_pause":
		return 3 // south
	case "volume_down":
		return 4 // west
	case "center":
		return 5 // centre
	}
	return 0
}

// DigitFrame returns the four cells for a held-key digit. The digit occupies
// the rightmost cell and the other cells are dark, so the value is unambiguous
// on a four-digit display.
func DigitFrame(digit int) []uint16 {
	cells := make([]uint16, len(Clock))
	if digit >= 1 && digit <= 9 {
		cells[len(cells)-1] = digitMasks[digit]
	}
	return cells
}

// RainbowChase renders one clockwise rainbow frame. Pixel 0 is the top and
// indices advance clockwise; a full hue rotation takes rotationPeriodNS.
func RainbowChase(pixels int, elapsedNS uint64) []capsulad.RGB {
	frame := make([]capsulad.RGB, pixels)
	if pixels <= 0 {
		return frame
	}
	const rotationPeriodNS = 2 * 1_000_000_000
	offset := float64(elapsedNS%rotationPeriodNS) / float64(rotationPeriodNS)
	for i := range frame {
		hue := float64(i)/float64(pixels) - offset
		hue -= math.Floor(hue)
		frame[i] = HSV(hue, 1, 1)
	}
	return frame
}

// RedFlash renders the failure blink. The whole ring is red for one half of
// each 500 ms period and dark for the other half.
func RedFlash(pixels int, elapsedNS uint64) []capsulad.RGB {
	frame := make([]capsulad.RGB, pixels)
	if elapsedNS%500_000_000 < 250_000_000 {
		for i := range frame {
			frame[i] = capsulad.RGB{R: 255}
		}
	}
	return frame
}

// HSV converts a hue in [0,1) at full saturation and value.
func HSV(h, s, v float64) capsulad.RGB {
	if s <= 0 {
		level := uint8(math.Round(clamp01(v) * 255))
		return capsulad.RGB{R: level, G: level, B: level}
	}
	h -= math.Floor(h)
	i := int(h * 6)
	f := h*6 - float64(i)
	p := v * (1 - s)
	q := v * (1 - f*s)
	t := v * (1 - (1-f)*s)
	var r, g, b float64
	switch i % 6 {
	case 0:
		r, g, b = v, t, p
	case 1:
		r, g, b = q, v, p
	case 2:
		r, g, b = p, v, t
	case 3:
		r, g, b = p, q, v
	case 4:
		r, g, b = t, p, v
	default:
		r, g, b = v, p, q
	}
	return capsulad.RGB{
		R: uint8(math.Round(clamp01(r) * 255)),
		G: uint8(math.Round(clamp01(g) * 255)),
		B: uint8(math.Round(clamp01(b) * 255)),
	}
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	}
	return v
}

// ToneAmplitude reports the 440 Hz component and the overall RMS of PCM in
// the advertised capture format. Both are normalised to full scale.
func ToneAmplitude(pcm []byte, format capsulad.AudioFormat, frequency float64) (amplitude, rms float64) {
	channels := int(format.Channels)
	if channels <= 0 {
		channels = 1
	}
	rate := float64(format.SampleRateHz)
	if rate <= 0 {
		rate = 16000
	}
	samples := decodeSamples(pcm, format.SampleFormat, channels)
	if len(samples) == 0 {
		return 0, 0
	}
	var sumSquares, re, im float64
	for i, sample := range samples {
		sumSquares += sample * sample
		angle := 2 * math.Pi * frequency * float64(i) / rate
		re += sample * math.Cos(angle)
		im += sample * math.Sin(angle)
	}
	rms = math.Sqrt(sumSquares / float64(len(samples)))
	amplitude = 2 * math.Hypot(re, im) / float64(len(samples))
	return amplitude, rms
}

// ToneHeard decides whether the recorded tone is present. A recording with
// no signal, or one whose 440 Hz component is not clearly above the pre-tone
// floor, fails.
func ToneHeard(toneAmplitude, baselineAmplitude float64) bool {
	const floor = 0.004
	return toneAmplitude >= floor && toneAmplitude >= baselineAmplitude*2.5
}

func decodeSamples(pcm []byte, format capsulad.SampleFormat, channels int) []float64 {
	switch format {
	case capsulad.SampleFormatPCMF32LE:
		out := make([]float64, 0, len(pcm)/(4*channels))
		for i := 0; i+4*channels <= len(pcm); i += 4 * channels {
			var sum float64
			for ch := 0; ch < channels; ch++ {
				bits := binary.LittleEndian.Uint32(pcm[i+4*ch:])
				sum += float64(math.Float32frombits(bits))
			}
			out = append(out, sum/float64(channels))
		}
		return out
	default:
		out := make([]float64, 0, len(pcm)/(2*channels))
		for i := 0; i+2*channels <= len(pcm); i += 2 * channels {
			var sum float64
			for ch := 0; ch < channels; ch++ {
				sample := int16(binary.LittleEndian.Uint16(pcm[i+2*ch:]))
				sum += float64(sample) / 32768
			}
			out = append(out, sum/float64(channels))
		}
		return out
	}
}

// TonePCM generates a soft 440 Hz tone with short raised-cosine edges so the
// playback stream starts and ends without a click. amplitude is the fraction
// of full scale (0.2 for the requested 20% level).
func TonePCM(rate, channels int, durationNS uint64, frequency, amplitude float64) []byte {
	if rate <= 0 || channels <= 0 {
		return nil
	}
	frames := int(uint64(rate) * durationNS / 1_000_000_000)
	pcm := make([]byte, frames*channels*2)
	fade := max(rate/100, 1)
	for i := range frames {
		envelope := 1.0
		if i < fade {
			envelope = 0.5 - 0.5*math.Cos(math.Pi*float64(i)/float64(fade))
		} else if remaining := frames - i; remaining < fade {
			envelope = 0.5 - 0.5*math.Cos(math.Pi*float64(remaining)/float64(fade))
		}
		sample := amplitude * envelope * math.Sin(2*math.Pi*frequency*float64(i)/float64(rate))
		value := uint16(int16(math.Round(clamp01Abs(sample) * math.MaxInt16)))
		for ch := range channels {
			binary.LittleEndian.PutUint16(pcm[(i*channels+ch)*2:], value)
		}
	}
	return pcm
}

func clamp01Abs(v float64) float64 {
	switch {
	case v < -1:
		return -1
	case v > 1:
		return 1
	}
	return v
}
