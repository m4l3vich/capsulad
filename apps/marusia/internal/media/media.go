// Package media fetches and decodes the TTS media a play_url command points
// at. The vendor streams TTS as a URL, not as audio frames, and its own decoder
// is a proprietary library; this package supports the minimum a strict
// implementation can prove: RIFF/WAVE 16-bit PCM, mono or stereo, any rate,
// resampled to the framework's 16 kHz mono s16le playback layout.
package media

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// MaxBody caps one media download. A spoken reply is far smaller.
	MaxBody = 10 << 20
	// PlaybackRate is the framework playback rate.
	PlaybackRate = 16000
)

// Fetch retrieves one media body over HTTP(S) with bounded time, size and
// redirect policy.
type Fetch struct {
	Client *http.Client
	// MaxBytes defaults to MaxBody.
	MaxBytes int64
}

// NewFetch returns a fetcher with a 20 s connect and 30 s total deadline.
func NewFetch() *Fetch {
	return &Fetch{
		Client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 20 * time.Second}).DialContext,
				TLSHandshakeTimeout:   20 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				DisableCompression:    true,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 4 {
					return errors.New("too many redirects")
				}
				if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
					return fmt.Errorf("redirect to unsupported scheme %q", req.URL.Scheme)
				}
				return nil
			},
		},
		MaxBytes: MaxBody,
	}
}

// Fetch downloads rawURL. The scheme must be http or https; the body is capped
// and returned whole for decoding by magic, never by Content-Type.
func (f *Fetch) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("media: invalid play_url %q: %w", rawURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("media: play_url %q must use http or https", rawURL)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("media: play_url %q has no host", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("media: play_url request: %w", err)
	}
	// The vendor's media player identifies itself this way; it is the only
	// header the recon recovered for media fetches.
	req.Header.Set("User-Agent", "voice-agent/1")

	client := f.Client
	if client == nil {
		client = NewFetch().Client
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("media: fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("media: fetch %s: HTTP %d", rawURL, resp.StatusCode)
	}
	limit := f.MaxBytes
	if limit <= 0 {
		limit = MaxBody
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("media: read %s: %w", rawURL, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("media: %s exceeds the %d MiB playback limit", rawURL, limit>>20)
	}
	return body, nil
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
