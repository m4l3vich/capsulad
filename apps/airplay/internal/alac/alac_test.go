package alac

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// fixture describes one generated test vector: fixtures are produced by
// testdata/gen.sh, which uses ffmpeg to encode PCM into ALAC-in-MP4 and
// extracts the individual ALAC frames with testdata/gen_frames.py.
type fixture struct {
	name       string
	frameLen   uint32
	bitDepth   uint8
	channels   uint8
	sampleRate uint32
}

var fixtures = []fixture{
	{"alac-stereo44100-16", 4096, 16, 2, 44100},
	{"alac-mono48000-16", 4096, 16, 1, 48000},
	{"alac-stereo44100-24", 4096, 24, 2, 44100},
	{"alac-mono48000-16-verbatim", 4096, 16, 1, 48000},
	{"alac-mono44100-16-silence", 4096, 16, 1, 44100},
}

// container is the parsed testdata/<name>.frames.bin file.
type container struct {
	cookie []byte
	frames [][]byte
}

func loadContainer(t *testing.T, name string) container {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".frames.bin"))
	if err != nil {
		t.Fatalf("reading frames: %v", err)
	}
	if len(data) < 20 || string(data[:8]) != "ALACFRMS" {
		t.Fatalf("%s: not an ALACFRMS container", name)
	}
	if v := binary.BigEndian.Uint32(data[8:12]); v != 1 {
		t.Fatalf("%s: unsupported container version %d", name, v)
	}
	count := int(binary.BigEndian.Uint32(data[12:16]))
	cookieLen := int(binary.BigEndian.Uint32(data[16:20]))
	if len(data) < 20+cookieLen {
		t.Fatalf("%s: truncated container header", name)
	}
	c := container{cookie: data[20 : 20+cookieLen]}
	off := 20 + cookieLen
	for i := 0; i < count; i++ {
		if off+4 > len(data) {
			t.Fatalf("%s: truncated frame %d", name, i)
		}
		n := int(binary.BigEndian.Uint32(data[off : off+4]))
		off += 4
		if off+n > len(data) {
			t.Fatalf("%s: frame %d overruns the container", name, i)
		}
		c.frames = append(c.frames, data[off:off+n])
		off += n
	}
	if off != len(data) {
		t.Fatalf("%s: %d trailing bytes", name, len(data)-off)
	}
	return c
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

// decodeAll decodes every frame of a fixture and returns the concatenated PCM.
func decodeAll(t *testing.T, d *Decoder, c container) []byte {
	t.Helper()
	var out []byte
	for i, frame := range c.frames {
		var err error
		before := len(out)
		out, err = d.Decode(frame, out)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if (len(out)-before)%(2*int(d.channels)) != 0 {
			t.Fatalf("frame %d: %d PCM bytes is not a whole number of samples", i, len(out)-before)
		}
	}
	return out
}

// samples16 interprets b as interleaved 16-bit little-endian PCM.
func samples16(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return out
}

// samples24to16 interprets b as interleaved 24-bit little-endian PCM and keeps
// the top 16 bits of every sample, which is what Decode outputs for 24-bit
// input.
func samples24to16(b []byte) []int16 {
	out := make([]int16, len(b)/3)
	for i := range out {
		v := int32(b[3*i]) | int32(b[3*i+1])<<8 | int32(b[3*i+2])<<16
		v = v << 8 >> 8 // sign extend from 24 bits
		out[i] = int16(v >> 8)
	}
	return out
}

func compareSamples(t *testing.T, got, want []int16) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("sample count: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d: got %d, want %d", i, got[i], want[i])
		}
	}
}

// TestDecodeFixtures decodes every extracted frame and compares the result
// bit-exactly with the PCM ffmpeg decodes from the same file.
func TestDecodeFixtures(t *testing.T) {
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			c := loadContainer(t, f.name)
			raw := readFile(t, filepath.Join("testdata", f.name+".raw"))

			cfg := Config{
				FrameLength: f.frameLen,
				BitDepth:    f.bitDepth,
				Channels:    f.channels,
				SampleRate:  f.sampleRate,
				MagicCookie: c.cookie,
			}
			dec, err := NewDecoder(cfg)
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			if got := dec.FrameSamples(); got != int(f.frameLen) {
				t.Fatalf("FrameSamples() = %d, want %d", got, f.frameLen)
			}

			var got []int16
			pcm := decodeAll(t, dec, c)
			var want []int16
			switch f.bitDepth {
			case 16:
				got, want = samples16(pcm), samples16(raw)
			case 24:
				got, want = samples16(pcm), samples24to16(raw)
			default:
				t.Fatalf("no reference conversion for bit depth %d", f.bitDepth)
			}
			compareSamples(t, got, want)
			if len(got) == 0 {
				t.Fatal("decoded no samples")
			}

			// The same stream must decode identically when the decoder
			// builds the standard cookie itself, which proves the extracted
			// cookie's tuning parameters are the standard ones.
			cfg.MagicCookie = nil
			dec2, err := NewDecoder(cfg)
			if err != nil {
				t.Fatalf("NewDecoder without cookie: %v", err)
			}
			if pcm2 := decodeAll(t, dec2, c); !bytes.Equal(pcm, pcm2) {
				t.Error("decoding differs depending on the magic cookie")
			}
		})
	}
}

// TestDecodeAllocatesNothing checks that decoding reuses the decoder's buffers
// and never allocates as long as the destination slice has room.
func TestDecodeAllocatesNothing(t *testing.T) {
	c := loadContainer(t, "alac-stereo44100-16")
	dec, err := NewDecoder(Config{FrameLength: 4096, BitDepth: 16, Channels: 2, SampleRate: 44100, MagicCookie: c.cookie})
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, 0, 4096*2*2)
	frame := c.frames[0]
	allocs := testing.AllocsPerRun(20, func() {
		if _, err := dec.Decode(frame, dst[:0]); err != nil {
			t.Fatalf("Decode: %v", err)
		}
	})
	if allocs != 0 {
		t.Errorf("Decode allocated %v times per frame, want 0", allocs)
	}
}

func TestNewDecoderRejectsBadConfig(t *testing.T) {
	valid := Config{FrameLength: 352, BitDepth: 16, Channels: 2, SampleRate: 44100}
	if _, err := NewDecoder(valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	bad := []struct {
		name string
		cfg  Config
	}{
		{"bit depth", Config{FrameLength: 4096, BitDepth: 8, Channels: 2, SampleRate: 44100}},
		{"channels", Config{FrameLength: 4096, BitDepth: 16, Channels: 3, SampleRate: 44100}},
		{"sample rate", Config{FrameLength: 4096, BitDepth: 16, Channels: 2}},
		{"frame length", Config{FrameLength: 0, BitDepth: 16, Channels: 2, SampleRate: 44100}},
		{"huge frame length", Config{FrameLength: maxFrameSamples + 1, BitDepth: 16, Channels: 2, SampleRate: 44100}},
		{"short cookie", Config{FrameLength: 4096, BitDepth: 16, Channels: 2, SampleRate: 44100, MagicCookie: make([]byte, 10)}},
	}
	for _, tc := range bad {
		if _, err := NewDecoder(tc.cfg); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}

func TestRealtimeFrameLength(t *testing.T) {
	dec, err := NewDecoder(Config{FrameLength: 352, BitDepth: 16, Channels: 2, SampleRate: 44100})
	if err != nil {
		t.Fatal(err)
	}
	if got := dec.FrameSamples(); got != 352 {
		t.Fatalf("FrameSamples() = %d, want 352", got)
	}
}

// TestCookieLayouts checks that the bare 24-byte ALACSpecificConfig and the
// 28/36-byte wrappers used by MP4 and by ffmpeg's codec extradata are all
// understood.
func TestCookieLayouts(t *testing.T) {
	c := loadContainer(t, "alac-stereo44100-16")
	if len(c.cookie) != 28 {
		t.Fatalf("fixture cookie is %d bytes, expected 28", len(c.cookie))
	}
	bare := c.cookie[4:]
	prefixed := []byte{0, 0, 0, byte(len(bare) + 12), 'a', 'l', 'a', 'c', 0, 0, 0, 0}
	prefixed = append(prefixed, bare...)

	cfg := Config{FrameLength: 4096, BitDepth: 16, Channels: 2, SampleRate: 44100}

	var want []byte
	for i, cookie := range [][]byte{bare, c.cookie, prefixed} {
		cfg.MagicCookie = cookie
		dec, err := NewDecoder(cfg)
		if err != nil {
			t.Fatalf("cookie %d (%d bytes): %v", i, len(cookie), err)
		}
		pcm := decodeAll(t, dec, c)
		if i == 0 {
			want = pcm
			continue
		}
		if !bytes.Equal(pcm, want) {
			t.Errorf("cookie %d (%d bytes) decodes differently", i, len(cookie))
		}
	}

	// A cookie that does not describe the configured stream must be refused.
	cfg.MagicCookie = c.cookie
	cfg.SampleRate = 48000
	if _, err := NewDecoder(cfg); err == nil {
		t.Error("mismatched sample rate accepted")
	}
	cfg.SampleRate = 44100
	cfg.BitDepth = 24
	if _, err := NewDecoder(cfg); err == nil {
		t.Error("mismatched bit depth accepted")
	}
	cfg.BitDepth = 16
	cfg.FrameLength = 352
	if _, err := NewDecoder(cfg); err == nil {
		t.Error("mismatched frame length accepted")
	}

	// A corrupt extradata prefix must be refused.
	bad := append([]byte{}, prefixed...)
	copy(bad[4:8], "xxxx")
	cfg.FrameLength = 4096
	cfg.MagicCookie = bad
	if _, err := NewDecoder(cfg); err == nil {
		t.Error("extradata with a bad 'alac' tag accepted")
	}
}

// testBitWriter builds bitstreams MSB first for the hand written frames below.
type testBitWriter struct {
	buf []byte
	bit uint
}

func (w *testBitWriter) putSigned(v int32, n uint) {
	w.put(uint32(v)&(uint32(1)<<n-1), n)
}

func (w *testBitWriter) put(v uint32, n uint) {
	for i := int(n) - 1; i >= 0; i-- {
		if w.bit == 0 {
			w.buf = append(w.buf, 0)
		}
		w.buf[len(w.buf)-1] |= byte((v>>uint(i))&1) << (7 - w.bit)
		w.bit = (w.bit + 1) & 7
	}
}

// verbatimElement writes a channel element header for an uncompressed element.
func (w *testBitWriter) verbatimElement(tag, instance uint32, samples uint32, sendSize bool) {
	w.put(tag, 3)
	w.put(instance, 4)
	w.put(0, 12)
	if sendSize {
		w.put(1, 1)
	} else {
		w.put(0, 1)
	}
	w.put(0, 2) // no extra bytes
	w.put(1, 1) // verbatim
	if sendSize {
		w.put(samples, 32)
	}
}

func TestDecodeVerbatimStereo(t *testing.T) {
	// A hand built CPE with two uncompressed samples per channel.
	w := &testBitWriter{}
	w.verbatimElement(elementCPE, 0, 2, true)
	for _, s := range []int32{100, 200, -100, -200} {
		w.putSigned(s, 16)
	}
	w.put(elementEND, 3)

	dec, err := NewDecoder(Config{FrameLength: 4096, BitDepth: 16, Channels: 2, SampleRate: 44100})
	if err != nil {
		t.Fatal(err)
	}
	out, err := dec.Decode(w.buf, nil)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want := []int16{100, 200, -100, -200}
	if got := samples16(out); len(got) != 4 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDecodeVerbatimMonoDownmix(t *testing.T) {
	// A stereo element decoded by a mono decoder emits the left channel.
	w := &testBitWriter{}
	w.verbatimElement(elementCPE, 0, 1, true)
	w.putSigned(7, 16)
	w.putSigned(-9, 16)
	w.put(elementEND, 3)

	dec, err := NewDecoder(Config{FrameLength: 4096, BitDepth: 16, Channels: 1, SampleRate: 44100})
	if err != nil {
		t.Fatal(err)
	}
	out, err := dec.Decode(w.buf, nil)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := samples16(out); len(got) != 1 || got[0] != 7 {
		t.Fatalf("got %v, want [7]", got)
	}
}

func TestDecodeRejectsUnsupportedFrames(t *testing.T) {
	dec, err := NewDecoder(Config{FrameLength: 4096, BitDepth: 16, Channels: 2, SampleRate: 44100})
	if err != nil {
		t.Fatal(err)
	}

	// Element tag 2 (coupling channel element) is not implemented.
	unsupported := &testBitWriter{}
	unsupported.put(2, 3)
	unsupported.put(0, 16)

	// Two channel elements in one frame: more channels than the decoder has.
	multiple := &testBitWriter{}
	multiple.verbatimElement(elementSCE, 0, 1, true)
	multiple.put(uint32(1), 16)
	multiple.put(elementSCE, 3)

	// A frame that declares more samples than the cookie allows.
	tooMany := &testBitWriter{}
	tooMany.verbatimElement(elementSCE, 0, 4097, true)
	tooMany.put(elementEND, 3)

	// An END tag with no element before it.
	empty := &testBitWriter{}
	empty.put(elementEND, 3)

	cases := []struct {
		name  string
		frame []byte
	}{
		{"element tag", unsupported.buf},
		{"multiple elements", multiple.buf},
		{"too many samples", tooMany.buf},
		{"no element", empty.buf},
		{"nil frame", nil},
		{"truncated", []byte{0x00}},
		{"truncated mid element", multiple.buf[:2]},
	}
	for _, tc := range cases {
		if _, err := dec.Decode(tc.frame, nil); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}

// TestDecodeDoesNotEmitSilenceOnError checks that a failing Decode reports the
// error instead of appending samples for the part it understood.
func TestDecodeDoesNotEmitSilenceOnError(t *testing.T) {
	dec, err := NewDecoder(Config{FrameLength: 4096, BitDepth: 16, Channels: 1, SampleRate: 44100})
	if err != nil {
		t.Fatal(err)
	}
	bad := []byte{0x00, 0x00, 0x00, 0x00} // SCE tag then nothing
	if out, err := dec.Decode(bad, nil); err == nil || len(out) != 0 {
		t.Fatalf("Decode = (%d bytes, %v), want an error and no output", len(out), err)
	}
	// A frame whose first element decodes and whose second element is not
	// implemented must not leave the first element's PCM behind.
	w := &testBitWriter{}
	w.verbatimElement(elementSCE, 0, 1, true)
	w.putSigned(1234, 16)
	w.put(elementSCE, 3)
	prefix := []byte{1, 2, 3}
	out, err := dec.Decode(w.buf, prefix)
	if err == nil {
		t.Fatal("a frame with two elements was accepted")
	}
	if len(out) != len(prefix) || out[len(out)-1] != 3 {
		t.Fatalf("Decode left %d bytes behind after an error", len(out)-len(prefix))
	}
}

// frameHeader is the part of an ALAC element header the coverage test below
// inspects.
type frameHeader struct {
	tag          uint32
	samples      int
	extraBits    int
	verbatim     bool
	decorrShift  uint32
	decorrWeight uint32
	predType     [2]uint32
	order        [2]int
}

func parseFrameHeader(t *testing.T, frame []byte, frameLength int) frameHeader {
	t.Helper()
	br := bitReader{data: frame}
	h := frameHeader{tag: br.bits(3), samples: frameLength}
	var channels int
	switch h.tag {
	case elementSCE, elementLFE:
		channels = 1
	case elementCPE:
		channels = 2
	default:
		t.Fatalf("unexpected element tag %d", h.tag)
	}
	br.bits(4)
	br.bits(12)
	hasSize := br.bits(1) != 0
	h.extraBits = int(br.bits(2)) * 8
	h.verbatim = br.bits(1) != 0
	if hasSize {
		h.samples = int(br.bits(32))
	}
	if h.verbatim {
		return h
	}
	h.decorrShift = br.bits(8)
	h.decorrWeight = br.bits(8)
	for ch := 0; ch < channels; ch++ {
		h.predType[ch] = br.bits(4)
		br.bits(4) // quantization
		br.bits(3) // Rice modifier
		h.order[ch] = int(br.bits(5))
		for i := 0; i < h.order[ch]; i++ {
			br.bits(16)
		}
	}
	if err := br.err(); err != nil {
		t.Fatalf("frame header is truncated: %v", err)
	}
	return h
}

// TestFixturesCoverDecoderPaths checks that the checked in fixtures really do
// exercise the decoder paths they are named after, so that passing tests mean
// something: stereo decorrelation, the extra bits of 24-bit samples, verbatim
// elements and the run limited zero blocks of the Rice coder.
func TestFixturesCoverDecoderPaths(t *testing.T) {
	for _, f := range fixtures {
		c := loadContainer(t, f.name)
		if len(c.frames) < 2 {
			t.Errorf("%s has %d frames, want at least 2", f.name, len(c.frames))
		}
		var partial, decorrelated, verbatim int
		for _, frame := range c.frames {
			h := parseFrameHeader(t, frame, int(f.frameLen))
			if h.samples < int(f.frameLen) {
				partial++
			}
			switch {
			case h.verbatim:
				verbatim++
			case h.decorrWeight != 0:
				decorrelated++
			}
			if h.extraBits != 0 && h.extraBits != int(f.bitDepth)-16 {
				t.Errorf("%s: extra bits %d, want %d", f.name, h.extraBits, int(f.bitDepth)-16)
			}
			for ch := 0; ch < int(f.channels); ch++ {
				if !h.verbatim && h.predType[ch] != 0 {
					t.Errorf("%s: predictor type %d, want 0", f.name, h.predType[ch])
				}
			}
		}
		if partial == 0 {
			t.Errorf("%s: no frame carries a partial sample count", f.name)
		}
		switch f.name {
		case "alac-stereo44100-16":
			if decorrelated == 0 {
				t.Error("no frame uses stereo decorrelation")
			}
		case "alac-mono48000-16-verbatim":
			if verbatim != len(c.frames) {
				t.Errorf("%d of %d frames are not verbatim", len(c.frames)-verbatim, len(c.frames))
			}
		}
	}

	// The silence fixture must contain a long run of zero samples, which can
	// only come from the zero block of the Rice coder.
	c := loadContainer(t, "alac-mono44100-16-silence")
	dec, err := NewDecoder(Config{FrameLength: 4096, BitDepth: 16, Channels: 1, SampleRate: 44100, MagicCookie: c.cookie})
	if err != nil {
		t.Fatal(err)
	}
	run, longest := 0, 0
	for _, v := range samples16(decodeAll(t, dec, c)) {
		if v == 0 {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	if longest < 1000 {
		t.Errorf("longest run of zero samples is %d, want at least 1000", longest)
	}
}
