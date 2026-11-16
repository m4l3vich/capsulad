package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

// levelSink records mixer output so tests can measure the digital result.
type levelSink struct {
	mu  sync.Mutex
	buf []byte
}

func (s *levelSink) WritePCM(p []byte) error {
	s.mu.Lock()
	s.buf = append(s.buf, p...)
	s.mu.Unlock()
	return nil
}
func (s *levelSink) Close() error { return nil }

func (s *levelSink) reset() {
	s.mu.Lock()
	s.buf = s.buf[:0]
	s.mu.Unlock()
}

func (s *levelSink) peak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	pk := 0
	for i := 0; i+1 < len(s.buf); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(s.buf[i:])))
		if v < 0 {
			v = -v
		}
		if v > pk {
			pk = v
		}
	}
	return pk
}

type levelSource struct{}

func (s *levelSource) ReadPCM(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	time.Sleep(20 * time.Millisecond)
	return len(p), nil
}
func (s *levelSource) Close() error { return nil }

type levelDevice struct{ sink *levelSink }

func (d *levelDevice) PlaybackFormat() backend.AudioFormat {
	return backend.AudioFormat{Format: backend.S16LE, RateHz: 48000, Channels: 2}
}
func (d *levelDevice) CaptureFormat() backend.AudioFormat {
	return backend.AudioFormat{Format: backend.S16LE, RateHz: 16000, Channels: 1}
}
func (d *levelDevice) OpenOutput() (backend.Sink, error)  { return d.sink, nil }
func (d *levelDevice) OpenInput() (backend.Source, error) { return &levelSource{}, nil }
func (d *levelDevice) SetMasterVolume(float64) error      { return nil }
func (d *levelDevice) SetHardwareMute(bool) (bool, error) { return false, nil }
func (d *levelDevice) Close() error                       { return nil }

func newLevelManager(t *testing.T, sink *levelSink) *Manager {
	t.Helper()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background(), clock, logs, &levelDevice{sink: sink}, Options{})
	if err := m.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

// s16Level is the int16 code the mixer produces for one float level.
func s16Level(v float64) int { return int(v * 32767) }

func s16FullScale(rate, frames int) []byte {
	b := make([]byte, frames*4)
	for i := range frames {
		binary.LittleEndian.PutUint16(b[i*4:], 32767)
		binary.LittleEndian.PutUint16(b[i*4+2:], 32767)
	}
	return b
}

func f32Constant(rate, frames int, v float32) []byte {
	b := make([]byte, frames*8)
	for i := range frames {
		binary.LittleEndian.PutUint32(b[i*8:], math.Float32bits(v))
		binary.LittleEndian.PutUint32(b[i*8+4:], math.Float32bits(v))
	}
	return b
}

// playAndMeasure opens one stream, writes pcm, waits for the mixer to consume
// it, and returns the recorded peak.
func playAndMeasure(t *testing.T, m *Manager, sink *levelSink, appID string, format backend.AudioFormat, pcm []byte, wait time.Duration) int {
	t.Helper()
	sink.reset()
	p, err := m.OpenPlayback(appID, "run", "audio.output.speaker", format, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Write(pcm); err != nil {
		t.Fatal(err)
	}
	time.Sleep(wait)
	p.Close()
	time.Sleep(30 * time.Millisecond)
	return sink.peak()
}

// TestMixerAppliesLinearMaster pins the unchanged linear master below the
// limiter threshold. The earlier contract was verified at 1.0 as well; that
// level now passes through the limiter and is covered separately.
func TestMixerAppliesLinearMaster(t *testing.T) {
	format := backend.AudioFormat{Format: backend.S16LE, RateHz: 44100, Channels: 2}
	clip := s16FullScale(44100, 4410*4)
	for _, vol := range []float64{0.5, 0.25, 0.1} {
		sink := &levelSink{}
		m := newLevelManager(t, sink)
		m.SetMasterVolume(vol)
		peak := playAndMeasure(t, m, sink, "test.audio", format, clip, 400*time.Millisecond)
		want := s16Level(vol)
		if diff := peak - want; diff < -2 || diff > 2 {
			t.Errorf("master %.2f: sink peak %d, want %d ±2", vol, peak, want)
		}
		m.Close()
	}
}

// TestMixerFullScaleAtUnityIsLimited documents the one non-linear level: at
// master 1.0 a full-scale stream sits at the limiter threshold, not at the
// int16 rail.
func TestMixerFullScaleAtUnityIsLimited(t *testing.T) {
	sink := &levelSink{}
	m := newLevelManager(t, sink)
	m.SetMasterVolume(1)
	peak := playAndMeasure(t, m, sink, "test.audio",
		backend.AudioFormat{Format: backend.S16LE, RateHz: 48000, Channels: 2},
		s16FullScale(48000, 4800), 250*time.Millisecond)
	want := s16Level(limiterThreshold)
	if peak > want+1 || peak < want-2 {
		t.Fatalf("unity full-scale peak = %d, want limiter threshold %d", peak, want)
	}
}

func TestF32OutOfRangeClamped(t *testing.T) {
	format := backend.AudioFormat{Format: backend.F32LE, RateHz: 48000, Channels: 2}
	want := s16Level(0.1)
	for _, amp := range []float32{1, 2, 10, 100} {
		sink := &levelSink{}
		m := newLevelManager(t, sink)
		m.SetMasterVolume(0.1)
		peak := playAndMeasure(t, m, sink, "test.audio", format, f32Constant(48000, 4800, amp), 200*time.Millisecond)
		if diff := peak - want; diff < -2 || diff > 2 {
			t.Errorf("F32 amplitude %.0f at master 0.1: sink peak %d, want %d (master as ceiling)", amp, peak, want)
		}
		m.Close()
	}
}

func TestF32NonFiniteBecomesSilence(t *testing.T) {
	format := backend.AudioFormat{Format: backend.F32LE, RateHz: 48000, Channels: 2}
	sink := &levelSink{}
	m := newLevelManager(t, sink)
	m.SetMasterVolume(1)
	pcm := append(f32Constant(48000, 4800, float32(math.NaN())), f32Constant(48000, 4800, float32(math.Inf(1)))...)
	peak := playAndMeasure(t, m, sink, "test.audio", format, pcm, 250*time.Millisecond)
	if peak != 0 {
		t.Fatalf("NaN/Inf chunk produced peak %d, want silence", peak)
	}
}

// TestPerAppCeilingWithManyStreams proves one app's contribution is bounded no
// matter how many streams it opens, and that the documented fifth stream is
// rejected.
func TestPerAppCeilingWithManyStreams(t *testing.T) {
	sink := &levelSink{}
	m := newLevelManager(t, sink)
	m.SetMasterVolume(0.6)
	format := backend.AudioFormat{Format: backend.S16LE, RateHz: 48000, Channels: 2}
	sink.reset()
	for i := 0; i < maxPlaybackStreamsPerApp; i++ {
		p, err := m.OpenPlayback("test.audio", "run", "audio.output.speaker", format, "")
		if err != nil {
			t.Fatalf("stream %d: %v", i+1, err)
		}
		if err := p.Write(s16FullScale(48000, 9600)); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(250 * time.Millisecond)
	peak := sink.peak()
	want := s16Level(0.6)
	if diff := peak - want; diff < -2 || diff > 2 {
		t.Fatalf("%d concurrent full-scale streams peaked %d, want the per-app ceiling %d", maxPlaybackStreamsPerApp, peak, want)
	}
	_, err := m.OpenPlayback("test.audio", "run", "audio.output.speaker", format, "")
	if !errors.Is(err, ErrPlaybackLimit) {
		t.Fatalf("5th stream error = %v, want ErrPlaybackLimit", err)
	}
}

// TestLimiterPreventsClippingOverlappingApps proves two apps cannot push the
// master bus past the limiter threshold, and that the gain recovers afterwards.
func TestLimiterPreventsClippingOverlappingApps(t *testing.T) {
	sink := &levelSink{}
	m := newLevelManager(t, sink)
	m.SetMasterVolume(0.9)
	format := backend.AudioFormat{Format: backend.S16LE, RateHz: 48000, Channels: 2}
	sink.reset()
	a, err := m.OpenPlayback("app.a", "run-a", "audio.output.speaker", format, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.OpenPlayback("app.b", "run-b", "audio.output.speaker", format, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*Playback{a, b} {
		if err := p.Write(s16FullScale(48000, 9600)); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	peak := sink.peak()
	limit := s16Level(limiterThreshold) + 1
	if peak > limit {
		t.Fatalf("overlapping apps peaked %d, above the limiter threshold %d", peak, limit)
	}
	if peak < 30000 {
		t.Fatalf("overlapping apps peaked %d; the limiter did not engage", peak)
	}
	a.Close()
	b.Close()
	// Release: after the attack window and the 150 ms release, a quiet stream
	// plays at its un-limited level again.
	time.Sleep(500 * time.Millisecond)
	quiet := make([]byte, 4800*4)
	for i := range 4800 {
		binary.LittleEndian.PutUint16(quiet[i*4:], 16384)
		binary.LittleEndian.PutUint16(quiet[i*4+2:], 16384)
	}
	peak = playAndMeasure(t, m, sink, "app.c",
		backend.AudioFormat{Format: backend.S16LE, RateHz: 48000, Channels: 2}, quiet, 200*time.Millisecond)
	want := s16Level(0.5 * 0.9)
	if diff := peak - want; diff < -50 || diff > 2 {
		t.Fatalf("post-release quiet peak %d, want about %d", peak, want)
	}
}

func TestDuckingUnaffectedByCeiling(t *testing.T) {
	sink := &levelSink{}
	m := newLevelManager(t, sink)
	m.SetMasterVolume(0.5)
	format := backend.AudioFormat{Format: backend.S16LE, RateHz: 48000, Channels: 2}
	// Without a conversation, a stream plays at the master level.
	peak := playAndMeasure(t, m, sink, "test.audio", format, s16FullScale(48000, 4800), 150*time.Millisecond)
	if diff := peak - s16Level(0.5); diff < -2 || diff > 2 {
		t.Fatalf("unducked stream peak %d, want 0.5 FS", peak)
	}
	// An unrelated stream ducks to 0.25 with the existing 150 ms fade. The
	// stream must be open for the fade to run: gain starts at unity when a
	// playback opens, so measure after the fade window.
	m.BeginConversation("conv-1", "test.audio", "run")
	defer m.EndConversation("conv-1")
	sink.reset()
	other, err := m.OpenPlayback("other.audio", "run-other", "audio.output.speaker", format, "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := other.Write(s16FullScale(48000, 4800)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	other.Close()
	time.Sleep(30 * time.Millisecond)
	peak = sink.peak()
	want := s16Level(0.5 * 0.25)
	if diff := peak - want; diff < -2 || diff > 2 {
		t.Fatalf("ducked stream peak %d, want %d", peak, want)
	}
}

func TestCueTrimStillApplies(t *testing.T) {
	sink := &levelSink{}
	m := newLevelManager(t, sink)
	m.SetMasterVolume(0.5)
	sink.reset()
	p, err := m.OpenPlayback("system", "", "audio.output.speaker",
		backend.AudioFormat{Format: backend.S16LE, RateHz: 48000, Channels: 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.SetTrim(0.4)
	if err := p.Write(s16FullScale(48000, 4800)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	p.Close()
	time.Sleep(30 * time.Millisecond)
	want := s16Level(0.5 * 0.4)
	if peak := sink.peak(); peak-want < -2 || peak-want > 2 {
		t.Fatalf("trimmed cue peak %d, want %d", peak, want)
	}
}

// TestOnlyMixLoopWritesPCM is the architecture guard for "master volume is a
// ceiling": the only place the daemon submits mixed PCM to a sink is the mixer
// loop. A new fast path that skips the master/limiter stage fails here.
func TestOnlyMixLoopWritesPCM(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "gen", "build", "dist", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "WritePCM" {
					return true
				}
				// The mixer loop is the one allowed producer. A sink
				// implementation may forward PCM to another sink inside its
				// own WritePCM method; method declarations alone are not calls.
				if fn.Name.Name == "WritePCM" {
					return true
				}
				if strings.HasPrefix(rel, filepath.Join("internal", "audio")) && fn.Name.Name == "mixLoop" {
					return true
				}
				pos := fset.Position(call.Pos())
				offenders = append(offenders, pos.String()+" in "+fn.Name.Name)
				return true
			})
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if len(offenders) > 0 {
		t.Fatalf("PCM written outside the mixer loop: %s", strings.Join(offenders, ", "))
	}
}

// TestMasterLimiterReleasesTowardThresholdAfterTransient covers the over-
// attenuation the review found: a 1.0 transient latched the gain at
// threshold/1.0, and a signal that then settled at 0.96 kept that lower gain
// forever (output 0.912 instead of 0.95). The release is now rate-limited in
// both directions, so the gain climbs back to the exact threshold gain for the
// held peak while no frame exceeds the threshold.
func TestMasterLimiterReleasesTowardThresholdAfterTransient(t *testing.T) {
	lim := newMasterLimiter(outRate)
	limit := float32(limiterThreshold)
	const transient, steady float32 = 1.0, 0.96
	holdSamples := limiterAttackMS * outRate / 1000
	releaseSamples := limiterReleaseMS * outRate / 1000

	for i := 0; i < 2*holdSamples; i++ {
		g := lim.next(transient)
		if out := transient * g; out > limit+1e-6 {
			t.Fatalf("transient frame %d output %v exceeds the threshold %v", i, out, limit)
		}
	}
	latched := lim.gain
	if want := limit / transient; math.Abs(float64(latched-want)) > 1e-5 {
		t.Fatalf("transient gain %v, want %v (immediate catch)", latched, want)
	}
	latchedOut := steady * latched

	worst := float32(0)
	upward := true
	prev := latched
	var afterOneWindow, afterThreeWindows float32
	const windows = 3
	for i := 0; i < holdSamples+windows*releaseSamples; i++ {
		g := lim.next(steady)
		out := steady * g
		if out > worst {
			worst = out
		}
		if g < prev-1e-6 {
			upward = false
		}
		prev = g
		switch i {
		case holdSamples + releaseSamples - 1:
			afterOneWindow = out
		}
	}
	afterThreeWindows = steady * lim.gain

	t.Logf("latched gain %v → output %v; after 1 release window %v; after %d windows %v (threshold %v)",
		latched, latchedOut, afterOneWindow, windows, afterThreeWindows, limit)

	if !upward {
		t.Fatalf("gain did not release monotonically upward while the signal stayed above the threshold")
	}
	if worst > limit+1e-6 {
		t.Fatalf("steady 0.96 peaked at %v, above the threshold %v", worst, limit)
	}
	// The old behaviour stayed at the transient's gain, so 0.912 was the
	// ceiling. One release window must recover most of the gap, and three must
	// land essentially on the threshold.
	if afterOneWindow < latchedOut {
		t.Fatalf("output fell to %v after one release window; the gain did not release upward (was %v)", afterOneWindow, latchedOut)
	}
	if min := limit * 0.995; afterOneWindow < min {
		t.Fatalf("after one release window output %v, want at least %v (99.5%% of the threshold)", afterOneWindow, min)
	}
	if min := limit * 0.9995; afterThreeWindows < min {
		t.Fatalf("after %d release windows output %v, want at least %v", windows, afterThreeWindows, min)
	}

	// The previous no-overshoot guarantee still holds: a new rising peak is
	// caught in the same frame, at the exact threshold gain.
	g := lim.next(transient)
	if out := transient * g; math.Abs(float64(out-limit)) > 1e-5 {
		t.Fatalf("rising peak output %v, want the threshold %v", out, limit)
	}
	// And a signal below the threshold still releases fully to unity.
	for i := 0; i < 5*releaseSamples; i++ {
		lim.next(0.5)
	}
	if lim.gain != 1 {
		t.Fatalf("gain %v after the signal fell below the threshold, want unity", lim.gain)
	}
}

// TestMasterLimiterSteadyLevelSitsOnThreshold proves the immediate catch by
// itself is exact: a steady signal just above the threshold is attenuated to
// the threshold and no further.
func TestMasterLimiterSteadyLevelSitsOnThreshold(t *testing.T) {
	lim := newMasterLimiter(outRate)
	limit := float32(limiterThreshold)
	const level float32 = 0.96
	for i := 0; i < outRate/10; i++ {
		g := lim.next(level)
		if out := level * g; out > limit+1e-6 {
			t.Fatalf("frame %d output %v exceeds the threshold %v", i, out, limit)
		}
	}
	if out := level * lim.gain; math.Abs(float64(out-limit)) > 1e-6 {
		t.Fatalf("steady 0.96 output %v, want the threshold %v", out, limit)
	}
}
