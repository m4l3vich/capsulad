package oww

// convTimeGo is the portable implementation of a kh=3, kw=1 convolution (the
// time convolutions) for every output channel of one output position.
//
// It accumulates bias first, then (ic, ky), exactly like the arm64 kernel, so
// the two produce the same values lane for lane.
func convTimeGo(dst, src, weights, bias []float32, inC, outC, inW, outW int) {
	for oc := range outC {
		d := dst[oc*outW : (oc+1)*outW]
		setBias(d, bias[oc])
		woc := weights[oc*inC*3:]
		for ic := range inC {
			for ky := range 3 {
				axpy(d, src[(ky*inC+ic)*inW:], woc[ic*3+ky])
			}
		}
	}
}

// convFreqGo is the portable implementation of a kh=1, kw=3 convolution with
// one bin of frequency padding on each side. The three taps are applied to
// their valid output ranges in kx order, matching convGeneric and the arm64
// kernel.
func convFreqGo(dst, src, weights, bias []float32, inC, outC, inW, outW int) {
	for oc := range outC {
		d := dst[oc*outW : (oc+1)*outW]
		setBias(d, bias[oc])
		woc := weights[oc*inC*3:]
		for ic := range inC {
			row := src[ic*inW : (ic+1)*inW]
			w := woc[ic*3 : ic*3+3]
			// kx=0: output x uses input x-1, valid for x >= 1.
			if outW > 1 {
				axpy(d[1:], row[:outW-1], w[0])
			}
			// kx=1: no shift.
			axpy(d, row[:outW], w[1])
			// kx=2: output x uses input x+1, valid for x < outW-1.
			if outW > 1 {
				axpy(d[:outW-1], row[1:], w[2])
			}
		}
	}
}

// setBias fills a row with its channel bias.
func setBias(d []float32, b float32) {
	for i := range d {
		d[i] = b
	}
}
