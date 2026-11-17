package turn

import (
	"encoding/binary"
	"math"
)

// Uplink framing: cadence-audiod sends exactly 20 ms of 16 kHz mono PCM as one
// Opus packet, so the assembler re-blocks whatever the framework delivers into
// 320-sample frames.
const (
	uplinkRate         = 16000
	uplinkFrameSamples = uplinkRate / 50
	uplinkFrameBytes   = uplinkFrameSamples * 2
)

// frameAssembler re-blocks captured PCM into exact 20 ms frames. The framework
// normally delivers 20 ms chunks, but a discontinuity can leave a partial tail;
// the buffer carries it into the next chunk rather than encoding a short frame.
type frameAssembler struct {
	buf []byte
}

func (a *frameAssembler) reset() { a.buf = a.buf[:0] }

// pending reports whether a partial frame is buffered.
func (a *frameAssembler) pending() bool { return len(a.buf) > 0 }

// push appends PCM and returns every whole frame it completes. The returned
// frames alias the assembler's buffer; the caller must encode them before the
// next push.
func (a *frameAssembler) push(pcm []byte) [][]byte {
	a.buf = append(a.buf, pcm...)
	var frames [][]byte
	for len(a.buf) >= uplinkFrameBytes {
		frames = append(frames, a.buf[:uplinkFrameBytes])
		a.buf = a.buf[uplinkFrameBytes:]
	}
	if len(a.buf) > 0 {
		// Detach the tail from the old chunk so a retention here cannot pin a
		// large read buffer.
		tail := make([]byte, len(a.buf))
		copy(tail, a.buf)
		a.buf = tail
	} else {
		a.buf = a.buf[:0]
	}
	return frames
}

// pcmRMS returns the root-mean-square level of little-endian 16-bit PCM in
// [0,1]. It is the real captured level the indication follows, never a
// synthetic animation.
func pcmRMS(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := range n {
		s := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
		sum += s * s
	}
	rms := math.Sqrt(sum / float64(n))
	if math.IsNaN(rms) || rms > 1 {
		return 1
	}
	return rms
}

// levelFromRMS maps a real capture RMS to a 0..1 indication brightness over
// -50..-10 dBFS, the speech range of the processed microphone tap. It is the
// only source of the listening brightness; there is no synthetic animation.
func levelFromRMS(rms float64) float64 {
	if rms <= 1e-6 {
		return 0
	}
	db := 20 * math.Log10(rms)
	v := (db + 50) / 40
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
