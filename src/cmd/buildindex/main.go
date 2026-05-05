package main

import (
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"log"
	"math"
	"os"
	"time"
)

const (
	D        = 14
	scale    = 10000
	ambigMin = 4
	ambigMax = 17
)

var magic = [4]byte{'R', 'K', 'N', '4'}

type rawRef struct {
	Vector [D]float32 `json:"vector"`
	Label  string     `json:"label"`
}

func q4(f float32) int16 {
	if f < -0.5 {
		return -scale
	}
	if f < 0 {
		return 0
	}
	if f > 1 {
		return scale
	}
	return int16(math.Round(float64(f * scale)))
}

func ruleScore(v *[D]int16) int {
	score := 0
	add := func(ok bool) {
		if ok {
			score++
		}
	}

	add(v[0] >= 2000) // amount >= 2000
	add(v[0] >= 500)  // amount >= 500
	add(v[1] >= 5000) // installments >= 6
	add(v[1] >= 3333) // installments >= 4
	add(v[3] < 3043)  // hour < 7
	add(v[3] < 3478 || v[3] >= 9130)
	add(v[2] >= 8000) // amount/customer_avg >= 8x
	add(v[2] >= 1000) // amount/customer_avg >= 1x
	add(v[8] >= 4000) // tx_count_24h >= 8
	add(v[8] >= 3000) // tx_count_24h >= 6
	add(v[11] >= 5000)
	add(v[12] >= 7500)
	add(v[12] >= 4500)
	add(v[9] >= 5000)
	add(v[10] < 5000)
	add(v[7] >= 2000) // km_from_home >= 200
	add(v[7] >= 500)  // km_from_home >= 50
	add(v[5] >= 0 && v[5] <= 69)
	add(v[5] >= 0 && v[5] <= 208)
	add(v[6] >= 2000)
	add(v[6] >= 200)
	add(v[13] <= 100) // merchant_avg_amount <= 100

	return score
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

	vecs := make([]int16, 0, 128000*D)
	labs := make([]uint8, 0, 128000)
	total := 0
	kept := 0
	scoreCounts := [23]int{}

	for dec.More() {
		var r rawRef
		if err := dec.Decode(&r); err != nil {
			log.Fatalf("decode ref %d: %v", total, err)
		}

		var q [D]int16
		for i := 0; i < D; i++ {
			q[i] = q4(r.Vector[i])
		}

		score := ruleScore(&q)
		if score >= 0 && score < len(scoreCounts) {
			scoreCounts[score]++
		}
		if score >= ambigMin && score <= ambigMax {
			vecs = append(vecs, q[:]...)
			if r.Label == "fraud" {
				labs = append(labs, 1)
			} else {
				labs = append(labs, 0)
			}
			kept++
		}
		total++
	}

	out, err := os.Create(outPath)
	if err != nil {
		log.Fatalf("create index: %v", err)
	}
	defer out.Close()

	out.Write(magic[:])
	binary.Write(out, binary.LittleEndian, uint32(kept))
	binary.Write(out, binary.LittleEndian, uint32(D))

	for _, v := range vecs {
		binary.Write(out, binary.LittleEndian, v)
	}
	out.Write(labs)

	fi, _ := out.Stat()
	log.Printf("Read %d references, kept %d ambiguous refs (scores %d..%d)", total, kept, ambigMin, ambigMax)
	log.Printf("Rule score counts: %v", scoreCounts)
	log.Printf("Index written: %s (%.1f MB, %.1fs)",
		outPath, float64(fi.Size())/1e6, time.Since(t0).Seconds())
}
