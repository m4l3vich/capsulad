// Package artwork extracts a small dominant-colour palette from AirPlay cover
// art and tracks a smoothed playback envelope for visualisers.
package artwork

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"sort"
)

// RGB is an 8-bit colour.
type RGB struct {
	R, G, B uint8
}

const (
	// maxImageBytes bounds the encoded artwork size.
	maxImageBytes = 8 << 20
	// maxDimension bounds the decoded width and height.
	maxDimension = 4096
	// sampleSide is the length of the longest side of the sampling grid.
	sampleSide = 96
	// paletteSize is the maximum number of returned colours.
	paletteSize = 3
	// quantShift is the number of low bits discarded per channel when
	// clustering. It yields a 16x16x16 colour grid.
	quantShift = 4
)

const (
	quantLevels = 1 << (8 - quantShift)
	binCount    = quantLevels * quantLevels * quantLevels
)

var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// Palette returns up to three dominant colours of a JPEG or PNG image, ordered
// by decreasing weight. The image is downsampled onto a fixed grid before
// colours are clustered, so the result is deterministic for the same input. A
// solid-colour image yields exactly one colour and greyscale input yields
// achromatic colours.
func Palette(data []byte) ([]RGB, error) {
	if len(data) == 0 {
		return nil, errors.New("artwork: empty image data")
	}
	if len(data) > maxImageBytes {
		return nil, fmt.Errorf("artwork: encoded image is %d bytes, limit is %d", len(data), maxImageBytes)
	}

	img, err := decode(data)
	if err != nil {
		return nil, err
	}
	bounds := img.Bounds()
	if err := checkDimensions(bounds.Dx(), bounds.Dy()); err != nil {
		return nil, err
	}
	return dominantColours(img), nil
}

// decode sniffs and decodes a PNG or JPEG image, rejecting oversized images
// from their header before the pixel buffer is allocated.
func decode(data []byte) (image.Image, error) {
	switch {
	case bytes.HasPrefix(data, pngSignature):
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("artwork: reading PNG header: %w", err)
		}
		if err := checkDimensions(cfg.Width, cfg.Height); err != nil {
			return nil, err
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("artwork: decoding PNG: %w", err)
		}
		return img, nil
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8}):
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("artwork: reading JPEG header: %w", err)
		}
		if err := checkDimensions(cfg.Width, cfg.Height); err != nil {
			return nil, err
		}
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("artwork: decoding JPEG: %w", err)
		}
		return img, nil
	default:
		return nil, errors.New("artwork: unsupported image format (want JPEG or PNG)")
	}
}

func checkDimensions(width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("artwork: invalid image dimensions %dx%d", width, height)
	}
	if width > maxDimension || height > maxDimension {
		return fmt.Errorf("artwork: image is %dx%d, limit is %dx%d", width, height, maxDimension, maxDimension)
	}
	return nil
}

// dominantColours downsamples img and clusters the samples into quantised RGB
// bins. Each bin is represented by the exact average of its samples.
func dominantColours(img image.Image) []RGB {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil
	}

	step := 1
	if longest := max(width, height); longest > sampleSide {
		step = (longest + sampleSide - 1) / sampleSide
	}

	var counts [binCount]uint32
	var sumR, sumG, sumB [binCount]uint64
	var first [binCount]int

	seq := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y += step {
		for x := bounds.Min.X; x < bounds.Max.X; x += step {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			key := int(c.R>>quantShift)<<8 | int(c.G>>quantShift)<<4 | int(c.B>>quantShift)
			if counts[key] == 0 {
				first[key] = seq
			}
			counts[key]++
			sumR[key] += uint64(c.R)
			sumG[key] += uint64(c.G)
			sumB[key] += uint64(c.B)
			seq++
		}
	}

	type bin struct {
		count uint32
		first int
		color RGB
	}
	bins := make([]bin, 0, binCount)
	for i := range counts {
		if counts[i] == 0 {
			continue
		}
		bins = append(bins, bin{
			count: counts[i],
			first: first[i],
			color: RGB{
				R: average(sumR[i], counts[i]),
				G: average(sumG[i], counts[i]),
				B: average(sumB[i], counts[i]),
			},
		})
	}

	sort.Slice(bins, func(i, j int) bool {
		if bins[i].count != bins[j].count {
			return bins[i].count > bins[j].count
		}
		return bins[i].first < bins[j].first
	})

	if len(bins) > paletteSize {
		bins = bins[:paletteSize]
	}
	colours := make([]RGB, len(bins))
	for i := range bins {
		colours[i] = bins[i].color
	}
	return colours
}

func average(sum uint64, count uint32) uint8 {
	return uint8((sum + uint64(count)/2) / uint64(count))
}

// Scale multiplies each channel by f and clamps the result to [0,255]. A
// non-positive or NaN factor yields black.
func Scale(c RGB, f float64) RGB {
	if math.IsNaN(f) || f <= 0 {
		return RGB{}
	}
	return RGB{
		R: scaleChannel(c.R, f),
		G: scaleChannel(c.G, f),
		B: scaleChannel(c.B, f),
	}
}

func scaleChannel(value uint8, factor float64) uint8 {
	scaled := math.Round(float64(value) * factor)
	if scaled <= 0 {
		return 0
	}
	if scaled >= 255 {
		return 255
	}
	return uint8(scaled)
}

const (
	// envelopeSampleRate is the AirPlay PCM wire rate.
	envelopeSampleRate = 44100
	// attackTau and releaseTau are one-pole time constants.
	attackTau  = 0.040
	releaseTau = 0.600
)

var (
	attackCoefficient  = math.Exp(-1 / (attackTau * envelopeSampleRate))
	releaseCoefficient = math.Exp(-1 / (releaseTau * envelopeSampleRate))
)

// Envelope tracks a smoothed peak level with a fast attack and slow release.
type Envelope struct {
	level float64
}

// NewEnvelope returns an envelope at silence.
func NewEnvelope() *Envelope { return &Envelope{} }

// Update consumes interleaved 16-bit little-endian PCM and returns the level
// in [0,1]. The PCM is treated as 44.1 kHz stereo, the AirPlay wire format:
// each 4-byte frame advances one sample interval. Empty input (or a slice too
// short for one frame) leaves the level unchanged.
func (e *Envelope) Update(pcm []byte) float64 {
	frames := len(pcm) / 4
	for i := range frames {
		left := int16(binary.LittleEndian.Uint16(pcm[i*4:]))
		right := int16(binary.LittleEndian.Uint16(pcm[i*4+2:]))
		peak := sampleLevel(left, right)
		coefficient := releaseCoefficient
		if peak > e.level {
			coefficient = attackCoefficient
		}
		e.level = peak + (e.level-peak)*coefficient
	}
	return e.level
}

// sampleLevel converts a stereo frame to a magnitude in [0,1].
func sampleLevel(left, right int16) float64 {
	magnitude := math.Abs(float64(left))
	if r := math.Abs(float64(right)); r > magnitude {
		magnitude = r
	}
	magnitude /= 32768
	if magnitude > 1 {
		magnitude = 1
	}
	return magnitude
}
