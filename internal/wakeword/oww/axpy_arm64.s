#include "textflag.h"

// func axpyNEON(w float32, dst, src []float32)
//
// dst[i] += w*src[i], four float32 lanes at a time. Slice lengths are assumed
// equal (the callers only pass matching row slices). The four-lane loop uses
// fused multiply-add; the scalar tail uses separate multiply and add.
TEXT ·axpyNEON(SB), NOSPLIT, $0-56
	MOVWU	w+0(FP), R5
	MOVD	dst_base+8(FP), R0
	MOVD	dst_len+16(FP), R2
	MOVD	src_base+32(FP), R1
	VMOV	R5, V0.S[0]
	VDUP	V0.S[0], V0.S4

	LSR	$2, R2, R3
	CBZ	R3, scalar

vector:
	VLD1	(R1), [V1.S4]
	VLD1	(R0), [V2.S4]
	VFMLA	V1.S4, V0.S4, V2.S4	// V2 += V1*V0
	VST1.P	[V2.S4], 16(R0)		// store and advance dst
	ADD	$16, R1
	SUB	$1, R3
	CBNZ	R3, vector

scalar:
	AND	$3, R2, R4
	CBZ	R4, done

scalarLoop:
	FMOVS	(R1), F1
	FMOVS	(R0), F2
	FMULS	F0, F1, F3		// F3 = F0*F1
	FADDS	F3, F2, F2		// F2 = F2+F3
	FMOVS	F2, (R0)
	ADD	$4, R1
	ADD	$4, R0
	SUB	$1, R4
	CBNZ	R4, scalarLoop

done:
	RET
