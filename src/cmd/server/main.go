// server: HTTP API for fraud detection via VP-Tree KNN search
// Listens on Unix Domain Socket to eliminate TCP overhead between HAProxy and API
package main

import (
	"container/heap"
	"encoding/binary"
	"encoding/json"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	jsoniter "github.com/json-iterator/go"
)

const (
	D         = 14
	K         = 5
	THRESHOLD = 0.6
)

var (
	mmapData    []byte
	indexN      int
	indexRoot   int32
	indexNNodes int32

	idxVecs   []uint8
	idxLabs   []uint8
	idxVP     []int32
	idxLeft   []int32
	idxRight  []int32
	idxThresh []float32
)

// dqTable replaces the dequantize branch with a 1KB L1-resident lookup.
var dqTable [256]float32

func init() {
	for i := 0; i < 255; i++ {
		dqTable[i] = float32(i) / 254.0
	}
	dqTable[255] = -1.0
}

func euclidean(q []float32, refIdx int32) float32 {
	ref := idxVecs[refIdx*D : refIdx*D+D]
	var sum float32
	for i := 0; i < D; i++ {
		d := q[i] - dqTable[ref[i]]
		sum += d * d
	}
	return float32(math.Sqrt(float64(sum)))
}

type knnItem struct {
	dist  float32
	label uint8
}

type knnHeap []knnItem

func (h knnHeap) Len() int            { return len(h) }
func (h knnHeap) Less(i, j int) bool  { return h[i].dist > h[j].dist }
func (h knnHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *knnHeap) Push(x interface{}) { *h = append(*h, x.(knnItem)) }
func (h *knnHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// heapPool eliminates per-request knnHeap allocation.
var heapPool = sync.Pool{
	New: func() interface{} {
		h := make(knnHeap, 0, K+1)
		return &h
	},
}

func vpSearch(nodeIdx int32, q []float32, h *knnHeap, tau float32) float32 {
	if nodeIdx < 0 {
		return tau
	}

	vp := idxVP[nodeIdx]
	d := euclidean(q, vp)

	if h.Len() < K || d < (*h)[0].dist {
		if h.Len() == K {
			heap.Pop(h)
		}
		heap.Push(h, knnItem{d, idxLabs[vp]})
		if h.Len() == K {
			tau = (*h)[0].dist
		}
	}

	mu := idxThresh[nodeIdx]
	left := idxLeft[nodeIdx]
	right := idxRight[nodeIdx]

	if d < mu {
		tau = vpSearch(left, q, h, tau)
		if d+tau >= mu {
			tau = vpSearch(right, q, h, tau)
		}
	} else {
		tau = vpSearch(right, q, h, tau)
		if d-tau <= mu {
			tau = vpSearch(left, q, h, tau)
		}
	}
	return tau
}

func knnSearch(q []float32) (fraudCount, total int) {
	hp := heapPool.Get().(*knnHeap)
	*hp = (*hp)[:0]
	vpSearch(indexRoot, q, hp, math.MaxFloat32)
	total = hp.Len()
	for _, item := range *hp {
		if item.label == 1 {
			fraudCount++
		}
	}
	heapPool.Put(hp)
	return
}

var (
	maxAmount       = 10000.0
	maxInstallments = 12.0
	amtVsAvgRatio   = 10.0
	maxMinutes      = 1440.0
	maxKm           = 1000.0
	maxTx24h        = 20.0
	maxMerchantAmt  = 10000.0
	mccRisk         map[string]float32
)

func clamp(x, lo, hi float64) float32 {
	if x < lo {
		return float32(lo)
	}
	if x > hi {
		return float32(hi)
	}
	return float32(x)
}

type Payload struct {
	ID          string `json:"id"`
	Transaction struct {
		Amount       float64 `json:"amount"`
		Installments int     `json:"installments"`
		RequestedAt  string  `json:"requested_at"`
	} `json:"transaction"`
	Customer struct {
		AvgAmount      float64  `json:"avg_amount"`
		TxCount24h     int      `json:"tx_count_24h"`
		KnownMerchants []string `json:"known_merchants"`
	} `json:"customer"`
	Merchant struct {
		ID        string  `json:"id"`
		MCC       string  `json:"mcc"`
		AvgAmount float64 `json:"avg_amount"`
	} `json:"merchant"`
	Terminal struct {
		IsOnline    bool    `json:"is_online"`
		CardPresent bool    `json:"card_present"`
		KmFromHome  float64 `json:"km_from_home"`
	} `json:"terminal"`
	LastTx *struct {
		Timestamp     string  `json:"timestamp"`
		KmFromCurrent float64 `json:"km_from_current"`
	} `json:"last_transaction"`
}

// parseRFC3339HourDOW extracts hour (0-23) and weekday (0=Mon..6=Sun) from a
// UTC RFC3339 string without calling time.Parse, saving ~500ns on the hot path.
func parseRFC3339HourDOW(s string) (hour, dow int, ok bool) {
	if len(s) < 20 {
		return
	}
	y := int(s[0]-'0')*1000 + int(s[1]-'0')*100 + int(s[2]-'0')*10 + int(s[3]-'0')
	mo := int(s[5]-'0')*10 + int(s[6]-'0')
	d := int(s[8]-'0')*10 + int(s[9]-'0')
	h := int(s[11]-'0')*10 + int(s[12]-'0')
	if mo < 1 || mo > 12 || d < 1 || d > 31 || h > 23 {
		return
	}
	// Tomohiko Sakamoto's algorithm — 0=Sunday
	tab := [12]int{0, 3, 2, 5, 0, 3, 5, 1, 4, 6, 2, 4}
	yy := y
	if mo < 3 {
		yy--
	}
	w := (yy + yy/4 - yy/100 + yy/400 + tab[mo-1] + d) % 7
	return h, (w + 6) % 7, true // convert to 0=Monday
}

// vec14 is a fixed-size feature vector for pool reuse.
type vec14 [D]float32

// vecPool eliminates per-request []float32 allocation.
var vecPool = sync.Pool{
	New: func() interface{} { return new(vec14) },
}

func vectorize(p *Payload, v []float32) {
	v[0] = clamp(p.Transaction.Amount/maxAmount, 0, 1)
	v[1] = clamp(float64(p.Transaction.Installments)/maxInstallments, 0, 1)

	if p.Customer.AvgAmount > 0 {
		v[2] = clamp((p.Transaction.Amount/p.Customer.AvgAmount)/amtVsAvgRatio, 0, 1)
	} else {
		v[2] = 0
	}

	hour, dow, ok := parseRFC3339HourDOW(p.Transaction.RequestedAt)
	if ok {
		v[3] = float32(hour) / 23.0
		v[4] = float32(dow) / 6.0

		if p.LastTx != nil {
			// Full parse only when we need to compute a time difference.
			t, err := time.Parse(time.RFC3339, p.Transaction.RequestedAt)
			if err == nil {
				lastT, err2 := time.Parse(time.RFC3339, p.LastTx.Timestamp)
				if err2 == nil {
					mins := t.Sub(lastT).Minutes()
					if mins < 0 {
						mins = -mins
					}
					v[5] = clamp(mins/maxMinutes, 0, 1)
				} else {
					v[5] = -1
				}
				v[6] = clamp(p.LastTx.KmFromCurrent/maxKm, 0, 1)
			} else {
				v[5] = -1
				v[6] = -1
			}
		} else {
			v[5] = -1
			v[6] = -1
		}
	} else {
		v[3] = 0
		v[4] = 0
		v[5] = -1
		v[6] = -1
	}

	v[7] = clamp(p.Terminal.KmFromHome/maxKm, 0, 1)
	v[8] = clamp(float64(p.Customer.TxCount24h)/maxTx24h, 0, 1)

	// Explicit zeroing required: pooled slices carry values from prior requests.
	v[9] = 0
	if p.Terminal.IsOnline {
		v[9] = 1
	}
	v[10] = 0
	if p.Terminal.CardPresent {
		v[10] = 1
	}

	known := false
	for _, m := range p.Customer.KnownMerchants {
		if m == p.Merchant.ID {
			known = true
			break
		}
	}
	v[11] = 0
	if !known {
		v[11] = 1
	}

	if risk, ok := mccRisk[p.Merchant.MCC]; ok {
		v[12] = risk
	} else {
		v[12] = 0.5
	}

	v[13] = clamp(p.Merchant.AvgAmount/maxMerchantAmt, 0, 1)
}

var json2 = jsoniter.ConfigCompatibleWithStandardLibrary

type Response struct {
	Approved   bool    `json:"approved"`
	FraudScore float64 `json:"fraud_score"`
}

func fraudHandler(w http.ResponseWriter, r *http.Request) {
	var p Payload
	if err := json2.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	vp := vecPool.Get().(*vec14)
	vectorize(&p, vp[:])
	fraudCount, total := knnSearch(vp[:])
	vecPool.Put(vp)

	var fraudScore float64
	if total > 0 {
		fraudScore = float64(fraudCount) / float64(total)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Response{
		Approved:   fraudScore < THRESHOLD,
		FraudScore: fraudScore,
	})
}

func loadIndex(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}

	mmapData, err = syscall.Mmap(int(f.Fd()), 0, int(fi.Size()),
		syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return err
	}

	if string(mmapData[0:4]) != "VPQU" {
		log.Fatal("index.bin: wrong magic")
	}
	indexN = int(binary.LittleEndian.Uint32(mmapData[4:8]))
	indexRoot = int32(binary.LittleEndian.Uint32(mmapData[12:16]))
	indexNNodes = int32(binary.LittleEndian.Uint32(mmapData[16:20]))
	offset := 20

	N := indexN
	idxVecs = mmapData[offset : offset+N*D]
	offset += N * D

	idxLabs = mmapData[offset : offset+N]
	offset += N

	if offset%4 != 0 {
		offset += 4 - offset%4
	}

	// Non-interleaved node arrays: vp[nn] left[nn] right[nn] thresh[nn]
	nn := int(indexNNodes)
	idxVP = unsafe.Slice((*int32)(unsafe.Pointer(&mmapData[offset])), nn)
	offset += nn * 4
	idxLeft = unsafe.Slice((*int32)(unsafe.Pointer(&mmapData[offset])), nn)
	offset += nn * 4
	idxRight = unsafe.Slice((*int32)(unsafe.Pointer(&mmapData[offset])), nn)
	offset += nn * 4
	idxThresh = unsafe.Slice((*float32)(unsafe.Pointer(&mmapData[offset])), nn)

	return nil
}

func loadMCCRisk(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var m map[string]float32
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		return err
	}
	mccRisk = m
	return nil
}

func main() {
	indexPath := os.Getenv("INDEX_PATH")
	if indexPath == "" {
		indexPath = "/app/resources/index.bin"
	}
	mccPath := os.Getenv("MCC_RISK_PATH")
	if mccPath == "" {
		mccPath = "/app/resources/mcc_risk.json"
	}
	udsPath := os.Getenv("UDS_PATH")
	if udsPath == "" {
		udsPath = "/tmp/api.sock"
	}

	if err := loadMCCRisk(mccPath); err != nil {
		log.Fatalf("load mcc risk: %v", err)
	}
	if err := loadIndex(indexPath); err != nil {
		log.Fatalf("load index: %v", err)
	}
	log.Printf("Index loaded: N=%d nNodes=%d root=%d", indexN, indexNNodes, indexRoot)

	os.Remove(udsPath)
	ln, err := net.Listen("unix", udsPath)
	if err != nil {
		log.Fatalf("listen unix: %v", err)
	}
	os.Chmod(udsPath, 0666)

	mux := http.NewServeMux()
	mux.HandleFunc("/fraud-score", fraudHandler)
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	}

	log.Printf("Listening on %s", udsPath)
	if err := srv.Serve(ln); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
