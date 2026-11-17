package alac

import "math/bits"

// riceThreshold is the largest unary quotient that is coded directly; larger
// quotients use the escape code (nine 1 bits followed by the raw value).
const riceThreshold = 8

// log2Floor returns floor(log2(v)), and 0 for v == 0, matching the reference
// decoders' av_log2/clz based formulation.
func log2Floor(v uint32) int {
	if v == 0 {
		return 0
	}
	return bits.Len32(v) - 1
}

// decodeScalar reads one Rice/Golomb coded value of bps bits.
func decodeScalar(br *bitReader, k int, bps int) uint32 {
	var x uint32
	for x <= riceThreshold && br.bits(1) != 0 {
		x++
	}
	if x > riceThreshold {
		// Escape: the value follows verbatim.
		return br.bits(uint(bps))
	}
	if k != 1 {
		extra := br.bits(uint(k))
		x *= (uint32(1) << uint(k)) - 1
		if extra > 1 {
			x += extra - 1
		} else {
			br.unread(1)
		}
	}
	return x
}

// riceDecode fills out with the residual samples of one channel.
func (d *Decoder) riceDecode(br *bitReader, out []int32, bps int, historyMult uint32) error {
	n := len(out)
	history := uint32(d.mb)
	signModifier := uint32(0)
	kb := int(d.kb)

	for i := 0; i < n; i++ {
		if err := br.err(); err != nil {
			return err
		}
		k := log2Floor((history >> 9) + 3)
		if k > kb {
			k = kb
		}
		x := decodeScalar(br, k, bps)
		x += signModifier
		signModifier = 0
		out[i] = int32(x>>1) ^ -int32(x&1)

		if x > 0xffff {
			history = 0xffff
		} else {
			history += x*historyMult - ((history * historyMult) >> 9)
		}

		// A low history means the encoder may have signalled a run of
		// zero residuals, which is coded as a block size.
		if history < 128 && i+1 < n {
			k = 7 - log2Floor(history) + int((history+16)>>6)
			blockSize := int(decodeScalar(br, k, 16))
			if blockSize > n-i-1 {
				blockSize = n - i - 1
			}
			for j := i + 1; j <= i+blockSize; j++ {
				out[j] = 0
			}
			i += blockSize
			signModifier = 1
			history = 0
		}
	}
	return br.err()
}

// signOnly returns the sign of v as -1, 0 or 1.
func signOnly(v int32) int32 {
	switch {
	case v < 0:
		return -1
	case v > 0:
		return 1
	default:
		return 0
	}
}

// firAdapt reconstructs samples from the residual buffer with the adaptive FIR
// predictor (prediction type 0). coefs[k] applies to the sample k+1 steps
// before the sample being predicted, which is the order the encoder writes the
// coefficients in. The coefficient table is adapted in place.
func firAdapt(residual, out []int32, bps int, coefs []int16, quant int) {
	n := len(out)
	out[0] = residual[0]
	if n <= 1 {
		return
	}
	order := len(coefs)
	if order == 0 {
		copy(out[1:], residual[1:])
		return
	}
	if order == 31 {
		// "11111": first order prediction without adaptation.
		for i := 1; i < n; i++ {
			out[i] = signExtend(out[i-1]+residual[i], bps)
		}
		return
	}

	// Warm-up samples are coded as a difference to the previous sample.
	for i := 1; i <= order && i < n; i++ {
		out[i] = signExtend(out[i-1]+residual[i], bps)
	}

	rounding := int32(1) << uint(quant-1)
	for i := order + 1; i < n; i++ {
		var sum int32
		for k := range order {
			sum += (out[i-1-k] - out[i-order-1]) * int32(coefs[k])
		}
		prev := out[i-order-1]
		val := int32((int64(sum) + int64(rounding)) >> uint(quant))
		val += prev + residual[i]
		out[i] = signExtend(val, bps)

		// Adapt the coefficients towards the error of this sample. The
		// reference walks the table from the oldest sample backwards, which
		// matters because the loop stops as soon as the error changes sign
		// and because it updates the running error as it goes.
		errVal := residual[i]
		errSign := signOnly(errVal)
		if errSign == 0 {
			continue
		}
		for k := order - 1; k >= 0; k-- {
			if errVal*errSign <= 0 {
				break
			}
			diff := prev - out[i-1-k]
			sign := signOnly(diff) * errSign
			coefs[k] -= int16(sign)
			magnitude := diff * sign
			errVal -= (magnitude >> uint(quant)) * int32(order-k)
		}
	}
}
