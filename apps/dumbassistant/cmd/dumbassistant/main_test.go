package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"capsula/sdk/capsula"
)

func testWAV(t *testing.T, format uint16, channels, rate, bits int, pcm []byte) []byte {
	t.Helper()
	var body bytes.Buffer
	body.WriteString("WAVE")
	body.WriteString("fmt ")
	_ = binary.Write(&body, binary.LittleEndian, uint32(16))
	_ = binary.Write(&body, binary.LittleEndian, format)
	_ = binary.Write(&body, binary.LittleEndian, uint16(channels))
	_ = binary.Write(&body, binary.LittleEndian, uint32(rate))
	_ = binary.Write(&body, binary.LittleEndian, uint32(rate*channels*bits/8))
	_ = binary.Write(&body, binary.LittleEndian, uint16(channels*bits/8))
	_ = binary.Write(&body, binary.LittleEndian, uint16(bits))
	body.WriteString("data")
	_ = binary.Write(&body, binary.LittleEndian, uint32(len(pcm)))
	body.Write(pcm)
	if len(pcm)%2 != 0 {
		body.WriteByte(0)
	}

	var out bytes.Buffer
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(body.Len()))
	out.Write(body.Bytes())
	return out.Bytes()
}

func TestParseWAV(t *testing.T) {
	pcm := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	got, err := parseWAV(testWAV(t, 1, 2, 48000, 16, pcm))
	if err != nil {
		t.Fatalf("parseWAV: %v", err)
	}
	if got.format.SampleRateHz != 48000 || got.format.Channels != 2 {
		t.Fatalf("format = %d Hz/%d ch", got.format.SampleRateHz, got.format.Channels)
	}
	if got.format.SampleFormat != capsula.SampleFormatPCM16LE {
		t.Fatalf("sample format = %s, want PCM16LE", got.format.SampleFormat)
	}
	if got.bytesPerFrame != 4 || got.frames() != 2 {
		t.Fatalf("frame accounting: bytesPerFrame=%d frames=%d", got.bytesPerFrame, got.frames())
	}
	if !bytes.Equal(got.pcm, pcm) {
		t.Fatalf("pcm changed: %v", got.pcm)
	}
}

func TestParseWAVRejectsUnsupportedAudio(t *testing.T) {
	valid := testWAV(t, 1, 1, 16000, 16, []byte{1, 0, 2, 0})
	cases := map[string][]byte{
		"empty":        nil,
		"not riff":     []byte("NOPE0000WAVE"),
		"float pcm":    testWAV(t, 3, 1, 16000, 32, make([]byte, 8)),
		"8-bit pcm":    testWAV(t, 1, 1, 16000, 8, []byte{1, 2}),
		"misaligned":   testWAV(t, 1, 2, 16000, 16, []byte{1, 0, 2}),
		"empty data":   testWAV(t, 1, 1, 16000, 16, nil),
		"short format": valid[:20],
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseWAV(data); err == nil {
				t.Fatal("parseWAV accepted invalid audio")
			}
		})
	}
}

func TestPackageRoot(t *testing.T) {
	root, err := packageRoot("/app/dumbassistant")
	if err != nil {
		t.Fatal(err)
	}
	if root != "/app" {
		t.Fatalf("package root = %q, want /app", root)
	}
	rel, err := packageRoot("dumbassistant")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(rel) {
		t.Fatalf("relative argv path resolved to %q", rel)
	}
}

func TestParseDelay(t *testing.T) {
	valid := []struct {
		raw  string
		want delaySpec
	}{
		{"", delaySpec{}},
		{"   ", delaySpec{}},
		{"0", delaySpec{0, 0}},
		{"100", delaySpec{100, 100}},
		{"000100", delaySpec{100, 100}},
		{"200-1500", delaySpec{200, 1500}},
		{" 200 - 1500 ", delaySpec{200, 1500}},
		{"200-200", delaySpec{200, 200}},
		{"60000", delaySpec{60000, 60000}},
		{"0-60000", delaySpec{0, 60000}},
	}
	for _, tc := range valid {
		got, err := parseDelay(tc.raw)
		if err != nil {
			t.Fatalf("parseDelay(%q): %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("parseDelay(%q) = %+v, want %+v", tc.raw, got, tc.want)
		}
	}

	invalid := []string{
		"-1", "+1", "1.5", "abc", "1e3", "0x10",
		"200-", "-200", "300-200", "200-1500-9",
		"60001", "70000-80000", "100 200", "100--200",
		"999999999999999999999",
	}
	for _, raw := range invalid {
		if _, err := parseDelay(raw); err == nil {
			t.Fatalf("parseDelay(%q) accepted invalid input", raw)
		}
	}
	if _, err := parseDelay("1500-200"); err == nil || !strings.Contains(err.Error(), "greater") {
		t.Fatalf("inverted range error = %v", err)
	}
	if _, err := parseDelay("60001"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("overflow error = %v", err)
	}
}

func TestDelaySpec(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	if got := (delaySpec{}).delay(rng); got != 0 {
		t.Fatalf("empty delay = %s", got)
	}
	if got := (delaySpec{minMS: 100, maxMS: 100}).delay(rng); got != 100*time.Millisecond {
		t.Fatalf("constant delay = %s", got)
	}

	spec := delaySpec{minMS: 200, maxMS: 202}
	seen := map[int64]bool{}
	for range 1000 {
		got := int64(spec.delay(rng) / time.Millisecond)
		if got < 200 || got > 202 {
			t.Fatalf("random delay %d ms outside 200..202", got)
		}
		seen[got] = true
	}
	if !seen[200] || !seen[202] {
		t.Fatalf("inclusive bounds not reached: %v", seen)
	}
}

func TestNormalizeStoreAsset(t *testing.T) {
	valid := []struct{ value, rel, canonical string }{
		{"audio/reply.wav", "audio/reply.wav", "/assets/audio/reply.wav"},
		{"./audio/reply.wav", "audio/reply.wav", "/assets/audio/reply.wav"},
		{"audio//reply.wav", "audio/reply.wav", "/assets/audio/reply.wav"},
		{" audio/reply.wav ", "audio/reply.wav", "/assets/audio/reply.wav"},
	}
	for _, tc := range valid {
		rel, canonical, err := normalizeStoreAsset(tc.value)
		if err != nil {
			t.Fatalf("normalizeStoreAsset(%q): %v", tc.value, err)
		}
		if rel != tc.rel || canonical != tc.canonical {
			t.Fatalf("normalizeStoreAsset(%q) = %q, %q; want %q, %q",
				tc.value, rel, canonical, tc.rel, tc.canonical)
		}
	}
	invalid := []string{
		"", "   ", "/etc/passwd", "../x.wav", "audio/../x.wav",
		"audio/../../x", ".", "..", "a\x00b",
	}
	for _, value := range invalid {
		if _, _, err := normalizeStoreAsset(value); err == nil {
			t.Fatalf("normalizeStoreAsset(%q) accepted invalid input", value)
		}
	}
}

func TestResolveContainedPath(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"audio", "other"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name string, frames int) {
		data := testWAV(t, 1, 1, 16000, 16, make([]byte, frames*2))
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("audio/reply.wav", 2)
	write("other/file.wav", 2)
	if err := os.Symlink(filepath.Join(root, "other", "file.wav"), filepath.Join(root, "audio", "link.wav")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "other"), filepath.Join(root, "audio", "dirlink")); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(root, "audio", "reply.wav")
	if got, err := resolveContainedPath(root, "audio/reply.wav"); err != nil || got != want {
		t.Fatalf("contained path = %q, %v", got, err)
	}

	bad := map[string]string{
		"":                       "empty",
		"/etc/passwd":            "relative",
		"../reply.wav":           "..",
		"audio/../reply.wav":     "..",
		"audio/missing.wav":      "no such file",
		"audio":                  "regular file",
		"audio/link.wav":         "symlink",
		"audio/dirlink/file.wav": "symlink",
		"audio/\x00reply.wav":    "NUL",
	}
	for value, wantError := range bad {
		_, err := resolveContainedPath(root, value)
		if err == nil {
			t.Fatalf("resolveContainedPath(%q) accepted invalid path", value)
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(wantError)) {
			t.Fatalf("resolveContainedPath(%q) error = %v, want %q", value, err, wantError)
		}
	}
}

func TestStoreAssetDeclarationAndLoad(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "audio"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, frames int) {
		data := testWAV(t, 1, 1, 16000, 16, make([]byte, frames*2))
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("audio/alt.wav", 4)
	write("audio/other.wav", 3)
	write("audio/bad.wav", 2)
	write("audio/undeclared.wav", 2)
	if err := os.Symlink(filepath.Join(root, "audio", "alt.wav"), filepath.Join(root, "audio", "link.wav")); err != nil {
		t.Fatal(err)
	}
	manifest := `{"assets":[
		{"id":"assistant.alt","path":"audio/alt.wav","media_type":"audio/wav"},
		{"id":"assistant.other","path":"/assets/audio/other.wav","media_type":"audio/x-wav"},
		{"id":"assistant.bad","path":"audio/bad.wav","media_type":"video/mp4"},
		{"id":"assistant.link","path":"audio/link.wav","media_type":"audio/wav"}
	]}`
	if err := os.WriteFile(filepath.Join(root, assetManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	clip, path, err := loadStoreReply(root, "audio/alt.wav")
	if err != nil {
		t.Fatal(err)
	}
	if clip.frames() != 4 || path != filepath.Join(root, "audio", "alt.wav") {
		t.Fatalf("loaded %d frames from %q", clip.frames(), path)
	}
	if clip, _, err := loadStoreReply(root, "audio/other.wav"); err != nil || clip.frames() != 3 {
		t.Fatalf("canonical manifest path: %d frames, %v", clip.frames(), err)
	}

	bad := map[string]string{
		"audio/undeclared.wav": "not declared",
		"assets/audio/alt.wav": "not declared",
		"audio/bad.wav":        "media type",
		"audio/link.wav":       "symlink",
		"../alt.wav":           "..",
		"/etc/passwd":          "absolute",
	}
	for value, wantError := range bad {
		_, _, err := loadStoreReply(root, value)
		if err == nil {
			t.Fatalf("loadStoreReply(%q) accepted invalid asset", value)
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(wantError)) {
			t.Fatalf("loadStoreReply(%q) error = %v, want %q", value, err, wantError)
		}
	}

	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, "audio"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadStoreReply(empty, "audio/alt.wav"); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("missing manifest error = %v", err)
	}
}

func TestPackagedReply(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := testWAV(t, 1, 1, 16000, 16, make([]byte, 8))
	if err := os.WriteFile(filepath.Join(root, "assets", defaultReplyFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	clip, path, err := loadPackagedReply(root)
	if err != nil {
		t.Fatal(err)
	}
	if clip.frames() != 4 || path != filepath.Join(root, "assets", defaultReplyFile) {
		t.Fatalf("loaded %d frames from %q", clip.frames(), path)
	}
	if _, _, err := loadPackagedReply(t.TempDir()); err == nil {
		t.Fatal("missing packaged reply was accepted")
	}
}

func TestApplySettings(t *testing.T) {
	pkgRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(pkgRoot, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgRoot, "assets", defaultReplyFile),
		testWAV(t, 1, 1, 16000, 16, make([]byte, 4)), 0o644); err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()
	if err := os.MkdirAll(filepath.Join(store, "audio"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, frames := range map[string]int{"alt.wav": 8, "undeclared.wav": 4} {
		if err := os.WriteFile(filepath.Join(store, "audio", name),
			testWAV(t, 1, 1, 16000, 16, make([]byte, frames)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(store, assetManifestName),
		[]byte(`{"assets":[{"id":"assistant.alt","path":"audio/alt.wav","media_type":"audio/wav"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	base, path, err := loadPackagedReply(pkgRoot)
	if err != nil {
		t.Fatal(err)
	}
	a := &assistant{
		pkgRoot: pkgRoot, assetStore: store,
		settings: assistantSettings{
			delay:     delaySpec{minMS: 1000, maxMS: 1000},
			replyPath: path, clip: base,
		},
	}
	a.applySettings(&capsula.SettingsChanged{
		Revision: 7,
		Values: []capsula.SettingValue{
			{Key: "delay", ValueJSON: `"200-1500"`},
			{Key: "reply_asset", ValueJSON: `"audio/alt.wav"`},
		},
	})
	if a.settings.delay != (delaySpec{200, 1500}) {
		t.Fatalf("delay = %+v", a.settings.delay)
	}
	if a.settings.clip == base || a.settings.clip.frames() != 4 {
		t.Fatalf("clip was not replaced: %+v", a.settings.clip)
	}
	if a.settings.replyPath != filepath.Join(store, "audio", "alt.wav") {
		t.Fatalf("store reply path = %q", a.settings.replyPath)
	}
	other := a.settings.clip

	a.applySettings(&capsula.SettingsChanged{
		Revision: 8,
		Values: []capsula.SettingValue{
			{Key: "delay", ValueJSON: `"1500-200"`},
			{Key: "reply_asset", ValueJSON: `"../etc/passwd"`},
			{Key: "delay", ValueJSON: `123`},
		},
	})
	if a.settings.delay != (delaySpec{200, 1500}) {
		t.Fatalf("invalid delay replaced settings: %+v", a.settings.delay)
	}
	if a.settings.clip != other || a.settings.replyPath != filepath.Join(store, "audio", "alt.wav") {
		t.Fatalf("invalid asset replaced settings: %+v", a.settings)
	}

	a.applySettings(&capsula.SettingsChanged{
		Revision: 9,
		Values:   []capsula.SettingValue{{Key: "reply_asset", ValueJSON: `"audio/undeclared.wav"`}},
	})
	if a.settings.clip != other || a.settings.replyPath != filepath.Join(store, "audio", "alt.wav") {
		t.Fatalf("undeclared asset replaced settings: %+v", a.settings)
	}

	a.applySettings(&capsula.SettingsChanged{
		Revision: 10,
		Values: []capsula.SettingValue{
			{Key: "delay", ValueJSON: `""`},
			{Key: "reply_asset", ValueJSON: `""`},
		},
	})
	if !a.settings.delay.isZero() {
		t.Fatalf("empty delay = %+v", a.settings.delay)
	}
	if a.settings.replyPath != filepath.Join(pkgRoot, "assets", defaultReplyFile) || a.settings.clip.frames() != 2 {
		t.Fatalf("empty asset did not restore packaged default: %+v", a.settings)
	}
}

// The tracker's behaviour on real room audio is covered by the acoustic
// fixtures below; the small helpers that used to be tested here directly
// (thresholds, feed with ZCR) no longer exist.

func TestPCMRMS(t *testing.T) {
	if got := pcmRMS(nil); got != 0 {
		t.Fatalf("empty rms = %f", got)
	}
	if got := pcmRMS([]byte{0, 0, 0, 0}); got != 0 {
		t.Fatalf("silence rms = %f", got)
	}
	pcm := make([]byte, 8)
	for i := range 4 {
		binary.LittleEndian.PutUint16(pcm[i*2:], 16384)
	}
	if got := pcmRMS(pcm); got < 0.499 || got > 0.501 {
		t.Fatalf("half-scale rms = %f", got)
	}
}

func TestLevelFromRMS(t *testing.T) {
	if got := levelFromRMS(0); got != 0 {
		t.Fatalf("zero rms level = %f", got)
	}
	if got := levelFromRMS(1); got != 1 {
		t.Fatalf("full-scale rms level = %f", got)
	}
	if got := levelFromRMS(0.001); got > 0.001 {
		t.Fatalf("floor rms level = %f", got)
	}
	quiet, loud := levelFromRMS(0.01), levelFromRMS(0.2)
	if loud <= quiet {
		t.Fatalf("level is not monotonic: %f -> %f", quiet, loud)
	}
}

func TestIsPreroll(t *testing.T) {
	item := reply{activationNS: 1000}
	cases := []struct {
		name string
		item reply
		ns   uint64
		want bool
	}{
		{"before activation", item, 999, true},
		{"at activation", item, 1000, false},
		{"after activation", item, 1001, false},
		{"unknown activation time", reply{}, 1, false},
		{"unknown chunk time", item, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPreroll(tc.item, &capsula.AudioChunk{MonotonicTimeNS: tc.ns}); got != tc.want {
				t.Fatalf("isPreroll(activation=%d, chunk=%d) = %v, want %v",
					tc.item.activationNS, tc.ns, got, tc.want)
			}
		})
	}
}

func TestReleaseHandling(t *testing.T) {
	signalled := func(tr *turn) bool {
		t.Helper()
		select {
		case <-tr.release:
			return true
		default:
			return false
		}
	}

	a := &assistant{}

	// A release can arrive before its queued activation becomes the active
	// turn; it must be remembered and applied exactly once.
	queued := &turn{activationID: "queued", release: make(chan struct{})}
	a.releaseInput(&capsula.InputReleased{ActivationID: "queued"})
	if a.pendingRelease != "queued" {
		t.Fatalf("pending release = %q, want queued", a.pendingRelease)
	}
	if signalled(queued) {
		t.Fatal("queued turn was released before it became active")
	}
	a.setActive(queued)
	if !signalled(queued) {
		t.Fatal("pending release was not applied to the turn")
	}
	if a.pendingRelease != "" {
		t.Fatalf("pending release survived activation: %q", a.pendingRelease)
	}

	// A release for a different activation must not end the active turn; it
	// belongs to a turn that is still waiting in the queue.
	active := &turn{activationID: "active", release: make(chan struct{})}
	a.setActive(active)
	a.releaseInput(&capsula.InputReleased{ActivationID: "other"})
	if signalled(active) {
		t.Fatal("release for another activation ended the active turn")
	}
	if a.pendingRelease != "other" {
		t.Fatalf("pending release = %q, want other", a.pendingRelease)
	}

	// Releasing the active turn signals it immediately and clears it.
	a.releaseInput(&capsula.InputReleased{ActivationID: "active"})
	if !signalled(active) {
		t.Fatal("release did not signal the active turn")
	}
	a.clearActive(active)
	if a.active != nil {
		t.Fatalf("active turn = %+v after clearing", a.active)
	}

	// A nil release is ignored.
	a.releaseInput(nil)
}

func TestEmptyActivationIdentityIgnored(t *testing.T) {
	a := &assistant{queue: make(chan reply, 1)}
	a.enqueue(&capsula.Activation{})
	select {
	case item := <-a.queue:
		t.Fatalf("activation without an id was queued: %+v", item)
	default:
	}

	a.releaseInput(&capsula.InputReleased{})
	if a.pendingRelease != "" {
		t.Fatalf("release without an id became pending: %q", a.pendingRelease)
	}

	// The empty pending-release sentinel must not release an active turn that
	// also has no identity; such a turn would reply without listening.
	pending := &turn{release: make(chan struct{})}
	a.setActive(pending)
	select {
	case <-pending.release:
		t.Fatal("empty pending release signalled an unidentified turn")
	default:
	}
}

// Acoustic-fixture coverage for the speech band lives at the end of this file.

// The wakeword itself is in the pre-roll window. A low percentile must keep a
// late phrase from raising the floor above the following speech.
func TestSpeechTrackerCalibrationIgnoresLateWord(t *testing.T) {
	tr := &speechTracker{}
	for range 50 {
		tr.calibrate(0.002)
	}
	for range 30 {
		tr.calibrate(0.2) // the word that triggered the activation
	}
	tr.finishCalibration()
	if !tr.armed {
		t.Fatal("calibration did not arm the tracker")
	}
	if gate := math.Max(speechBandFloorMin, tr.floor*speechBandOnset); gate > 0.02 {
		t.Fatalf("late word raised the onset gate to %f", gate)
	}
}

func TestEarlyPrerollExcludesActivationSpeech(t *testing.T) {
	const second = uint64(time.Second)
	item := reply{activationNS: 2 * second}
	cases := []struct {
		name string
		item reply
		ns   uint64
		want bool
	}{
		{"unknown activation time", reply{}, second, true},
		{"unknown chunk time", item, 0, true},
		{"whole second old", item, second, true},
		{"inside the guard", item, 2*second - uint64(300*time.Millisecond), false},
		{"before the guard", item, 2*second - uint64(500*time.Millisecond), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := earlyPreroll(tc.item, &capsula.AudioChunk{MonotonicTimeNS: tc.ns})
			if got != tc.want {
				t.Fatalf("earlyPreroll(%d, %d) = %v, want %v", tc.item.activationNS, tc.ns, got, tc.want)
			}
		})
	}
}

// The old detector classified background by zero-crossing rate, which only
// recognizes broadband noise. A colored room (fan hum, air conditioner,
// low-frequency rumble) reads as low-frequency, so the escape never fired and a
// turn held the microphone until the listen cap. These fixtures reproduce that
// room and the speech over it without recorded audio.

const fixtureRate = 16000

// pcmFromSamples normalizes samples to the requested full-scale RMS and encodes
// 16-bit little-endian mono.
func pcmFromSamples(samples []float64, level float64) []byte {
	sum := 0.0
	for _, s := range samples {
		sum += s * s
	}
	scale := 1.0
	if rms := math.Sqrt(sum / float64(len(samples))); rms > 0 {
		scale = level / rms
	}
	pcm := make([]byte, len(samples)*2)
	for i, s := range samples {
		v := s * scale
		if v > 1 {
			v = 1
		}
		if v < -1 {
			v = -1
		}
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(v*32767)))
	}
	return pcm
}

// roomNoisePCM renders a colored, non-stationary room: a low-frequency harmonic
// hum with slow wobble, shaped hiss and fan buffeting that moves the level by
// several times. Deterministic; no recorded audio.
func roomNoisePCM(seconds int, level float64) []byte {
	frames := seconds * fixtureRate
	samples := make([]float64, frames)
	rng := rand.New(rand.NewPCG(0x9e3779b9, 0x85ebca6b))
	var shaped [3]float64
	for i := range samples {
		t := float64(i) / fixtureRate
		hum := 0.0
		for k, tone := range [][2]float64{{95, 1}, {145, .7}, {205, .5}, {310, .35}, {430, .25}, {620, .15}} {
			wobble := 1 + 0.02*math.Sin(2*math.Pi*0.11*t+float64(k)*1.7)
			hum += tone[1] * math.Sin(2*math.Pi*tone[0]*wobble*t+float64(k)*1.3)
		}
		white := rng.Float64()*2 - 1
		for k := range shaped {
			shaped[k] += (white - shaped[k]) * 0.25
		}
		// A synthetic fan/AC room matched to the authorized capture's band
		// statistics: band/full RMS 0.39, 160 ms band-level relative deviation
		// 0.118 median / 0.180 at the 90th percentile. (That capture is external
		// music, not a fan and not speech-free; only its level statistics are
		// borrowed here, not a noise label.) Fan buffeting moves the low hum,
		// in-band air movement wanders the hiss, and the room breathes slightly.
		am := 1 + 0.28*math.Sin(2*math.Pi*0.17*t) + 0.18*math.Sin(2*math.Pi*3.7*t+1.1)
		wander := 1 + 0.4*math.Sin(2*math.Pi*0.23*t+0.7) + 0.2*math.Sin(2*math.Pi*4.3*t+2.1)
		glob := 1 + 0.25*math.Sin(2*math.Pi*0.31*t+1.9) + 0.15*math.Sin(2*math.Pi*6.7*t)
		// The authorized capture's 99th-percentile deviation came from music
		// from external speakers, not from the fan this fixture models; its
		// p50/p90 (matched above) are the noise profile to preserve.
		samples[i] = (hum*am + shaped[2]*wander) * glob
	}
	return pcmFromSamples(samples, level)
}

// whiteNoisePCM renders flat broadband noise from a seeded generator.
func whiteNoisePCM(seconds int, level float64) []byte {
	samples := make([]float64, seconds*fixtureRate)
	rng := rand.New(rand.NewPCG(0x2545f491, 0x1b873593))
	for i := range samples {
		samples[i] = rng.Float64()*2 - 1
	}
	return pcmFromSamples(samples, level)
}

// speechPCM renders speech-like audio: a glottal harmonic stack with a moving
// spectral tilt, a 4.5 Hz syllable envelope and periodic fricative bursts. It
// matches the way real speech modulates the 300-3400 Hz band; a steady tone is
// deliberately not used, because a tone is indistinguishable from a hiss.
func speechPCM(seconds, level float64) []byte {
	frames := int(seconds * fixtureRate)
	samples := make([]float64, frames)
	rng := rand.New(rand.NewPCG(0x51ed270b, 0xc2b2ae35))
	var phase float64
	for i := range samples {
		t := float64(i) / fixtureRate
		f0 := 118 + 10*math.Sin(2*math.Pi*0.9*t)
		phase += 2 * math.Pi * f0 / fixtureRate
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
	for start := int(0.06 * fixtureRate); start < frames; start += 4000 {
		for j := 0; j < 800 && start+j < frames; j++ {
			samples[start+j] += 0.6 * (rng.Float64()*2 - 1) * float64(800-j) / 800
		}
	}
	return pcmFromSamples(samples, level)
}

// mixPCM adds two PCM streams over their common length, clamping to full scale.
func mixPCM(a, b []byte) []byte {
	n := min(len(a), len(b))
	out := make([]byte, n)
	for i := 0; i+1 < n; i += 2 {
		sum := int32(int16(binary.LittleEndian.Uint16(a[i:]))) +
			int32(int16(binary.LittleEndian.Uint16(b[i:])))
		if sum > math.MaxInt16 {
			sum = math.MaxInt16
		}
		if sum < math.MinInt16 {
			sum = math.MinInt16
		}
		binary.LittleEndian.PutUint16(out[i:], uint16(int16(sum)))
	}
	return out
}

type trackerEvents struct {
	started, ended, rejected int // sample index, -1 when absent
	confirmed                bool
	active                   bool
}

// runTracker calibrates from preroll and then classifies pcm as live audio,
// feeding both in chunks of chunkBytes.
func runTracker(pcm, preroll []byte, chunkBytes int) trackerEvents {
	res := trackerEvents{started: -1, ended: -1, rejected: -1}
	tr := &speechTracker{}
	feed := func(data []byte, mode speechFeed) {
		for offset := 0; offset < len(data); offset += chunkBytes {
			end := min(offset+chunkBytes, len(data))
			if end%2 != 0 {
				end--
			}
			if end <= offset {
				break
			}
			ev := tr.observe(data[offset:end], fixtureRate, mode)
			at := end / 2
			if ev.Started && res.started < 0 {
				res.started = at
			}
			if ev.Ended && res.ended < 0 {
				res.ended = at
				// An end is only produced for a confirmed utterance; capture
				// that before the tracker clears the per-hypothesis state.
				res.confirmed = true
			}
			if ev.Rejected && res.rejected < 0 {
				res.rejected = at
			}
		}
	}
	feed(preroll, speechFeedCalibrate)
	feed(pcm, speechFeedLive)
	res.active = tr.active()
	return res
}

func sampleClock(frames int) time.Duration {
	return time.Duration(frames) * time.Second / fixtureRate
}

// Intended contract: a voice in the 300-3400 Hz band whose level is modulated,
// over a loud colored room that swings in level and returns afterwards, must
// start a turn after 60 ms and end it 700 ms after the voice stops, producing
// exactly one reply. Room noise alone must never become an utterance.
func TestSpeechTrackerEndsAfterSpeechOverColoredRoom(t *testing.T) {
	noise := roomNoisePCM(6, 0.06)
	preroll := noise[:fixtureRate*2]
	speech := speechPCM(1.2, 0.2)
	start := fixtureRate * 2
	body := mixPCM(speech, noise[start:start+len(speech)])
	pcm := append(body, noise[start+len(speech):]...)

	res := runTracker(pcm, preroll, 640)
	if res.started < 0 {
		t.Fatalf("speech over the colored room was not detected: %+v", res)
	}
	if onset := sampleClock(res.started); onset > 300*time.Millisecond {
		t.Fatalf("onset at %s, want within 300 ms", onset)
	}
	if res.rejected >= 0 || res.ended < 0 {
		t.Fatalf("turn did not finish as speech: %+v", res)
	}
	speechEnd := sampleClock(len(speech) / 2)
	if end := sampleClock(res.ended); end < speechEnd+400*time.Millisecond || end > speechEnd+1100*time.Millisecond {
		t.Fatalf("end at %s, want about 700 ms after speech at %s", end, speechEnd)
	}
	if !res.confirmed {
		t.Fatal("the reply decision was not confirmed")
	}
}

// Colored room noise with variable amplitude must not produce a reply, however
// long the turn listens. A rejected hypothesis is allowed; an utterance is not.
func TestSpeechTrackerColoredRoomNoiseProducesNoReply(t *testing.T) {
	noise := roomNoisePCM(8, 0.06)
	res := runTracker(noise[fixtureRate*2:], noise[:fixtureRate*2], 640)
	if res.confirmed || res.ended >= 0 {
		t.Fatalf("colored room noise produced a reply: %+v", res)
	}
}

// A steady hiss or tone that starts mid-turn is never an utterance: its level
// never falls away from a voiced reference, so it cannot produce an end (and
// therefore never a reply). The caller's no-speech window or listen cap bounds
// the hypothesis instead; ending such a level on stationarity alone is what
// would also end a sustained real vowel.
func TestSpeechTrackerSteadyNoiseNeverReplies(t *testing.T) {
	quiet := whiteNoisePCM(2, 0.0015)
	loud := whiteNoisePCM(4, 0.1)
	res := runTracker(loud, quiet, 640)
	if res.ended >= 0 {
		t.Fatalf("steady broadband noise became an utterance: %+v", res)
	}
}

// resonatorSection is a two-pole band-pass used to synthesize formants.
func resonatorSection(rate, freq, bandwidth float64) biquad {
	w0 := 2 * math.Pi * freq / rate
	q := freq / bandwidth
	alpha := math.Sin(w0) / (2 * q)
	cos := math.Cos(w0)
	a0 := 1 + alpha
	return biquad{
		b0: alpha / a0, b1: 0, b2: -alpha / a0,
		a1: -2 * cos / a0, a2: (1 - alpha) / a0,
	}
}

// sustainedVowelPCM renders a continuously voiced vowel: a jittered, vibrato
// glottal pulse train through three fixed formants, plus faint breath noise.
// Unlike speechPCM it has no syllables or consonants, so it tests the case a
// modulation-only endpoint would wrongly cut off.
func sustainedVowelPCM(seconds, level float64) []byte {
	frames := int(seconds * fixtureRate)
	source := make([]float64, frames)
	rng := rand.New(rand.NewPCG(0x2f6b1e3d, 0x9a4c7f11))
	var phase float64
	for i := range source {
		t := float64(i) / fixtureRate
		f0 := 120 * (1 + 0.02*math.Sin(2*math.Pi*5.2*t)) * (1 + 0.003*(rng.Float64()*2-1))
		phase += 2 * math.Pi * f0 / fixtureRate
		sum := 0.0
		for k := 1; k <= 40; k++ {
			if 120*k > 7000 {
				break
			}
			sum += math.Sin(float64(k)*phase) / float64(k)
		}
		source[i] = sum
	}
	samples := make([]float64, frames)
	for n, formant := range [][2]float64{{700, 90}, {1220, 120}, {2600, 180}} {
		section := resonatorSection(fixtureRate, formant[0], formant[1])
		for i, v := range source {
			samples[i] += math.Pow(0.9, float64(n)) * section.process(v)
		}
	}
	attack := fixtureRate * 30 / 1000
	for i := range samples {
		samples[i] += 0.03 * (rng.Float64()*2 - 1)
		if i < attack {
			samples[i] *= float64(i) / float64(attack)
		}
	}
	return pcmFromSamples(samples, level)
}

// With no pre-roll at all, the first sound of the turn must not be swallowed by
// the cold floor. A sustained vowel from the first sample is detected and
// answered; the cold floor stays at the digital-silence value for exactly this
// reason.
func TestSpeechTrackerColdStartVoicedFromFirstSample(t *testing.T) {
	vowel := sustainedVowelPCM(1.2, 0.2)
	quiet := whiteNoisePCM(3, 0.0015)
	res := runTracker(append(append([]byte{}, vowel...), quiet...), nil, 640)
	if res.started < 0 || res.ended < 0 || !res.confirmed {
		t.Fatalf("a cold-start voice was swallowed by its own floor: %+v", res)
	}
}

// A capture gap must not re-learn a floor from a voice already in progress: the
// room estimate survives, and the stream state around it is discarded.
func TestSpeechTrackerResetKeepsTheLearnedFloor(t *testing.T) {
	noise := roomNoisePCM(6, 0.06)
	tr := &speechTracker{}
	feed := func(pcm []byte, mode speechFeed) speechEvents {
		var ev speechEvents
		for offset := 0; offset < len(pcm); offset += 640 {
			end := min(offset+640, len(pcm))
			if end%2 != 0 {
				end--
			}
			e := tr.observe(pcm[offset:end], fixtureRate, mode)
			ev.Started = ev.Started || e.Started
			ev.Ended = ev.Ended || e.Ended
			ev.Rejected = ev.Rejected || e.Rejected
		}
		return ev
	}
	feed(noise[:fixtureRate*2], speechFeedCalibrate)
	feed(noise[fixtureRate*2:fixtureRate*3], speechFeedLive)
	floor := tr.floor
	if floor <= 0 {
		t.Fatal("no floor was learned")
	}
	tr.reset()
	if tr.floor != floor || !tr.armed {
		t.Fatalf("reset dropped the learned floor: got %f armed=%v, want %f", tr.floor, tr.armed, floor)
	}
	speech := speechPCM(1.0, 0.12)
	mixed := mixPCM(speech, noise[fixtureRate*3:fixtureRate*3+len(speech)])
	if ev := feed(mixed, speechFeedLive); !ev.Started {
		t.Fatal("speech after a gap was not detected against the retained floor")
	}
}

// Intended contract: a sustained voiced vowel is an utterance, not room. It must
// stay listening while the voice holds its level and only finish 700 ms after
// the level actually falls away to the room — no premature end, and a reply
// once it does end.
func TestSpeechTrackerKeepsSustainedVoicedSpeech(t *testing.T) {
	noise := roomNoisePCM(6, 0.06)
	preroll := noise[:fixtureRate*2]
	vowel := sustainedVowelPCM(1.6, 0.2)
	start := fixtureRate * 2
	body := mixPCM(vowel, noise[start:start+len(vowel)])
	pcm := append(body, noise[start+len(vowel):]...)
	res := runTracker(pcm, preroll, 640)
	if res.started < 0 || res.ended < 0 || !res.confirmed {
		t.Fatalf("sustained vowel over the room was not answered: %+v", res)
	}
	vowelEnd := sampleClock(len(vowel) / 2)
	if end := sampleClock(res.ended); end < vowelEnd+400*time.Millisecond {
		t.Fatalf("sustained vowel ended at %s, before it finished at %s: %+v", end, vowelEnd, res)
	}

	// A vowel that ends into a quiet room is answered the same way.
	quiet := whiteNoisePCM(4, 0.0015)
	res = runTracker(append(append([]byte{}, vowel...), quiet...), quiet, 640)
	if res.started < 0 || res.ended < 0 || !res.confirmed {
		t.Fatalf("sustained vowel followed by silence was not answered: %+v", res)
	}

	// A voice that gets louder without articulating is still the voice: it must
	// start, confirm, and finish only when the room returns — never early.
	soft := sustainedVowelPCM(0.8, 0.12)
	loud := sustainedVowelPCM(0.8, 0.25)
	pcm = append(append(append([]byte{}, soft...), loud...), noise[start:]...)
	res = runTracker(pcm, preroll, 640)
	voiceEnd := sampleClock(len(soft)/2 + len(loud)/2)
	if res.started < 0 {
		t.Fatalf("louder sustained voice was not detected: %+v", res)
	}
	if res.ended < 0 || !res.confirmed {
		t.Fatalf("louder sustained voice did not confirm and complete: %+v", res)
	}
	if end := sampleClock(res.ended); end < voiceEnd+400*time.Millisecond {
		t.Fatalf("louder sustained voice ended early at %s, voice ends at %s: %+v", end, voiceEnd, res)
	}
}

// Documented limitation, pinned deliberately: a perfectly steady tone or hiss
// carries no amplitude modulation and no formant movement, which this tap cannot
// separate from a sustained voiced vowel. A tone that starts mid-turn (or a cold
// turn with no pre-roll) and later stops is therefore answered once its level
// falls, exactly like a held vowel. A fan/air-conditioner room (buffeted, see
// TestSpeechTrackerMidTurnRoomNeverReplies) is never answered. This is a
// limitation, not a claim of separation.
func TestSpeechTrackerSteadyToneMidTurnIsALimitation(t *testing.T) {
	noise := roomNoisePCM(6, 0.06)
	preroll := noise[:fixtureRate*2]
	tone := make([]byte, fixtureRate*2) // 1 s at 430 Hz
	for i := 0; i < fixtureRate; i++ {
		v := int16(0.15 * 32767 * math.Sin(2*math.Pi*430*float64(i)/fixtureRate))
		binary.LittleEndian.PutUint16(tone[i*2:], uint16(v))
	}
	body := mixPCM(tone, noise[fixtureRate*2:fixtureRate*2+len(tone)])
	pcm := append(body, noise[fixtureRate*2+len(tone):]...)
	res := runTracker(pcm, preroll, 640)
	if !res.confirmed || res.ended < 0 {
		t.Fatalf("steady-tone limitation changed: expected it to be answered, got %+v", res)
	}
}

// composedWordPCM renders a short, low-SNR voiced word: a 60 ms articulated
// onset, a steady voiced vowel, one 60 ms articulated burst, then a steady
// vowel. Its total modulated time is ~120 ms (below the 140 ms articulation
// budget used before the fix), while its voiced time is ~340 ms. It is the
// public oracle for confirmation-on-voiced-time: modulation-only confirmation
// rejects it, the settled logic answers it.
func composedWordPCM() []byte {
	var out []byte
	out = append(out, speechPCM(0.06, 0.2)...)
	out = append(out, sustainedVowelPCM(0.1, 0.15)...)
	out = append(out, speechPCM(0.06, 0.2)...)
	out = append(out, sustainedVowelPCM(0.12, 0.15)...)
	return out
}

// A quiet compposed word over a synthetic fan room must be answered. The
// synthetic room has the authorized capture's band/full ratio and 160 ms band
// deviation (p50/p90); it is a fan model, not the external-music capture.
func TestSpeechTrackerQuietComposedWordOverFanRoom(t *testing.T) {
	noise := roomNoisePCM(6, 0.06)
	preroll := noise[:fixtureRate*2]
	word := composedWordPCM()
	body := mixPCM(word, noise[fixtureRate*2:fixtureRate*2+len(word)])
	pcm := append(body, noise[fixtureRate*2+len(word):]...)
	res := runTracker(pcm, preroll, 640)
	if res.started < 0 || res.ended < 0 || !res.confirmed {
		t.Fatalf("a quiet composed word over the fan room was not answered: %+v", res)
	}
}

// A quiet word over the synthetic fan room must still be answered at a level
// well above the room; this is the level reference, not the discriminating
// oracle (see TestSpeechTrackerQuietComposedWordOverFanRoom).
func TestSpeechTrackerQuietWordOverColoredRoom(t *testing.T) {
	noise := roomNoisePCM(6, 0.06)
	preroll := noise[:fixtureRate*2]
	speech := speechPCM(1.0, 0.12)
	body := mixPCM(speech, noise[fixtureRate*2:fixtureRate*2+len(speech)])
	pcm := append(body, noise[fixtureRate*2+len(speech):]...)
	res := runTracker(pcm, preroll, 640)
	if res.started < 0 || res.ended < 0 || !res.confirmed {
		t.Fatalf("a quiet word over the room was not answered: %+v", res)
	}
}

// A fan/AC room that starts after the turn is already listening must not
// produce a reply. It has no sustained fall away from its own level, so it is
// bounded by the caller's no-speech window or listen cap, never answered.
func TestSpeechTrackerMidTurnRoomNeverReplies(t *testing.T) {
	quiet := whiteNoisePCM(2, 0.0015)
	room := roomNoisePCM(6, 0.06)
	res := runTracker(room, quiet, 640)
	if res.ended >= 0 {
		t.Fatalf("room noise starting mid-turn produced a reply: %+v", res)
	}
}

// A room stream with brief silent splices models the dropped/silent audio a
// slow consumer receives. It must never be confirmed as an utterance: splicing
// lowers the learned floor, so a loud room sits far above it.
func TestSpeechTrackerSilenceSplicedRoomDoesNotReply(t *testing.T) {
	room := roomNoisePCM(5, 0.06)
	block := fixtureRate / 5 * 2 // 200 ms
	silence := make([]byte, fixtureRate*60/1000*2)
	var spliced []byte
	for i := 0; i < len(room); i += block {
		spliced = append(spliced, room[i:min(i+block, len(room))]...)
		spliced = append(spliced, silence...)
	}
	res := runTracker(spliced, nil, 640)
	if res.ended >= 0 {
		t.Fatalf("silence-spliced room became an utterance: %+v", res)
	}
}

// A short unmodulated loud event in the room (a bump, a door, a knock) must not
// become an utterance, while a sustained voiced vowel must (covered above).
func TestSpeechTrackerRoomBumpIsNotAnUtterance(t *testing.T) {
	noise := roomNoisePCM(6, 0.06)
	preroll := noise[:fixtureRate*2]
	for _, duration := range []float64{0.15, 0.25} {
		bump := whiteNoisePCM(1, 0.3)[:int(duration*fixtureRate)*2]
		start := fixtureRate * 2
		body := mixPCM(bump, noise[start:start+len(bump)])
		pcm := append(body, noise[start+len(bump):]...)
		res := runTracker(pcm, preroll, 640)
		if res.ended >= 0 {
			t.Fatalf("a %.0f ms room bump became an utterance: %+v", duration*1000, res)
		}
		// The bump opens a hypothesis and the classifier rejects it: assert the
		// rejection path itself, not only the absence of a reply.
		if res.rejected < 0 {
			t.Fatalf("a %.0f ms room bump was not rejected as noise: %+v", duration*1000, res)
		}
	}
}

// Quiet speech and a short word must still be answered; a sub-minSpeech blip
// must not.
func TestSpeechTrackerRetainsQuietAndShortSpeech(t *testing.T) {
	quiet := whiteNoisePCM(4, 0.0015)
	speech := speechPCM(0.3, 0.05)
	pcm := append(mixPCM(speech, quiet[:len(speech)]), quiet...)
	res := runTracker(pcm, quiet, 640)
	if res.started < 0 || res.ended < 0 || !res.confirmed {
		t.Fatalf("quiet 300 ms speech was not answered: %+v", res)
	}

	blip := speechPCM(0.12, 0.12)
	pcm = append(mixPCM(blip, quiet[:len(blip)]), quiet...)
	res = runTracker(pcm, quiet, 640)
	if res.ended >= 0 || res.confirmed {
		t.Fatalf("a 120 ms blip became an utterance: %+v", res)
	}
}

// Pre-activation audio establishes the room, but the first live chunk is
// already speech: calibration must not absorb it.
func TestSpeechTrackerKeepsSpeechFromTheFirstLiveChunk(t *testing.T) {
	quiet := whiteNoisePCM(3, 0.0015)
	speech := speechPCM(0.6, 0.12)
	res := runTracker(append(append([]byte{}, speech...), quiet...), quiet, 640)
	if res.started < 0 || res.ended < 0 || !res.confirmed {
		t.Fatalf("initial speech was calibrated away: %+v", res)
	}
}

// Without any pre-roll the cold floor stays at the digital-silence value and a
// level-only onset is allowed once, so a voice already in progress is not
// swallowed. A cold loud room may open a hypothesis and stay in it (bounded by
// the caller's cap); it must never produce a reply, including through its
// transients.
func TestSpeechTrackerColdStartRoomDoesNotReply(t *testing.T) {
	noise := roomNoisePCM(6, 0.06)
	res := runTracker(noise, nil, 640)
	if res.ended >= 0 {
		t.Fatalf("cold-start room noise produced a reply: %+v", res)
	}

	quiet := whiteNoisePCM(3, 0.0015)
	speech := speechPCM(0.8, 0.15)
	pcm := append(append([]byte{}, quiet[:fixtureRate]...), speech...)
	pcm = append(pcm, quiet...)
	res = runTracker(pcm, nil, 640)
	if res.started < 0 || res.ended < 0 {
		t.Fatalf("cold-start speech was not detected: %+v", res)
	}
}

// The decision is integrated in sample time over fixed analysis frames, so the
// transport's chunk size cannot move the onset or the end by more than one
// chunk.
func TestSpeechTrackerChunkSizeDoesNotChangeDecisions(t *testing.T) {
	noise := roomNoisePCM(6, 0.06)
	preroll := noise[:fixtureRate*2]
	speech := speechPCM(1.2, 0.2)
	body := mixPCM(speech, noise[fixtureRate*2:fixtureRate*2+len(speech)])
	pcm := append(body, noise[fixtureRate*2+len(speech):]...)

	ref := runTracker(pcm, preroll, 640)
	for _, chunk := range []int{160, 320, 640, 1280, 2560} {
		got := runTracker(pcm, preroll, chunk)
		if (got.started >= 0) != (ref.started >= 0) || (got.ended >= 0) != (ref.ended >= 0) {
			t.Fatalf("chunk %d changed the decision sequence: %+v vs %+v", chunk, got, ref)
		}
		// A decision is attributed to the chunk that completed its analysis
		// frame, so the reported time may trail by up to one chunk.
		tolerance := sampleClock(chunk / 2)
		for _, pair := range [][2]int{{got.started, ref.started}, {got.ended, ref.ended}} {
			if pair[0] < 0 {
				continue
			}
			if diff := sampleClock(pair[0] - pair[1]); diff < -tolerance || diff > tolerance {
				t.Fatalf("chunk %d moved a decision by %s: %+v vs %+v", chunk, diff, got, ref)
			}
		}
	}
}

// Genuinely continuous speech has no 700 ms unmodulated tail, so the tracker
// stays in the hypothesis and the caller's listen cap decides.
func TestSpeechTrackerContinuousSpeechHoldsTheTurn(t *testing.T) {
	quiet := whiteNoisePCM(2, 0.0015)
	res := runTracker(speechPCM(11, 0.2), quiet, 640)
	if res.ended >= 0 || res.rejected >= 0 {
		t.Fatalf("continuous speech auto-ended: %+v", res)
	}
	if !res.active {
		t.Fatal("continuous speech left the tracker inactive")
	}
}

// A dismiss action cancels the active turn and drops activations that were
// queued behind it, so a stale request cannot be answered after the user has
// dismissed the assistant.
func TestDismissCancelsActiveTurnAndQueuedActivations(t *testing.T) {
	a := &assistant{queue: make(chan reply, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.active = &turn{activationID: "act_1", release: make(chan struct{}), cancel: cancel}
	a.queue <- reply{activationID: "act_2"}

	a.dismiss()

	select {
	case <-ctx.Done():
	default:
		t.Fatal("dismiss did not cancel the active turn")
	}
	if got := len(a.queue); got != 0 {
		t.Fatalf("%d queued activations survived dismissal", got)
	}

	// Dismissing with no active turn is a no-op, not a panic.
	a.active = nil
	a.dismiss()
}
