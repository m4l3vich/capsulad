package vkclient

import "encoding/binary"

// wavHeader builds the 44-byte canonical RIFF/WAVE PCM header for 16 kHz mono
// s16le carrying exactly n bytes of PCM.
//
// The service repacks each upload chunk as its own WAV file, so the header must
// describe that chunk's payload precisely: a streaming header with all-ones
// sizes is refused ("audio data is not valid WAV"/"Incoming chunk have not any
// data") and a header whose data size differs from the bytes in the chunk is
// refused with "PCM sizes mismatch <header> in header != <read> actually read".
func wavHeader(n int) []byte {
	h := make([]byte, 44)
	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:8], uint32(36+n))
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16)           // fmt chunk size
	binary.LittleEndian.PutUint16(h[20:22], 1)            // PCM
	binary.LittleEndian.PutUint16(h[22:24], 1)            // mono
	binary.LittleEndian.PutUint32(h[24:28], SampleRate)   // 16000
	binary.LittleEndian.PutUint32(h[28:32], SampleRate*2) // byte rate
	binary.LittleEndian.PutUint16(h[32:34], 2)            // block align
	binary.LittleEndian.PutUint16(h[34:36], 16)           // bits per sample
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], uint32(n))
	return h
}

// frameUploader turns arbitrary PCM writes into the upload blocks the service
// accepts: one self-contained WAV file per FrameMS, with real sizes. Capture
// arrives in 20 ms chunks, so a block usually spans several capture chunks; the
// trailing partial block is zero-padded on flush rather than sent short,
// because the server's frames are fixed-length by construction.
type frameUploader struct {
	opus    bool
	pending []byte
}

func newFrameUploader(format AudioFormat) *frameUploader {
	return &frameUploader{opus: format == FormatOggOpus}
}

// push returns every complete FrameMS block available after appending pcm.
func (u *frameUploader) push(pcm []byte) [][]byte {
	if len(pcm) == 0 {
		return nil
	}
	u.pending = append(u.pending, pcm...)
	var blocks [][]byte
	for len(u.pending) >= FrameBytes {
		block := make([]byte, FrameBytes)
		copy(block, u.pending[:FrameBytes])
		u.pending = u.pending[FrameBytes:]
		blocks = append(blocks, u.decorate(block))
	}
	if len(u.pending) == 0 {
		u.pending = nil
	}
	return blocks
}

// flush returns the zero-padded final block, or nil when nothing is pending.
func (u *frameUploader) flush() []byte {
	if len(u.pending) == 0 {
		return nil
	}
	block := make([]byte, FrameBytes)
	copy(block, u.pending)
	u.pending = nil
	return u.decorate(block)
}

func (u *frameUploader) decorate(block []byte) []byte {
	if u.opus {
		return block
	}
	out := make([]byte, 0, 44+len(block))
	out = append(out, wavHeader(len(block))...)
	out = append(out, block...)
	return out
}
