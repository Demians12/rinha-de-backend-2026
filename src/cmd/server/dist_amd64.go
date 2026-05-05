//go:build amd64

package main

// dist2SIMD is implemented in dist_amd64.s using AVX2.
func dist2SIMD(q, ref *vec16) int64

func dist2(q, ref *vec16) int64 { return dist2SIMD(q, ref) }
