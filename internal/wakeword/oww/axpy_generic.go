//go:build !arm64

package oww

// axpy computes dst[i] += w*src[i] for the whole slice. It is the inner loop of
// every convolution; arm64 has a NEON implementation in axpy_arm64.s.
func axpy(dst, src []float32, w float32) {
	for i := range dst {
		dst[i] += w * src[i]
	}
}
