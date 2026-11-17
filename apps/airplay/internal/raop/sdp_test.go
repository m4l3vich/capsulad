package raop

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestParseSDPFullALAC(t *testing.T) {
	cipher := readTestFile(t, "openssl_oaep_sha1_aes.enc")
	body := fmt.Sprintf("v=0\r\n"+
		"o=iTunes 3413821438 0 IN IP4 192.168.1.10\r\n"+
		"s=iTunes\r\n"+
		"c=IN IP4 192.168.1.20\r\n"+
		"t=0 0\r\n"+
		"m=audio 0 RTP/AVP 96\r\n"+
		"a=rtpmap:96 AppleLossless\r\n"+
		"a=fmtp:96 352 0 16 40 10 14 2 255 0 0 44100\r\n"+
		"a=rsaaeskey:%s\r\n"+
		"a=aesiv:AAAAAAAAAAAAAAAAAAAAAA==\r\n"+
		"a=min-latency:11025\r\n"+
		"a=max-latency:88200\r\n"+
		"a=ptime:352\r\n",
		base64.StdEncoding.EncodeToString(cipher))

	st, err := ParseSDP([]byte(body))
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if st.PayloadType != 96 {
		t.Errorf("PayloadType = %d, want 96", st.PayloadType)
	}
	if st.Codec != "alac" {
		t.Errorf("Codec = %q, want alac", st.Codec)
	}
	wantALAC := ALACParams{
		FrameLength:       352,
		CompatibleVersion: 0,
		BitDepth:          16,
		PB:                40,
		MB:                10,
		KB:                14,
		Channels:          2,
		MaxRun:            255,
		MaxFrameBytes:     0,
		AvgBitRate:        0,
		SampleRate:        44100,
	}
	if st.ALAC != wantALAC {
		t.Errorf("ALAC = %+v, want %+v", st.ALAC, wantALAC)
	}
	if !st.HasRSA {
		t.Error("HasRSA = false, want true")
	}
	if !bytes.Equal(st.RSAAESKey, cipher) {
		t.Errorf("RSAAESKey is %d bytes, want the %d-byte decoded ciphertext", len(st.RSAAESKey), len(cipher))
	}
	if !bytes.Equal(st.AESIV, make([]byte, 16)) {
		t.Errorf("AESIV = %x, want 16 zero bytes", st.AESIV)
	}
	if st.MinLatency != 11025 || st.MaxLatency != 88200 {
		t.Errorf("latencies = %d/%d, want 11025/88200", st.MinLatency, st.MaxLatency)
	}
	if st.PTimeMS != 352 {
		t.Errorf("PTimeMS = %d, want 352", st.PTimeMS)
	}
}

func TestParseSDPWithoutEncryption(t *testing.T) {
	body := strings.Join([]string{
		"v=0",
		"o=iTunes 1 0 IN IP4 10.0.0.2",
		"s=iTunes",
		"m=audio 0 RTP/AVP 96",
		"a=rtpmap:96 AppleLossless",
		"a=fmtp:96 352 0 16 40 10 14 2 255 0 0 44100",
		"",
	}, "\n")

	st, err := ParseSDP([]byte(body))
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if st.HasRSA {
		t.Error("HasRSA = true, want false")
	}
	if st.RSAAESKey != nil || st.AESIV != nil {
		t.Errorf("key material = %v/%v, want nil/nil", st.RSAAESKey, st.AESIV)
	}
	if st.Codec != "alac" {
		t.Errorf("Codec = %q, want alac", st.Codec)
	}
}

func TestParseSDPCodecs(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
		alac bool
	}{
		{
			name: "pcm L16 with fmtp",
			body: "m=audio 0 RTP/AVP 96\na=rtpmap:96 L16/44100/2\n" +
				"a=fmtp:96 352 0 16 40 10 14 2 255 0 0 44100\n",
			want: "pcm",
			alac: true,
		},
		{
			name: "aac mpeg4-generic",
			body: "m=audio 0 RTP/AVP 96\na=rtpmap:96 mpeg4-generic/44100/2\n",
			want: "aac",
		},
		{
			name: "unknown encoding",
			body: "m=audio 0 RTP/AVP 97\na=rtpmap:97 Opus/48000/2\n",
			want: "unknown",
		},
		{
			name: "missing rtpmap",
			body: "m=audio 0 RTP/AVP 96\n",
			want: "unknown",
		},
		{
			name: "alac rtpmap without fmtp",
			body: "m=audio 0 RTP/AVP 96\na=rtpmap:96 AppleLossless\n",
			want: "alac",
		},
		{
			name: "fmtp for another payload type is ignored",
			body: "m=audio 0 RTP/AVP 96\na=rtpmap:96 ALAC\n" +
				"a=fmtp:97 352 0 16 40 10 14 2 255 0 0 44100\n",
			want: "alac",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, err := ParseSDP([]byte(tc.body))
			if err != nil {
				t.Fatalf("ParseSDP: %v", err)
			}
			if st.Codec != tc.want {
				t.Errorf("Codec = %q, want %q", st.Codec, tc.want)
			}
			if tc.alac && st.ALAC.SampleRate != 44100 {
				t.Errorf("ALAC.SampleRate = %d, want 44100", st.ALAC.SampleRate)
			}
			if !tc.alac && st.ALAC != (ALACParams{}) {
				t.Errorf("ALAC = %+v, want zero", st.ALAC)
			}
		})
	}
}

func TestParseSDPErrors(t *testing.T) {
	const validFmtp = "a=fmtp:96 352 0 16 40 10 14 2 255 0 0 44100\n"
	tests := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"no audio line", "v=0\ns=nothing\n"},
		{"truncated media line", "m=audio 0 RTP/AVP\n"},
		{"non-numeric payload type", "m=audio 0 RTP/AVP ALAC\n"},
		{"payload type out of range", "m=audio 0 RTP/AVP 200\n"},
		{"multiple audio lines", "m=audio 0 RTP/AVP 96\nm=audio 0 RTP/AVP 96\n"},
		{"short fmtp", "m=audio 0 RTP/AVP 96\n" + "a=fmtp:96 352 0 16 40 10 14 2 255 0 0\n"},
		{"non-numeric fmtp", "m=audio 0 RTP/AVP 96\n" + "a=fmtp:96 352 0 16 40 10 14 2 255 0 0 x\n"},
		{"negative fmtp", "m=audio 0 RTP/AVP 96\n" + "a=fmtp:96 352 0 16 40 10 14 2 255 0 0 -44100\n"},
		{"zero frame length", "m=audio 0 RTP/AVP 96\n" + "a=fmtp:96 0 0 16 40 10 14 2 255 0 0 44100\n"},
		{"zero channels", "m=audio 0 RTP/AVP 96\n" + "a=fmtp:96 352 0 16 40 10 14 0 255 0 0 44100\n"},
		{"zero sample rate", "m=audio 0 RTP/AVP 96\n" + "a=fmtp:96 352 0 16 40 10 14 2 255 0 0 0\n"},
		{"bad rtpmap payload type", "m=audio 0 RTP/AVP 96\na=rtpmap:x AppleLossless\n"},
		{"bad rsaaeskey base64", "m=audio 0 RTP/AVP 96\na=rsaaeskey:!!!not-base64!!!\n" + validFmtp},
		{"empty rsaaeskey", "m=audio 0 RTP/AVP 96\na=rsaaeskey:\n" + validFmtp},
		{"bad aesiv base64", "m=audio 0 RTP/AVP 96\na=aesiv:!!!!\n" + validFmtp},
		{"short aesiv", "m=audio 0 RTP/AVP 96\na=aesiv:" + base64.StdEncoding.EncodeToString(make([]byte, 15)) + "\n" + validFmtp},
		{"negative latency", "m=audio 0 RTP/AVP 96\na=min-latency:-1\n" + validFmtp},
		{"huge ptime", "m=audio 0 RTP/AVP 96\na=ptime:60001\n" + validFmtp},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseSDP([]byte(tc.body)); err == nil {
				t.Fatalf("ParseSDP(%q) = nil error, want error", tc.body)
			}
		})
	}
}

// TestParseSDPDoesNotPanic feeds every prefix and a few corruptions of a real
// body; the parser must return an error rather than panic.
func TestParseSDPDoesNotPanic(t *testing.T) {
	body := []byte("v=0\r\nm=audio 0 RTP/AVP 96\r\na=rtpmap:96 AppleLossless\r\n" +
		"a=fmtp:96 352 0 16 40 10 14 2 255 0 0 44100\r\n" +
		"a=rsaaeskey:" + base64.StdEncoding.EncodeToString(make([]byte, 256)) + "\r\n" +
		"a=aesiv:AAAAAAAAAAAAAAAAAAAAAA==\r\n")
	for i := range len(body) + 1 {
		_, _ = ParseSDP(body[:i])
	}
	for _, b := range [][]byte{nil, {}, {0x00}, bytes.Repeat([]byte("="), 64), bytes.Repeat([]byte{0xff}, 256)} {
		_, _ = ParseSDP(b)
	}
}

func readTestFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return b
}
