package main

import (
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"log"
	"math"
	"math/rand"
	"os"
	"time"
)

const D = 14

var magic = [4]byte{'V', 'P', 'Q', 'U'}

var (
	vecs   []uint8
	labs   []uint8
	nodeL  []int32
	nodeR  []int32
	nodeT  []float32
	vpArr  []int32
	nNodes int32
)

func quantize(f float32) uint8 {
	if f < -0.5 {
		return 255
	}
	v := f * 254.0
	if v < 0 {
		return 0
	}
	if v > 254 {
		return 254
	}
	return uint8(v + 0.5)
}

func dequantize(u uint8) float32 {
	if u == 255 {
		return -1.0
	}
	return float32(u) / 254.0
}

func distQQ(a, b []uint8) float32 {
	var sum float32
	for i := 0; i < D; i++ {
		d := dequantize(a[i]) - dequantize(b[i])
		sum += d * d
	}
	return float32(math.Sqrt(float64(sum)))
}

func buildVP(indices []int32) int32 {
	if len(indices) == 0 {
		return -1
	}

	myIdx := nNodes
	nNodes++

	vpPos := rand.Intn(len(indices))
	vp := indices[vpPos]
	vpArr[myIdx] = vp

	indices[vpPos], indices[len(indices)-1] = indices[len(indices)-1], indices[vpPos]
	rest := indices[:len(indices)-1]

	if len(rest) == 0 {
		nodeL[myIdx] = -1
		nodeR[myIdx] = -1
		nodeT[myIdx] = 0
		return myIdx
	}

	dists := make([]float32, len(rest))
	vpVec := vecs[vp*D : vp*D+D]
	for i, idx := range rest {
		dists[i] = distQQ(vpVec, vecs[idx*D:idx*D+D])
	}

	mu := medianFloat32(dists)
	nodeT[myIdx] = mu

	inside := rest[:0]
	outside := make([]int32, 0, len(rest)/2)
	for i, idx := range rest {
		if dists[i] < mu {
			inside = append(inside, idx)
		} else {
			outside = append(outside, idx)
		}
	}

	nodeL[myIdx] = buildVP(inside)
	nodeR[myIdx] = buildVP(outside)

	return myIdx
}

func medianFloat32(a []float32) float32 {
	if len(a) == 0 {
		return 0
	}
	b := make([]float32, len(a))
	copy(b, a)
	return quickselect(b, len(b)/2)
}

func quickselect(a []float32, k int) float32 {
	if len(a) == 1 {
		return a[0]
	}
	pivot := a[len(a)/2]
	var lo, mid, hi []float32
	for _, v := range a {
		if v < pivot {
			lo = append(lo, v)
		} else if v == pivot {
			mid = append(mid, v)
		} else {
			hi = append(hi, v)
		}
	}
	if k < len(lo) {
		return quickselect(lo, k)
	}
	if k < len(lo)+len(mid) {
		return pivot
	}
	return quickselect(hi, k-len(lo)-len(mid))
}

type rawRef struct {
	Vector [D]float32 `json:"vector"`
	Label  string     `json:"label"`
}

func main() {
	t0 := time.Now()

	refsPath := os.Getenv("REFS_PATH")
	if refsPath == "" {
		refsPath = "/app/resources/references.json.gz"
	}
	outPath := os.Getenv("INDEX_PATH")
	if outPath == "" {
		outPath = "/app/resources/index.bin"
	}

	f, err := os.Open(refsPath)
	if err != nil {
		log.Fatalf("open refs: %v", err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		log.Fatalf("gzip: %v", err)
	}
	var refs []rawRef
	if err := json.NewDecoder(gz).Decode(&refs); err != nil {
		log.Fatalf("decode: %v", err)
	}
	gz.Close()
	f.Close()

	N := len(refs)
	log.Printf("Loaded %d references (%.1fs)", N, time.Since(t0).Seconds())

	vecs = make([]uint8, N*D)
	labs = make([]uint8, N)
	for i, r := range refs {
		for d := 0; d < D; d++ {
			vecs[i*D+d] = quantize(r.Vector[d])
		}
		if r.Label == "fraud" {
			labs[i] = 1
		}
	}
	refs = nil

	log.Printf("Quantized vectors (%.1fs)", time.Since(t0).Seconds())

	nodeL = make([]int32, N)
	nodeR = make([]int32, N)
	nodeT = make([]float32, N)
	vpArr = make([]int32, N)

	indices := make([]int32, N)
	for i := range indices {
		indices[i] = int32(i)
	}

	log.Printf("Building VP-Tree over %d vectors...", N)
	root := buildVP(indices)
	log.Printf("VP-Tree built: %d nodes, root=%d (%.1fs)", nNodes, root, time.Since(t0).Seconds())

	out, err := os.Create(outPath)
	if err != nil {
		log.Fatalf("create index: %v", err)
	}

	// Header: magic(4) + N(4) + D(4) + root(4) + nNodes(4)
	out.Write(magic[:])
	binary.Write(out, binary.LittleEndian, uint32(N))
	binary.Write(out, binary.LittleEndian, uint32(D))
	binary.Write(out, binary.LittleEndian, root)
	binary.Write(out, binary.LittleEndian, uint32(nNodes))

	// Vectors: N*D uint8
	out.Write(vecs)

	// Labels: N uint8
	out.Write(labs)

	// Align to 4 bytes
	total := N*D + N
	if total%4 != 0 {
		pad := make([]byte, 4-total%4)
		out.Write(pad)
	}

	// Nodes: non-interleaved arrays — vp[nNodes] left[nNodes] right[nNodes] thresh[nNodes]
	// Server reads them as four separate unsafe.Slice arrays at fixed offsets.
	nn := int(nNodes)
	for i := 0; i < nn; i++ {
		binary.Write(out, binary.LittleEndian, vpArr[i])
	}
	for i := 0; i < nn; i++ {
		binary.Write(out, binary.LittleEndian, nodeL[i])
	}
	for i := 0; i < nn; i++ {
		binary.Write(out, binary.LittleEndian, nodeR[i])
	}
	for i := 0; i < nn; i++ {
		binary.Write(out, binary.LittleEndian, nodeT[i])
	}

	out.Close()

	fi, _ := os.Stat(outPath)
	log.Printf("Index written: %s (%.1f MB, %.1fs)",
		outPath, float64(fi.Size())/1e6, time.Since(t0).Seconds())
}
