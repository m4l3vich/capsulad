//go:build !arm64

package oww

func convTime(dst, src, weights, bias []float32, inC, outC, inW, outW int) {
	convTimeGo(dst, src, weights, bias, inC, outC, inW, outW)
}

func convFreq(dst, src, weights, bias []float32, inC, outC, inW, outW int) {
	convFreqGo(dst, src, weights, bias, inC, outC, inW, outW)
}
