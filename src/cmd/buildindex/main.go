package main

import (
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"log"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"
)

const (
	D            = 14
	vecD         = 16
	scale        = 127
	ambigMin     = 3
	ambigMax     = 19
	ivfClusters  = 2048
	scoreBuckets = 23
)

var magic = [4]byte{'R', 'I', 'V', '3'}

type rawRef struct {
	Vector [D]float32 `json:"vector"`
	Label  string     `json:"label"`
}

func q4(f float32) int8 {
	if f < -0.5 {
		return -scale
	}
	if f < 0 {
		return 0
	}
	if f > 1 {
		return scale
	}
	return int8(f*scale + 0.5)
}

func ruleScore(v *[D]float32) int {
	score := 0
	add := func(ok bool) {
		if ok {
			score++
		}
	}

	add(v[0] >= 0.2)      // amount >= 2000
	add(v[0] >= 0.05)     // amount >= 500
	add(v[1] >= 0.5)      // installments >= 6
	add(v[1] >= 4.0/12.0) // installments >= 4
	add(v[3] < 7.0/23.0)  // hour < 7
	add(v[3] < 8.0/23.0 || v[3] >= 21.0/23.0)
	add(v[2] >= 0.8) // amount/customer_avg >= 8x
	add(v[2] >= 0.1) // amount/customer_avg >= 1x
	add(v[8] >= 0.4) // tx_count_24h >= 8
	add(v[8] >= 0.3) // tx_count_24h >= 6
	add(v[11] >= 0.5)
	add(v[12] >= 0.75)
	add(v[12] >= 0.45)
	add(v[9] >= 0.5)
	add(v[10] < 0.5)
	add(v[7] >= 0.2)  // km_from_home >= 200
	add(v[7] >= 0.05) // km_from_home >= 50
	add(v[5] >= 0 && v[5] <= 10.0/1440.0)
	add(v[5] >= 0 && v[5] <= 30.0/1440.0)
	add(v[6] >= 0.2)
	add(v[6] >= 0.02)
	add(v[13] <= 0.01) // merchant_avg_amount <= 100

	return score
}

func envInt(name string, def, min, max int) int {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func vecDistToCentroid(vecs []byte, off int, centroids []int16, c int) int {
	coff := c * vecD
	sum := 0
	for j := 0; j < vecD; j++ {
		d := int(int8(vecs[off+j])) - int(centroids[coff+j])
		sum += d * d
	}
	return sum
}

func nearestCentroid(vecs []byte, idx int, centroids []int16, k int) int {
	off := idx * vecD
	bestC := 0
	bestD := vecDistToCentroid(vecs, off, centroids, 0)
	for c := 1; c < k; c++ {
		d := vecDistToCentroid(vecs, off, centroids, c)
		if d < bestD {
			bestD = d
			bestC = c
		}
	}
	return bestC
}

func trainKMeans(vecs []byte, n, k, sampleN, iters int) []int16 {
	if sampleN > n {
		sampleN = n
	}
	if sampleN < k {
		sampleN = k
	}
	centroids := make([]int16, k*vecD)
	for c := 0; c < k; c++ {
		idx := 0
		if k > 1 {
			idx = c * (n - 1) / (k - 1)
		}
		off := idx * vecD
		for j := 0; j < vecD; j++ {
			centroids[c*vecD+j] = int16(int8(vecs[off+j]))
		}
	}

	sample := make([]int, sampleN)
	for i := 0; i < sampleN; i++ {
		if sampleN == 1 {
			sample[i] = 0
		} else {
			sample[i] = i * (n - 1) / (sampleN - 1)
		}
	}

	workers := runtime.NumCPU()
	if workers > sampleN {
		workers = sampleN
	}
	if workers < 1 {
		workers = 1
	}

	for it := 0; it < iters; it++ {
		localSums := make([][]int64, workers)
		localCounts := make([][]int, workers)
		for w := 0; w < workers; w++ {
			localSums[w] = make([]int64, k*vecD)
			localCounts[w] = make([]int, k)
		}

		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			start := w * sampleN / workers
			end := (w + 1) * sampleN / workers
			wg.Add(1)
			go func(w, start, end int) {
				defer wg.Done()
				sums := localSums[w]
				counts := localCounts[w]
				for _, idx := range sample[start:end] {
					c := nearestCentroid(vecs, idx, centroids, k)
					counts[c]++
					voff := idx * vecD
					coff := c * vecD
					for j := 0; j < vecD; j++ {
						sums[coff+j] += int64(int8(vecs[voff+j]))
					}
				}
			}(w, start, end)
		}
		wg.Wait()

		sums := make([]int64, k*vecD)
		counts := make([]int, k)
		for w := 0; w < workers; w++ {
			for c := 0; c < k; c++ {
				counts[c] += localCounts[w][c]
			}
			for i, v := range localSums[w] {
				sums[i] += v
			}
		}

		empty := 0
		for c := 0; c < k; c++ {
			if counts[c] == 0 {
				empty++
				continue
			}
			coff := c * vecD
			half := int64(counts[c] / 2)
			for j := 0; j < vecD; j++ {
				v := sums[coff+j]
				if v >= 0 {
					centroids[coff+j] = int16((v + half) / int64(counts[c]))
				} else {
					centroids[coff+j] = int16((v - half) / int64(counts[c]))
				}
			}
		}
		log.Printf("kmeans iter %d/%d sample=%d empty=%d", it+1, iters, sampleN, empty)
	}

	return centroids
}

func assignClusters(vecs []byte, n, k int, centroids []int16) ([]uint16, []uint32) {
	assign := make([]uint16, n)
	counts := make([]uint32, k)
	workers := runtime.NumCPU()
	if workers > n {
		workers = n
	}
	if workers < 1 {
		workers = 1
	}
	localCounts := make([][]uint32, workers)
	for w := 0; w < workers; w++ {
		localCounts[w] = make([]uint32, k)
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := w * n / workers
		end := (w + 1) * n / workers
		wg.Add(1)
		go func(w, start, end int) {
			defer wg.Done()
			lc := localCounts[w]
			for i := start; i < end; i++ {
				c := nearestCentroid(vecs, i, centroids, k)
				assign[i] = uint16(c)
				lc[c]++
			}
		}(w, start, end)
	}
	wg.Wait()

	for w := 0; w < workers; w++ {
		for c := 0; c < k; c++ {
			counts[c] += localCounts[w][c]
		}
	}
	return assign, counts
}

func reorderByCluster(vecs []byte, labs []uint8, scores []uint8, assign []uint16, counts []uint32, n, k int) ([]uint32, []byte, []uint8, []uint32, []byte, []byte, []byte, []byte) {
	bucketCounts := make([]uint32, k*scoreBuckets)
	for i := 0; i < n; i++ {
		c := int(assign[i])
		score := int(scores[i])
		if score < 0 {
			score = 0
		} else if score >= scoreBuckets {
			score = scoreBuckets - 1
		}
		bucketCounts[c*scoreBuckets+score]++
	}

	offsets := make([]uint32, k*(scoreBuckets+1))
	var running uint32
	for c := 0; c < k; c++ {
		base := c * (scoreBuckets + 1)
		for score := 0; score < scoreBuckets; score++ {
			offsets[base+score] = running
			running += bucketCounts[c*scoreBuckets+score]
		}
		offsets[base+scoreBuckets] = running
	}

	outVecs := make([]byte, len(vecs))
	outLabs := make([]uint8, len(labs))
	outIDs := make([]uint32, n)
	bboxMin := make([]byte, k*vecD)
	bboxMax := make([]byte, k*vecD)
	bucketBBoxMin := make([]byte, k*scoreBuckets*vecD)
	bucketBBoxMax := make([]byte, k*scoreBuckets*vecD)
	minInit := int8(scale)
	maxInit := -minInit
	for c := 0; c < k; c++ {
		for j := 0; j < vecD; j++ {
			bboxMin[c*vecD+j] = byte(minInit)
			bboxMax[c*vecD+j] = byte(maxInit)
		}
		for score := 0; score < scoreBuckets; score++ {
			boff := (c*scoreBuckets + score) * vecD
			for j := 0; j < vecD; j++ {
				bucketBBoxMin[boff+j] = byte(minInit)
				bucketBBoxMax[boff+j] = byte(maxInit)
			}
		}
	}

	writePos := make([]uint32, len(bucketCounts))
	for c := 0; c < k; c++ {
		base := c * (scoreBuckets + 1)
		for score := 0; score < scoreBuckets; score++ {
			writePos[c*scoreBuckets+score] = offsets[base+score]
		}
	}
	for i := 0; i < n; i++ {
		c := int(assign[i])
		score := int(scores[i])
		if score < 0 {
			score = 0
		} else if score >= scoreBuckets {
			score = scoreBuckets - 1
		}
		widx := c*scoreBuckets + score
		pos := int(writePos[widx])
		writePos[widx]++

		src := i * vecD
		dst := pos * vecD
		copy(outVecs[dst:dst+vecD], vecs[src:src+vecD])
		outLabs[pos] = labs[i]
		outIDs[pos] = uint32(i)

		boff := c * vecD
		bboff := (c*scoreBuckets + score) * vecD
		for j := 0; j < vecD; j++ {
			v := int8(vecs[src+j])
			if v < int8(bboxMin[boff+j]) {
				bboxMin[boff+j] = byte(v)
			}
			if v > int8(bboxMax[boff+j]) {
				bboxMax[boff+j] = byte(v)
			}
			if v < int8(bucketBBoxMin[bboff+j]) {
				bucketBBoxMin[bboff+j] = byte(v)
			}
			if v > int8(bucketBBoxMax[bboff+j]) {
				bucketBBoxMax[bboff+j] = byte(v)
			}
		}
	}

	for c := 0; c < k; c++ {
		if counts[c] == 0 {
			boff := c * vecD
			for j := 0; j < vecD; j++ {
				bboxMin[boff+j] = 0
				bboxMax[boff+j] = 0
			}
		}
		for score := 0; score < scoreBuckets; score++ {
			base := c * (scoreBuckets + 1)
			if offsets[base+score] == offsets[base+score+1] {
				boff := (c*scoreBuckets + score) * vecD
				for j := 0; j < vecD; j++ {
					bucketBBoxMin[boff+j] = 0
					bucketBBoxMax[boff+j] = 0
				}
			}
		}
	}

	return offsets, outVecs, outLabs, outIDs, bboxMin, bboxMax, bucketBBoxMin, bucketBBoxMax
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
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		log.Fatalf("gzip: %v", err)
	}
	defer gz.Close()

	dec := json.NewDecoder(gz)
	tok, err := dec.Token()
	if err != nil {
		log.Fatalf("decode opening token: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		log.Fatalf("references must be a JSON array")
	}

	vecs := make([]byte, 0, 3000000*vecD)
	labs := make([]uint8, 0, 3000000)
	scores := make([]uint8, 0, 3000000)
	total := 0
	scoreCounts := [23]int{}

	for dec.More() {
		var r rawRef
		if err := dec.Decode(&r); err != nil {
			log.Fatalf("decode ref %d: %v", total, err)
		}

		for i := 0; i < D; i++ {
			vecs = append(vecs, byte(q4(r.Vector[i])))
		}
		for i := D; i < vecD; i++ {
			vecs = append(vecs, 0)
		}
		if r.Label == "fraud" {
			labs = append(labs, 1)
		} else {
			labs = append(labs, 0)
		}

		score := ruleScore(&r.Vector)
		if score >= 0 && score < len(scoreCounts) {
			scoreCounts[score]++
		}
		scores = append(scores, uint8(score))
		total++
	}
	n := total
	k := ivfClusters
	if n < k {
		k = n
	}
	if k < 1 {
		log.Fatalf("no references loaded")
	}

	sampleN := envInt("IVF_TRAIN_SAMPLE", 65536, k, n)
	iters := envInt("IVF_TRAIN_ITERS", 8, 1, 64)
	log.Printf("Read %d references, training IVF int8 K=%d sample=%d iters=%d", n, k, sampleN, iters)
	log.Printf("Rule score counts: %v (ambiguous runtime scores %d..%d)", scoreCounts, ambigMin, ambigMax)

	centroids := trainKMeans(vecs, n, k, sampleN, iters)
	assign, counts := assignClusters(vecs, n, k, centroids)
	offsets, outVecs, outLabs, outIDs, bboxMin, bboxMax, bucketBBoxMin, bucketBBoxMax := reorderByCluster(vecs, labs, scores, assign, counts, n, k)
	vecs = nil
	labs = nil
	scores = nil
	assign = nil

	out, err := os.Create(outPath)
	if err != nil {
		log.Fatalf("create index: %v", err)
	}
	defer out.Close()

	out.Write(magic[:])
	binary.Write(out, binary.LittleEndian, uint32(n))
	binary.Write(out, binary.LittleEndian, uint32(k))
	binary.Write(out, binary.LittleEndian, uint32(vecD))
	binary.Write(out, binary.LittleEndian, uint32(scale))
	binary.Write(out, binary.LittleEndian, uint32(scoreBuckets))
	binary.Write(out, binary.LittleEndian, centroids)
	out.Write(bboxMin)
	out.Write(bboxMax)
	out.Write(bucketBBoxMin)
	out.Write(bucketBBoxMax)
	binary.Write(out, binary.LittleEndian, offsets)
	out.Write(outVecs)
	out.Write(outLabs)
	if pad := (4 - (len(outLabs) & 3)) & 3; pad != 0 {
		out.Write(make([]byte, pad))
	}
	binary.Write(out, binary.LittleEndian, outIDs)

	fi, _ := out.Stat()
	log.Printf("Index written: %s (%.1f MB, %.1fs)",
		outPath, float64(fi.Size())/1e6, time.Since(t0).Seconds())
}
