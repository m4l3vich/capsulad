package main

import (
	"bytes"
	"context"
	"encoding/binary"
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

func TestSpeechTracker(t *testing.T) {
	tr := &speechTracker{}
	if started, ended := tr.feed(0.001, 10*time.Millisecond); started || ended || tr.active() {
		t.Fatal("silence started speech")
	}
	if started, _ := tr.feed(speechOnRMS+0.01, 10*time.Millisecond); started {
		t.Fatal("one loud chunk started speech")
	}
	if started, _ := tr.feed(speechOnRMS+0.01, 10*time.Millisecond); !started || !tr.active() {
		t.Fatal("sustained energy did not start speech")
	}
	for range 20 {
		tr.feed(speechOnRMS+0.01, 10*time.Millisecond)
	}

	ended := false
	for range 80 {
		var e bool
		_, e = tr.feed(0.001, 10*time.Millisecond)
		if e {
			ended = true
			break
		}
	}
	if !ended {
		t.Fatal("silence did not end speech")
	}

	blip := &speechTracker{}
	blip.feed(speechOnRMS+0.01, 10*time.Millisecond)
	blip.feed(speechOnRMS+0.01, 10*time.Millisecond)
	for range 80 {
		if _, e := blip.feed(0.001, 10*time.Millisecond); e {
			break
		}
	}
	if blip.active() {
		t.Fatal("short blip left the tracker active")
	}
	if started, _ := blip.feed(speechOnRMS+0.01, 10*time.Millisecond); started {
		t.Fatal("blip restarted speech on one chunk")
	}
	if started, _ := blip.feed(speechOnRMS+0.01, 10*time.Millisecond); !started {
		t.Fatal("speech did not restart after a rejected blip")
	}

	long := &speechTracker{}
	long.feed(speechOnRMS+0.01, 10*time.Millisecond)
	long.feed(speechOnRMS+0.01, 10*time.Millisecond)
	for range 1100 {
		if _, ended := long.feed(speechOnRMS+0.01, 10*time.Millisecond); ended {
			t.Fatal("continuous speech auto-ended; the caller's listen cap must cancel instead")
		}
	}
	if !long.active() {
		t.Fatal("continuous speech left the tracker inactive")
	}
}

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

// A steady room noise floor (a fan, a fridge, traffic) must not be mistaken for
// speech, and speech above it must still start and end. The old fixed
// thresholds either never ended the turn or let the background keep it open
// until the listen cap cancelled it.
func TestSpeechTrackerAdaptsToRoomNoise(t *testing.T) {
	tr := &speechTracker{}
	for range 50 {
		if started, _ := tr.feed(0.03, 20*time.Millisecond); started {
			t.Fatal("room noise started speech")
		}
	}
	started := false
	for range 20 {
		if s, _ := tr.feed(0.18, 20*time.Millisecond); s {
			started = true
		}
	}
	if !started {
		t.Fatal("speech above the room noise was not detected")
	}
	ended := false
	for range 100 {
		if _, e := tr.feed(0.03, 20*time.Millisecond); e {
			ended = true
			break
		}
	}
	if !ended {
		t.Fatal("speech tail against the same room noise did not end the turn")
	}
	if tr.active() {
		t.Fatal("tracker stayed active after the utterance")
	}

	// A quiet room keeps the absolute floors: a level that is well above the
	// background but still quiet is not speech.
	quiet := &speechTracker{}
	for range 50 {
		quiet.feed(0.001, 20*time.Millisecond)
	}
	on, _ := quiet.thresholds()
	if on != speechOnRMS {
		t.Fatalf("quiet-room onset threshold = %f, want %f", on, speechOnRMS)
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

// Pre-activation audio characterizes the room before any speech window is
// classified, so a loud steady background is not mistaken for an utterance.
func TestSpeechTrackerPreRollCalibratesLoudRoom(t *testing.T) {
	tr := &speechTracker{}
	for range 20 {
		tr.calibrate(0.06)
	}
	on, off := tr.thresholds()
	if on != noiseFloorMax*speechNoiseOn || off != noiseFloorMax*speechNoiseOff {
		t.Fatalf("calibrated thresholds on=%f off=%f", on, off)
	}
	for range 50 {
		if started, _ := tr.feed(0.06, 20*time.Millisecond); started {
			t.Fatal("calibrated room noise started speech")
		}
	}
	started := false
	for range 20 {
		if s, _ := tr.feed(0.2, 20*time.Millisecond); s {
			started = true
		}
	}
	if !started {
		t.Fatal("speech above the calibrated room noise was not detected")
	}
	ended := false
	for range 100 {
		if _, e := tr.feed(0.06, 20*time.Millisecond); e {
			ended = true
			break
		}
	}
	if !ended {
		t.Fatal("speech did not end against the calibrated room noise")
	}
}
