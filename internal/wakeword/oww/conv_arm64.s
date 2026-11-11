#include "textflag.h"

// Batched convolution kernels. Each call computes every output channel for one
// output time position, keeping the destination row in a vector register across
// all input channels and taps. This removes the per-tap call and Go indexing
// overhead that dominated the profile and avoids the read-modify-write of the
// destination for every tap.
//
// Accumulation order is bias, then (input channel, tap), matching convGeneric
// and the portable kernels in conv.go.

// func convTimeNEON(dst, src, weights, bias []float32, inC, outC, inW, outW int)
//
// kh=3, kw=1, no padding. weight layout is [oc][ic][ky].
TEXT ·convTimeNEON(SB), NOSPLIT, $0-128
	MOVD	dst_base+0(FP), R0
	MOVD	src_base+24(FP), R1
	MOVD	weights_base+48(FP), R2
	MOVD	bias_base+72(FP), R3
	MOVD	inC+96(FP), R4
	MOVD	outC+104(FP), R5
	MOVD	inW+112(FP), R6
	MOVD	outW+120(FP), R7
	CBZ	R5, doneT

	LSL	$2, R6			// rowBytes
	LSL	$2, R7, R25		// outW*4
	MUL	R4, R6, R8		// position stride = inC*rowBytes
	LSL	$1, R8, R23		// 2*position stride
	MOVD	$12, R21
	MUL	R21, R4, R24		// inC*12
	MOVD	R0, R10			// dst row
	MOVD	R2, R11			// weight row

ocLoopT:
	LSR	$2, R7, R12		// full vector groups
	AND	$3, R7, R13		// scalar tail lanes
	MOVD	$0, R14			// byte offset within the row

gLoopT:
	CBZ	R12, tailT
	ADD	R1, R14, R19		// source lane group for ky=0, ic=0
	MOVD	R11, R20
	MOVD	R4, R15
	MOVWU	(R3), R21
	VMOV	R21, V4.S[0]
	VDUP	V4.S[0], V0.S4		// accumulator = bias

icLoopT:
	FMOVS	(R20), F4
	VDUP	V4.S[0], V1.S4
	VLD1	(R19), [V2.S4]
	VFMLA	V2.S4, V1.S4, V0.S4
	FMOVS	4(R20), F4
	VDUP	V4.S[0], V1.S4
	ADD	R19, R8, R22
	VLD1	(R22), [V2.S4]
	VFMLA	V2.S4, V1.S4, V0.S4
	FMOVS	8(R20), F4
	VDUP	V4.S[0], V1.S4
	ADD	R19, R23, R22
	VLD1	(R22), [V2.S4]
	VFMLA	V2.S4, V1.S4, V0.S4
	ADD	R6, R19, R19
	ADD	$12, R20, R20
	SUB	$1, R15
	CBNZ	R15, icLoopT

	ADD	R10, R14, R22
	VST1.P	[V0.S4], 16(R22)
	ADD	$16, R14, R14
	SUB	$1, R12
	B	gLoopT

tailT:
	CBZ	R13, nextOcT
tailLoopT:
	FMOVS	(R3), F0		// accumulator = bias
	ADD	R1, R14, R19
	MOVD	R11, R20
	MOVD	R4, R15
tailICT:
	FMOVS	(R20), F1
	FMOVS	(R19), F2
	FMADDS	F2, F0, F1, F0
	ADD	R19, R8, R22
	FMOVS	4(R20), F1
	FMOVS	(R22), F2
	FMADDS	F2, F0, F1, F0
	ADD	R19, R23, R22
	FMOVS	8(R20), F1
	FMOVS	(R22), F2
	FMADDS	F2, F0, F1, F0
	ADD	R6, R19, R19
	ADD	$12, R20, R20
	SUB	$1, R15
	CBNZ	R15, tailICT
	ADD	R10, R14, R22
	FMOVS	F0, (R22)
	ADD	$4, R14, R14
	SUB	$1, R13
	CBNZ	R13, tailLoopT

nextOcT:
	ADD	R25, R10, R10
	ADD	R24, R11, R11
	ADD	$4, R3, R3
	SUB	$1, R5
	CBNZ	R5, ocLoopT
doneT:
	RET

// func convFreqNEON(dst, src, weights, bias []float32, inC, outC, inW, outW, groups int)
//
// kh=1, kw=3, one padded bin on each side. weight layout is [oc][ic][kx].
// groups is the number of full four-lane interior groups: the vectorised range
// is x in [1, 1+4*groups), the scalar range is the rest. x=0 and the final lane
// use only the taps that exist.
TEXT ·convFreqNEON(SB), NOSPLIT, $0-136
	MOVD	dst_base+0(FP), R0
	MOVD	src_base+24(FP), R1
	MOVD	weights_base+48(FP), R2
	MOVD	bias_base+72(FP), R3
	MOVD	inC+96(FP), R4
	MOVD	outC+104(FP), R5
	MOVD	inW+112(FP), R6
	MOVD	outW+120(FP), R7
	MOVD	groups+128(FP), R8
	CBZ	R5, doneF

	LSL	$2, R6			// rowBytes
	LSL	$2, R7, R25		// outW*4
	MOVD	$12, R21
	MUL	R21, R4, R24		// inC*12
	MOVD	R0, R10
	MOVD	R2, R11

ocLoopF:
	// x = 0: kx=1 always, kx=2 only when outW > 1.
	FMOVS	(R3), F0
	MOVD	R4, R14
	MOVD	R1, R19
	MOVD	R11, R20
x0LoopF:
	FMOVS	4(R20), F1
	FMOVS	(R19), F2
	FMADDS	F2, F0, F1, F0
	CMP	$1, R7
	BEQ	x0skip2F
	FMOVS	8(R20), F1
	FMOVS	4(R19), F2
	FMADDS	F2, F0, F1, F0
x0skip2F:
	ADD	R6, R19, R19
	ADD	$12, R20, R20
	SUB	$1, R14
	CBNZ	R14, x0LoopF
	FMOVS	F0, (R10)
	CMP	$1, R7
	BEQ	nextOcF

	// Interior vectors: x starts at 1.
	MOVD	$1, R13
	MOVD	R8, R12
	CBZ	R12, tailPrepF
gLoopF:
	SUB	$1, R13, R21
	LSL	$2, R21
	ADD	R1, R21, R19
	MOVD	R11, R20
	MOVD	R4, R14
	MOVWU	(R3), R21
	VMOV	R21, V4.S[0]
	VDUP	V4.S[0], V0.S4
icLoopF:
	FMOVS	(R20), F4
	VDUP	V4.S[0], V1.S4
	VLD1	(R19), [V2.S4]
	VFMLA	V2.S4, V1.S4, V0.S4
	FMOVS	4(R20), F4
	VDUP	V4.S[0], V1.S4
	ADD	$4, R19, R22
	VLD1	(R22), [V2.S4]
	VFMLA	V2.S4, V1.S4, V0.S4
	FMOVS	8(R20), F4
	VDUP	V4.S[0], V1.S4
	ADD	$8, R19, R22
	VLD1	(R22), [V2.S4]
	VFMLA	V2.S4, V1.S4, V0.S4
	ADD	R6, R19, R19
	ADD	$12, R20, R20
	SUB	$1, R14
	CBNZ	R14, icLoopF
	LSL	$2, R13, R21
	ADD	R10, R21, R22
	VST1.P	[V0.S4], 16(R22)
	ADD	$4, R13, R13
	SUB	$1, R12
	CBNZ	R12, gLoopF

tailPrepF:
	SUB	$1, R7, R23		// final x
tailLoopF:
	CMP	R23, R13
	BGE	lastLaneF
	FMOVS	(R3), F0
	LSL	$2, R13, R21
	ADD	R1, R21, R19
	MOVD	R11, R20
	MOVD	R4, R14
tailICF:
	FMOVS	(R20), F1
	SUB	$4, R19, R22
	FMOVS	(R22), F2
	FMADDS	F2, F0, F1, F0
	FMOVS	4(R20), F1
	FMOVS	(R19), F2
	FMADDS	F2, F0, F1, F0
	FMOVS	8(R20), F1
	FMOVS	4(R19), F2
	FMADDS	F2, F0, F1, F0
	ADD	R6, R19, R19
	ADD	$12, R20, R20
	SUB	$1, R14
	CBNZ	R14, tailICF
	LSL	$2, R13, R21
	ADD	R10, R21, R22
	FMOVS	F0, (R22)
	ADD	$1, R13, R13
	B	tailLoopF

lastLaneF:
	FMOVS	(R3), F0
	LSL	$2, R23, R21
	ADD	R1, R21, R19
	MOVD	R11, R20
	MOVD	R4, R14
lastICF:
	FMOVS	(R20), F1
	SUB	$4, R19, R22
	FMOVS	(R22), F2
	FMADDS	F2, F0, F1, F0
	FMOVS	4(R20), F1
	FMOVS	(R19), F2
	FMADDS	F2, F0, F1, F0
	ADD	R6, R19, R19
	ADD	$12, R20, R20
	SUB	$1, R14
	CBNZ	R14, lastICF
	LSL	$2, R23, R21
	ADD	R10, R21, R22
	FMOVS	F0, (R22)

nextOcF:
	ADD	R25, R10, R10
	ADD	R24, R11, R11
	ADD	$4, R3, R3
	SUB	$1, R5
	CBNZ	R5, ocLoopF
doneF:
	RET
