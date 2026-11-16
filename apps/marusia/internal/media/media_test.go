package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// wavFile builds a RIFF/WAVE 16-bit PCM file with canonical sizes.
func wavFile(rate, channels int, samples []int16) []byte {
	dataLen := len(samples) * 2
	buf := make([]byte, 0, 44+dataLen)
	appendU32 := func(v uint32) { buf = binary.LittleEndian.AppendUint32(buf, v) }
	appendU16 := func(v uint16) { buf = binary.LittleEndian.AppendUint16(buf, v) }
	buf = append(buf, "RIFF"...)
	appendU32(uint32(36 + dataLen))
	buf = append(buf, "WAVEfmt "...)
	appendU32(16)
	appendU16(1)
	appendU16(uint16(channels))
	appendU32(uint32(rate))
	appendU32(uint32(rate * channels * 2))
	appendU16(uint16(channels * 2))
	appendU16(16)
	buf = append(buf, "data"...)
	appendU32(uint32(dataLen))
	for _, sample := range samples {
		appendU16(uint16(sample))
	}
	return buf
}

func TestDecodeWAVDownmixesStereo(t *testing.T) {
	samples := []int16{1000, 3000, -1000, -3000, 32767, 32767}
	data := wavFile(48000, 2, samples)
	pcm, format, err := DecodeWAV(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if format.SampleRate != 48000 || format.Channels != 2 {
		t.Fatalf("format = %+v", format)
	}
	want := []int16{2000, -2000, 32767}
	if len(pcm) != len(want)*2 {
		t.Fatalf("pcm length = %d", len(pcm))
	}
	for i, expected := range want {
		if got := int16(binary.LittleEndian.Uint16(pcm[i*2:])); got != expected {
			t.Fatalf("sample %d = %d, want %d", i, got, expected)
		}
	}
}

func TestDecodeWAVRejectsUnsupported(t *testing.T) {
	cases := map[string][]byte{
		"ogg":     append([]byte("OggS"), make([]byte, 64)...),
		"garbage": []byte("not audio at all"),
	}
	for name, body := range cases {
		_, _, err := DecodeWAV(body)
		if err == nil {
			t.Fatalf("%s decoded", name)
		}
	}
	data := wavFile(48000, 1, []int16{1, 2, 3})
	// Turn the sample width into 24 bits: same header layout is no longer valid.
	data[34] = 24
	data[35] = 0
	if _, _, err := DecodeWAV(data); err == nil || !strings.Contains(err.Error(), "24-bit") {
		t.Fatalf("24-bit error = %v", err)
	}
}

func TestResampleKeepsDCGain(t *testing.T) {
	samples := make([]int16, 4800)
	for i := range samples {
		samples[i] = 1000
	}
	pcm := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(sample))
	}
	out := Resample(pcm, 48000, 16000)
	if len(out) != 1600*2 {
		t.Fatalf("output length = %d", len(out))
	}
	for i := 100; i < 1500; i++ {
		got := int16(binary.LittleEndian.Uint16(out[i*2:]))
		if got < 995 || got > 1005 {
			t.Fatalf("sample %d = %d, want ~1000", i, got)
		}
	}
}

func TestResamplePreservesSineRMS(t *testing.T) {
	const rate = 48000
	const freq = 440.0
	samples := make([]int16, rate/2)
	for i := range samples {
		samples[i] = int16(8000 * math.Sin(2*math.Pi*freq*float64(i)/rate))
	}
	pcm := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(sample))
	}
	out := Resample(pcm, rate, 16000)
	if len(out) != (rate/2)*2/3 {
		t.Fatalf("output length = %d", len(out))
	}
	var sum float64
	count := 0
	for i := 200; i < len(out)/2-200; i++ {
		value := float64(int16(binary.LittleEndian.Uint16(out[i*2:])))
		sum += value * value
		count++
	}
	rms := math.Sqrt(sum / float64(count))
	want := 8000 / math.Sqrt2
	if math.Abs(rms-want)/want > 0.05 {
		t.Fatalf("rms = %.1f, want ~%.1f", rms, want)
	}
}

// testTransport is the in-process HTTP adapter behind media.Transport.
type testTransport struct{ client *http.Client }

func (t *testTransport) Do(ctx context.Context, req HTTPRequest, timeout time.Duration) (*HTTPResponse, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	for name, value := range req.Header {
		request.Header.Set(name, value)
	}
	response, err := t.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	headers := make(map[string]string, len(response.Header))
	for name, values := range response.Header {
		if len(values) > 0 {
			headers[strings.ToLower(name)] = values[len(values)-1]
		}
	}
	return &HTTPResponse{StatusCode: response.StatusCode, Header: headers, Body: body}, nil
}

func TestFetchBoundsAndHeaders(t *testing.T) {
	var userAgent string
	body := wavFile(16000, 1, []int16{0, 100, -100})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent = r.Header.Get("User-Agent")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	fetcher := &Fetch{Transport: &testTransport{client: server.Client()}}
	got, err := fetcher.Fetch(context.Background(), server.URL+"/tts.wav")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("body mismatch")
	}
	if userAgent != "voice-agent/1" {
		t.Fatalf("User-Agent = %q", userAgent)
	}

	if _, err := fetcher.Fetch(context.Background(), "file:///etc/passwd"); err == nil {
		t.Fatal("file scheme must be rejected")
	}

	small := &Fetch{Transport: &testTransport{client: server.Client()}, MaxBytes: 10}
	if _, err := small.Fetch(context.Background(), server.URL+"/tts.wav"); err == nil ||
		!strings.Contains(err.Error(), "playback limit") {
		t.Fatalf("size cap error = %v", err)
	}
}

// stubTransport returns a canned response or error; it is the smallest way to
// drive the fetch failure paths without a network.
type stubTransport struct {
	response *HTTPResponse
	err      error
}

func (t stubTransport) Do(context.Context, HTTPRequest, time.Duration) (*HTTPResponse, error) {
	return t.response, t.err
}

// TestFetchErrorsDropSignedQuery covers the leak the media layer must not open:
// the vendor signs the play_url in its query string, and a transport error can
// quote the request URL back. Every returned error must name only
// scheme://host/path.
func TestFetchErrorsDropSignedQuery(t *testing.T) {
	const signed = "https://tts.example/voice/reply.wav?X-Amz-Signature=deadbeef&X-Amz-Credential=secret"
	const safe = "https://tts.example/voice/reply.wav"

	check := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("fetch must fail")
		}
		message := err.Error()
		for _, secret := range []string{"deadbeef", "secret", "X-Amz-Signature"} {
			if strings.Contains(message, secret) {
				t.Fatalf("error leaked the signed query (%s): %v", secret, err)
			}
		}
		if !strings.Contains(message, safe) {
			t.Fatalf("error must name scheme://host/path, got: %v", err)
		}
	}

	t.Run("transport error quoting the request url", func(t *testing.T) {
		fetcher := &Fetch{Transport: stubTransport{
			err: fmt.Errorf("capsulad: http request: GET %s: connection refused", signed),
		}}
		_, err := fetcher.Fetch(context.Background(), signed)
		check(t, err)
	})

	t.Run("non-2xx status", func(t *testing.T) {
		fetcher := &Fetch{Transport: stubTransport{response: &HTTPResponse{StatusCode: 404}}}
		_, err := fetcher.Fetch(context.Background(), signed)
		check(t, err)
		if !strings.Contains(err.Error(), "HTTP 404") {
			t.Fatalf("status must stay actionable: %v", err)
		}
	})

	t.Run("body over the cap", func(t *testing.T) {
		fetcher := &Fetch{Transport: stubTransport{response: &HTTPResponse{
			StatusCode: 200, Body: []byte("0123456789")}}, MaxBytes: 4}
		_, err := fetcher.Fetch(context.Background(), signed)
		check(t, err)
		if !strings.Contains(err.Error(), "playback limit") {
			t.Fatalf("cap must stay actionable: %v", err)
		}
	})

	t.Run("malformed url with a signed query", func(t *testing.T) {
		fetcher := &Fetch{Transport: stubTransport{}}
		_, err := fetcher.Fetch(context.Background(), "https://tts.example/%zz?X-Amz-Signature=deadbeef")
		if err == nil {
			t.Fatal("a malformed URL must be rejected")
		}
		if strings.Contains(err.Error(), "deadbeef") {
			t.Fatalf("the parse error echoed the signed query: %v", err)
		}
		if !strings.Contains(err.Error(), "invalid play_url") {
			t.Fatalf("the parse error must stay actionable: %v", err)
		}
	})
}

func TestDecodeToPlayback(t *testing.T) {
	body := wavFile(48000, 2, []int16{1000, 1000, 2000, 2000})
	pcm, err := DecodeToPlayback(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Two input frames at 48 kHz round to one output sample (s16le = 2 bytes).
	if len(pcm) != 2 {
		t.Fatalf("playback pcm length = %d", len(pcm))
	}
}

func TestDecodeNamesUnsupportedContainers(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"ogg", []byte("OggS\x00\x02not really opus"), "Ogg"},
		{"mp3-id3", []byte("ID3\x04\x00\x00not really mp3"), "MP3"},
		{"mp3-frame", []byte{0xFF, 0xFB, 0x90, 0x00, 0x01, 0x02, 0x03, 0x04}, "MP3"},
		{"flac", []byte("fLaC\x00\x00\x00\x22rest"), "FLAC"},
		{"mp4", []byte("\x00\x00\x00\x18ftypM4A \x00\x00\x00\x00"), "MP4/AAC"},
	} {
		_, _, err := DecodeWAV(tc.body)
		if err == nil {
			t.Fatalf("%s: expected an error", tc.name)
		}
		if !errors.Is(err, ErrNotWAV) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want a %s name", tc.name, err, tc.want)
		}
	}
}
