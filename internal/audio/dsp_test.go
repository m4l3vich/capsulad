package audio

import (
	"context"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

func dspTestFormat() backend.AudioFormat {
	return backend.AudioFormat{Format: backend.S16LE, RateHz: 16000, Channels: 1}
}

func encodeS16(samples []float64) []byte {
	pcm := make([]byte, len(samples)*2)
	for i, v := range samples {
		if v > 0.999 {
			v = 0.999
		} else if v < -0.999 {
			v = -0.999
		}
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(math.Round(v*32767))))
	}
	return pcm
}

func decodeS16(pcm []byte) []float64 {
	out := make([]float64, len(pcm)/2)
	for i := range out {
		out[i] = float64(int16(binary.LittleEndian.Uint16(pcm[i*2:]))) / 32768
	}
	return out
}

func s16Samples(pcm []byte) []int16 {
	out := make([]int16, len(pcm)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
	}
	return out
}

func samplesRMS(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, v := range samples {
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(samples)))
}

func sine(rate int, amplitude, hz float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = amplitude * math.Sin(2*math.Pi*hz*float64(i)/float64(rate))
	}
	return out
}

type testLCG uint64

func (r *testLCG) uniform() float64 {
	*r = *r*6364136223846793005 + 1442695040888963407
	return float64(*r>>11) / float64(1<<53)
}

func uniformNoise(amplitude float64, n int, seed uint64) []float64 {
	rng := testLCG(seed)
	out := make([]float64, n)
	for i := range out {
		out[i] = (rng.uniform()*2 - 1) * amplitude
	}
	return out
}

func runProcessor(proc *captureProcessor, chunks [][]byte) []byte {
	var out []byte
	for _, src := range chunks {
		dst := make([]byte, len(src))
		proc.process(dst, src)
		out = append(out, dst...)
	}
	return out
}

func chunkBy(pcm []byte, sizes []int) [][]byte {
	var chunks [][]byte
	for off := 0; off < len(pcm); {
		size := len(pcm) - off
		if len(sizes) > 0 {
			size = sizes[0]
			sizes = sizes[1:]
		}
		if size > len(pcm)-off {
			size = len(pcm) - off
		}
		size &^= 1
		if size == 0 {
			break
		}
		chunks = append(chunks, pcm[off:off+size])
		off += size
	}
	return chunks
}

// A streaming processor must depend only on the sample sequence, never on how
// the transport happened to split it: a daemon read boundary is not an audio
// boundary.
func TestCaptureProcessorChunkIndependent(t *testing.T) {
	rate := 16000
	n := rate
	signal := make([]float64, n)
	tone := sine(rate, 0.2, 440, n)
	noise := uniformNoise(0.02, n, 7)
	for i := range signal {
		signal[i] = tone[i] + noise[i] + 0.1
	}
	pcm := encodeS16(signal)

	one := newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{})
	whole := runProcessor(one, [][]byte{pcm})

	many := newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{})
	pieces := chunkBy(pcm, []int{2, 14, 400, 6, 1000, 322, 64})
	split := runProcessor(many, pieces)

	if len(whole) != len(split) {
		t.Fatalf("length mismatch: whole=%d split=%d", len(whole), len(split))
	}
	for i := range whole {
		if whole[i] != split[i] {
			t.Fatalf("chunking changed output at byte %d: %d != %d", i, whole[i], split[i])
		}
	}
}

// The high-pass removes the DC offset and low-frequency rumble that otherwise
// walk a learned noise floor away from the actual room.
func TestCaptureProcessorRemovesDC(t *testing.T) {
	proc := newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{})
	rate := 16000
	input := make([]float64, rate)
	for i := range input {
		input[i] = 0.25
	}
	out := decodeS16(runProcessor(proc, [][]byte{encodeS16(input)}))
	tail := out[len(out)-rate/2:]
	if rms := samplesRMS(tail); rms > 0.01 {
		t.Fatalf("DC was not removed: tail RMS %.4f", rms)
	}
}

// Suppression must leave speech-like energy intact: a tonal frame is not noise
// and must not become its own suppression target.
func TestCaptureProcessorPreservesTonalSpeech(t *testing.T) {
	proc := newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{})
	rate := 16000
	quiet := uniformNoise(0.001, rate/2, 3)
	tone := sine(rate, 0.2, 440, rate)
	input := append(quiet, tone...)
	out := decodeS16(runProcessor(proc, [][]byte{encodeS16(input)}))
	// Ignore the priming latency at the front and the first adapted frames.
	tail := out[len(out)-rate/2:]
	inTail := input[len(input)-rate/2:]
	got, want := samplesRMS(tail), samplesRMS(inTail)
	if want < 0.13 || want > 0.15 {
		t.Fatalf("test signal RMS %.4f out of range", want)
	}
	if got < want*0.75 || got > want*1.05 {
		t.Fatalf("tonal speech changed too much: got %.4f want ~%.4f", got, want)
	}
}

// A word or vowel that is already present at capture open (or immediately
// after a mute/source reset) must not teach itself as the room. Priming's
// median flatness marks this tonal start and suppression stays at unity.
func TestCaptureProcessorPreservesSpeechFromReset(t *testing.T) {
	rate := 16000
	n := rate
	tone := sine(rate, 0.2, 440, n)
	vowel := make([]float64, n)
	for i := range vowel {
		tt := float64(i) / float64(rate)
		vowel[i] = 0.12*math.Sin(2*math.Pi*350*tt) + 0.08*math.Sin(2*math.Pi*900*tt) + 0.04*math.Sin(2*math.Pi*2400*tt)
	}
	for name, input := range map[string][]float64{"tone": tone, "vowel": vowel} {
		t.Run(name, func(t *testing.T) {
			proc := newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{})
			out := decodeS16(runProcessor(proc, [][]byte{encodeS16(input)}))
			tail := out[len(out)-rate/2:]
			inTail := input[len(input)-rate/2:]
			got, want := samplesRMS(tail), samplesRMS(inTail)
			if got < want*0.95 || got > want*1.05 {
				t.Fatalf("%s at reset was suppressed: got RMS %.4f want ~%.4f", name, got, want)
			}
			// Per-100 ms sample outcome: no window after the fixed latency may
			// be driven toward the suppression floor.
			const delay = captureDSPFrame - 1
			const win = 1600
			for off := delay; off+win <= len(out); off += win {
				o := samplesRMS(out[off : off+win])
				i := samplesRMS(input[off-delay : off-delay+win])
				if i == 0 {
					continue
				}
				if o < i*0.95 {
					t.Fatalf("%s window at %.1fs suppressed: out RMS %.4f in %.4f", name, float64(off)/float64(rate), o, i)
				}
			}
			// A reset during the same speech must not learn it either.
			proc.reset()
			out = decodeS16(runProcessor(proc, [][]byte{encodeS16(input)}))
			tail = out[len(out)-rate/2:]
			if got := samplesRMS(tail); got < want*0.95 || got > want*1.05 {
				t.Fatalf("%s after reset was suppressed: got RMS %.4f want ~%.4f", name, got, want)
			}
		})
	}
}

// wordAtZero builds a word-like signal from reset: a modulated fricative
// onset, harmonic vowels, and quiet room-noise gaps. Its priming median
// flatness is above the tonal threshold, so only the modulation/quiet
// confirmation can keep it from teaching itself as noise.
func wordAtZero(rate int) []float64 {
	n := rate * 3 / 2
	out := pinkNoise(0.004, n, 53)
	fric := uniformNoise(0.08, n, 59)
	vowel := func(i int) float64 {
		t := float64(i) / float64(rate)
		return 0.18*math.Sin(2*math.Pi*350*t) + 0.10*math.Sin(2*math.Pi*900*t) + 0.05*math.Sin(2*math.Pi*2400*t)
	}
	for i := range out {
		ms := float64(i) * 1000 / float64(rate)
		switch {
		case ms < 180: // fricative onset, amplitude modulated
			out[i] += fric[i] * (0.55 + 0.45*math.Sin(2*math.Pi*7*float64(i)/float64(rate)))
		case ms < 450, ms >= 620 && ms < 950, ms >= 1050:
			out[i] += vowel(i)
		}
	}
	return out
}

// speechLikePCM mirrors the repository's speech fixture: a glottal harmonic
// stack with moving tilt, 4.5 Hz syllable envelope and periodic fricative
// bursts. Its priming median flatness is above the tonal threshold, so it
// exercises the broadband-speech-at-reset case.
func speechLikePCM(rate int, seconds float64) []float64 {
	frames := int(seconds * float64(rate))
	samples := make([]float64, frames)
	rng := rand.New(rand.NewPCG(0x51ed270b, 0xc2b2ae35))
	var phase float64
	for i := range samples {
		t := float64(i) / float64(rate)
		f0 := 118 + 10*math.Sin(2*math.Pi*0.9*t)
		phase += 2 * math.Pi * f0 / float64(rate)
		tilt := 0.85 + 0.45*math.Sin(2*math.Pi*1.3*t+0.8)
		src := 0.0
		for k := 1; k <= 40; k++ {
			if 130*k > 7000 {
				break
			}
			src += math.Sin(float64(k)*phase+float64(k)*0.7) / math.Pow(float64(k), tilt)
		}
		env := math.Max(0, math.Sin(2*math.Pi*4.5*t+0.2))
		samples[i] = math.Pow(env, 0.5) * src
	}
	for start := int(0.06 * float64(rate)); start < frames; start += 4000 {
		for j := 0; j < 800 && start+j < frames; j++ {
			samples[start+j] += 0.6 * (rng.Float64()*2 - 1) * float64(800-j) / 800
		}
	}
	if rms := samplesRMS(samples); rms > 0 {
		scale := 0.2 / rms
		for i := range samples {
			samples[i] *= scale
		}
	}
	return samples
}

func TestCaptureProcessorPreservesSpeechPCMFromReset(t *testing.T) {
	rate := 16000
	input := speechLikePCM(rate, 1.5)
	// Compare against the same chain with noise suppression disabled: the
	// 80 Hz high-pass legitimately removes low speech harmonics, and this test
	// isolates the estimator's effect.
	ref := decodeS16(runProcessor(newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{DisableNoiseSuppression: true}), [][]byte{encodeS16(input)}))
	proc := newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{})
	out := decodeS16(runProcessor(proc, [][]byte{encodeS16(input)}))
	const win = 1600
	for off := 0; off+win <= len(out); off += win {
		o := samplesRMS(out[off : off+win])
		in := samplesRMS(ref[off : off+win])
		if in == 0 {
			continue
		}
		if o < in*0.90 {
			t.Fatalf("speech window at %.1fs suppressed: out RMS %.4f ref %.4f", float64(off)/float64(rate), o, in)
		}
	}
}

func TestCaptureProcessorPreservesWordFromReset(t *testing.T) {
	rate := 16000
	input := wordAtZero(rate)
	proc := newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{})
	refProc := newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{DisableNoiseSuppression: true})
	out := decodeS16(runProcessor(proc, [][]byte{encodeS16(input)}))
	ref := decodeS16(runProcessor(refProc, [][]byte{encodeS16(input)}))
	const delay = captureDSPFrame - 1
	const frame = 320 // 20 ms
	check := func(label string, out, ref []float64) {
		// Every 20 ms of every vowel region, including the first immediately
		// after the initial fricative, must survive against the same chain with
		// NS disabled. Pauses carry only the low room bed, so they may be
		// learned as the floor.
		worst, worstAt := 1.0, 0
		for _, rg := range [][2]int{{rate * 180 / 1000, rate * 450 / 1000}, {rate * 620 / 1000, rate * 950 / 1000}, {rate * 1050 / 1000, rate * 1400 / 1000}} {
			for off := rg[0]; off+frame <= rg[1]; off += frame {
				o := samplesRMS(out[off+delay : off+delay+frame])
				r := samplesRMS(ref[off+delay : off+delay+frame])
				if r == 0 {
					continue
				}
				if ratio := o / r; ratio < worst {
					worst, worstAt = ratio, off
				}
			}
		}
		t.Logf("%s worst 20ms vowel ratio %.3f at %dms", label, worst, worstAt*1000/rate)
		if worst < 0.85 {
			t.Fatalf("%s worst 20ms vowel ratio %.3f at %dms, want >= 0.85", label, worst, worstAt*1000/rate)
		}
	}
	check("at reset", out, ref)
	proc.reset()
	refProc.reset()
	out = decodeS16(runProcessor(proc, [][]byte{encodeS16(input)}))
	ref = decodeS16(runProcessor(refProc, [][]byte{encodeS16(input)}))
	check("after reset", out, ref)
}

func TestCaptureProcessorSuppressesStationaryBroadbandNoise(t *testing.T) {
	proc := newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{})
	rate := 16000
	input := uniformNoise(0.05, 2*rate, 11)
	out := decodeS16(runProcessor(proc, [][]byte{encodeS16(input)}))
	tail := out[len(out)-rate/2:]
	inTail := input[len(input)-rate/2:]
	got, want := samplesRMS(tail), samplesRMS(inTail)
	if got > want*0.75 {
		t.Fatalf("stationary noise not suppressed: got %.4f want < %.4f", got, want*0.75)
	}
}

func TestCaptureProcessorLimitsPeaks(t *testing.T) {
	proc := newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{})
	rate := 16000
	input := sine(rate, 0.99, 1000, rate/2)
	out := decodeS16(runProcessor(proc, [][]byte{encodeS16(input)}))
	for i, v := range out {
		if math.Abs(v) > 0.999 || math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("sample %d is not bounded: %f", i, v)
		}
	}
}

// A discontinuity must clear every filter, FFT and noise-estimate state, and
// the stream must resume from the same bounded latency rather than replaying
// stale ringing or suppressing a fresh room for several seconds.
func TestCaptureProcessorResetClearsState(t *testing.T) {
	proc := newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{})
	rate := 16000
	runProcessor(proc, [][]byte{encodeS16(sine(rate, 0.9, 1000, rate/2))})
	proc.reset()
	silence := make([]byte, rate*2)
	out := decodeS16(runProcessor(proc, [][]byte{silence}))
	for i := 0; i < captureDSPFrame; i++ {
		if out[i] != 0 {
			t.Fatalf("reset stream emitted stale sample %d: %f", i, out[i])
		}
	}
	if rms := samplesRMS(out); rms > 1e-4 {
		t.Fatalf("reset stream rings after silence: RMS %.6f", rms)
	}
}

func TestCaptureProcessorDisabledOrUnsupportedPassesThrough(t *testing.T) {
	if newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{Disabled: true}) != nil {
		t.Fatal("disabled processing still allocated a processor")
	}
	for _, format := range []backend.AudioFormat{
		{Format: backend.S16LE, RateHz: 48000, Channels: 2},
		{Format: backend.S16LE, RateHz: 48000, Channels: 1},
	} {
		if newCaptureProcessor(format, CaptureDSPOptions{}) != nil {
			t.Fatalf("unsupported capture format %+v was processed", format)
		}
	}
}

// The application capture tap and the framework frame subscription carry the
// same framework provenance, so wakeword enrollment and inference see the
// processed samples while the explicit raw endpoint remains available.
func TestCaptureTapProvenance(t *testing.T) {
	source := &gatedSource{sizes: make(chan int), entered: make(chan struct{}, 1)}
	m := newAudioManager(t, &gatedDevice{source: source})
	defer m.Close()
	frames, cancel := m.SubscribeFrames(4)
	defer cancel()

	capture, err := m.StartCapture("test.app", "run", CaptureEndpoint, backend.AudioFormat{}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.StopCapture(capture.ID())
	started := <-capture.Events()
	if started.Started == nil || started.Started.ProcessingTap != CaptureProcessingTap {
		t.Fatalf("application capture tap = %+v, want %q", started.Started, CaptureProcessingTap)
	}

	advanceCapture(t, source, 640)
	chunk := <-capture.Events()
	if chunk.Chunk == nil || chunk.Chunk.ProcessingTap != CaptureProcessingTap {
		t.Fatalf("application chunk tap = %+v", chunk.Chunk)
	}
	if chunk.Chunk.Frames != 320 || len(chunk.Chunk.PCM) != 640 {
		t.Fatalf("processed chunk length = frames %d bytes %d", chunk.Chunk.Frames, len(chunk.Chunk.PCM))
	}
	raw := <-frames
	if raw.ProcessingTap != CaptureProcessingTap {
		t.Fatalf("framework frame tap = %q, want %q", raw.ProcessingTap, CaptureProcessingTap)
	}
	if len(raw.PCM) != 640 {
		t.Fatalf("processed frame length = %d", len(raw.PCM))
	}

	rawCapture, err := m.StartCapture("test.app", "run", RawCaptureEndpoint, backend.AudioFormat{}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.StopCapture(rawCapture.ID())
	rawStarted := <-rawCapture.Events()
	if rawStarted.Started == nil || rawStarted.Started.ProcessingTap != "" {
		t.Fatalf("raw capture tap = %+v", rawStarted.Started)
	}
}

// The framework subscription must deliver the exact cleaned samples, not the
// physical input or an approximation of the chain: wakeword inference consumes
// this stream. The expected buffer is produced by the same DSP implementation
// from reset state, which is bit-identical to chunked streaming.
func TestSubscribeFramesDeliversProcessedSamples(t *testing.T) {
	rate := 16000
	n := 3 * 320
	signal := make([]float64, n)
	// Speech-like tone plus low rumble that the 80 Hz high-pass must remove.
	tone := sine(rate, 0.2, 440, n)
	rumble := sine(rate, 0.2, 40, n)
	for i := range signal {
		signal[i] = tone[i] + rumble[i]
	}
	srcPCM := encodeS16(signal)
	source := &gatedSource{sizes: make(chan int), entered: make(chan struct{}, 1), data: srcPCM}
	m := newAudioManager(t, &gatedDevice{source: source})
	defer m.Close()
	frames, cancel := m.SubscribeFrames(8)
	defer cancel()

	var got []byte
	for range n / 320 {
		advanceCapture(t, source, 640)
		select {
		case f := <-frames:
			if f.ProcessingTap != CaptureProcessingTap {
				t.Fatalf("frame tap = %q, want %q", f.ProcessingTap, CaptureProcessingTap)
			}
			got = append(got, f.PCM...)
		case <-time.After(time.Second):
			t.Fatal("processed frame not delivered")
		}
	}
	want, tap := ProcessCaptureS16(s16Samples(srcPCM), CaptureDSPOptions{})
	if tap != CaptureProcessingTap {
		t.Fatalf("offline tap = %q", tap)
	}
	rawSamples := s16Samples(srcPCM)
	gotSamples := s16Samples(got)
	if len(gotSamples) != len(want) {
		t.Fatalf("frame samples = %d, want %d", len(gotSamples), len(want))
	}
	changed := false
	for i := range want {
		if gotSamples[i] != want[i] {
			t.Fatalf("frame sample %d = %d, want %d", i, gotSamples[i], want[i])
		}
		if gotSamples[i] != rawSamples[i] {
			changed = true
		}
	}
	if !changed {
		t.Fatal("frame stream is indistinguishable from the raw physical signal")
	}
}

func advanceCapture(t *testing.T, source *gatedSource, n int) {
	t.Helper()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("capture did not request a chunk")
	}
	source.sizes <- n
	// Wait until the chunk has been read and dispatched before the next call.
	select {
	case <-source.entered:
		source.entered <- struct{}{}
	case <-time.After(time.Second):
		t.Fatal("capture did not dispatch the chunk")
	}
}

// Unity regression for the analysis/synthesis windows. Both windows are
// sqrt-Hann, so the two 50 % overlapping contributions sum to one at every
// sample. Comparing sample by sample (not chunk RMS) catches the old
// phase-dependent 0.667..1.333 gain ripple that a 125 Hz tone exposed.
func TestCaptureProcessorUnitySampleWise(t *testing.T) {
	opts := CaptureDSPOptions{DisableNoiseSuppression: true, DisableHighPass: true}
	proc := newCaptureProcessor(dspTestFormat(), opts)
	if proc == nil {
		t.Fatal("processor was not created")
	}
	rate := 16000
	n := rate
	signal := make([]float64, n)
	tone := sine(rate, 0.2, 125, n)
	noise := uniformNoise(0.002, n, 17)
	for i := range signal {
		signal[i] = tone[i] + noise[i]
	}
	pcm := encodeS16(signal)
	outPCM := runProcessor(proc, [][]byte{pcm})
	in, out := decodeS16(pcm), decodeS16(outPCM)

	// The STFT is zero-phase with the fixed latency of the priming window.
	const delay = captureDSPFrame - 1
	// The very first frame is missing its negative-time predecessor; skip the
	// priming region, then require steady-state sample-wise unity.
	for i := delay + 2*captureDSPFrame; i < len(out); i++ {
		want := in[i-delay]
		if math.Abs(out[i]-want) > 2.0/32768 {
			t.Fatalf("sample %d gain error: got %f want %f (delta %.6f)",
				i, out[i], want, out[i]-want)
		}
		if math.Abs(out[i]) >= 0.891 {
			t.Fatalf("limiter engaged in unity fixture at sample %d: %f", i, out[i])
		}
	}
}

type fortyEightMonoDevice struct{ fakeDevice }

func (d *fortyEightMonoDevice) CaptureFormat() backend.AudioFormat {
	return backend.AudioFormat{Format: backend.S16LE, RateHz: 48000, Channels: 1}
}

// Endpoint advertisement must describe what a capture actually receives. A
// disabled DSP or an unsupported backend format delivers the raw physical tap,
// so the effective tap is empty and the Start event agrees.
func TestCaptureProcessingTapMatchesEffectiveCapture(t *testing.T) {
	supported := newAudioManager(t, &fakeDevice{})
	defer supported.Close()
	if got := supported.CaptureProcessingTap(); got != CaptureProcessingTap {
		t.Fatalf("supported manager tap = %q, want %q", got, CaptureProcessingTap)
	}
	capture, err := supported.StartCapture("", "", CaptureEndpoint, backend.AudioFormat{}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	started := <-capture.Events()
	if started.Started == nil || started.Started.ProcessingTap != supported.CaptureProcessingTap() {
		t.Fatalf("started tap = %+v, manager tap = %q", started.Started, supported.CaptureProcessingTap())
	}
	supported.StopCapture(capture.ID())

	for _, tc := range []struct {
		name string
		dev  backend.AudioDevice
		opts Options
	}{
		{"disabled", &fakeDevice{}, Options{CaptureDSP: CaptureDSPOptions{Disabled: true}}},
		{"unsupported format", &fortyEightMonoDevice{}, Options{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := ids.NewClock()
			logs, err := logbus.New(clock, 100, "")
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(context.Background(), clock, logs, tc.dev, tc.opts)
			defer m.Close()
			if got := m.CaptureProcessingTap(); got != "" {
				t.Fatalf("effective tap = %q, want empty", got)
			}
			capture, err := m.StartCapture("", "", CaptureEndpoint, backend.AudioFormat{}, 0, "", "")
			if err != nil {
				t.Fatal(err)
			}
			defer m.StopCapture(capture.ID())
			started := <-capture.Events()
			if started.Started == nil || started.Started.ProcessingTap != "" {
				t.Fatalf("started tap = %+v, want empty", started.Started)
			}
		})
	}
}

// --- colored-noise audit ---------------------------------------------------

// pinkNoise approximates a 1/f room spectrum with Paul Kellet's filter. It is
// deterministic so effectiveness bounds are reproducible.
func pinkNoise(amplitude float64, n int, seed uint64) []float64 {
	rng := testLCG(seed)
	var b0, b1, b2, b3, b4, b5, b6 float64
	out := make([]float64, n)
	for i := range n {
		white := rng.uniform()*2 - 1
		b0 = 0.99886*b0 + white*0.0555179
		b1 = 0.99332*b1 + white*0.0750759
		b2 = 0.96900*b2 + white*0.1538520
		b3 = 0.86650*b3 + white*0.3104856
		b4 = 0.55000*b4 + white*0.5329522
		b5 = -0.7616*b5 - white*0.0168980
		pink := b0 + b1 + b2 + b3 + b4 + b5 + b6 + white*0.5362
		b6 = white * 0.115926
		out[i] = amplitude * pink / 3.5
	}
	return out
}

// brownNoise integrates white noise into the low-frequency rumble a room,
// traffic or a fan housing produces.
func brownNoise(amplitude float64, n int, seed uint64) []float64 {
	rng := testLCG(seed)
	out := make([]float64, n)
	var v float64
	for i := range n {
		v = (v + 0.02*(rng.uniform()*2-1)) / 1.02
		out[i] = amplitude * v / 0.02
	}
	return out
}

// coloredRoomNoise is the deterministic colored/AM room fixture used by the
// suppression and cold-start re-engagement regressions.
func coloredRoomNoise(n int, seed uint64) []float64 {
	input := pinkNoise(0.05, n, seed)
	rumble := brownNoise(0.15, n, seed+2)
	for i := range input {
		t := float64(i) / 16000
		input[i] = (input[i] + rumble[i]) * (0.7 + 0.3*math.Sin(2*math.Pi*0.8*t))
	}
	return input
}

// bandEnergy sums STFT power in [lo,hi) over the tail of pcm, using the same
// 256/128 analysis frame as the suppressor.
func bandEnergy(pcm []byte, lo, hi float64) float64 {
	samples := decodeS16(pcm)
	plan := newFFTPlan()
	var re, im [captureDSPFrame]float64
	var energy float64
	binHz := 16000.0 / captureDSPFrame
	for off := 0; off+captureDSPFrame <= len(samples); off += captureDSPHop {
		for i := range captureDSPFrame {
			re[i] = samples[off+i]
			im[i] = 0
		}
		plan.forward(&re, &im)
		for k := 1; k < captureDSPBins-1; k++ {
			f := float64(k) * binHz
			if f >= lo && f < hi {
				energy += re[k]*re[k] + im[k]*im[k]
			}
		}
	}
	return energy
}

func attenuationDB(raw, processed float64) float64 {
	if processed <= 0 {
		return 120
	}
	return 10 * math.Log10(raw/processed)
}

func TestCaptureProcessorSuppressesColoredPinkNoise(t *testing.T) {
	rate := 16000
	n := 4 * rate
	input := pinkNoise(0.05, n, 23)
	pcm := encodeS16(input)
	out := runProcessor(newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{}), [][]byte{pcm})
	tail := 2 * rate
	raw := pcm[len(pcm)-tail*2:]
	proc := out[len(out)-tail*2:]
	rawDB := attenuationDB(bandEnergy(raw, 100, 6000), bandEnergy(proc, 100, 6000))
	t.Logf("pink 100-6000 Hz suppression %.2f dB", rawDB)
	if rawDB < 2 {
		t.Fatalf("colored stationary noise suppressed only %.2f dB, want >= 2", rawDB)
	}
	if got := samplesRMS(decodeS16(proc)); got > samplesRMS(decodeS16(raw))*0.9 {
		t.Fatalf("pink noise RMS not reduced: got %.5f raw %.5f", got, samplesRMS(decodeS16(raw)))
	}
}

func TestCaptureProcessorRemovesBrownRumble(t *testing.T) {
	rate := 16000
	n := 4 * rate
	input := brownNoise(0.2, n, 29)
	pcm := encodeS16(input)
	out := runProcessor(newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{}), [][]byte{pcm})
	tail := 2 * rate
	raw := pcm[len(pcm)-tail*2:]
	proc := out[len(out)-tail*2:]
	lowDB := attenuationDB(bandEnergy(raw, 0, 160), bandEnergy(proc, 0, 160))
	t.Logf("brown 0-160 Hz suppression %.2f dB", lowDB)
	// The 80 Hz high-pass dominates here; the spectral suppressor is
	// conservative on strongly colored noise because its per-bin minimum
	// statistic under-estimates a high-variance stationary floor. Recorded so
	// a future retune cannot silently make bluer promises.
	if lowDB < 4.5 {
		t.Fatalf("rumble attenuated only %.2f dB, want >= 4.5", lowDB)
	}
}

func TestCaptureProcessorSuppressesColoredRoomNoise(t *testing.T) {
	rate := 16000
	n := 5 * rate
	input := coloredRoomNoise(n, 41)
	pcm := encodeS16(input)
	out := runProcessor(newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{}), [][]byte{pcm})
	tail := 2 * rate
	raw := pcm[len(pcm)-tail*2:]
	proc := out[len(out)-tail*2:]
	lowDB := attenuationDB(bandEnergy(raw, 80, 300), bandEnergy(proc, 80, 300))
	allDB := attenuationDB(bandEnergy(raw, 80, 8000), bandEnergy(proc, 80, 8000))
	t.Logf("colored AM room 80-300 suppression %.2f dB, 80-8000 suppression %.2f dB", lowDB, allDB)
	if lowDB < 3 || allDB < 2.5 {
		t.Fatalf("colored room noise under-suppressed: 80-300 %.2f dB, 80-8000 %.2f dB", lowDB, allDB)
	}
	if got := samplesRMS(decodeS16(proc)); got > samplesRMS(decodeS16(raw))*0.8 {
		t.Fatalf("colored room RMS not reduced enough: got %.5f raw %.5f", got, samplesRMS(decodeS16(raw)))
	}
}

// After a reset/start during speech, the synthetic non-speech colored room must
// re-confirm tracking and be suppressed again; preserving speech must not leave
// the DSP permanently at unity. This uses the deterministic coloredRoomNoise
// fixture, not the authorized clip (external music, not a room label). The
// comparison uses the same chain with NS disabled, isolating the estimator from
// the high-pass/limiter.
func TestCaptureProcessorReengagesRoomAfterColdSpeech(t *testing.T) {
	rate := 16000
	cases := map[string][]float64{
		"tone": sine(rate, 0.2, 440, rate),
		"word": speechLikePCM(rate, 1.5),
	}
	for name, speech := range cases {
		t.Run(name, func(t *testing.T) {
			room := coloredRoomNoise(4*rate, 71)
			input := append(append([]float64{}, speech...), room...)
			proc := newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{})
			out := runProcessor(proc, [][]byte{encodeS16(input)})
			ref := runProcessor(newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{DisableNoiseSuppression: true}), [][]byte{encodeS16(input)})
			if !proc.ns.tracking {
				t.Fatal("room did not re-confirm tracking after cold speech")
			}
			tail := 2 * rate
			rawTail := ref[len(ref)-tail*2:]
			procTail := out[len(out)-tail*2:]
			db := attenuationDB(bandEnergy(rawTail, 80, 8000), bandEnergy(procTail, 80, 8000))
			t.Logf("re-engaged room tail suppression %.2f dB", db)
			if db < 2 {
				t.Fatalf("room tail suppressed only %.2f dB after cold speech, want >= 2", db)
			}
		})
	}
}

// A second speech after the room has re-engaged must stay protected: the
// confirmation reference is the room-run mean, so 3x speech is above the
// 2.5x floor gate. Also checks that the room immediately before the second
// speech is actually suppressed.
func TestCaptureProcessorProtectsSpeechAfterRoomReengagement(t *testing.T) {
	rate := 16000
	first := speechLikePCM(rate, 1.5)
	second := speechLikePCM(rate, 1.0)
	for _, tc := range []struct {
		name     string
		roomSecs int
	}{
		{"immediate", 1},
		{"after1s", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			room := coloredRoomNoise(tc.roomSecs*rate, 73)
			input := append(append(append([]float64{}, first...), room...), second...)
			proc := newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{})
			out := runProcessor(proc, [][]byte{encodeS16(input)})
			ref := runProcessor(newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{DisableNoiseSuppression: true}), [][]byte{encodeS16(input)})
			if !proc.ns.tracking {
				t.Fatal("room did not re-confirm tracking before second speech")
			}
			secondStart := len(first) + len(room)
			secondOut := out[secondStart*2:]
			secondRef := ref[secondStart*2:]
			speechDB := attenuationDB(bandEnergy(secondRef, 300, 3000), bandEnergy(secondOut, 300, 3000))
			t.Logf("second-speech band change %.2f dB", speechDB)
			if speechDB > 1.5 {
				t.Fatalf("second speech lost %.2f dB, want <= 1.5", speechDB)
			}
			// Room immediately before the second speech.
			roomTailStart := secondStart - rate/2
			roomOut := out[roomTailStart*2 : secondStart*2]
			roomRef := ref[roomTailStart*2 : secondStart*2]
			roomDB := attenuationDB(bandEnergy(roomRef, 80, 8000), bandEnergy(roomOut, 80, 8000))
			t.Logf("pre-second-speech room suppression %.2f dB", roomDB)
			if roomDB < 1 {
				t.Fatalf("room before second speech suppressed only %.2f dB, want >= 1", roomDB)
			}
		})
	}
}

func TestCaptureProcessorPreservesSpeechOnColoredNoise(t *testing.T) {
	rate := 16000
	n := 5 * rate
	noise := pinkNoise(0.02, n, 31)
	speech := make([]float64, n)
	// Speech starts after 1.5 s of room noise: a constant harmonic from sample
	// zero is stationary and indistinguishable from ambience, which is not the
	// operating case (priming and the energy average see the room first).
	start := rate + rate/2
	for i := start; i < n; i++ {
		t := float64(i-start) / float64(rate)
		speech[i] = 0.12*math.Sin(2*math.Pi*350*t) + 0.08*math.Sin(2*math.Pi*900*t) + 0.04*math.Sin(2*math.Pi*2400*t)
	}
	mix := make([]float64, n)
	for i := range mix {
		mix[i] = noise[i] + speech[i]
	}
	// Speech-only after the same room preroll isolates the chain's effect on
	// the speech component itself; comparing the mixture's absolute band level
	// would count intended noise suppression as speech loss.
	speechOnly := make([]float64, n)
	copy(speechOnly[:start], noise[:start])
	copy(speechOnly[start:], speech[start:])
	procSpeech := runProcessor(newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{}), [][]byte{encodeS16(speechOnly)})
	tail := 2 * rate
	speechIn := bandEnergy(encodeS16(speechOnly)[len(speechOnly)*2-tail*2:], 300, 3000)
	speechOut := bandEnergy(procSpeech[len(procSpeech)-tail*2:], 300, 3000)
	lossDB := attenuationDB(speechIn, speechOut)
	t.Logf("speech-only (300-3000 Hz) level change %.2f dB", lossDB)
	if lossDB > 1.5 {
		t.Fatalf("speech lost %.2f dB after room preroll, want <= 1.5", lossDB)
	}

	// In the simultaneous mixture, subtract the processed noise-only band to
	// bound the speech residual. The bound is looser because the estimate is
	// shared between speech and noise.
	procMix := runProcessor(newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{}), [][]byte{encodeS16(mix)})
	procNoise := runProcessor(newCaptureProcessor(dspTestFormat(), CaptureDSPOptions{}), [][]byte{encodeS16(noise)})
	mixTail := encodeS16(mix)[len(mix)*2-tail*2:]
	noiseTail := encodeS16(noise)[len(noise)*2-tail*2:]
	mixOut := procMix[len(procMix)-tail*2:]
	noiseOut := procNoise[len(procNoise)-tail*2:]
	speechInMix := bandEnergy(mixTail, 300, 3000) - bandEnergy(noiseTail, 300, 3000)
	speechOutMix := bandEnergy(mixOut, 300, 3000) - bandEnergy(noiseOut, 300, 3000)
	mixDB := attenuationDB(speechInMix, speechOutMix)
	t.Logf("speech residual in colored noise (300-3000 Hz) change %.2f dB", mixDB)
	if mixDB > 3 {
		t.Fatalf("speech residual lost %.2f dB in noise, want <= 3", mixDB)
	}
	if mixDB < -2 {
		t.Fatalf("speech residual gained %.2f dB in noise, want >= -2", mixDB)
	}
}
