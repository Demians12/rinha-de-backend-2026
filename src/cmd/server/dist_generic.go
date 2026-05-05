//go:build !amd64

package main

import "unsafe"

func dist2(q, ref *vec16) int64 {
	var sum int64
	for i := 0; i < vecD; i++ {
		d := int64(q[i]) - int64(ref[i])
		sum += d * d
	}
	return sum
}

func centroidDistSIMD(q *vec16, centroid *int16) int64 {
	cs := unsafe.Slice(centroid, vecD)
	var sum int64
	for i := 0; i < vecD; i++ {
		d := int64(q[i]) - int64(cs[i])
		sum += d * d
	}
	return sum
}

func lboundSIMD(q *vec16, lo, hi *int8) int64 {
	ls := unsafe.Slice(lo, vecD)
	hs := unsafe.Slice(hi, vecD)
	var sum int64
	for i := 0; i < vecD; i++ {
		x := q[i]
		var d int64
		if x < ls[i] {
			d = int64(ls[i]) - int64(x)
		} else if x > hs[i] {
			d = int64(x) - int64(hs[i])
		}
		sum += d * d
	}
	return sum
}
