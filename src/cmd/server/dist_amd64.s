#include "textflag.h"

// func dist2SIMD(q, ref *vec16) int64
//
// vec16 = [16]int8 (16 bytes, one XMM load).
// Sign-extend both to int16, subtract, square via VPMADDWD, reduce to int64.
// Max squared distance: 16 × (254²) = 1 032 256 — fits in int32.
//
TEXT ·dist2SIMD(SB), NOSPLIT, $0-24
    MOVQ    q+0(FP), SI
    MOVQ    ref+8(FP), DI

    VMOVDQU (SI), X0            // 16 × int8 from q
    VMOVDQU (DI), X1            // 16 × int8 from ref

    VPMOVSXBW X0, Y2            // sign-extend → 16 × int16
    VPMOVSXBW X1, Y3            // sign-extend → 16 × int16
    VPSUBW  Y3, Y2, Y4          // 16 int16 differences  (range [-254, 254])
    VPMADDWD Y4, Y4, Y5         // 8 int32: d[2i]² + d[2i+1]²  (max 129 032)

    // Reduce 8 int32 → 1 int32 (max 1 032 256, safe in int32)
    VEXTRACTI128 $1, Y5, X6     // upper 4 int32
    VPADDD  X5, X6, X5          // 4 int32
    VPSRLDQ $8, X5, X6
    VPADDD  X5, X6, X5          // 2 int32
    VPSRLDQ $4, X5, X6
    VPADDD  X5, X6, X5          // 1 int32

    MOVL    X5, AX
    MOVLQSX AX, AX              // sign-extend to int64; value is non-negative
    MOVQ    AX, ret+16(FP)
    VZEROUPPER
    RET

// func centroidDistSIMD(q *vec16, centroid *int16) int64
//
// q: [16]int8, centroid: [16]int16 (32 bytes, one YMM load).
// Sign-extend q to int16, subtract centroid, square via VPMADDWD, reduce.
// Max squared distance: 16 × 254² = 1 032 256 — fits in int32.
//
TEXT ·centroidDistSIMD(SB), NOSPLIT, $0-24
    MOVQ    q+0(FP), SI
    MOVQ    centroid+8(FP), DI

    VMOVDQU (SI), X0
    VPMOVSXBW X0, Y0            // 16 int8 → 16 int16

    VMOVDQU (DI), Y1            // 16 int16 from centroid (32 bytes)

    VPSUBW  Y1, Y0, Y0          // diff = q - centroid  (range [-254, 254])
    VPMADDWD Y0, Y0, Y0         // 8 int32: diff[2i]² + diff[2i+1]²

    VEXTRACTI128 $1, Y0, X1
    VPADDD  X0, X1, X0          // 4 int32
    VPSRLDQ $8, X0, X1
    VPADDD  X0, X1, X0          // 2 int32
    VPSRLDQ $4, X0, X1
    VPADDD  X0, X1, X0          // 1 int32

    MOVL    X0, AX
    MOVLQSX AX, AX
    MOVQ    AX, ret+16(FP)
    VZEROUPPER
    RET

// func lboundSIMD(q *vec16, lo, hi *int8) int64
//
// Computes sum of max(0, lo[i]-q[i], q[i]-hi[i])^2.
// Works in int16 to avoid overflow; max sum: 16 × 254² = 1 032 256 → fits in int32.
//
TEXT ·lboundSIMD(SB), NOSPLIT, $0-32
    MOVQ    q+0(FP), SI
    MOVQ    lo+8(FP), DI
    MOVQ    hi+16(FP), BX

    VMOVDQU (SI), X0
    VPMOVSXBW X0, Y0            // q:  16 int16

    VMOVDQU (DI), X1
    VPMOVSXBW X1, Y1            // lo: 16 int16

    VMOVDQU (BX), X2
    VPMOVSXBW X2, Y2            // hi: 16 int16

    VPSUBW  Y0, Y1, Y3          // d_lo = lo - q  (positive when q < lo)
    VPSUBW  Y2, Y0, Y4          // d_hi = q - hi  (positive when q > hi)

    VPXOR   Y5, Y5, Y5          // zero
    VPMAXSW Y5, Y3, Y3          // clip d_lo ≥ 0
    VPMAXSW Y5, Y4, Y4          // clip d_hi ≥ 0
    VPMAXSW Y3, Y4, Y0          // d = max(d_lo, d_hi)

    VPMADDWD Y0, Y0, Y0         // 8 int32: d[2i]² + d[2i+1]²

    VEXTRACTI128 $1, Y0, X1
    VPADDD  X0, X1, X0
    VPSRLDQ $8, X0, X1
    VPADDD  X0, X1, X0
    VPSRLDQ $4, X0, X1
    VPADDD  X0, X1, X0

    MOVL    X0, AX
    MOVLQSX AX, AX
    MOVQ    AX, ret+24(FP)
    VZEROUPPER
    RET
