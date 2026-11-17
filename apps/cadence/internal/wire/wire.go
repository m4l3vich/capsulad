// Package wire frames cadence.v2 envelopes for the device link, matching the
// upstream pkg/wire exactly: one binary protobuf frame carries one Envelope,
// a frame over MaxFrameBytes is refused, and a frame that parses but carries no
// body this side declares is ErrUnknown so the link can skip it.
package wire

import (
	"crypto/sha256"
	"errors"
	"sort"

	"google.golang.org/protobuf/proto"

	cadencev2 "capsulad/apps/cadence/internal/proto/cadencev2"
)

// MaxFrameBytes bounds one envelope on the device link. The upstream server and
// daemon use the same cap, so a larger frame is refused on both sides.
const MaxFrameBytes = 16 << 20

var (
	ErrFrameTooLarge = errors.New("wire: frame too large")
	ErrInvalid       = errors.New("wire: invalid frame")
	// ErrUnknown: the frame parses but carries no body this side knows, such as
	// a newer peer's message type; a link skips it rather than closing.
	ErrUnknown = errors.New("wire: unknown message")
)

// Encode marshals one envelope deterministically, so the same envelope is the
// same bytes on every peer and in tests.
func Encode(env *cadencev2.Envelope) ([]byte, error) {
	if env == nil {
		return nil, ErrInvalid
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(env)
	if err != nil {
		return nil, ErrInvalid
	}
	if len(b) > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	return b, nil
}

// Decode parses one frame. An envelope with no oneof arm this side declares is
// ErrUnknown, never ErrInvalid, so a newer peer's message type does not close
// the link.
func Decode(b []byte) (*cadencev2.Envelope, error) {
	if len(b) > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	env := &cadencev2.Envelope{}
	if err := proto.Unmarshal(b, env); err != nil {
		return nil, ErrInvalid
	}
	if env.GetBody() == nil {
		return nil, ErrUnknown
	}
	return env, nil
}

// ReportHash is the capability-report hash Hello carries and every report frame
// repeats: the deterministic marshal of the whole report with its chunk header
// cleared and its descriptors sorted by id, then SHA-256. It mirrors upstream
// pkg/caphash exactly, so an empty report hashes to SHA-256 of the empty
// message.
func ReportHash(r *cadencev2.CapabilityReport) ([]byte, error) {
	if r == nil {
		r = &cadencev2.CapabilityReport{}
	}
	clone := proto.Clone(r).(*cadencev2.CapabilityReport)
	clone.Hash, clone.Part, clone.Parts = nil, 0, 0
	sort.SliceStable(clone.Descriptors, func(i, j int) bool {
		return clone.Descriptors[i].GetId() < clone.Descriptors[j].GetId()
	})
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(clone)
	if err != nil {
		return nil, ErrInvalid
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}
