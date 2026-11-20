// Package media fetches and decodes the TTS body a play_url or result tts
// command points at. The vendor streams TTS as a URL, not as audio frames, and
// its own decoder is a proprietary library; this package supports the minimum
// a strict implementation can prove: RIFF/WAVE 16-bit PCM and raw audio/l16,
// mono or stereo, any rate, resampled to the framework's 16 kHz mono s16le
// playback layout.
package media

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"mime"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"capsulad/apps/marusia/internal/redact"
	"capsulad/sdk/capsulad"
)

const (
	// MaxBody caps one media download. A spoken reply is far smaller.
	MaxBody = 10 << 20
	// PlaybackRate is the framework playback rate.
	PlaybackRate = 16000
)

// HTTPRequest is one media fetch.
type HTTPRequest struct {
	Method string
	URL    string
	Header map[string]string
	Body   []byte
}

// HTTPResponse is a completed fetch; non-2xx is not an error.
type HTTPResponse struct {
	StatusCode int
	Header     map[string]string
	Body       []byte
}

// Transport is the HTTP seam. Production uses the native capsulad client;
// tests inject an in-process adapter so no net/http is linked into the app.
type Transport interface {
	Do(ctx context.Context, req HTTPRequest, timeout time.Duration) (*HTTPResponse, error)
}

// Fetch retrieves one media body over HTTP(S) with bounded time, size and
// redirect policy.
type Fetch struct {
	Transport Transport
	// MaxBytes defaults to MaxBody.
	MaxBytes int64

	mu       sync.Mutex
	fallback *capsulad.HTTPClient
}

// NewFetch returns a fetcher with a 20 s connect and 30 s total deadline.
// The native client is opened lazily on the first fetch.
func NewFetch() *Fetch {
	return &Fetch{MaxBytes: MaxBody}
}

// nativeClient returns the shared native HTTP client, opening it once.
func (f *Fetch) nativeClient(limit int64) (*capsulad.HTTPClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fallback != nil {
		return f.fallback, nil
	}
	client, err := capsulad.OpenHTTP(capsulad.FromEnv(), capsulad.HTTPOptions{
		Roots:           "system",
		UserAgent:       "voice-agent/1",
		MaxBody:         limit,
		FollowRedirects: 4,
		ConnectTimeout:  20 * time.Second,
		ReadTimeout:     30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	f.fallback = client
	return client, nil
}

// Fetch downloads rawURL. The scheme must be http or https; the body is capped
// and returned whole for decoding by magic, never by Content-Type.
//
// Every error names the URL as scheme://host/path only: a vendor TTS URL
// carries its signature in the query string, so the raw value must never reach
// a log line or an operator message.
func (f *Fetch) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	body, _, err := f.FetchMedia(ctx, rawURL, "")
	return body, err
}

// FetchMedia is Fetch plus the Authorization header the vendor's /stream body
// requires and the response Content-Type needed to decode raw audio/l16. The
// header is passed per request so a shared Fetch cannot race a rotated session.
func (f *Fetch) FetchMedia(ctx context.Context, rawURL, authHeader string) ([]byte, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		// The URL parser's error embeds the input, query included; only its
		// reason is reported.
		return nil, "", fmt.Errorf("media: invalid play_url %s: %v", redact.URL(rawURL), parseReason(err))
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, "", fmt.Errorf("media: play_url %s must use http or https", redact.URL(rawURL))
	}
	if parsed.Host == "" {
		return nil, "", fmt.Errorf("media: play_url %s has no host", redact.URL(rawURL))
	}
	limit := f.MaxBytes
	if limit <= 0 {
		limit = MaxBody
	}
	// The vendor's media player identifies itself this way; it is the only
	// header the recon recovered for media fetches.
	headers := map[string]string{"User-Agent": "voice-agent/1"}
	if authHeader != "" {
		headers["Authorization"] = authHeader
	}

	var body []byte
	var status int
	var contentType string
	if f.Transport != nil {
		response, err := f.Transport.Do(ctx, HTTPRequest{
			Method: "GET",
			URL:    parsed.String(),
			Header: headers,
		}, 30*time.Second)
		if err != nil {
			return nil, "", fetchError(rawURL, err)
		}
		status, body = response.StatusCode, response.Body
		contentType = headerValue(response.Header, "content-type")
	} else {
		client, err := f.nativeClient(limit)
		if err != nil {
			return nil, "", fmt.Errorf("media: open http client: %w", err)
		}
		response, err := client.Do(ctx, capsulad.HTTPRequest{
			Method:  "GET",
			URL:     parsed.String(),
			Header:  headers,
			Timeout: 30 * time.Second,
		})
		if err != nil {
			return nil, "", fetchError(rawURL, err)
		}
		status, body = response.StatusCode, response.Body
		contentType = headerValue(response.Header, "content-type")
	}
	if status < 200 || status > 299 {
		return nil, "", fmt.Errorf("media: fetch %s: HTTP %d", redact.URL(rawURL), status)
	}
	if int64(len(body)) > limit {
		return nil, "", fmt.Errorf("media: %s exceeds the %d MiB playback limit", redact.URL(rawURL), limit>>20)
	}
	return body, contentType, nil
}

// headerValue reads a response header case-insensitively; the native client
// lowercases names while test adapters may preserve the canonical spelling.
func headerValue(header map[string]string, name string) string {
	if value := header[name]; value != "" {
		return value
	}
	for key, value := range header {
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

// fetchError wraps a transport failure without the signed URL. The transport's
// own message may quote the request URL, so the exact raw string is replaced by
// its redacted form before the error leaves this package.
func fetchError(rawURL string, err error) error {
	return fmt.Errorf("media: fetch %s: %s", redact.URL(rawURL), redact.ErrorText(err, rawURL))
}

// parseReason strips net/url's wrapper, whose message repeats the raw URL.
func parseReason(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

// ErrNotWAV is returned for a body this package cannot decode.
var ErrNotWAV = errors.New("not a RIFF/WAVE PCM stream")

// sniffEncoding names a container this build cannot decode. The service's reply
// format was not observable during the live work, so the message names whatever
// actually arrived instead of guessing.
func sniffEncoding(data []byte) string {
	switch {
	case len(data) >= 4 && string(data[0:4]) == "OggS":
		return "Ogg (Opus/Vorbis)"
	case len(data) >= 3 && string(data[0:3]) == "ID3":
		return "MP3 (ID3-tagged)"
	case len(data) >= 2 && data[0] == 0xFF && data[1]&0xE0 == 0xE0:
		return "MP3 (MPEG audio)"
	case len(data) >= 4 && string(data[0:4]) == "fLaC":
		return "FLAC"
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		return "MP4/AAC"
	case len(data) >= 4 && string(data[0:4]) == "RF64":
		return "RF64"
	default:
		return ""
	}
}

// WAVFormat describes the decoded PCM before resampling.
type WAVFormat struct {
	SampleRate int
	Channels   int
}

// DecodeWAV parses a canonical or extended RIFF/WAVE file with 16-bit linear
// PCM and returns mono s16le samples at the source rate. Unsupported encodings
// (float, ADPCM, 8/24/32-bit, RF64) are rejected with a message naming the
// actual encoding so the operator can act on it.
func DecodeWAV(data []byte) ([]byte, WAVFormat, error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		if encoding := sniffEncoding(data); encoding != "" {
			return nil, WAVFormat{}, fmt.Errorf("%w: the body is %s, which this build cannot decode", ErrNotWAV, encoding)
		}
		return nil, WAVFormat{}, fmt.Errorf("%w: missing RIFF/WAVE header", ErrNotWAV)
	}
	var format WAVFormat
	var pcm []byte
	sawFmt := false
	for offset := 12; offset+8 <= len(data); {
		id := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		offset += 8
		if size < 0 || offset+size > len(data) {
			return nil, WAVFormat{}, fmt.Errorf("%w: chunk %q overruns the body", ErrNotWAV, id)
		}
		chunk := data[offset : offset+size]
		switch id {
		case "fmt ":
			if len(chunk) < 16 {
				return nil, WAVFormat{}, fmt.Errorf("%w: short fmt chunk", ErrNotWAV)
			}
			formatTag := binary.LittleEndian.Uint16(chunk[0:2])
			channels := int(binary.LittleEndian.Uint16(chunk[2:4]))
			rate := int(binary.LittleEndian.Uint32(chunk[4:8]))
			bits := int(binary.LittleEndian.Uint16(chunk[14:16]))
			if formatTag != 1 {
				return nil, WAVFormat{}, fmt.Errorf("%w: format tag %d is not linear PCM", ErrNotWAV, formatTag)
			}
			if bits != 16 {
				return nil, WAVFormat{}, fmt.Errorf("%w: %d-bit samples are not supported (need 16-bit PCM)", ErrNotWAV, bits)
			}
			if channels < 1 || channels > 2 {
				return nil, WAVFormat{}, fmt.Errorf("%w: %d channels are not supported (need mono or stereo)", ErrNotWAV, channels)
			}
			if rate < 8000 || rate > 192000 {
				return nil, WAVFormat{}, fmt.Errorf("%w: %d Hz is outside the supported range", ErrNotWAV, rate)
			}
			format = WAVFormat{SampleRate: rate, Channels: channels}
			sawFmt = true
		case "data":
			if !sawFmt {
				return nil, WAVFormat{}, fmt.Errorf("%w: data chunk precedes fmt", ErrNotWAV)
			}
			pcm = chunk
		}
		offset += size
		if size%2 == 1 {
			offset++
		}
	}
	if !sawFmt {
		return nil, WAVFormat{}, fmt.Errorf("%w: no fmt chunk", ErrNotWAV)
	}
	if pcm == nil {
		return nil, WAVFormat{}, fmt.Errorf("%w: no data chunk", ErrNotWAV)
	}
	return downmixToMono(pcm, format.Channels), format, nil
}

// DecodeToPlayback decodes a media body and converts it to the 16 kHz mono
// s16le layout OpenPlayback accepts.
func DecodeToPlayback(data []byte) ([]byte, error) {
	pcm, format, err := DecodeWAV(data)
	if err != nil {
		return nil, err
	}
	return Resample(pcm, format.SampleRate, PlaybackRate), nil
}

// DecodeMedia decodes the body the /stream endpoint returns. The endpoint's
// observed reply is raw audio/l16 (24 kHz mono big-endian in the 2026-11-21
// live turn), not a container; a WAV body remains accepted for the play_url
// path.
func DecodeMedia(data []byte, contentType string) ([]byte, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err == nil && strings.EqualFold(mediaType, "audio/l16") {
		rate := intParam(params["rate"], 8000)
		channels := intParam(params["channels"], 1)
		if channels < 1 || channels > 2 {
			return nil, fmt.Errorf("media: audio/l16 has %d channels (need mono or stereo)", channels)
		}
		if rate < 8000 || rate > 192000 {
			return nil, fmt.Errorf("media: audio/l16 rate %d is outside the supported range", rate)
		}
		bigEndian := !strings.EqualFold(params["endianness"], "little-endian")
		return Resample(DecodeL16(data, channels, bigEndian), rate, PlaybackRate), nil
	}
	return DecodeToPlayback(data)
}

// DecodeL16 converts linear 16-bit PCM to mono s16le at the source rate.
// bigEndian selects the wire byte order; channels may be mono or stereo.
func DecodeL16(data []byte, channels int, bigEndian bool) []byte {
	if channels < 1 {
		channels = 1
	}
	frameBytes := channels * 2
	frames := len(data) / frameBytes
	out := make([]byte, frames*2)
	order := binary.ByteOrder(binary.LittleEndian)
	if bigEndian {
		order = binary.BigEndian
	}
	for i := range frames {
		var sum int32
		for channel := range channels {
			sum += int32(int16(order.Uint16(data[i*frameBytes+channel*2:])))
		}
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(sum/int32(channels))))
	}
	return out
}

func intParam(value string, fallback int) int {
	if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
		return parsed
	}
	return fallback
}

// downmixToMono averages stereo pairs so a mono upload/playback stream carries
// the full signal instead of one channel.
func downmixToMono(pcm []byte, channels int) []byte {
	if channels == 1 {
		out := make([]byte, len(pcm)&^1)
		copy(out, pcm)
		return out
	}
	frames := len(pcm) / 4
	out := make([]byte, frames*2)
	for i := range frames {
		left := int32(int16(binary.LittleEndian.Uint16(pcm[i*4:])))
		right := int32(int16(binary.LittleEndian.Uint16(pcm[i*4+2:])))
		sum := (left + right) / 2
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(sum)))
	}
	return out
}

// Resample converts mono s16le from fromRate to toRate with a windowed-sinc
// low-pass. Weights are normalised per output sample, so DC and passband gain
// stay at unity regardless of the ratio.
func Resample(pcm []byte, fromRate, toRate int) []byte {
	frames := len(pcm) / 2
	if frames == 0 || fromRate <= 0 || toRate <= 0 || fromRate == toRate {
		out := make([]byte, frames*2)
		copy(out, pcm)
		return out
	}
	// Input as floats, once.
	input := make([]float64, frames)
	for i := range input {
		input[i] = float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
	}

	ratio := float64(toRate) / float64(fromRate)
	outFrames := int(math.Round(float64(frames) * ratio))
	if outFrames <= 0 {
		return nil
	}
	// Cutoff in cycles per input sample, leaving a 6% transition band below the
	// lower Nyquist.
	cutoff := 0.5 * math.Min(1, ratio) * 0.94
	step := float64(fromRate) / float64(toRate)
	const halfTaps = 16

	out := make([]byte, outFrames*2)
	for n := range outFrames {
		center := float64(n) * step
		left := int(math.Ceil(center - halfTaps))
		right := int(math.Floor(center + halfTaps))
		var sum, weightSum float64
		for k := left; k <= right; k++ {
			x := center - float64(k)
			weight := sinc(2*cutoff*x) * blackman(x/halfTaps)
			if weight == 0 {
				continue
			}
			index := k
			if index < 0 {
				index = 0
			} else if index >= frames {
				index = frames - 1
			}
			sum += input[index] * weight
			weightSum += weight
		}
		value := 0.0
		if weightSum != 0 {
			value = sum / weightSum
		}
		binary.LittleEndian.PutUint16(out[n*2:], uint16(sampleToInt16(value)))
	}
	return out
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	px := math.Pi * x
	return math.Sin(px) / px
}

// blackman is a compact Blackman window over [-1, 1]; outside it the kernel is
// zero so adjacent output samples do not see distant input.
func blackman(t float64) float64 {
	if t <= -1 || t >= 1 {
		return 0
	}
	x := math.Pi * (t + 1)
	return 0.42 - 0.5*math.Cos(x) + 0.08*math.Cos(2*x)
}

func sampleToInt16(value float64) int16 {
	switch {
	case value > 32767:
		return 32767
	case value < -32768:
		return -32768
	default:
		return int16(math.Round(value))
	}
}

// ToMono16k converts captured PCM to the upload layout: mono s16le at 16 kHz.
// channels must be 1 or 2; sampleRate is the capture rate.
func ToMono16k(pcm []byte, sampleRate, channels int) []byte {
	if channels == 2 {
		pcm = downmixToMono(pcm, channels)
	}
	return Resample(pcm, sampleRate, PlaybackRate)
}
