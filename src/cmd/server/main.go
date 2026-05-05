// server: HTTP API for fraud detection via cheap rules + exact KNN fallback.
package main

import (
	"encoding/binary"
	"encoding/json"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"
	"unsafe"

	jsoniter "github.com/json-iterator/go"
)

const (
	D         = 14
	K         = 5
	THRESHOLD = 0.6
	scale     = 10000

	ambigMin = 4
	ambigMax = 17
)

var (
	mmapData []byte
	indexN   int
	idxVecs  []int16
	idxLabs  []uint8
)

type vec14 [D]int16

func q01(x float64) int16 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return scale
	}
	return int16(math.Round(x * scale))
}

func daysFromCivil(y, m, d int) int {
	if m <= 2 {
		y--
	}
	era := y / 400
	if y < 0 && y%400 != 0 {
		era--
	}
	yoe := y - era*400
	mp := m
	if mp > 2 {
		mp -= 3
	} else {
		mp += 9
	}
	doy := (153*mp+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

func parseRFC3339Parts(s string) (hour, dow, epochMinute int, ok bool) {
	if len(s) < 20 {
		return
	}
	y := int(s[0]-'0')*1000 + int(s[1]-'0')*100 + int(s[2]-'0')*10 + int(s[3]-'0')
	mo := int(s[5]-'0')*10 + int(s[6]-'0')
	d := int(s[8]-'0')*10 + int(s[9]-'0')
	h := int(s[11]-'0')*10 + int(s[12]-'0')
	mi := int(s[14]-'0')*10 + int(s[15]-'0')
	if mo < 1 || mo > 12 || d < 1 || d > 31 || h > 23 || mi > 59 {
		return
	}
	days := daysFromCivil(y, mo, d)
	return h, (days + 3) % 7, days*1440 + h*60 + mi, true
}

func ruleScore(v *vec14) int {
	score := 0
	add := func(ok bool) {
		if ok {
			score++
		}
	}

	add(v[0] >= 2000)
	add(v[0] >= 500)
	add(v[1] >= 5000)
	add(v[1] >= 3333)
	add(v[3] < 3043)
	add(v[3] < 3478 || v[3] >= 9130)
	add(v[2] >= 8000)
	add(v[2] >= 1000)
	add(v[8] >= 4000)
	add(v[8] >= 3000)
	add(v[11] >= 5000)
	add(v[12] >= 7500)
	add(v[12] >= 4500)
	add(v[9] >= 5000)
	add(v[10] < 5000)
	add(v[7] >= 2000)
	add(v[7] >= 500)
	add(v[5] >= 0 && v[5] <= 69)
	add(v[5] >= 0 && v[5] <= 208)
	add(v[6] >= 2000)
	add(v[6] >= 200)
	add(v[13] <= 100)

	return score
}

func dist2(q *vec14, ref []int16) int64 {
	var sum int64
	for i := 0; i < D; i++ {
		d := int64(q[i]) - int64(ref[i])
		sum += d * d
	}
	return sum
}

func insertTopK(best *[K]int64, labels *[K]uint8, d int64, label uint8) {
	if d >= best[K-1] {
		return
	}
	pos := K - 1
	for pos > 0 && d < best[pos-1] {
		best[pos] = best[pos-1]
		labels[pos] = labels[pos-1]
		pos--
	}
	best[pos] = d
	labels[pos] = label
}

func knnSearch(q *vec14) (fraudCount int) {
	best := [K]int64{math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64}
	var labels [K]uint8
	for i := 0; i < indexN; i++ {
		ref := idxVecs[i*D : i*D+D]
		insertTopK(&best, &labels, dist2(q, ref), idxLabs[i])
	}
	for i := 0; i < K; i++ {
		if labels[i] == 1 {
			fraudCount++
		}
	}
	return fraudCount
}

var (
	maxAmount       = 10000.0
	maxInstallments = 12.0
	amtVsAvgRatio   = 10.0
	maxMinutes      = 1440.0
	maxKm           = 1000.0
	maxTx24h        = 20.0
	maxMerchantAmt  = 10000.0
	mccRisk         map[string]float64
)

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

func vectorize(p *Payload, v *vec14) {
	v[0] = q01(p.Transaction.Amount / maxAmount)
	v[1] = q01(float64(p.Transaction.Installments) / maxInstallments)

	if p.Customer.AvgAmount > 0 {
		v[2] = q01((p.Transaction.Amount / p.Customer.AvgAmount) / amtVsAvgRatio)
	} else {
		v[2] = 0
	}

	hour, dow, reqMinute, ok := parseRFC3339Parts(p.Transaction.RequestedAt)
	if ok {
		v[3] = q01(float64(hour) / 23.0)
		v[4] = q01(float64(dow) / 6.0)
		if p.LastTx != nil {
			_, _, lastMinute, lastOK := parseRFC3339Parts(p.LastTx.Timestamp)
			if lastOK {
				mins := reqMinute - lastMinute
				if mins < 0 {
					mins = -mins
				}
				v[5] = q01(float64(mins) / maxMinutes)
				v[6] = q01(p.LastTx.KmFromCurrent / maxKm)
			} else {
				v[5] = -scale
				v[6] = -scale
			}
		} else {
			v[5] = -scale
			v[6] = -scale
		}
	} else {
		v[3] = 0
		v[4] = 0
		v[5] = -scale
		v[6] = -scale
	}

	v[7] = q01(p.Terminal.KmFromHome / maxKm)
	v[8] = q01(float64(p.Customer.TxCount24h) / maxTx24h)

	v[9] = 0
	if p.Terminal.IsOnline {
		v[9] = scale
	}
	v[10] = 0
	if p.Terminal.CardPresent {
		v[10] = scale
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
		v[11] = scale
	}

	if risk, ok := mccRisk[p.Merchant.MCC]; ok {
		v[12] = q01(risk)
	} else {
		v[12] = 5000
	}

	v[13] = q01(p.Merchant.AvgAmount / maxMerchantAmt)
}

var json2 = jsoniter.ConfigCompatibleWithStandardLibrary

var responses = [...][]byte{
	[]byte(`{"approved":true,"fraud_score":0}` + "\n"),
	[]byte(`{"approved":true,"fraud_score":0.2}` + "\n"),
	[]byte(`{"approved":true,"fraud_score":0.4}` + "\n"),
	[]byte(`{"approved":false,"fraud_score":0.6}` + "\n"),
	[]byte(`{"approved":false,"fraud_score":0.8}` + "\n"),
	[]byte(`{"approved":false,"fraud_score":1}` + "\n"),
}

func writeScore(w http.ResponseWriter, fraudCount int) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(responses[fraudCount])
}

func fraudHandler(w http.ResponseWriter, r *http.Request) {
	var p Payload
	if err := json2.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var v vec14
	vectorize(&p, &v)
	score := ruleScore(&v)
	if score < ambigMin {
		writeScore(w, 0)
		return
	}
	if score > ambigMax {
		writeScore(w, 5)
		return
	}
	writeScore(w, knnSearch(&v))
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

	if len(mmapData) < 12 || string(mmapData[0:4]) != "RKN4" {
		log.Fatal("index.bin: wrong magic")
	}
	indexN = int(binary.LittleEndian.Uint32(mmapData[4:8]))
	dims := int(binary.LittleEndian.Uint32(mmapData[8:12]))
	if dims != D {
		log.Fatalf("index.bin: wrong dimension %d", dims)
	}

	offset := 12
	vecBytes := indexN * D * 2
	idxVecs = unsafe.Slice((*int16)(unsafe.Pointer(&mmapData[offset])), indexN*D)
	offset += vecBytes
	idxLabs = mmapData[offset : offset+indexN]

	return nil
}

func loadMCCRisk(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var m map[string]float64
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
	log.Printf("Index loaded: ambiguous refs=%d", indexN)

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
