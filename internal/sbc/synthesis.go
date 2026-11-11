package sbc

import (
	"encoding/binary"
	"errors"
)

// DecodeFrame decodes one complete SBC frame into S16LE PCM.
func (d *Decoder) DecodeFrame(frame []byte) ([]byte, int, int, error) {
	fi, err := parseHeader(frame)
	if err != nil {
		return nil, 0, 0, err
	}
	if fi.subbands != 8 {
		return nil, 0, 0, errors.New("sbc: only 8-subband frames are supported")
	}
	if fi.frameLen == 0 || len(frame) < fi.frameLen {
		return nil, 0, 0, errors.New("sbc: short frame")
	}
	if !d.started || d.rate != fi.rate || d.channels != fi.channels || d.subbands != fi.subbands {
		d.reset(fi.rate, fi.channels, fi.subbands)
	}
	n := fi.subbands
	ch := fi.channels
	sf := make([]int8, ch*n)
	pos := 4
	if fi.mode == 3 {
		fi.join = frame[pos]
		pos++
	}
	scaleBytes := ch * n / 2
	if pos+scaleBytes > fi.frameLen {
		return nil, 0, 0, errors.New("sbc: short scale factors")
	}
	for i := 0; i < scaleBytes; i++ {
		b := frame[pos]
		pos++
		sf[2*i] = int8(b >> 4)
		sf[2*i+1] = int8(b & 0x0f)
	}
	bits, err := d.bitAllocation(fi, sf)
	if err != nil {
		return nil, 0, 0, err
	}
	br := bitReader{data: frame[pos:fi.frameLen]}
	sub := make([]int32, fi.blocks*ch*n)
	if fi.mode == 3 {
		for blk := 0; blk < fi.blocks; blk++ {
			base := blk * ch * n
			joint := fi.join
			for sb := 0; sb < n; sb++ {
				v, err := readDequant(&br, bits[sb], int(sf[sb]))
				if err != nil {
					return nil, 0, 0, err
				}
				sub[base+sb] = v
			}
			for sb := 0; sb < n; sb++ {
				v, err := readDequant(&br, bits[n+sb], int(sf[n+sb]))
				if err != nil {
					return nil, 0, 0, err
				}
				if joint&0x80 != 0 {
					mid := sub[base+sb]
					sub[base+sb] = mid + v
					v = mid - v
				}
				joint <<= 1
				sub[base+n+sb] = v
			}
		}
	} else {
		iter := ch * n
		for blk := 0; blk < fi.blocks; blk++ {
			base := blk * iter
			for i := 0; i < iter; i++ {
				v, err := readDequant(&br, bits[i], int(sf[i]))
				if err != nil {
					return nil, 0, 0, err
				}
				sub[base+i] = v
			}
		}
	}
	pcm := make([]int16, fi.blocks*n*ch)
	strideShift := uint(0)
	if ch == 2 {
		strideShift = 1
	}
	for blk := 0; blk < fi.blocks; blk++ {
		if d.offset == 0 {
			n := len(d.filter[0])
			copy(d.filter[0][n-72:], d.filter[0][0:72])
			if ch == 2 {
				copy(d.filter[1][n-72:], d.filter[1][0:72])
			}
			d.offset = n - 80
		} else {
			d.offset -= 8
		}
		for c := 0; c < ch; c++ {
			src := sub[blk*ch*n+c*n : blk*ch*n+(c+1)*n]
			in := d.filter[c][d.offset : d.offset+8]
			dct2_8(in, src)
			out := pcm[(blk*n)<<strideShift:]
			if c == 1 {
				out = out[1:]
			}
			synthWindow80(out, d.filter[c][d.offset:], strideShift)
		}
	}
	raw := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(raw[i*2:], uint16(v))
	}
	return raw, fi.rate, ch, nil
}

func readDequant(br *bitReader, bits uint8, sf int) (int32, error) {
	if bits == 0 {
		return 0, nil
	}
	raw, err := br.read(uint(bits))
	if err != nil {
		return 0, err
	}
	return dequant(raw, sf, int(bits)), nil
}

// --- 8-point AAN DCT (Android SBC decoder, Apache-2.0) ----------------------

const (
	aanC4 = 759250125
	aanC6 = 410903207
	aanQ0 = 581104888
	aanQ1 = 1402911301
)

func mul32hi(u, v int32) int32 {
	return int32((int64(u) * int64(v)) >> 32)
}

func scale32(x int32, n uint) int32 {
	if n == 0 {
		return x
	}
	return (x + (1 << (n - 1))) >> n
}

func fixMul(k, x int32) int32 { return mul32hi(k, x) << 2 }

func dct2_8(out []int16, in []int32) {
	in0, in1, in2, in3 := in[0], in[1], in[2], in[3]
	in4, in5, in6, in7 := in[4], in[5], in[6], in[7]
	l00 := in0 + in7
	l01 := in1 + in6
	l02 := in2 + in5
	l03 := in3 + in4
	l04 := in3 - in4
	l05 := in2 - in5
	l06 := in1 - in6
	l07 := in0 - in7

	butterfly := func(x, y *int32) {
		*x += *y
		*y = *x - (*y << 1)
	}
	butterfly(&l00, &l03)
	butterfly(&l01, &l02)
	l02 += l03
	l02 = fixMul(aanC4, l02)
	butterfly(&l00, &l01)
	out[0] = int16(scale32(l00, 16))
	out[4] = int16(scale32(l01, 16))
	butterfly(&l03, &l02)
	out[6] = int16(scale32(l02, 15))
	out[2] = int16(scale32(l03, 16))

	l04 += l05
	l05 += l06
	l06 += l07
	l04 /= 2
	l05 /= 2
	l06 /= 2
	l07 /= 2
	l05 = fixMul(aanC4, l05)
	l25 := l06 - l04
	l25 = fixMul(aanC6, l25)
	l04 = fixMul(aanQ0, l04)
	l04 -= l25
	l06 = fixMul(aanQ1, l06)
	l06 -= l25

	butterfly(&l07, &l05)
	butterfly(&l05, &l04)
	out[3] = int16(scale32(l04, 15))
	out[5] = int16(scale32(l05, 15))
	butterfly(&l07, &l06)
	out[7] = int16(scale32(l06, 13))
	out[1] = int16(scale32(l07, 15))
}

func clip16(x int32) int32 {
	if x > 32767 {
		return 32767
	}
	if x < -32768 {
		return -32768
	}
	return x
}

// synthWindow80 is the generated 80-tap synthesis window. The constants are
// the A2DP prototype filter folded with the DCT scaling (Apache-2.0 Android).
func synthWindow80(out []int16, buf []int16, strideShift uint) {
	var a, b int32
	b = 0
	b += (int32(8235) * int32(buf[12])) >> 3
	b += (int32(-23167) * int32(buf[20])) >> 3
	b += (int32(26479) * int32(buf[28])) >> 2
	b += (int32(-17397) * int32(buf[36])) << 1
	b += (int32(9399) * int32(buf[44])) << 3
	b += (int32(17397) * int32(buf[52])) << 1
	b += (int32(26479) * int32(buf[60])) >> 2
	b += (int32(23167) * int32(buf[68])) >> 3
	b += (int32(8235) * int32(buf[76])) >> 3
	b /= 32768
	out[0<<strideShift] = int16(clip16(b))

	a = 0
	b = 0
	a += (int32(-3263) * int32(buf[5])) >> 5
	b += (int32(9293) * int32(buf[5])) >> 3
	a += (int32(29293) * int32(buf[11])) >> 5
	b += (int32(-6087) * int32(buf[11])) >> 2
	a += int32(-5229) * int32(buf[21])
	b += (int32(1247) * int32(buf[21])) << 3
	a += (int32(30835) * int32(buf[27])) >> 3
	b += (int32(-2893) * int32(buf[27])) << 3
	a += (int32(-27021) * int32(buf[37])) << 1
	b += (int32(23671) * int32(buf[37])) << 2
	a += (int32(31633) * int32(buf[43])) << 1
	b += (int32(18055) * int32(buf[43])) << 1
	a += (int32(17319) * int32(buf[53])) << 1
	b += (int32(11537) * int32(buf[53])) >> 1
	a += (int32(26663) * int32(buf[59])) >> 2
	b += (int32(1747) * int32(buf[59])) << 1
	a += (int32(4555) * int32(buf[69])) >> 1
	b += (int32(685) * int32(buf[69])) << 1
	a += (int32(12419) * int32(buf[75])) >> 4
	b += (int32(8721) * int32(buf[75])) >> 7
	a /= 32768
	out[1<<strideShift] = int16(clip16(a))
	b /= 32768
	out[7<<strideShift] = int16(clip16(b))

	a = 0
	b = 0
	a += (int32(-10385) * int32(buf[6])) >> 6
	b += (int32(11167) * int32(buf[6])) >> 4
	a += (int32(24995) * int32(buf[10])) >> 5
	b += (int32(-10337) * int32(buf[10])) >> 4
	a += (int32(-309) * int32(buf[22])) << 4
	b += (int32(1917) * int32(buf[22])) << 2
	a += (int32(9161) * int32(buf[26])) >> 3
	b += (int32(-30605) * int32(buf[26])) >> 1
	a += (int32(-23063) * int32(buf[38])) << 1
	b += (int32(8317) * int32(buf[38])) << 3
	a += (int32(27561) * int32(buf[42])) << 1
	b += (int32(9553) * int32(buf[42])) << 2
	a += (int32(2309) * int32(buf[54])) << 3
	b += (int32(22117) * int32(buf[54])) >> 4
	a += (int32(12705) * int32(buf[58])) >> 1
	b += (int32(16383) * int32(buf[58])) >> 2
	a += (int32(6239) * int32(buf[70])) >> 3
	b += (int32(7543) * int32(buf[70])) >> 3
	a += (int32(9251) * int32(buf[74])) >> 4
	b += (int32(8603) * int32(buf[74])) >> 6
	a /= 32768
	out[2<<strideShift] = int16(clip16(a))
	b /= 32768
	out[6<<strideShift] = int16(clip16(b))

	a = 0
	b = 0
	a += (int32(-16457) * int32(buf[7])) >> 6
	b += (int32(16913) * int32(buf[7])) >> 5
	a += (int32(19083) * int32(buf[9])) >> 5
	b += (int32(-8443) * int32(buf[9])) >> 7
	a += (int32(-23641) * int32(buf[23])) >> 2
	b += (int32(3687) * int32(buf[23])) << 1
	a += (int32(-29015) * int32(buf[25])) >> 4
	b += (int32(-301) * int32(buf[25])) << 5
	a += (int32(-12889) * int32(buf[39])) << 2
	b += (int32(15447) * int32(buf[39])) << 2
	a += (int32(6145) * int32(buf[41])) << 3
	b += (int32(10255) * int32(buf[41])) << 2
	a += (int32(24211) * int32(buf[55])) >> 1
	b += (int32(-18233) * int32(buf[55])) >> 3
	a += (int32(23469) * int32(buf[57])) >> 2
	b += (int32(9405) * int32(buf[57])) >> 1
	a += (int32(21223) * int32(buf[71])) >> 8
	b += (int32(1499) * int32(buf[71])) >> 1
	a += (int32(26913) * int32(buf[73])) >> 6
	b += (int32(26189) * int32(buf[73])) >> 7
	a /= 32768
	out[3<<strideShift] = int16(clip16(a))
	b /= 32768
	out[5<<strideShift] = int16(clip16(b))

	a = 0
	a += (int32(10445) * int32(buf[8])) >> 4
	a += (int32(-5297) * int32(buf[24])) << 1
	a += (int32(22299) * int32(buf[40])) << 2
	a += int32(10603) * int32(buf[56])
	a += (int32(9539) * int32(buf[72])) >> 4
	a /= 32768
	out[4<<strideShift] = int16(clip16(a))
}
