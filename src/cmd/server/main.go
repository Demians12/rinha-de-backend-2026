// server: HTTP API for fraud detection via cheap rules + exact KNN fallback.
package main

import (
	"encoding/binary"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	jsoniter "github.com/json-iterator/go"
	"github.com/valyala/fasthttp"
)

const (
	D            = 14
	vecD         = 16
	K            = 5
	scale        = 127
	ambigMin     = 3
	ambigMax     = 19
	ivfClusters  = 2048
	scoreBuckets = 23
	maxProbe     = 16
	maxDist      = 1<<63 - 1
)

var (
	mmapData     []byte
	indexN       int
	indexK       int
	idxCentroids []int16
	idxBBoxMin   []int8
	idxBBoxMax   []int8
	idxBucketMin []int8
	idxBucketMax []int8
	idxOffsets   []uint32
	idxVecs      []int8
	idxLabs      []uint8
	idxIDs       []uint32
	ivfNProbe    = 1
	ivfRepair    = true
)

type vec16 [vecD]int8

func q01(x float64) int8 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return scale
	}
	return int8(x*scale + 0.5)
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

func betterPair(d int64, id uint32, bestD int64, bestID uint32) bool {
	return d < bestD || (d == bestD && id < bestID)
}

func insertTopK(best *[K]int64, labels *[K]uint8, ids *[K]uint32, d int64, label uint8, id uint32) {
	if !betterPair(d, id, best[K-1], ids[K-1]) {
		return
	}
	pos := K - 1
	for pos > 0 && betterPair(d, id, best[pos-1], ids[pos-1]) {
		best[pos] = best[pos-1]
		labels[pos] = labels[pos-1]
		ids[pos] = ids[pos-1]
		pos--
	}
	best[pos] = d
	labels[pos] = label
	ids[pos] = id
}

func insertProbe(bestC *[maxProbe]int, bestD *[maxProbe]int64, nprobe int, c int, d int64) {
	if d >= bestD[nprobe-1] {
		return
	}
	pos := nprobe - 1
	for pos > 0 && d < bestD[pos-1] {
		bestD[pos] = bestD[pos-1]
		bestC[pos] = bestC[pos-1]
		pos--
	}
	bestD[pos] = d
	bestC[pos] = c
}

func centroidDist(q *vec16, c int) int64 {
	return centroidDistSIMD(q, &idxCentroids[c*vecD])
}

func bboxLowerBound(q *vec16, c int) int64 {
	off := c * vecD
	return lboundSIMD(q, &idxBBoxMin[off], &idxBBoxMax[off])
}

func bucketLowerBound(q *vec16, c, score int) int64 {
	off := (c*scoreBuckets + score) * vecD
	return lboundSIMD(q, &idxBucketMin[off], &idxBucketMax[off])
}

func scanRange(q *vec16, start, end int, best *[K]int64, labels *[K]uint8, ids *[K]uint32) {
	for i := start; i < end; i++ {
		ref := (*vec16)(unsafe.Pointer(&idxVecs[i*vecD]))
		insertTopK(best, labels, ids, dist2(q, ref), idxLabs[i], idxIDs[i])
	}
}

func insertRepairCluster(clusters *[ivfClusters]int, bounds *[ivfClusters]int64, n int, c int, bound int64) int {
	pos := n
	for pos > 0 && bound < bounds[pos-1] {
		bounds[pos] = bounds[pos-1]
		clusters[pos] = clusters[pos-1]
		pos--
	}
	bounds[pos] = bound
	clusters[pos] = c
	return n + 1
}

func insertScoreBucket(scores *[scoreBuckets]int, bounds *[scoreBuckets]int64, n int, score int, bound int64) int {
	pos := n
	for pos > 0 && bound < bounds[pos-1] {
		bounds[pos] = bounds[pos-1]
		scores[pos] = scores[pos-1]
		pos--
	}
	bounds[pos] = bound
	scores[pos] = score
	return n + 1
}

func clusterStart(c int) int {
	return int(idxOffsets[c*(scoreBuckets+1)])
}

func clusterEnd(c int) int {
	return int(idxOffsets[c*(scoreBuckets+1)+scoreBuckets])
}

func scoreBucketStart(c, score int) int {
	return int(idxOffsets[c*(scoreBuckets+1)+score])
}

func scoreBucketEnd(c, score int) int {
	return int(idxOffsets[c*(scoreBuckets+1)+score+1])
}

func countFrauds(labels *[K]uint8) (fraudCount int) {
	for i := 0; i < K; i++ {
		if labels[i] == 1 {
			fraudCount++
		}
	}
	return fraudCount
}

func initTopK() ([K]int64, [K]uint8, [K]uint32) {
	return [K]int64{maxDist, maxDist, maxDist, maxDist, maxDist},
		[K]uint8{},
		[K]uint32{^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0), ^uint32(0)}
}

func knnTopKIVF(q *vec16) ([K]int64, [K]uint8, [K]uint32) {
	best, labels, ids := initTopK()
	nprobe := ivfNProbe
	if nprobe > indexK {
		nprobe = indexK
	}
	if nprobe < 1 {
		nprobe = 1
	}

	bestC := [maxProbe]int{}
	bestCD := [maxProbe]int64{}
	for i := 0; i < nprobe; i++ {
		bestC[i] = -1
		bestCD[i] = maxDist
	}
	for c := 0; c < indexK; c++ {
		insertProbe(&bestC, &bestCD, nprobe, c, centroidDist(q, c))
	}

	var scanned [ivfClusters]bool
	for i := 0; i < nprobe; i++ {
		c := bestC[i]
		if c < 0 {
			continue
		}
		scanned[c] = true
		var bScores [scoreBuckets]int
		var bBounds [scoreBuckets]int64
		bN := 0
		for score := 0; score < scoreBuckets; score++ {
			if scoreBucketStart(c, score) == scoreBucketEnd(c, score) {
				continue
			}
			bound := bucketLowerBound(q, c, score)
			if bound <= best[K-1] {
				bN = insertScoreBucket(&bScores, &bBounds, bN, score, bound)
			}
		}
		for j := 0; j < bN; j++ {
			if bBounds[j] > best[K-1] {
				break
			}
			scanRange(q, scoreBucketStart(c, bScores[j]), scoreBucketEnd(c, bScores[j]), &best, &labels, &ids)
		}
	}
	if ivfRepair {
		var repairC [ivfClusters]int
		var repairBound [ivfClusters]int64
		repairN := 0
		for c := 0; c < indexK; c++ {
			if scanned[c] || clusterStart(c) == clusterEnd(c) {
				continue
			}
			bound := bboxLowerBound(q, c)
			if bound <= best[K-1] {
				repairN = insertRepairCluster(&repairC, &repairBound, repairN, c, bound)
			}
		}
		for i := 0; i < repairN; i++ {
			if repairBound[i] > best[K-1] {
				break
			}
			c := repairC[i]
			var bucketScores [scoreBuckets]int
			var bucketBounds [scoreBuckets]int64
			bucketN := 0
			for score := 0; score < scoreBuckets; score++ {
				start := scoreBucketStart(c, score)
				end := scoreBucketEnd(c, score)
				if start == end {
					continue
				}
				bound := bucketLowerBound(q, c, score)
				if bound <= best[K-1] {
					bucketN = insertScoreBucket(&bucketScores, &bucketBounds, bucketN, score, bound)
				}
			}
			for j := 0; j < bucketN; j++ {
				if bucketBounds[j] > best[K-1] {
					break
				}
				score := bucketScores[j]
				scanRange(q, scoreBucketStart(c, score), scoreBucketEnd(c, score), &best, &labels, &ids)
			}
		}
	}

	return best, labels, ids
}

func knnSearch(q *vec16) int {
	_, labels, _ := knnTopKIVF(q)
	return countFrauds(&labels)
}

var (
	maxAmount       = 10000.0
	maxInstallments = 12.0
	amtVsAvgRatio   = 10.0
	maxMinutes      = 1440.0
	maxKm           = 1000.0
	maxTx24h        = 20.0
	maxMerchantAmt  = 10000.0
	mccRisk         [10000]int8
)

func mccIdx(s string) int {
	if len(s) != 4 {
		return -1
	}
	n := 0
	for i := 0; i < 4; i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
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

var payloadPool = sync.Pool{
	New: func() any { return &Payload{} },
}

func vectorize(p *Payload, v *vec16) int {
	score := 0

	amount := p.Transaction.Amount
	v[0] = q01(amount / maxAmount)
	if amount >= 2000 {
		score++
	}
	if amount >= 500 {
		score++
	}

	installments := p.Transaction.Installments
	v[1] = q01(float64(installments) / maxInstallments)
	if installments >= 6 {
		score++
	}
	if installments >= 4 {
		score++
	}

	if p.Customer.AvgAmount > 0 {
		amountVsAvg := amount / p.Customer.AvgAmount
		v[2] = q01(amountVsAvg / amtVsAvgRatio)
		if amountVsAvg >= 8 {
			score++
		}
		if amountVsAvg >= 1 {
			score++
		}
	} else {
		v[2] = 0
	}

	hour, dow, reqMinute, ok := parseRFC3339Parts(p.Transaction.RequestedAt)
	if ok {
		v[3] = q01(float64(hour) / 23.0)
		v[4] = q01(float64(dow) / 6.0)
		if hour < 7 {
			score++
		}
		if hour < 8 || hour >= 21 {
			score++
		}
		if p.LastTx != nil {
			_, _, lastMinute, lastOK := parseRFC3339Parts(p.LastTx.Timestamp)
			if lastOK {
				mins := reqMinute - lastMinute
				if mins < 0 {
					mins = -mins
				}
				v[5] = q01(float64(mins) / maxMinutes)
				if mins <= 10 {
					score++
				}
				if mins <= 30 {
					score++
				}
				lastKm := p.LastTx.KmFromCurrent
				v[6] = q01(lastKm / maxKm)
				if lastKm >= 200 {
					score++
				}
				if lastKm >= 20 {
					score++
				}
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

	kmFromHome := p.Terminal.KmFromHome
	v[7] = q01(kmFromHome / maxKm)
	if kmFromHome >= 200 {
		score++
	}
	if kmFromHome >= 50 {
		score++
	}

	txCount24h := p.Customer.TxCount24h
	v[8] = q01(float64(txCount24h) / maxTx24h)
	if txCount24h >= 8 {
		score++
	}
	if txCount24h >= 6 {
		score++
	}

	v[9] = 0
	if p.Terminal.IsOnline {
		v[9] = scale
		score++
	}
	v[10] = 0
	if p.Terminal.CardPresent {
		v[10] = scale
	} else {
		score++
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
		score++
	}

	mccRiskVal := int8((scale + 1) / 2)
	if idx := mccIdx(p.Merchant.MCC); idx >= 0 {
		mccRiskVal = mccRisk[idx]
	}
	v[12] = mccRiskVal
	if mccRiskVal >= 95 {
		score++
	}
	if mccRiskVal >= 57 {
		score++
	}

	merchantAvg := p.Merchant.AvgAmount
	v[13] = q01(merchantAvg / maxMerchantAmt)
	if merchantAvg <= 100 {
		score++
	}

	v[14] = 0
	v[15] = 0

	return score
}

var json2 = jsoniter.ConfigCompatibleWithStandardLibrary

var (
	ctJSON    = []byte("application/json")
	responses = [...][]byte{
		[]byte(`{"approved":true,"fraud_score":0}` + "\n"),
		[]byte(`{"approved":true,"fraud_score":0.2}` + "\n"),
		[]byte(`{"approved":false,"fraud_score":0.4}` + "\n"),
		[]byte(`{"approved":false,"fraud_score":0.6}` + "\n"),
		[]byte(`{"approved":false,"fraud_score":0.8}` + "\n"),
		[]byte(`{"approved":false,"fraud_score":1}` + "\n"),
	}
)

func fraudHandler(ctx *fasthttp.RequestCtx) {
	p := payloadPool.Get().(*Payload)
	defer func() {
		p.Customer.KnownMerchants = p.Customer.KnownMerchants[:0]
		p.LastTx = nil
		payloadPool.Put(p)
	}()

	if err := json2.Unmarshal(ctx.PostBody(), p); err != nil {
		ctx.SetStatusCode(fasthttp.StatusBadRequest)
		return
	}

	var v vec16
	score := vectorize(p, &v)

	var fraudCount int
	if score < ambigMin {
		fraudCount = 0
	} else if score > ambigMax {
		fraudCount = 5
	} else {
		fraudCount = knnSearch(&v)
	}

	ctx.SetContentTypeBytes(ctJSON)
	ctx.SetBody(responses[fraudCount])
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

	if len(mmapData) < 24 || string(mmapData[0:4]) != "RIV3" {
		log.Fatal("index.bin: wrong magic")
	}
	indexN = int(binary.LittleEndian.Uint32(mmapData[4:8]))
	indexK = int(binary.LittleEndian.Uint32(mmapData[8:12]))
	dims := int(binary.LittleEndian.Uint32(mmapData[12:16]))
	indexScale := int(binary.LittleEndian.Uint32(mmapData[16:20]))
	indexScoreBuckets := int(binary.LittleEndian.Uint32(mmapData[20:24]))
	if dims != vecD {
		log.Fatalf("index.bin: wrong dimension %d (want %d)", dims, vecD)
	}
	if indexScale != scale {
		log.Fatalf("index.bin: wrong scale %d (want %d)", indexScale, scale)
	}
	if indexK < 1 || indexK > ivfClusters {
		log.Fatalf("index.bin: wrong cluster count %d", indexK)
	}
	if indexScoreBuckets != scoreBuckets {
		log.Fatalf("index.bin: wrong score buckets %d (want %d)", indexScoreBuckets, scoreBuckets)
	}

	centroidBytes := indexK * vecD * 2
	bboxBytes := indexK * vecD
	bucketBBoxBytes := indexK * scoreBuckets * vecD
	offsetBytes := indexK * (scoreBuckets + 1) * 4
	vecBytes := indexN * vecD
	labelBytes := indexN
	idPad := (4 - (labelBytes & 3)) & 3
	idBytes := indexN * 4
	expected := 24 + centroidBytes + bboxBytes*2 + bucketBBoxBytes*2 + offsetBytes + vecBytes + labelBytes + idPad + idBytes
	if len(mmapData) < expected {
		log.Fatalf("index.bin: truncated: got %d bytes, want at least %d", len(mmapData), expected)
	}

	offset := 24
	idxCentroids = unsafe.Slice((*int16)(unsafe.Pointer(&mmapData[offset])), indexK*vecD)
	offset += centroidBytes
	idxBBoxMin = unsafe.Slice((*int8)(unsafe.Pointer(&mmapData[offset])), indexK*vecD)
	offset += bboxBytes
	idxBBoxMax = unsafe.Slice((*int8)(unsafe.Pointer(&mmapData[offset])), indexK*vecD)
	offset += bboxBytes
	idxBucketMin = unsafe.Slice((*int8)(unsafe.Pointer(&mmapData[offset])), indexK*scoreBuckets*vecD)
	offset += bucketBBoxBytes
	idxBucketMax = unsafe.Slice((*int8)(unsafe.Pointer(&mmapData[offset])), indexK*scoreBuckets*vecD)
	offset += bucketBBoxBytes
	idxOffsets = unsafe.Slice((*uint32)(unsafe.Pointer(&mmapData[offset])), indexK*(scoreBuckets+1))
	offset += offsetBytes
	idxVecs = unsafe.Slice((*int8)(unsafe.Pointer(&mmapData[offset])), indexN*vecD)
	offset += vecBytes
	idxLabs = mmapData[offset : offset+indexN]
	offset += labelBytes + idPad
	idxIDs = unsafe.Slice((*uint32)(unsafe.Pointer(&mmapData[offset])), indexN)

	return nil
}

func loadMCCRisk(path string) error {
	defaultRisk := int8((scale + 1) / 2)
	for i := range mccRisk {
		mccRisk[i] = defaultRisk
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var m map[string]float64
	if err := jsoniter.NewDecoder(f).Decode(&m); err != nil {
		return err
	}

	for k, v := range m {
		n, err := strconv.Atoi(k)
		if err != nil || n < 0 || n >= 10000 {
			continue
		}
		val := int(v*scale + 0.5)
		if val < 0 {
			val = 0
		}
		if val > scale {
			val = scale
		}
		mccRisk[n] = int8(val)
	}
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
	ivfNProbe = envInt("IVF_NPROBE", 1, 1, maxProbe)
	ivfRepair = envInt("IVF_REPAIR", 1, 0, 1) != 0
	log.Printf("Index loaded: refs=%d clusters=%d nprobe=%d repair=%t", indexN, indexK, ivfNProbe, ivfRepair)

	os.Remove(udsPath)

	mux := func(ctx *fasthttp.RequestCtx) {
		switch string(ctx.Path()) {
		case "/fraud-score":
			fraudHandler(ctx)
		case "/ready":
			ctx.SetStatusCode(fasthttp.StatusOK)
		default:
			ctx.SetStatusCode(fasthttp.StatusNotFound)
		}
	}

	srv := &fasthttp.Server{
		Handler:            mux,
		ReadTimeout:        2 * time.Second,
		WriteTimeout:       2 * time.Second,
		MaxRequestBodySize: 64 * 1024,
	}

	ln, err := net.Listen("unix", udsPath)
	if err != nil {
		log.Fatalf("listen unix: %v", err)
	}
	os.Chmod(udsPath, 0666)

	log.Printf("Listening on %s", udsPath)
	if err := srv.Serve(ln); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
