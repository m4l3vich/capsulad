// Package raop implements the legacy RAOP (AirPlay 1) wire pieces used by the
// AirPlay receiver: the ANNOUNCE SDP body, the receiver RSA key exchange, the
// RAOP timing packet codec and the playout clock inferred from timing
// exchanges.
//
// The layouts are taken from shairport-sync 5.6.2 (commit
// 0a34fbda83724cc06266aadcf7c88acc8fef4e7d); the reference file and function
// are named at each structure. No code is copied from that project.
package raop

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// Stream is the audio stream description carried by a legacy RAOP ANNOUNCE
// request. shairport-sync parses the same lines in rtsp.c, handle_announce().
type Stream struct {
	// PayloadType is the RTP payload type from "m=audio 0 RTP/AVP <pt>".
	PayloadType uint8
	// Codec is one of "alac", "pcm", "aac" or "unknown", derived from the
	// a=rtpmap encoding name (rtsp.c checks for an L16 rtpmap and treats any
	// a=fmtp line without one as Apple Lossless).
	Codec string
	// ALAC holds the parameters of an a=fmtp line. It is zero when the
	// ANNOUNCE carried no fmtp line.
	ALAC ALACParams
	// RSAAESKey is the base64-decoded a=rsaaeskey value: the RSA ciphertext
	// of the 16-byte AES key, still encrypted (rtsp.c base64_dec()s the
	// attribute before rsa_apply()). nil when the attribute is absent.
	RSAAESKey []byte
	// AESIV is the 16-byte AES initialisation vector from a=aesiv (rtsp.c
	// rejects any other length). nil when absent.
	AESIV []byte
	// HasRSA reports whether the sender supplied an encrypted AES key.
	HasRSA bool
	// MinLatency and MaxLatency are a=min-latency / a=max-latency in audio
	// frames (rtsp.c stores them as conn->minimum_latency/maximum_latency,
	// which the player compares against frame counts).
	MinLatency int
	MaxLatency int
	// PTimeMS is a=ptime in milliseconds.
	PTimeMS int
}

// ALACParams describes an Apple Lossless a=fmtp line. The eleven values are,
// in order, the fields below; shairport-sync loads them into
// conn->stream.fmtp[1..11] in rtsp.c, handle_announce().
type ALACParams struct {
	FrameLength       int
	CompatibleVersion int
	BitDepth          int
	PB                int
	MB                int
	KB                int
	Channels          int
	MaxRun            int
	MaxFrameBytes     int
	AvgBitRate        int
	SampleRate        int
}

// rtpmapEntry is a decoded a=rtpmap line.
type rtpmapEntry struct {
	payloadType uint8
	codec       string
}

// ParseSDP parses the SDP body of a legacy RAOP ANNOUNCE request. Unknown
// lines are ignored; malformed values that the receiver must act on are
// errors. It never panics, including on an empty or truncated body.
func ParseSDP(body []byte) (*Stream, error) {
	lines := splitSDP(body)
	if len(lines) == 0 {
		return nil, fmt.Errorf("raop: empty SDP body")
	}

	st := &Stream{Codec: "unknown"}

	var (
		sawAudio  bool
		rtpmaps   []rtpmapEntry
		fmtpPT    = -1
		fmtpVals  []int
		latencies = map[string]int{}
	)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		switch {
		case strings.HasPrefix(line, "m="):
			// Only one audio stream is representable by Stream. Legacy RAOP
			// ANNOUNCE always sends a single "m=audio 0 RTP/AVP <pt>".
			if !strings.HasPrefix(line, "m=audio") {
				continue
			}
			if sawAudio {
				return nil, fmt.Errorf("raop: multiple m=audio lines are not supported")
			}
			fields := strings.Fields(line)
			if len(fields) < 4 {
				return nil, fmt.Errorf("raop: malformed m=audio line %q", line)
			}
			pt, err := strconv.ParseUint(fields[3], 10, 7)
			if err != nil {
				return nil, fmt.Errorf("raop: invalid RTP payload type in %q: %w", line, err)
			}
			st.PayloadType = uint8(pt)
			sawAudio = true

		case strings.HasPrefix(line, "a=rtpmap:"):
			fields := strings.Fields(strings.TrimPrefix(line, "a=rtpmap:"))
			if len(fields) < 2 {
				return nil, fmt.Errorf("raop: malformed rtpmap line %q", line)
			}
			pt, err := strconv.ParseUint(fields[0], 10, 7)
			if err != nil {
				return nil, fmt.Errorf("raop: invalid rtpmap payload type in %q: %w", line, err)
			}
			// fields[1] is "<encoding>/<clock rate>[/<channels>]".
			encoding := fields[1]
			if i := strings.IndexByte(encoding, '/'); i >= 0 {
				encoding = encoding[:i]
			}
			rtpmaps = append(rtpmaps, rtpmapEntry{
				payloadType: uint8(pt),
				codec:       codecFromEncoding(encoding),
			})

		case strings.HasPrefix(line, "a=fmtp:"):
			fields := strings.Fields(strings.TrimPrefix(line, "a=fmtp:"))
			if len(fields) < 12 {
				return nil, fmt.Errorf("raop: ALAC fmtp line %q has %d fields, want the payload type plus 11 parameters", line, len(fields))
			}
			pt, err := strconv.ParseUint(fields[0], 10, 7)
			if err != nil {
				return nil, fmt.Errorf("raop: invalid fmtp payload type in %q: %w", line, err)
			}
			vals := make([]int, 11)
			for i := range vals {
				v, err := strconv.Atoi(fields[i+1])
				if err != nil {
					return nil, fmt.Errorf("raop: fmtp parameter %d in %q is not an integer: %w", i+1, line, err)
				}
				if v < 0 {
					return nil, fmt.Errorf("raop: fmtp parameter %d in %q is negative", i+1, line)
				}
				vals[i] = v
			}
			fmtpPT = int(pt)
			fmtpVals = vals

		case strings.HasPrefix(line, "a=rsaaeskey:"):
			blob, err := decodeBase64Attribute("a=rsaaeskey", strings.TrimPrefix(line, "a=rsaaeskey:"))
			if err != nil {
				return nil, err
			}
			st.RSAAESKey = blob

		case strings.HasPrefix(line, "a=aesiv:"):
			iv, err := decodeBase64Attribute("a=aesiv", strings.TrimPrefix(line, "a=aesiv:"))
			if err != nil {
				return nil, err
			}
			if len(iv) != 16 {
				return nil, fmt.Errorf("raop: a=aesiv decodes to %d bytes, want 16", len(iv))
			}
			st.AESIV = iv

		case strings.HasPrefix(line, "a=min-latency:"),
			strings.HasPrefix(line, "a=max-latency:"):
			attr := strings.TrimPrefix(line, "a=")
			name, value, _ := strings.Cut(attr, ":")
			v, err := parseNonNegativeInt("a="+name, value)
			if err != nil {
				return nil, err
			}
			latencies[name] = v

		case strings.HasPrefix(line, "a=ptime:"):
			v, err := parseNonNegativeInt("a=ptime", strings.TrimPrefix(line, "a=ptime:"))
			if err != nil {
				return nil, err
			}
			if v > 60000 {
				return nil, fmt.Errorf("raop: a=ptime %d is out of range", v)
			}
			st.PTimeMS = v
		}
	}

	if !sawAudio {
		return nil, fmt.Errorf("raop: SDP body has no m=audio line")
	}

	// Attributes can only be interpreted against the stream's payload type.
	for _, m := range rtpmaps {
		if m.payloadType == st.PayloadType {
			st.Codec = m.codec
			break
		}
	}

	if fmtpPT == int(st.PayloadType) {
		if err := validateALACParams(fmtpVals); err != nil {
			return nil, err
		}
		// The ANNOUNCE value order is fixed and mirrors conn->stream.fmtp
		// in rtsp.c, handle_announce().
		st.ALAC = ALACParams{
			FrameLength:       fmtpVals[0],
			CompatibleVersion: fmtpVals[1],
			BitDepth:          fmtpVals[2],
			PB:                fmtpVals[3],
			MB:                fmtpVals[4],
			KB:                fmtpVals[5],
			Channels:          fmtpVals[6],
			MaxRun:            fmtpVals[7],
			MaxFrameBytes:     fmtpVals[8],
			AvgBitRate:        fmtpVals[9],
			SampleRate:        fmtpVals[10],
		}
		// rtsp.c treats an fmtp line as Apple Lossless unless the sender also
		// advertised uncompressed L16.
		if st.Codec == "unknown" {
			st.Codec = "alac"
		}
	}

	st.MinLatency = latencies["min-latency"]
	st.MaxLatency = latencies["max-latency"]
	st.HasRSA = len(st.RSAAESKey) > 0

	return st, nil
}

// codecFromEncoding maps an RTP encoding name to the frozen Codec strings.
func codecFromEncoding(encoding string) string {
	switch {
	case strings.EqualFold(encoding, "AppleLossless"), strings.EqualFold(encoding, "ALAC"):
		return "alac"
	case strings.EqualFold(encoding, "L16"):
		return "pcm"
	case strings.EqualFold(encoding, "mpeg4-generic"), strings.EqualFold(encoding, "AAC"),
		strings.EqualFold(encoding, "MP4A-LATM"):
		return "aac"
	default:
		return "unknown"
	}
}

func validateALACParams(v []int) error {
	if v[0] <= 0 {
		return fmt.Errorf("raop: ALAC frame length %d is not positive", v[0])
	}
	if v[0] > 65536 {
		return fmt.Errorf("raop: ALAC frame length %d is out of range", v[0])
	}
	if v[2] <= 0 || v[2] > 64 {
		return fmt.Errorf("raop: ALAC bit depth %d is out of range", v[2])
	}
	if v[6] <= 0 || v[6] > 16 {
		return fmt.Errorf("raop: ALAC channel count %d is out of range", v[6])
	}
	if v[10] <= 0 || v[10] > 768000 {
		return fmt.Errorf("raop: ALAC sample rate %d is out of range", v[10])
	}
	return nil
}

func parseNonNegativeInt(name, value string) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("raop: invalid %s value %q: %w", name, value, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("raop: %s value %d is negative", name, v)
	}
	return v, nil
}

// decodeBase64Attribute decodes a base64 SDP attribute. Senders use padded
// standard base64; the raw alphabet is accepted as well for robustness.
func decodeBase64Attribute(name, value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("raop: %s is empty", name)
	}
	if b, err := base64.StdEncoding.DecodeString(value); err == nil {
		return b, nil
	}
	b, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("raop: %s is not valid base64: %w", name, err)
	}
	return b, nil
}

// splitSDP splits an SDP body on CRLF, CR or LF and drops empty lines.
func splitSDP(body []byte) []string {
	s := string(body)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
