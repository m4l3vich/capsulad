package oww

import "math"

// Frontend constants, matching the reference pipeline.
const (
	SampleRate   = 16000
	FFTSize      = 512
	Hop          = 160
	MelBins      = 32
	WindowLength = 400
	WindowOffset = 56 // centred Hann-400 inside the 512-point frame
)

// Mel computes 32-bin log-mel frames from a 16 kHz mono stream.
type Mel struct {
	weights []float32 // 257 x 32 filterbank

	window [FFTSize]float64
	cosTbl [FFTSize / 2]float64
	sinTbl [FFTSize / 2]float64
	bitrev [FFTSize]int

	pending []float64
	re      [FFTSize]float64
	im      [FFTSize]float64
}

// NewMel builds a frontend from the embedded filterbank.
func NewMel(a *Assets) (*Mel, error) {
	w, ok := a.Tensor("melW")
	if !ok {
		return nil, ErrMissingWeights
	}
	m := &Mel{weights: w}
	for i := 0; i < WindowLength; i++ {
		m.window[WindowOffset+i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(WindowLength))
	}
	for i := 0; i < FFTSize/2; i++ {
		theta := -2 * math.Pi * float64(i) / FFTSize
		m.cosTbl[i] = math.Cos(theta)
		m.sinTbl[i] = math.Sin(theta)
	}
	bits := 9 // log2(512)
	for i := 0; i < FFTSize; i++ {
		r := 0
		for b := 0; b < bits; b++ {
			r = (r << 1) | ((i >> b) & 1)
		}
		m.bitrev[i] = r
	}
	return m, nil
}

// Reset clears buffered audio.
func (m *Mel) Reset() { m.pending = m.pending[:0] }

// Feed appends PCM samples as raw int16 magnitudes, matching the reference
// pipeline, and returns the mel frames completed by this call.
func (m *Mel) Feed(samples []float64) [][]float32 {
	m.pending = append(m.pending, samples...)
	if len(m.pending) < FFTSize {
		return nil
	}
	var frames [][]float32
	consumed := 0
	for len(m.pending)-consumed >= FFTSize {
		frames = append(frames, m.frame(m.pending[consumed:consumed+FFTSize]))
		consumed += Hop
	}
	// Retain the backing buffer rather than slicing away its capacity on every
	// 10 ms hop, which otherwise reallocates the capture history each feed.
	m.pending = m.pending[:copy(m.pending, m.pending[consumed:])]
	return frames
}

// frame computes one mel frame: Hann window, real FFT, power, filterbank, dB.
func (m *Mel) frame(block []float64) []float32 {
	for i := 0; i < FFTSize; i++ {
		v := block[i] * m.window[i]
		m.re[m.bitrev[i]] = v
		m.im[m.bitrev[i]] = 0
	}
	// Iterative radix-2 FFT.
	for size := 2; size <= FFTSize; size <<= 1 {
		half := size / 2
		step := FFTSize / size
		for start := 0; start < FFTSize; start += size {
			k := 0
			for j := start; j < start+half; j++ {
				wr, wi := m.cosTbl[k], m.sinTbl[k]
				k += step
				ur, ui := m.re[j], m.im[j]
				vr, vi := m.re[j+half], m.im[j+half]
				tr := vr*wr - vi*wi
				ti := vr*wi + vi*wr
				m.re[j], m.im[j] = ur+tr, ui+ti
				m.re[j+half], m.im[j+half] = ur-tr, ui-ti
			}
		}
	}
	power := make([]float32, FFTSize/2+1)
	for i := range power {
		power[i] = float32(m.re[i]*m.re[i] + m.im[i]*m.im[i])
	}
	out := make([]float32, MelBins)
	for j := 0; j < MelBins; j++ {
		var sum float32
		for i := 0; i < len(power); i++ {
			sum += power[i] * m.weights[i*MelBins+j]
		}
		if sum < 1e-10 {
			sum = 1e-10
		}
		// 10*log10(mel) then the reference melspec_transform: /10 + 2.
		db := 10 * math.Log10(float64(sum))
		out[j] = float32(db/10 + 2)
	}
	return out
}
