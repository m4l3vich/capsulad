//go:build arm64

package oww

// axpyNEON computes dst[i] += w*src[i] with four-lane NEON (see axpy_arm64.s).
func axpyNEON(w float32, dst, src []float32)

// axpy is the ARM64 build's convolution inner loop.
func axpy(dst, src []float32, w float32) {
	axpyNEON(w, dst, src)
}
