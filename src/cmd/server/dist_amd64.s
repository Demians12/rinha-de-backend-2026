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
