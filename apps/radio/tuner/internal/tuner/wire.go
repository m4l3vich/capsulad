// Package tuner implements the native companion for org.capsulad.radio.
//
// The wire types in this file are the subset of org.capsulad.radio.v1 the
// tuner serves or calls. The radio app owns the authoritative .proto; these
// messages are encoded by hand so the tuner needs no generated descriptor and
// the app bus path stays raw protobuf (the radio's MicroPython handlers use
// recv_bytes/send_bytes).
package tuner

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Protobuf wire types used by the radio contract.
const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

var errMalformed = errors.New("malformed protobuf message")

// TuneRequest is org.capsulad.radio.v1.TuneRequest.
type TuneRequest struct {
	SessionID   string
	URL         string
	StationName string
	PlayerAppID string
}

// TuneResult is org.capsulad.radio.v1.TuneResult.
type TuneResult struct {
	Accepted    bool
	Detail      string
	SessionID   string
	ContentType string
}

// StopRequest is org.capsulad.radio.v1.StopRequest.
type StopRequest struct {
	SessionID string
	Detail    string
}

// StopResult is org.capsulad.radio.v1.StopResult.
type StopResult struct {
	Accepted bool
	Detail   string
}

// PushRequest is org.capsulad.radio.v1.PushRequest. A Push call that cannot
// complete (transport failure, timeout or refusal) means the session is over:
// the tuner sends a best-effort final failed=1 and stops pushing.
type PushRequest struct {
	Sequence      uint32
	SessionID     string
	PCM           []byte
	SampleRateHz  uint32
	Channels      uint32
	SampleFormat  uint32
	FirstFrame    uint64
	Discontinuity bool
	DroppedFrames uint32
	Title         string
	Artist        string
	Finished      bool
	Failed        bool
	Detail        string
}

// PushResult is org.capsulad.radio.v1.PushResult.
type PushResult struct {
	Accepted bool
	Detail   string
}

// DecodeTuneRequest parses a serialized TuneRequest. Unknown fields are
// ignored, matching protobuf forward compatibility.
func DecodeTuneRequest(b []byte) (TuneRequest, error) {
	var m TuneRequest
	err := walk(b, func(f field) error {
		switch f.num {
		case 1:
			m.SessionID = f.text()
		case 2:
			m.URL = f.text()
		case 3:
			m.StationName = f.text()
		case 4:
			m.PlayerAppID = f.text()
		}
		return nil
	})
	return m, err
}

// Marshal encodes a TuneResult.
func (m TuneResult) Marshal() []byte {
	var b []byte
	if m.Accepted {
		b = appendVarintField(b, 1, 1)
	}
	b = appendStringField(b, 2, m.Detail)
	b = appendStringField(b, 3, m.SessionID)
	b = appendStringField(b, 4, m.ContentType)
	return b
}

// DecodeStopRequest parses a serialized StopRequest.
func DecodeStopRequest(b []byte) (StopRequest, error) {
	var m StopRequest
	err := walk(b, func(f field) error {
		switch f.num {
		case 1:
			m.SessionID = f.text()
		case 2:
			m.Detail = f.text()
		}
		return nil
	})
	return m, err
}

// Marshal encodes a StopResult.
func (m StopResult) Marshal() []byte {
	var b []byte
	if m.Accepted {
		b = appendVarintField(b, 1, 1)
	}
	b = appendStringField(b, 2, m.Detail)
	return b
}

// Marshal encodes a PushRequest into a fresh buffer. Zero-valued scalar
// fields are omitted, which is exactly what proto3 does; the radio's decoder
// sees the same defaults.
func (m PushRequest) Marshal() []byte {
	return m.MarshalTo(nil)
}

// MarshalTo appends the encoded PushRequest to dst, letting a caller reuse one
// per-session buffer across chunks.
func (m PushRequest) MarshalTo(dst []byte) []byte {
	b := dst
	if m.Sequence != 0 {
		b = appendVarintField(b, 1, uint64(m.Sequence))
	}
	b = appendStringField(b, 2, m.SessionID)
	b = appendBytesField(b, 3, m.PCM)
	if m.SampleRateHz != 0 {
		b = appendVarintField(b, 4, uint64(m.SampleRateHz))
	}
	if m.Channels != 0 {
		b = appendVarintField(b, 5, uint64(m.Channels))
	}
	if m.SampleFormat != 0 {
		b = appendVarintField(b, 6, uint64(m.SampleFormat))
	}
	if m.FirstFrame != 0 {
		b = appendVarintField(b, 7, m.FirstFrame)
	}
	if m.Discontinuity {
		b = appendVarintField(b, 8, 1)
	}
	if m.DroppedFrames != 0 {
		b = appendVarintField(b, 9, uint64(m.DroppedFrames))
	}
	b = appendStringField(b, 10, m.Title)
	b = appendStringField(b, 11, m.Artist)
	if m.Finished {
		b = appendVarintField(b, 12, 1)
	}
	if m.Failed {
		b = appendVarintField(b, 13, 1)
	}
	b = appendStringField(b, 14, m.Detail)
	return b
}

// DecodePushResult parses a serialized PushResult.
func DecodePushResult(b []byte) (PushResult, error) {
	var m PushResult
	err := walk(b, func(f field) error {
		switch f.num {
		case 1:
			m.Accepted = f.uvarint() != 0
		case 2:
			m.Detail = f.text()
		}
		return nil
	})
	return m, err
}

// field is one decoded protobuf field. Only the value that matches the field's
// wire type is populated.
type field struct {
	num  int
	kind int
	u    uint64
	data []byte
}

func (f field) uvarint() uint64 { return f.u }

func (f field) text() string {
	if f.kind != wireBytes {
		return ""
	}
	return string(f.data)
}

// walk decodes a protobuf message field by field. It stops early when fn
// returns an error, and rejects truncated or group-encoded input.
func walk(b []byte, fn func(field) error) error {
	for len(b) > 0 {
		tag, n := consumeVarint(b)
		if n <= 0 {
			return errMalformed
		}
		b = b[n:]
		num, kind := int(tag>>3), int(tag&7)
		if num <= 0 {
			return errMalformed
		}
		f := field{num: num, kind: kind}
		switch kind {
		case wireVarint:
			v, m := consumeVarint(b)
			if m <= 0 {
				return errMalformed
			}
			f.u = v
			b = b[m:]
		case wireFixed64:
			if len(b) < 8 {
				return errMalformed
			}
			f.u = binary.LittleEndian.Uint64(b)
			b = b[8:]
		case wireBytes:
			length, m := consumeVarint(b)
			if m <= 0 || length > uint64(len(b)-m) {
				return errMalformed
			}
			b = b[m:]
			f.data = b[:int(length)]
			b = b[int(length):]
		case wireFixed32:
			if len(b) < 4 {
				return errMalformed
			}
			f.u = uint64(binary.LittleEndian.Uint32(b))
			b = b[4:]
		default:
			return fmt.Errorf("unsupported protobuf wire type %d", kind)
		}
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}

func consumeVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		c := b[i]
		if i == 9 && c > 1 {
			return 0, -1
		}
		v |= uint64(c&0x7f) << (7 * i)
		if c < 0x80 {
			return v, i + 1
		}
	}
	return 0, -1
}

func appendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func appendTag(dst []byte, num, kind int) []byte {
	return appendVarint(dst, uint64(num)<<3|uint64(kind))
}

func appendVarintField(dst []byte, num int, v uint64) []byte {
	dst = appendTag(dst, num, wireVarint)
	return appendVarint(dst, v)
}

func appendStringField(dst []byte, num int, s string) []byte {
	if s == "" {
		return dst
	}
	return appendBytesField(dst, num, []byte(s))
}

func appendBytesField(dst []byte, num int, p []byte) []byte {
	if len(p) == 0 {
		return dst
	}
	dst = appendTag(dst, num, wireBytes)
	dst = appendVarint(dst, uint64(len(p)))
	return append(dst, p...)
}
