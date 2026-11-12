package main

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"path/filepath"
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

func TestAssetPath(t *testing.T) {
	got, err := assetPath("/app/dumbassistant")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/app/assets/reply.wav"; got != want {
		t.Fatalf("asset path = %q, want %q", got, want)
	}
	rel, err := assetPath("dumbassistant")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(rel) || filepath.Base(rel) != "reply.wav" {
		t.Fatalf("relative argv path resolved to %q", rel)
	}
}

func TestApplySettingsClampsAndIgnoresInvalid(t *testing.T) {
	a := &assistant{settings: defaultDelaySettings()}
	a.applySettings(&capsula.SettingsChanged{
		Revision: 7,
		Values: []capsula.SettingValue{
			{Key: "delay_mode", ValueJSON: `"random"`},
			{Key: "delay_ms", ValueJSON: `-10`},
			{Key: "delay_min_ms", ValueJSON: `-1`},
			{Key: "delay_max_ms", ValueJSON: `120000`},
		},
	})
	if a.settings.mode != "random" {
		t.Fatalf("mode = %q", a.settings.mode)
	}
	if a.settings.constantMS != 0 || a.settings.minMS != 0 || a.settings.maxMS != maxDelayMS {
		t.Fatalf("clamped settings = %+v", a.settings)
	}

	a.applySettings(&capsula.SettingsChanged{
		Revision: 8,
		Values:   []capsula.SettingValue{{Key: "delay_mode", ValueJSON: `"bogus"`}},
	})
	if a.settings.mode != "random" {
		t.Fatalf("invalid mode replaced mode: %q", a.settings.mode)
	}
}

func TestDelaySettings(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	constant := delaySettings{mode: "constant", constantMS: 1234}
	if got := constant.delay(rng); got != 1234*time.Millisecond {
		t.Fatalf("constant delay = %s", got)
	}

	random := delaySettings{mode: "random", minMS: 500, maxMS: 2500}
	for i := 0; i < 1000; i++ {
		got := int64(random.delay(rng) / time.Millisecond)
		if got < 500 || got > 2500 {
			t.Fatalf("random delay %d ms outside 500..2500", got)
		}
	}

	swapped := delaySettings{mode: "random", minMS: 2500, maxMS: 500}
	for i := 0; i < 100; i++ {
		got := int64(swapped.delay(rng) / time.Millisecond)
		if got < 500 || got > 2500 {
			t.Fatalf("swapped random delay %d ms outside 500..2500", got)
		}
	}

	equal := delaySettings{mode: "random", minMS: 700, maxMS: 700}
	if got := equal.delay(rng); got != 700*time.Millisecond {
		t.Fatalf("equal bounds delay = %s", got)
	}
}
