package main

import (
	"bytes"
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
