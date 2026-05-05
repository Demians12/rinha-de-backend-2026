//go:build !amd64

package main

func dist2(q, ref *vec16) int64 {
	var sum int64
	for i := 0; i < D; i++ {
		d := int64(q[i]) - int64(ref[i])
		sum += d * d
	}
	return sum
}
