package alac

import "errors"

// errShortFrame reports that a bitstream ran out of data.
var errShortFrame = errors.New("alac: unexpected end of frame")

// bitReader reads bits MSB first from a frame. It never panics: reading past
// the end of the buffer records errShortFrame, returns zeros and leaves the
// position pinned to the end. Unreading is supported so that the Rice decoder
// can peek ahead.
type bitReader struct {
	data   []byte
	pos    int // bit position
	failed bool
}

func (br *bitReader) err() error {
	if br.failed {
		return errShortFrame
	}
	return nil
}

// bits consumes n (0..32) bits and returns them MSB first.
func (br *bitReader) bits(n uint) uint32 {
	if n == 0 {
		return 0
	}
	if br.pos+int(n) > len(br.data)*8 {
		br.pos = len(br.data) * 8
		br.failed = true
		return 0
	}
	bytePos := br.pos >> 3
	offset := uint(br.pos & 7)
	br.pos += int(n)

	var w uint64
	nb := len(br.data) - bytePos
	if nb > 5 {
		nb = 5
	}
	for i := range nb {
		w = w<<8 | uint64(br.data[bytePos+i])
	}
	w <<= 64 - uint(nb*8) // left align the loaded bits
	w <<= offset          // drop the bits before the read position
	return uint32(w >> (64 - n))
}

// unread gives n (0..32) previously read bits back to the stream.
func (br *bitReader) unread(n int) {
	br.pos -= n
	if br.pos < 0 {
		br.pos = 0
	}
}
