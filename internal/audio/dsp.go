package audio

import (
	"encoding/binary"
	"math"
	"sort"

	"capsulad/internal/backend"
)

// Capture endpoint identities and processing provenance. The physical tap is
// the firmware-processed `speex` source; the framework tap adds a bounded,
// streaming 16 kHz mono cleanup. Application capture, wakeword inference and
// enrollment all consume that same processed tap, so training and detection
// share one declared policy. The raw physical stream remains available only
// through the explicit RawCaptureEndpoint and through offline conversion of
// legacy enrollment takes, never as an alternative inference path.
const (
	CaptureEndpoint      = "audio.input.microphone"
	RawCaptureEndpoint   = "audio.input.microphone.raw"
	CaptureProcessingTap = "framework.dsp.v1"
	captureDSPFrame      = 256 // 16 ms at 16 kHz; 50 % overlap
	captureDSPHop        = 128
	captureDSPBins       = captureDSPFrame/2 + 1
)

// captureS16Profile is the single capture profile the cleanup is tuned for.
var captureS16Profile = backend.AudioFormat{Format: backend.S16LE, RateHz: 16000, Channels: 1}

// CaptureDSPOptions tunes the framework capture cleanup. The zero value is
// the safe default for the vendor's already-processed source: a rumble
// high-pass, conservative spectral suppression and a peak limiter, with unity
// gain and no automatic gain control. Raising GainDB is an explicit operator
// decision, not an implicit effect of processing.
type CaptureDSPOptions struct {
	Disabled                bool
	DisableNoiseSuppression bool
	// DisableHighPass is for measurement/regression use.
	DisableHighPass bool
	HighPassHz      float64
	GainDB          float64
	// MaxAttenuationDB bounds how far stationary noise may be pulled down so
	// the framework never competes with the firmware's own suppressor.
	MaxAttenuationDB float64
}

// captureProcessor is a bounded, allocation-free per-sample DSP chain for one
// interleaved S16LE mono capture stream. It has fixed state, fixed latency of
// captureDSPFrame-1 samples, and produces exactly as many samples as it
// consumes, so chunk boundaries never change the output.
type captureProcessor struct {
	gain    float64
	hp      biquad
	limiter limiter
	ns      noiseSuppressor
	fft     fftPlan

	window [captureDSPFrame]float64
	acc    [captureDSPFrame]float64
	in     [captureDSPHop]float64
	inFill int
	prime  int

	ola  [captureDSPFrame]float64
	re   [captureDSPFrame]float64
	im   [captureDSPFrame]float64
	out  [captureDSPFrame + captureDSPHop]float64
	head int
	len  int
}

func newCaptureProcessor(format backend.AudioFormat, opts CaptureDSPOptions) *captureProcessor {
	if opts.Disabled || !captureDSPSupported(format) {
		return nil
	}
	rate := float64(format.RateHz)
	fc := opts.HighPassHz
	if math.IsNaN(fc) || math.IsInf(fc, 0) || fc <= 0 {
		fc = 80
	}
	if fc >= rate/4 {
		fc = rate / 4
	}
	maxAtten := opts.MaxAttenuationDB
	if math.IsNaN(maxAtten) || math.IsInf(maxAtten, 0) || maxAtten <= 0 {
		// The capture source is already processed by the firmware. Twelve dB
		// of extra suppression removes a steady fan without hollowing speech.
		maxAtten = 12
	}
	if maxAtten < 3 {
		maxAtten = 3
	}
	if maxAtten > 24 {
		maxAtten = 24
	}
	gainDB := opts.GainDB
	if math.IsNaN(gainDB) || math.IsInf(gainDB, 0) {
		gainDB = 0
	}
	if gainDB < -12 {
		gainDB = -12
	}
	if gainDB > 12 {
		gainDB = 12
	}
	p := &captureProcessor{
		gain:    math.Pow(10, gainDB/20),
		limiter: newLimiter(int(rate)),
		fft:     newFFTPlan(),
		prime:   captureDSPFrame,
	}
	if opts.DisableHighPass {
		p.hp = biquad{b0: 1}
	} else {
		p.hp = newHighPassBiquad(fc, rate, 0.707)
	}
	p.ns.enabled = !opts.DisableNoiseSuppression
	p.ns.maxAttenDB = maxAtten
	p.ns.floorGain = math.Pow(10, -maxAtten/20)
	p.ns.reset()
	for i := range captureDSPFrame {
		// sqrt-Hann analysis and synthesis. The product of the two windows is
		// a periodic Hann window, whose two 50 % overlapping contributions sum
		// to exactly one at every sample. Using Hann for both (and dividing by
		// its average 0.75) left a 0.667..1.333 sample-phase gain ripple.
		p.window[i] = math.Sqrt(0.5 * (1 - math.Cos(2*math.Pi*float64(i)/captureDSPFrame)))
	}
	return p
}

// captureDSPSupported matches the one production capture profile: mono
// S16LE at 16 kHz (`speex` on Rockchip, and the simulator). The DSP profile's
// frame/hop and adaptation times are tuned in 16 kHz sample counts, so other
// formats are passed through unchanged rather than silently running an
// untuned chain.
func captureDSPSupported(format backend.AudioFormat) bool {
	return format.Format == backend.S16LE && format.Channels == 1 && format.RateHz == 16000
}

// reset restores the chain after a discontinuity, mute or source reopen. The
// next samples fade in from the priming window, which is flagged to consumers
// as a discontinuity by the caller.
func (p *captureProcessor) reset() {
	p.hp.reset()
	p.limiter.reset()
	p.ns.reset()
	p.inFill = 0
	p.prime = captureDSPFrame
	p.head, p.len = 0, 0
	clear(p.acc[:])
	clear(p.in[:])
	clear(p.ola[:])
	clear(p.re[:])
	clear(p.im[:])
	clear(p.out[:])
}

// step advances the stream by one normalized sample and returns the cleaned
// sample at the chain's fixed latency. It is the single implementation shared
// by the live byte transport and offline enrollment conversion, so both
// observe identical filter, noise-estimate and limiter state evolution.
func (p *captureProcessor) step(x float64) float64 {
	x = p.hp.process(x)
	p.in[p.inFill] = x
	p.inFill++
	if p.inFill == captureDSPHop {
		p.analyzeHop()
		p.inFill = 0
	}
	y := 0.0
	if p.len > 0 {
		y = p.out[p.head]
		p.head++
		if p.head == len(p.out) {
			p.head = 0
		}
		p.len--
	}
	y *= p.gain
	y = p.limiter.process(y)
	if y > 0.999 {
		return 0.999
	}
	if y < -0.999 {
		return -0.999
	}
	return y
}

// process writes exactly len(src) bytes to dst. src and dst must be even
// length and may not overlap; the caller owns dst.
func (p *captureProcessor) process(dst, src []byte) {
	n := len(src) &^ 1
	write := 0
	for read := 0; read+1 < n; read += 2 {
		y := p.step(float64(int16(binary.LittleEndian.Uint16(src[read:]))) / 32768)
		if read+1 < len(dst) {
			binary.LittleEndian.PutUint16(dst[read:], uint16(int16(math.Round(y*32767))))
		}
		write++
	}
	// Preserve the stream length contract even for an odd trailing byte.
	for i := write * 2; i < len(dst); i++ {
		dst[i] = 0
	}
}

// processS16 is the sample-owned form used for offline conversion. dst and src
// must have the same length and may not overlap.
func (p *captureProcessor) processS16(dst, src []int16) {
	for i, v := range src {
		y := p.step(float64(v) / 32768)
		dst[i] = int16(math.Round(y * 32767))
	}
}

// ProcessCaptureS16 applies the configured cleanup to a complete 16 kHz mono
// recording, starting from reset state. Legacy enrollment takes were captured
// on the raw vendor tap; this brings them onto the same declared preprocessing
// as live enrollment and inference without rewriting the stored file. The
// returned tap is CaptureProcessingTap when the chain ran and "" when the
// profile is disabled or unsupported, in which case the input is copied
// unchanged. This is an enrollment-time helper, not a per-chunk capture path.
func ProcessCaptureS16(samples []int16, opts CaptureDSPOptions) ([]int16, string) {
	out := make([]int16, len(samples))
	proc := newCaptureProcessor(captureS16Profile, opts)
	if proc == nil {
		copy(out, samples)
		return out, ""
	}
	proc.processS16(out, samples)
	return out, CaptureProcessingTap
}

func (p *captureProcessor) analyzeHop() {
	copy(p.acc[:captureDSPFrame-captureDSPHop], p.acc[captureDSPHop:])
	copy(p.acc[captureDSPFrame-captureDSPHop:], p.in[:])
	if p.prime > 0 {
		// Prime with a full zero frame so the first window contains the real
		// first hop at the correct alignment rather than a half-zero frame.
		p.prime -= captureDSPHop
		if p.prime > 0 {
			p.len += captureDSPHop // proportional zero tail for the latency
			return
		}
	}

	for i := range captureDSPFrame {
		p.re[i] = p.acc[i] * p.window[i]
		p.im[i] = 0
	}
	p.fft.forward(&p.re, &p.im)
	p.ns.process(&p.re, &p.im, &p.acc)
	p.fft.inverse(&p.re, &p.im)

	for i := range captureDSPFrame {
		// Synthesis is the second sqrt-Hann window; the overlap sum is unity.
		p.ola[i] += p.re[i] * p.window[i]
	}
	for i := range captureDSPHop {
		slot := p.head + p.len
		if slot >= len(p.out) {
			slot -= len(p.out)
		}
		p.out[slot] = p.ola[i]
		p.len++
	}
	copy(p.ola[:], p.ola[captureDSPHop:])
	clear(p.ola[captureDSPFrame-captureDSPHop:])
}

// --- biquad high-pass ------------------------------------------------------

type biquad struct {
	b0, b1, b2, a1, a2 float64
	x1, x2, y1, y2     float64
}

func newHighPassBiquad(fc, fs, q float64) biquad {
	w0 := 2 * math.Pi * fc / fs
	cosw, sinw := math.Cos(w0), math.Sin(w0)
	alpha := sinw / (2 * q)
	a0 := 1 + alpha
	p := biquad{
		b0: (1 + cosw) / 2 / a0,
		b1: -(1 + cosw) / a0,
		b2: (1 + cosw) / 2 / a0,
		a1: -2 * cosw / a0,
		a2: (1 - alpha) / a0,
	}
	return p
}

func (b *biquad) reset() { b.x1, b.x2, b.y1, b.y2 = 0, 0, 0, 0 }

func (b *biquad) process(x float64) float64 {
	y := b.b0*x + b.b1*b.x1 + b.b2*b.x2 - b.a1*b.y1 - b.a2*b.y2
	b.x2, b.x1 = b.x1, x
	b.y2, b.y1 = b.y1, y
	return y
}

// --- limiter ---------------------------------------------------------------

// limiter bounds peaks at -1 dBFS with a short attack and slow release. It has
// no makeup gain: the framework does not add loudness on top of the speaker
// firmware's own automatic gain control.
type limiter struct {
	threshold       float64
	env             float64
	gain            float64
	attack, release float64
}

func newLimiter(rate int) limiter {
	return limiter{
		threshold: math.Pow(10, -1.0/20),
		gain:      1,
		attack:    1 - math.Exp(-1/(0.001*float64(rate))),
		release:   1 - math.Exp(-1/(0.050*float64(rate))),
	}
}

func (l *limiter) reset() {
	l.env, l.gain = 0, 1
}

func (l *limiter) process(x float64) float64 {
	a := math.Abs(x)
	if a > l.env {
		l.env = a
	} else {
		l.env += (a - l.env) * l.release
	}
	target := 1.0
	if l.env > l.threshold {
		target = l.threshold / l.env
	}
	if target < l.gain {
		l.gain += (target - l.gain) * l.attack
	} else {
		l.gain += (target - l.gain) * l.release
	}
	return x * l.gain
}

// --- spectral noise suppression -------------------------------------------

// noiseSuppressor applies a conservative Wiener gain from a per-bin noise
// estimate. The estimate is learned from frames whose total energy is near the
// recent floor/average. That gate makes the estimator work for colored,
// amplitude-modulated room noise, where a spectral-flatness test alone never
// admits a frame and a per-bin minimum under-estimates a high-variance floor by
// an order of magnitude. Speech and transients sit well above the energy
// average, so they do not raise the estimate and cannot become their own
// suppression target. A start that looks like speech is not learned: priming's
// median flatness, harmonic-frame count and periodicity decide whether the
// initial stream is room or voiced/modulated speech. If it is not confirmed as
// room, suppression stays at unity until a sustained quiet, broadband,
// low-periodicity run arrives. A steady unvoiced fricative from reset is
// indistinguishable from broadband room and may be learned; following voiced
// speech is still preserved by the energy gate. Colored room noise has enough
// priming flatness and low enough periodicity to be admitted, so this is not
// the old broadband-only gate.
type noiseSuppressor struct {
	enabled    bool
	noise      [captureDSPBins]float64
	gain       [captureDSPBins]float64
	primeMin   [captureDSPBins]float64
	primeFlat  [captureDSPPrimeFrames]float64
	primePitch [captureDSPPrimeFrames]float64
	primed     int
	primeLimit int
	floorGain  float64
	maxAttenDB float64
	// energyFloor and energyAvg are aggregate frame powers. The floor tracks
	// the quiet end quickly and the average adapts over about 0.8 s, so a
	// stationary colored source is admitted without a broadband flatness test.
	energyFloor float64
	energyAvg   float64
	// primeTonal counts priming frames whose flatness is below the tonal
	// threshold. A start with too many harmonic frames is speech, not room.
	primeTonal int
	// tracking stays false after an unconfirmed start. Suppression is then
	// unity so voiced/modulated speech present at reset is preserved; only a
	// sustained quiet room-like run can confirm it as noise.
	tracking     bool
	roomEvidence int
	roomRunSum   float64
}

const (
	// captureDSPPrimeFrames is 192 ms of 8 ms hops.
	captureDSPPrimeFrames = 24
	// noiseStartFlat is the priming median flatness above which the initial
	// stream can be room noise. Measured: colored room 0.022, 440 Hz tone ~6e-6,
	// harmonic vowel ~1e-6.
	noiseStartFlat = 0.01
	// noiseEvidenceConfirm is the consecutive room-like run needed to confirm a
	// deferred start: 12 hops = 96 ms. Speech fricative bursts are shorter.
	noiseEvidenceConfirm = 12
	// noiseStartPeriodicity is the median normalized autocorrelation (lags for
	// 100-400 Hz) above which priming is treated as voiced speech, not room.
	// Synthetic colored room is ~0.4-0.5; speech fixture 0.87; tone/vowel >0.99.
	noiseStartPeriodicity = 0.60
	// noiseFloorHeadroom / noiseAvgHeadroom admit ordinary fluctuations of a
	// stationary source while excluding speech and transients.
	noiseFloorHeadroom = 2.5
	noiseAvgHeadroom   = 2.5
	// noiseTrackUp is the symmetric per-frame rate used while a frame looks
	// like the stationary floor: one pole over about 8 frames (64 ms).
	noiseTrackUp = 0.12
)

func (n *noiseSuppressor) reset() {
	for i := range captureDSPBins {
		n.noise[i] = 0
		n.gain[i] = 1
		n.primeMin[i] = math.MaxFloat64
	}
	clear(n.primeFlat[:])
	clear(n.primePitch[:])
	n.primed = 0
	n.primeLimit = captureDSPPrimeFrames
	n.energyFloor = 0
	n.energyAvg = 0
	n.primeTonal = 0
	n.tracking = false
	n.roomEvidence = 0
	n.roomRunSum = 0
}

func (n *noiseSuppressor) process(re, im *[captureDSPFrame]float64, frame *[captureDSPFrame]float64) {
	if !n.enabled {
		return
	}
	var logSum, sum float64
	bins := captureDSPBins - 2
	for k := 1; k < captureDSPBins-1; k++ {
		p := re[k]*re[k] + im[k]*im[k]
		sum += p
		if p < 1e-20 {
			p = 1e-20
		}
		logSum += math.Log(p)
	}
	flat := 0.0
	if sum > 0 {
		flat = math.Exp(logSum/float64(bins)) / (sum / float64(bins))
	}
	if n.primed < n.primeLimit {
		if flat > 0.15 {
			for k := range captureDSPBins {
				p := re[k]*re[k] + im[k]*im[k]
				if p < n.primeMin[k] {
					n.primeMin[k] = p
				}
			}
		}
		if n.primed == 0 {
			n.energyFloor, n.energyAvg = sum, sum
		} else {
			if sum < n.energyFloor {
				n.energyFloor = sum
			}
			n.energyAvg += (sum - n.energyAvg) * 0.1
		}
		if flat < noiseStartFlat {
			n.primeTonal++
		}
		n.primeFlat[n.primed] = flat
		n.primePitch[n.primed] = framePeriodicity(frame)
		n.primed++
		if n.primed == n.primeLimit {
			sort.Float64s(n.primeFlat[:])
			sort.Float64s(n.primePitch[:])
			// A start with too many harmonic frames, or voiced periodicity, is
			// speech rather than room; a broadband room is neither.
			n.tracking = n.primeFlat[n.primeLimit/2] > noiseStartFlat &&
				n.primeTonal*5 <= n.primeLimit &&
				n.primePitch[n.primeLimit/2] < noiseStartPeriodicity
			for k := range captureDSPBins {
				if n.primeMin[k] == math.MaxFloat64 {
					n.primeMin[k] = 0
				}
				if !n.tracking {
					n.noise[k], n.gain[k] = 0, 1
					continue
				}
				n.noise[k] = n.primeMin[k] * 1.5
				if n.noise[k] < 1e-12 {
					n.noise[k] = 1e-12
				}
			}
		}
		return // unity gain while the estimate is learned
	}

	// An unconfirmed start (tonal, or a modulated word/fricative onset) is
	// preserved. Confirm room only after a minimal run near the learned
	// lower-energy reference; a single speech minimum or fricative burst must
	// not establish the floor. Periodicity is evaluated only on candidate quiet
	// frames, so an indefinitely deferred start does not pay it every hop.
	if !n.tracking {
		// Keep the lower-energy reference moving down while deferred.
		if sum < n.energyFloor {
			n.energyFloor += (sum - n.energyFloor) * 0.25
		}
		roomLike := sum > 0 && flat > noiseStartFlat && framePeriodicity(frame) < noiseStartPeriodicity
		if roomLike {
			n.roomEvidence++
			n.roomRunSum += sum
		} else {
			n.roomEvidence = 0
			n.roomRunSum = 0
		}
		if n.roomEvidence < noiseEvidenceConfirm {
			return
		}
		n.tracking = true
		mean := n.roomRunSum / float64(noiseEvidenceConfirm)
		n.roomEvidence = 0
		n.roomRunSum = 0
		// Use the mean of the room-like run, not a single sample or headroom:
		// the floor gate learns the room while 3x speech stays above 2.5x.
		n.energyFloor, n.energyAvg = mean, mean
		for k := range captureDSPBins {
			n.noise[k], n.gain[k] = 0, 1
		}
	}

	// Track the aggregate energy envelope independently of spectral shape.
	if sum < n.energyFloor {
		n.energyFloor += (sum - n.energyFloor) * 0.25
	} else {
		n.energyFloor += (sum - n.energyFloor) * 0.0005
	}
	// energyAvgRate down/up: adapt quickly to a quieter room and very slowly
	// upward, so a sustained word or vowel cannot become the reference and be
	// learned as noise. A genuinely louder stationary source is picked up by
	// the slower floor over seconds rather than within one utterance.
	if sum < n.energyAvg {
		n.energyAvg += (sum - n.energyAvg) * 0.05
	} else {
		n.energyAvg += (sum - n.energyAvg) * 0.0005
	}
	noiseFrame := sum < n.energyFloor*noiseFloorHeadroom || sum < n.energyAvg*noiseAvgHeadroom
	noiseLike := flat > 0.25
	for k := range captureDSPBins {
		p := re[k]*re[k] + im[k]*im[k]
		nz := n.noise[k]
		switch {
		case nz <= 1e-11 && noiseFrame:
			nz = p * noiseTrackUp
		case nz <= 1e-11:
			nz = p * 0.05
		case noiseFrame:
			// Symmetric tracking while the frame looks like the stationary
			// floor; this converges to the true per-bin mean instead of the
			// minimum bias of a short priming window.
			nz += (p - nz) * noiseTrackUp
		case p < nz:
			nz += (p - nz) * 0.3
		case noiseLike && p < nz*4:
			nz += (p - nz) * 0.02
		}
		if nz < 1e-12 {
			nz = 1e-12
		}
		n.noise[k] = nz
		ratio := p / (nz * 2)
		g := 1.0
		if ratio > 1 {
			snr := ratio - 1
			g = snr / (1 + snr)
		} else {
			g = n.floorGain
		}
		if g < n.floorGain {
			g = n.floorGain
		}
		n.gain[k] = n.gain[k]*0.55 + g*0.45
		re[k] *= n.gain[k]
		im[k] *= n.gain[k]
	}
}

// framePeriodicity returns the strongest normalized autocorrelation for lags
// corresponding to 100-400 Hz. It is called during the 24 priming hops and,
// while a start is deferred, during room verification. In the deferred path the
// energy/flatness tests short-circuit before this call, so a sustained tone
// (low flatness) does not pay it; a confirmed stream does not call it at all.
func framePeriodicity(frame *[captureDSPFrame]float64) float64 {
	var energy float64
	for _, v := range frame {
		energy += v * v
	}
	if energy <= 1e-12 {
		return 0
	}
	best := 0.0
	for lag := 40; lag <= 160; lag++ {
		var num, ea, eb float64
		for i := 0; i+lag < captureDSPFrame; i++ {
			a, b := frame[i], frame[i+lag]
			num += a * b
			ea += a * a
			eb += b * b
		}
		den := math.Sqrt(ea * eb)
		if den > 0 {
			if r := num / den; r > best {
				best = r
			}
		}
	}
	return best
}

// --- 256-point radix-2 FFT -------------------------------------------------

type fftPlan struct {
	cos    [captureDSPFrame / 2]float64
	sin    [captureDSPFrame / 2]float64
	bitrev [captureDSPFrame]int
}

func newFFTPlan() fftPlan {
	var p fftPlan
	n := captureDSPFrame
	for i := range n / 2 {
		theta := -2 * math.Pi * float64(i) / float64(n)
		p.cos[i] = math.Cos(theta)
		p.sin[i] = math.Sin(theta)
	}
	bits := 0
	for 1<<bits < n {
		bits++
	}
	for i := range n {
		rev, v := 0, i
		for range bits {
			rev = (rev << 1) | (v & 1)
			v >>= 1
		}
		p.bitrev[i] = rev
	}
	return p
}

func (p *fftPlan) forward(re, im *[captureDSPFrame]float64) {
	p.transform(re, im, false, 1)
}

func (p *fftPlan) inverse(re, im *[captureDSPFrame]float64) {
	p.transform(re, im, true, 1.0/captureDSPFrame)
}

func (p *fftPlan) transform(re, im *[captureDSPFrame]float64, inverse bool, scale float64) {
	n := captureDSPFrame
	for i := range n {
		j := p.bitrev[i]
		if j > i {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half := size / 2
		step := n / size
		for start := 0; start < n; start += size {
			for k := range half {
				wr := p.cos[k*step]
				wi := p.sin[k*step]
				if inverse {
					wi = -wi
				}
				a, b := start+k, start+k+half
				tr := wr*re[b] - wi*im[b]
				ti := wr*im[b] + wi*re[b]
				re[b] = re[a] - tr
				im[b] = im[a] - ti
				re[a] += tr
				im[a] += ti
			}
		}
	}
	if scale != 1 {
		for i := range n {
			re[i] *= scale
			im[i] *= scale
		}
	}
}
