// Package alac decodes Apple Lossless (ALAC) audio frames as they appear in
// AirPlay/RAOP RTP payloads: one call to Decode consumes exactly one ALAC
// frame (one element group terminated by an END tag) and appends interleaved
// 16-bit little-endian PCM to the caller's buffer.
//
// The bitstream layout implemented here is the one described by the ALAC
// magic cookie (ALACSpecificConfig) and used by every ALAC encoder in the
// wild: a 3-bit element tag (SCE/CPE/LFE/END), the element header, optionally
// a Rice/Golomb coded residual that is run through the adaptive FIR
// predictor, and finally optional stereo decorrelation plus the "extra bits"
// that carry the low-order bytes of 20/24-bit samples.
//
// The decoder is allocation free per frame apart from the growth of the
// caller's destination slice: all scratch buffers are allocated once in
// NewDecoder and reused. A Decoder is not safe for concurrent use.
package alac

import (
	"errors"
	"fmt"
)

// Element tags of the ALAC bitstream (ALACRawDataBlockType).
const (
	elementSCE = 0 // single channel element
	elementCPE = 1 // channel pair element
	elementLFE = 3 // low frequency element
	elementEND = 7 // end of frame
)

// maxPredictorOrder is the largest predictor order the bitstream can express
// (5 bits, values above 31 are rejected by the reference decoders).
const maxPredictorOrder = 32

// maxFrameSamples bounds the frame length taken from a magic cookie so that a
// corrupt cookie cannot make NewDecoder allocate absurd buffers.
const maxFrameSamples = 16384

// Config describes the stream a Decoder is set up for.
type Config struct {
	FrameLength uint32 // samples per frame, 4096 (buffered) or 352 (realtime)
	BitDepth    uint8  // 16, 20, 24 or 32
	Channels    uint8  // 1 or 2 (1 also covers mono in a stereo layout)
	SampleRate  uint32 // 44100 or 48000
	// MagicCookie is the optional ALAC specific info. It may be the bare
	// 24-byte ALACSpecificConfig, a 28-byte 'alac' sample entry payload
	// (4-byte version/flags followed by the config) or the 36-byte codec
	// extradata (size/'alac'/version followed by the config). Its
	// frameLength, bitDepth and sampleRate must agree with the fields above
	// and its tuning parameters (pb/mb/kb) are used for the residual
	// decoding. When empty, the standard cookie is built from the fields
	// above (pb=40, mb=10, kb=14, maxRun=255).
	MagicCookie []byte
}

// Decoder decodes ALAC frames for one stream configuration.
type Decoder struct {
	frameLength int
	bitDepth    uint8
	channels    int
	sampleRate  uint32
	pb          uint8
	mb          uint8
	kb          uint8
	// maxRun is carried by the magic cookie but, as its description in the
	// ALAC sample code says, it is unused: the run of zero residuals is
	// coded for whatever length occurs.
	maxRun uint16

	// Per-channel scratch, allocated once and reused by every Decode call.
	errBuf   [2][]int32 // Rice-decoded residuals
	outBuf   [2][]int32 // reconstructed samples
	extraBuf [2][]int32 // low-order "extra" bits of 20/24-bit samples
	coefs    [2][maxPredictorOrder]int16
}

// ALACSpecificConfig field offsets inside the 24-byte structure.
const (
	cookieSize          = 24
	cookieOffFrameLen   = 0
	cookieOffBitDepth   = 5
	cookieOffPB         = 6
	cookieOffMB         = 7
	cookieOffKB         = 8
	cookieOffChannels   = 9
	cookieOffMaxRun     = 10
	cookieOffMaxFrame   = 12
	cookieOffAvgBitRate = 16
	cookieOffSampleRate = 20
)

// standard tuning parameters of the ALACSpecificConfig.
const (
	standardPB     = 40
	standardMB     = 10
	standardKB     = 14
	standardMaxRun = 255
)

// NewDecoder validates cfg and returns a Decoder for it.
func NewDecoder(cfg Config) (*Decoder, error) {
	switch cfg.BitDepth {
	case 16, 20, 24, 32:
	default:
		return nil, fmt.Errorf("alac: unsupported bit depth %d", cfg.BitDepth)
	}
	if cfg.Channels != 1 && cfg.Channels != 2 {
		return nil, fmt.Errorf("alac: unsupported channel count %d", cfg.Channels)
	}
	if cfg.SampleRate == 0 {
		return nil, errors.New("alac: sample rate must not be zero")
	}
	if cfg.FrameLength == 0 || cfg.FrameLength > maxFrameSamples {
		return nil, fmt.Errorf("alac: invalid frame length %d", cfg.FrameLength)
	}

	d := &Decoder{
		frameLength: int(cfg.FrameLength),
		bitDepth:    cfg.BitDepth,
		channels:    int(cfg.Channels),
		sampleRate:  cfg.SampleRate,
		pb:          standardPB,
		mb:          standardMB,
		kb:          standardKB,
		maxRun:      standardMaxRun,
	}

	if len(cfg.MagicCookie) > 0 {
		c, err := parseCookie(cfg.MagicCookie)
		if err != nil {
			return nil, err
		}
		if c.frameLength != cfg.FrameLength {
			return nil, fmt.Errorf("alac: cookie frame length %d does not match configured %d", c.frameLength, cfg.FrameLength)
		}
		if c.bitDepth != cfg.BitDepth {
			return nil, fmt.Errorf("alac: cookie bit depth %d does not match configured %d", c.bitDepth, cfg.BitDepth)
		}
		if c.sampleRate != cfg.SampleRate {
			return nil, fmt.Errorf("alac: cookie sample rate %d does not match configured %d", c.sampleRate, cfg.SampleRate)
		}
		d.pb = c.pb
		d.mb = c.mb
		d.kb = c.kb
		d.maxRun = c.maxRun
	}

	for ch := range 2 {
		d.errBuf[ch] = make([]int32, d.frameLength)
		d.outBuf[ch] = make([]int32, d.frameLength)
		d.extraBuf[ch] = make([]int32, d.frameLength)
	}
	return d, nil
}

// FrameSamples reports the number of samples per frame this decoder produces
// when a frame does not carry an explicit sample count.
func (d *Decoder) FrameSamples() int { return d.frameLength }

// Decode decodes one ALAC frame (the payload of a single RTP packet, without
// any RTP header) and returns the interleaved 16-bit little-endian PCM for it
// appended to dst. Bit depths above 16 are reduced to 16 bits by a right shift
// (24-bit samples therefore become v>>8). The number of decoded samples is
// (len(result)-len(dst)) / (2*channels).
//
// A frame that cannot be decoded leaves dst untouched and returns an error:
// nothing is ever emitted for the parts that were understood.
func (d *Decoder) Decode(frame []byte, dst []byte) ([]byte, error) {
	start := len(dst)
	out, err := d.decode(frame, dst)
	if err != nil {
		return dst[:start], err
	}
	return out, nil
}

func (d *Decoder) decode(frame []byte, dst []byte) ([]byte, error) {
	if len(frame) < 4 {
		return dst, errors.New("alac: frame too short")
	}
	br := bitReader{data: frame}
	decoded := false
	for {
		tag := br.bits(3)
		if err := br.err(); err != nil {
			return dst, err
		}
		if tag == elementEND {
			break
		}
		if decoded {
			return dst, fmt.Errorf("alac: unsupported frame with multiple elements (tag %d)", tag)
		}
		var channels int
		switch tag {
		case elementSCE, elementLFE:
			channels = 1
		case elementCPE:
			channels = 2
		default:
			return dst, fmt.Errorf("alac: unsupported element tag %d", tag)
		}
		var err error
		dst, err = d.decodeElement(&br, channels, dst)
		if err != nil {
			return dst, err
		}
		decoded = true
	}
	if !decoded {
		return dst, errors.New("alac: frame contains no audio element")
	}
	return dst, nil
}

// decodeElement decodes one channel element and appends its PCM to dst.
func (d *Decoder) decodeElement(br *bitReader, channels int, dst []byte) ([]byte, error) {
	br.bits(4)  // element instance tag
	br.bits(12) // reserved / unused header bits
	hasSize := br.bits(1) != 0
	extraBits := int(br.bits(2)) * 8
	verbatim := br.bits(1) != 0
	if err := br.err(); err != nil {
		return dst, err
	}

	samples := d.frameLength
	if hasSize {
		n := br.bits(32)
		if err := br.err(); err != nil {
			return dst, err
		}
		if n == 0 || int(n) > d.frameLength {
			return dst, fmt.Errorf("alac: frame declares %d samples, maximum is %d", n, d.frameLength)
		}
		samples = int(n)
	}

	// Number of bits each coded sample occupies. The extra bytes carry the
	// low-order bits of 20/24-bit samples outside the compressed stream.
	bps := int(d.bitDepth) - extraBits + channels - 1
	if bps < 1 || bps > 32 {
		return dst, fmt.Errorf("alac: unsupported coded sample size %d bits", bps)
	}

	decorrShift, decorrWeight := uint32(0), uint32(0)
	if !verbatim {
		// The reference decoder rejects compressed elements whose cookie has
		// no Rice kmodifier; only "verbatim" streams may carry one.
		if d.kb == 0 {
			return dst, errors.New("alac: compressed element with a zero Rice kmodifier")
		}
		decorrShift = br.bits(8)
		decorrWeight = br.bits(8)

		var (
			predType [2]uint32
			quant    [2]uint32
			order    [2]int
			histMult [2]uint32
		)
		for ch := range channels {
			predType[ch] = br.bits(4)
			quant[ch] = br.bits(4)
			riceModifier := br.bits(3)
			order[ch] = int(br.bits(5))
			if order[ch] > maxPredictorOrder {
				return dst, fmt.Errorf("alac: invalid predictor order %d", order[ch])
			}
			if order[ch] > samples {
				return dst, fmt.Errorf("alac: predictor order %d exceeds frame size %d", order[ch], samples)
			}
			for i := range order[ch] {
				// The first coefficient read applies to the most recent
				// sample; coefs[ch][k] applies to the sample k+1 steps back.
				d.coefs[ch][i] = int16(br.bits(16))
			}
			// The reference encoder always writes 4 here, scaling the
			// cookie's history multiplier by riceModifier/4.
			histMult[ch] = riceModifier * uint32(d.pb) / 4
			if err := br.err(); err != nil {
				return dst, err
			}
		}

		if extraBits > 0 {
			for i := range samples {
				for ch := range channels {
					d.extraBuf[ch][i] = int32(br.bits(uint(extraBits)))
				}
			}
			if err := br.err(); err != nil {
				return dst, err
			}
		}

		for ch := range channels {
			residual := d.errBuf[ch][:samples]
			out := d.outBuf[ch][:samples]
			if err := d.riceDecode(br, residual, bps, histMult[ch]); err != nil {
				return dst, err
			}
			switch predType[ch] {
			case 0: // adaptive FIR predictor
				if order[ch] > 0 && quant[ch] == 0 {
					return dst, errors.New("alac: predictor quantization must not be zero")
				}
				firAdapt(residual, out, bps, d.coefs[ch][:order[ch]], int(quant[ch]))
			case 1:
				// Escape: the residual is the sample, no prediction is
				// applied. The reference encoder never emits this, but the
				// bitstream reserves the value for the uncompressed
				// per-sample path.
				copy(out, residual)
			default:
				return dst, fmt.Errorf("alac: unsupported predictor type %d", predType[ch])
			}
		}
	} else {
		// Uncompressed ("verbatim") element: raw samples, channel
		// interleaved, sign extended to the configured bit depth.
		for i := range samples {
			for ch := range channels {
				d.outBuf[ch][i] = signExtend(int32(br.bits(uint(d.bitDepth))), int(d.bitDepth))
			}
		}
		if err := br.err(); err != nil {
			return dst, err
		}
		extraBits = 0
	}

	if channels == 2 && decorrWeight != 0 {
		if decorrShift > 31 {
			return dst, fmt.Errorf("alac: invalid decorrelation shift %d", decorrShift)
		}
		// The first element channel carries mid/right, the second the
		// left-minus-right difference.
		for i := range samples {
			mid := d.outBuf[0][i]
			diff := d.outBuf[1][i]
			right := mid - int32((int64(diff)*int64(decorrWeight))>>decorrShift)
			d.outBuf[0][i] = right + diff
			d.outBuf[1][i] = right
		}
	}

	if extraBits > 0 {
		for i := range samples {
			for ch := range channels {
				d.outBuf[ch][i] = d.outBuf[ch][i]<<uint(extraBits) | d.extraBuf[ch][i]
			}
		}
	}

	// A stereo element decoded by a mono decoder is reduced to its left
	// channel; a mono element cannot be expanded to the configured stereo
	// layout.
	if channels == 1 && d.channels == 2 {
		return dst, fmt.Errorf("alac: frame has %d channel, decoder configured for %d", channels, d.channels)
	}

	shift := uint(0)
	if d.bitDepth > 16 {
		shift = uint(d.bitDepth) - 16
	}
	for i := range samples {
		for ch := range d.channels {
			v := int16(d.outBuf[ch][i] >> shift)
			dst = append(dst, byte(v), byte(v>>8))
		}
	}
	return dst, nil
}

// cookieFields holds the decoding relevant fields of an ALACSpecificConfig.
type cookieFields struct {
	frameLength uint32
	bitDepth    uint8
	pb          uint8
	mb          uint8
	kb          uint8
	channels    uint8
	maxRun      uint16
	sampleRate  uint32
}

// parseCookie accepts the three layouts of the ALAC specific info found in the
// wild and returns the fields the decoder needs.
func parseCookie(b []byte) (cookieFields, error) {
	var c cookieFields
	var base int
	switch len(b) {
	case cookieSize: // bare ALACSpecificConfig
		base = 0
	case cookieSize + 4: // 'alac' sample entry payload (version/flags + config)
		base = 4
	case cookieSize + 12: // codec extradata (size/'alac'/version + config)
		if string(b[4:8]) != "alac" {
			return c, errors.New("alac: magic cookie is not an 'alac' box")
		}
		base = 12
	default:
		return c, fmt.Errorf("alac: unsupported magic cookie length %d", len(b))
	}
	c.frameLength = be32(b[base+cookieOffFrameLen:])
	c.bitDepth = b[base+cookieOffBitDepth]
	c.pb = b[base+cookieOffPB]
	c.mb = b[base+cookieOffMB]
	c.kb = b[base+cookieOffKB]
	c.channels = b[base+cookieOffChannels]
	c.maxRun = uint16(be32(b[base+cookieOffMaxRun:]) >> 16)
	c.sampleRate = be32(b[base+cookieOffSampleRate:])
	if c.frameLength == 0 || c.frameLength > maxFrameSamples {
		return c, fmt.Errorf("alac: cookie declares invalid frame length %d", c.frameLength)
	}
	switch c.bitDepth {
	case 16, 20, 24, 32:
	default:
		return c, fmt.Errorf("alac: cookie declares unsupported bit depth %d", c.bitDepth)
	}
	return c, nil
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// signExtend sign extends the low bits of v, which is bps bits wide.
func signExtend(v int32, bps int) int32 {
	if bps >= 32 {
		return v
	}
	shift := uint(32 - bps)
	return v << shift >> shift
}
