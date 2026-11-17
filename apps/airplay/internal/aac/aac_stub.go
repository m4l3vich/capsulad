//go:build !ffmpeg

// Package aac exposes the AAC decoder seam. This build has no decoder: the
// receiver refuses AAC streams with an explicit reason instead of playing
// silence, and it does not advertise AAC support.
package aac

import "errors"

// Profile is the AAC object type carried by the stream.
type Profile string

const (
	// ProfileLC is AAC-LC.
	ProfileLC Profile = "lc"
	// ProfileELD is AAC-ELD.
	ProfileELD Profile = "eld"
)

// ErrUnavailable is returned when this build has no AAC decoder.
var ErrUnavailable = errors.New("aac: no AAC decoder in this build (build with -tags ffmpeg)")

// Config selects the decoder configuration for one stream.
type Config struct {
	SampleRate int
	Channels   int
	Profile    Profile
}

// Decoder is a decoder handle; this build never creates one.
type Decoder struct{}

// Available reports whether this build can decode AAC.
func Available() bool { return false }

// New always reports that AAC decoding is unavailable.
func New(Config) (*Decoder, error) { return nil, ErrUnavailable }

// FrameSamples reports the nominal AAC-LC frame size.
func (d *Decoder) FrameSamples() int { return 1024 }

// Decode always reports that AAC decoding is unavailable.
func (d *Decoder) Decode([]byte, []byte) ([]byte, error) { return nil, ErrUnavailable }

// Close is a no-op.
func (d *Decoder) Close() error { return nil }
