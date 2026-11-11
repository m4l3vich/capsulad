// Package sbc decodes Bluetooth SBC frames.
//
// The fixed-point allocation/dequantisation and the 8-subband synthesis
// follow the Apache-2.0 Android SBC decoder (Copyright 2014 The Android Open
// Source Project, Copyright 2003-2004 Open Interface North America, Inc.);
// the implementation here is a Go translation for the framework's A2DP sink.
package sbc

import (
	"errors"
	"fmt"
)

const syncWord = 0x9c

var (
	freqValues  = [4]int{16000, 32000, 44100, 48000}
	blockValues = [4]int{4, 8, 12, 16}
	bandValues  = [2]int{4, 8}
)

// Decoder keeps the synthesis filter history across SBC frames.
type Decoder struct {
	rate      int
	channels  int
	subbands  int
	filter    [2][]int16
	offset    int
	started   bool
	lastError error
}

func NewDecoder() *Decoder { return &Decoder{} }

func (d *Decoder) reset(rate, channels, subbands int) {
	d.rate, d.channels, d.subbands = rate, channels, subbands
	d.filter[0] = make([]int16, 768)
	d.filter[1] = make([]int16, 768)
	d.offset = 0
	d.started = true
}

// Decode decodes every complete SBC frame in data and returns S16LE PCM.
func (d *Decoder) Decode(data []byte) (pcm []byte, rate, channels int, err error) {
	for len(data) > 0 {
		off := 0
		for off < len(data) && data[off] != syncWord {
			off++
		}
		if off > 0 {
			data = data[off:]
		}
		if len(data) < 4 {
			break
		}
		n, err := frameLength(data)
		if err != nil {
			return pcm, d.rate, d.channels, err
		}
		if n > len(data) {
			// The caller will provide the rest on the next packet.
			break
		}
		part, r, c, err := d.DecodeFrame(data[:n])
		if err != nil {
			return pcm, d.rate, d.channels, err
		}
		pcm = append(pcm, part...)
		rate, channels = r, c
		data = data[n:]
	}
	if rate == 0 {
		rate, channels = d.rate, d.channels
	}
	return pcm, rate, channels, nil
}

type frameInfo struct {
	rate, blocks, mode, alloc, subbands, channels, bitpool int
	join                                                   byte
	frameLen                                               int
}

func parseHeader(frame []byte) (frameInfo, error) {
	var fi frameInfo
	if len(frame) < 4 {
		return fi, errors.New("sbc: short header")
	}
	if frame[0] != syncWord {
		return fi, errors.New("sbc: bad sync word")
	}
	d1 := frame[1]
	rateIndex := int(d1>>6) & 3
	blockIndex := int(d1>>4) & 3
	mode := int(d1>>2) & 3
	subbandIndex := int(d1) & 1
	fi.rate = freqValues[rateIndex]
	fi.blocks = blockValues[blockIndex]
	fi.mode = mode
	fi.alloc = int(d1>>1) & 1
	fi.subbands = bandValues[subbandIndex]
	fi.channels = 1
	if mode != 0 {
		fi.channels = 2
	}
	fi.bitpool = int(frame[2])
	fi.frameLen, _ = frameLength(frame)
	return fi, nil
}

// frameLength implements the SBC frame-length formula.
func frameLength(frame []byte) (int, error) {
	if len(frame) < 4 || frame[0] != syncWord {
		return 0, errors.New("sbc: bad frame")
	}
	fi, err := parseHeaderNoLength(frame)
	if err != nil {
		return 0, err
	}
	nbits := fi.blocks * fi.bitpool
	result := nbits
	if fi.mode == 3 { // joint stereo
		result += fi.subbands + 8*fi.subbands
	} else {
		if fi.mode == 1 { // dual channel
			result += nbits
		}
		if fi.mode == 0 {
			result += 4 * fi.subbands
		} else {
			result += 8 * fi.subbands
		}
	}
	return 4 + (result+7)/8, nil
}

// parseHeaderNoLength avoids the frameLength recursion.
func parseHeaderNoLength(frame []byte) (frameInfo, error) {
	var fi frameInfo
	if len(frame) < 4 || frame[0] != syncWord {
		return fi, errors.New("sbc: bad header")
	}
	d1 := frame[1]
	fi.rate = freqValues[int(d1>>6)&3]
	fi.blocks = blockValues[int(d1>>4)&3]
	fi.mode = int(d1>>2) & 3
	fi.alloc = int(d1>>1) & 1
	fi.subbands = bandValues[int(d1)&1]
	fi.channels = 1
	if fi.mode != 0 {
		fi.channels = 2
	}
	fi.bitpool = int(frame[2])
	return fi, nil
}

type bitReader struct {
	data []byte
	bit  int
}

func (r *bitReader) read(n uint) (uint32, error) {
	if n == 0 {
		return 0, nil
	}
	if n > 16 {
		return 0, fmt.Errorf("sbc: %d-bit read", n)
	}
	if r.bit+int(n) > len(r.data)*8 {
		return 0, errors.New("sbc: bitstream exhausted")
	}
	var v uint32
	for i := uint(0); i < n; i++ {
		b := r.data[r.bit>>3]
		bit := (b >> (7 - uint(r.bit&7))) & 1
		v = (v << 1) | uint32(bit)
		r.bit++
	}
	return v, nil
}

var dequantLongScaled = [17]uint32{
	0, 0, 0x1ee9e116, 0x0d3fa99c, 0x062ec69e, 0x02fddbfa, 0x0178d9f5,
	0x00baf129, 0x005d1abe, 0x002e760d, 0x00173536, 0x000b9928,
	0x0005cc37, 0x0002e604, 0x000172fc, 0x0000b97d, 0x00005cbe,
}

const dequantScaledOffset = 1555931970

func dequant(raw uint32, scaleFactor, bits int) int32 {
	if bits <= 1 {
		return 0
	}
	d := (raw*2 + 1) * dequantLongScaled[bits]
	result := int32(d - dequantScaledOffset)
	return result >> uint(15-scaleFactor)
}

var offset8 = [4][8]int8{
	{-2, 0, 0, 0, 0, 0, 0, 1},
	{-3, 0, 0, 0, 0, 0, 1, 2},
	{-4, 0, 0, 0, 0, 0, 1, 2},
	{-4, 0, 0, 0, 0, 0, 1, 2},
}

func (d *Decoder) bitAllocation(fi frameInfo, sf []int8) ([]uint8, error) {
	n := fi.subbands
	bits := make([]uint8, fi.channels*n)
	switch fi.mode {
	case 0: // mono
		need, count := bitneed(fi, sf, 0)
		adjust, excess := adjustToFitExcess(fi.bitpool, need, n, count)
		for sb := 0; sb < n; sb++ {
			excess = allocAdjusted(&bits[sb], int(need[sb])+adjust, excess)
		}
		for i := 0; excess > 0; i++ {
			excess = allocExcess(&bits[i%n], excess)
		}
	case 1: // dual channel
		for ch := 0; ch < 2; ch++ {
			need, count := bitneed(fi, sf, ch)
			adjust, excess := adjustToFitExcess(fi.bitpool, need, n, count)
			for sb := 0; sb < n; sb++ {
				excess = allocAdjusted(&bits[ch*n+sb], int(need[sb])+adjust, excess)
			}
			for i := 0; excess > 0; i++ {
				excess = allocExcess(&bits[ch*n+i%n], excess)
			}
		}
	default: // stereo or joint stereo
		need := make([]uint8, 2*n)
		count := 0
		c0, _ := bitneed(fi, sf, 0)
		c1, _ := bitneed(fi, sf, 1)
		copy(need[:n], c0)
		copy(need[n:], c1)
		for _, v := range need {
			if v > 1 {
				count += int(v)
			}
		}
		adjust, excess := adjustToFitExcess(fi.bitpool, need, 2*n, count)
		for sb := 0; sb < n; sb++ {
			excess = allocAdjusted(&bits[sb], int(need[sb])+adjust, excess)
			excess = allocAdjusted(&bits[n+sb], int(need[n+sb])+adjust, excess)
		}
		for i := 0; excess > 0 && i < n; i++ {
			excess = allocExcess(&bits[i], excess)
			if excess == 0 {
				break
			}
			excess = allocExcess(&bits[n+i], excess)
		}
	}
	return bits, nil
}

func bitneed(fi frameInfo, sf []int8, ch int) ([]uint8, int) {
	n := fi.subbands
	need := make([]uint8, n)
	base := ch * n
	count := 0
	if fi.alloc == 1 { // SNR
		for sb := 0; sb < n; sb++ {
			v := sf[base+sb]
			need[sb] = uint8(v)
			if v > 1 {
				count += int(v)
			}
		}
		return need, count
	}
	// Loudness: offset depends on sampling-frequency index.
	freqIndex := 0
	switch fi.rate {
	case 32000:
		freqIndex = 1
	case 44100:
		freqIndex = 2
	case 48000:
		freqIndex = 3
	}
	off := offset8[freqIndex]
	for sb := 0; sb < n; sb++ {
		v := int(sf[base+sb])
		if v != 0 {
			v -= int(off[sb])
			if v > 0 {
				v /= 2
			}
			v += 5
		}
		need[sb] = uint8(v)
		if v > 1 {
			count += v
		}
	}
	return need, count
}

func adjustToFitExcess(bitpool int, need []uint8, subbands, count int) (int, int) {
	bitadjust := 8
	if count > bitpool {
		bitadjust = -8
	}
	maxAdjust := 0
	chop := 8
	for count != bitpool && chop != 0 {
		total := 0
		for i := 0; i < subbands; i++ {
			n := int(need[i]) + bitadjust
			if n < 2 {
				n = 0
			} else if n > 16 {
				n = 16
			}
			total += n
		}
		chop >>= 1
		if total > bitpool {
			bitadjust -= chop
		} else {
			maxAdjust = bitadjust
			count = total
			bitadjust += chop
		}
	}
	return maxAdjust, bitpool - count
}

func allocAdjusted(dest *uint8, bits, excess int) int {
	if bits < 16 {
		if bits > 1 {
			if excess > 0 {
				bits++
				excess--
			}
		} else if bits == 1 && excess > 1 {
			bits = 2
			excess -= 2
		} else {
			bits = 0
		}
	} else {
		bits = 16
	}
	*dest = uint8(bits)
	return excess
}

func allocExcess(dest *uint8, excess int) int {
	if *dest < 16 {
		*dest++
		return excess - 1
	}
	return excess
}
