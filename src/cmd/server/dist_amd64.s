#include "textflag.h"

// func dist2SIMD(q, ref *vec16) int64
//
// Squared L2 distance between two vec16 (16 × int16, 32 bytes) using AVX2.
//
// Algorithm:
//   1. Load both vectors into YMM registers (one VMOVDQU each).
//   2. VPSUBW   → 16 int16 differences (d[i] = q[i] - ref[i]).
//   3. VPMADDWD → 8 int32: Y3[i] = d[2i]² + d[2i+1]²  (max 800M < 2^31, safe).
//   4. VPADDD   → add upper 4 + lower 4 int32 → 4 int32  (max 1.6G < 2^31, safe).
//   5. VPMOVSXDQ → sign-extend 4 int32 to 4 int64 (avoids overflow in step 6).
//   6. VPADDQ + VPSRLDQ + VPADDQ → reduce 4 int64 to 1 int64 (total distance²).
//
// Registers:
//   SI, DI  — q, ref pointers
//   Y0, Y1  — raw int16 vectors
//   Y2      — int16 differences
//   Y3      — int32 squared pair sums (X3 = lower 128 bits)
//   X4      — upper 4 int32 from Y3
//   X5      — merged 4 int32 (lower+upper)
//   Y6      — 4 int64 widened from X5 (X6 = lower 2 int64)
//   X7      — upper 2 int64, then final 2-element sum
//   X8      — shifted X7 for final horizontal add
//
TEXT ·dist2SIMD(SB), NOSPLIT, $0-24
    MOVQ    q+0(FP), SI
    MOVQ    ref+8(FP), DI

    VMOVDQU (SI), Y0             // load q[0..15]   (16 × int16)
    VMOVDQU (DI), Y1             // load ref[0..15] (16 × int16)

    VPSUBW  Y1, Y0, Y2           // Y2[i] = q[i] - ref[i]   (16 × int16)
    VPMADDWD Y2, Y2, Y3          // Y3[i] = Y2[2i]² + Y2[2i+1]²  (8 × int32)

    // Reduce 8 int32 → 4 int32 (still fits: max 1.6G < 2^31)
    VEXTRACTI128 $1, Y3, X4      // X4 = upper 128 bits of Y3 (4 × int32)
    VPADDD  X3, X4, X5           // X5 = lower4 + upper4     (4 × int32)

    // Widen to int64 before final reduction to guarantee no overflow
    VPMOVSXDQ X5, Y6             // Y6 = sign_extend(X5)     (4 × int64); X6 = lower 2
    VEXTRACTI128 $1, Y6, X7      // X7 = upper 2 int64
    VPADDQ  X6, X7, X7           // X7 = upper2 + lower2     (2 × int64)

    // Horizontal sum: 2 int64 → 1 int64
    VPSRLDQ $8, X7, X8           // X8[63:0] = X7[127:64]
    VPADDQ  X7, X8, X7           // X7[63:0] = total squared distance

    MOVQ    X7, AX
    MOVQ    AX, ret+16(FP)
    VZEROUPPER                   // avoid AVX↔SSE transition penalty
    RET
