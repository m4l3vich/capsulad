package audio

import (
	"encoding/binary"
	"math"

	"capsulad/internal/backend"
)

// resampler converts an arbitrary PCM stream to interleaved stereo float32 at
// the mixer rate. Linear interpolation is sufficient for local mixing; the
// framework does not claim audiophile resampling.
type resampler struct {
	in      backend.AudioFormat
	outRate int
	src     []float32 // interleaved stereo at the input rate
	pos     float64   // fractional position in src frames
}

func newResampler(in, out backend.AudioFormat) *resampler {
	return &resampler{in: in, outRate: out.RateHz}
}

func (r *resampler) srcFrames() int { return len(r.src) / 2 }

func (r *resampler) reset() {
	r.src = r.src[:0]
	r.pos = 0
}

func (r *resampler) feed(pcm []byte) {
	if len(pcm) == 0 || r.in.RateHz <= 0 || r.outRate <= 0 {
		return
	}
	r.src = append(r.src, decodeStereo(pcm, r.in)...)
}

func (r *resampler) pull(n int) []float32 {
	out := make([]float32, n*2)
	if r.in.RateHz == r.outRate {
		avail := n
		if avail > r.srcFrames() {
			avail = r.srcFrames()
		}
		copy(out, r.src[:avail*2])
		copy(r.src, r.src[avail*2:])
		r.src = r.src[:(r.srcFrames()-avail)*2]
		return out
	}
	ratio := float64(r.in.RateHz) / float64(r.outRate)
	frames := r.srcFrames()
	for i := 0; i < n; i++ {
		if frames < 1 {
			break
		}
		i0 := int(r.pos)
		if i0 >= frames {
			break
		}
		if i0+1 >= frames {
			out[i*2] = r.src[i0*2]
			out[i*2+1] = r.src[i0*2+1]
			r.pos += ratio
			continue
		}
		frac := r.pos - float64(i0)
		out[i*2] = float32(float64(r.src[i0*2])*(1-frac) + float64(r.src[(i0+1)*2])*frac)
		out[i*2+1] = float32(float64(r.src[i0*2+1])*(1-frac) + float64(r.src[(i0+1)*2+1])*frac)
		r.pos += ratio
	}
	consume := int(math.Floor(r.pos)) - 1
	if consume > 0 {
		if consume > frames {
			consume = frames
		}
		copy(r.src, r.src[consume*2:])
		r.src = r.src[:(frames-consume)*2]
		r.pos -= float64(consume)
	}
	if r.pos >= float64(r.srcFrames()) {
		r.pos = 0
		r.src = r.src[:0]
	}
	return out
}

func decodeStereo(pcm []byte, format backend.AudioFormat) []float32 {
	channels := format.Channels
	if channels <= 0 {
		return nil
	}
	bps := 2
	if format.Format == backend.F32LE {
		bps = 4
	}
	frameBytes := bps * channels
	n := len(pcm) / frameBytes
	out := make([]float32, n*2)
	for i := 0; i < n; i++ {
		base := i * frameBytes
		sample := func(c int) float64 {
			off := base + c*bps
			if format.Format == backend.F32LE {
				return float64(math.Float32frombits(binary.LittleEndian.Uint32(pcm[off:])))
			}
			return float64(int16(binary.LittleEndian.Uint16(pcm[off:]))) / 32768
		}
		l := sample(0)
		r := l
		if channels > 1 {
			r = sample(1)
		}
		out[i*2] = float32(l)
		out[i*2+1] = float32(r)
	}
	return out
}
