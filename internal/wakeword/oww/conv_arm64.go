//go:build arm64

package oww

// convTimeNEON computes every output channel for one output position of a
// kh=3, kw=1 convolution (see conv_arm64.s).
func convTimeNEON(dst, src, weights, bias []float32, inC, outC, inW, outW int)

// convFreqNEON computes every output channel for one output position of a
// kh=1, kw=3 convolution with one padded bin on each side. groups is the
// number of full four-lane interior groups.
func convFreqNEON(dst, src, weights, bias []float32, inC, outC, inW, outW, groups int)

func convTime(dst, src, weights, bias []float32, inC, outC, inW, outW int) {
	convTimeNEON(dst, src, weights, bias, inC, outC, inW, outW)
}

func convFreq(dst, src, weights, bias []float32, inC, outC, inW, outW int) {
	groups := 0
	if outW >= 2 {
		groups = (outW - 2) / 4
	}
	convFreqNEON(dst, src, weights, bias, inC, outC, inW, outW, groups)
}
