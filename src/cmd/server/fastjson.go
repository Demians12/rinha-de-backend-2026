package main

import "bytes"

// fastVectorize parses the raw JSON request body directly into vec16 + rule score.
// Zero allocation: no intermediate struct, no map, no interface{}.
// Returns ok=false only on malformed JSON (caller falls back to jsoniter path).
func fastVectorize(b []byte, v *vec16) (score int, ok bool) {
	tx := findFrom(b, kTransaction, 0)
	cu := findFrom(b, kCustomer, tx)
	me := findFrom(b, kMerchant, cu)
	te := findFrom(b, kTerminal, me)
	if tx < 0 || cu < 0 || me < 0 || te < 0 {
		return 0, false
	}

	amount, _, aok := numAfter(b, kAmount, tx)
	if !aok {
		return 0, false
	}
	installments, _, iok := intAfter(b, kInstallments, tx)
	if !iok {
		return 0, false
	}
	reqAt, _, rok := strAfter(b, kRequestedAt, tx)
	if !rok {
		return 0, false
	}
	hour, dow, reqMin, tok := parseRFC3339Bytes(reqAt)
	if !tok {
		return 0, false
	}
	customerAvg, _, caok := numAfter(b, kAvgAmount, cu)
	if !caok {
		return 0, false
	}
	txCount, _, tcok := intAfter(b, kTxCount24h, cu)
	if !tcok {
		return 0, false
	}
	knownStart := findFrom(b, kKnownMerchants, cu)
	if knownStart < 0 {
		return 0, false
	}
	knownStart += len(kKnownMerchants)
	knownEnd := findByteFrom(b, ']', knownStart)
	if knownEnd < 0 {
		return 0, false
	}
	merchantID, _, midok := strAfter(b, kID, me)
	if !midok {
		return 0, false
	}
	merchantMCC, _, mccok := strAfter(b, kMCC, me)
	if !mccok {
		return 0, false
	}
	merchantAvg, _, maok := numAfter(b, kAvgAmount, me)
	if !maok {
		return 0, false
	}
	isOnline, _, olok := boolAfter(b, kIsOnline, te)
	if !olok {
		return 0, false
	}
	cardPresent, _, cpok := boolAfter(b, kCardPresent, te)
	if !cpok {
		return 0, false
	}
	kmHome, _, khok := numAfter(b, kKmFromHome, te)
	if !khok {
		return 0, false
	}

	// --- vec16 + rule score ---

	v[0] = q01(amount / maxAmount)
	if amount >= 2000 {
		score++
	}
	if amount >= 500 {
		score++
	}

	v[1] = q01(float64(installments) / maxInstallments)
	if installments >= 6 {
		score++
	}
	if installments >= 4 {
		score++
	}

	if customerAvg > 0 {
		amountVsAvg := amount / customerAvg
		v[2] = q01(amountVsAvg / amtVsAvgRatio)
		if amountVsAvg >= 8 {
			score++
		}
		if amountVsAvg >= 1 {
			score++
		}
	}

	v[3] = q01(float64(hour) / 23.0)
	v[4] = q01(float64(dow) / 6.0)
	if hour < 7 {
		score++
	}
	if hour < 8 || hour >= 21 {
		score++
	}

	// last_transaction
	if findFrom(b, kLastNull, te) >= 0 {
		v[5] = -scale
		v[6] = -scale
	} else if la := findFrom(b, kLast, te); la >= 0 {
		lastAt, _, ltok := strAfter(b, kTimestamp, la)
		_, _, lastMin, ltpok := parseRFC3339Bytes(lastAt)
		if ltok && ltpok {
			mins := reqMin - lastMin
			if mins < 0 {
				mins = -mins
			}
			v[5] = q01(float64(mins) / maxMinutes)
			v[6] = 0
			if mins <= 10 {
				score++
			}
			if mins <= 30 {
				score++
			}
			kmCurrent, _, kcok := numAfter(b, kKmFromCurrent, la)
			if kcok {
				v[6] = q01(kmCurrent / maxKm)
				if kmCurrent >= 200 {
					score++
				}
				if kmCurrent >= 20 {
					score++
				}
			}
		} else {
			v[5] = -scale
			v[6] = -scale
		}
	} else {
		v[5] = -scale
		v[6] = -scale
	}

	v[7] = q01(kmHome / maxKm)
	if kmHome >= 200 {
		score++
	}
	if kmHome >= 50 {
		score++
	}

	v[8] = q01(float64(txCount) / maxTx24h)
	if txCount >= 8 {
		score++
	}
	if txCount >= 6 {
		score++
	}

	v[9] = 0
	if isOnline {
		v[9] = scale
		score++
	}
	v[10] = 0
	if cardPresent {
		v[10] = scale
	} else {
		score++
	}

	v[11] = 0
	if !containsQuoted(b[knownStart:knownEnd], merchantID) {
		v[11] = scale
		score++
	}

	mccRiskVal := int8((scale + 1) / 2)
	if idx := mccIdxBytes(merchantMCC); idx >= 0 {
		mccRiskVal = mccRisk[idx]
	}
	v[12] = mccRiskVal
	if mccRiskVal >= 95 {
		score++
	}
	if mccRiskVal >= 57 {
		score++
	}

	v[13] = q01(merchantAvg / maxMerchantAmt)
	if merchantAvg <= 100 {
		score++
	}

	v[14] = 0
	v[15] = 0

	return score, true
}

// --- JSON key literals ---

var (
	kTransaction   = []byte(`"transaction"`)
	kCustomer      = []byte(`"customer"`)
	kMerchant      = []byte(`"merchant"`)
	kTerminal      = []byte(`"terminal"`)
	kLastNull      = []byte(`"last_transaction":null`)
	kLast          = []byte(`"last_transaction"`)
	kAmount        = []byte(`"amount":`)
	kInstallments  = []byte(`"installments":`)
	kRequestedAt   = []byte(`"requested_at":"`)
	kAvgAmount     = []byte(`"avg_amount":`)
	kTxCount24h    = []byte(`"tx_count_24h":`)
	kKnownMerchants = []byte(`"known_merchants":[`)
	kID            = []byte(`"id":"`)
	kMCC           = []byte(`"mcc":"`)
	kIsOnline      = []byte(`"is_online":`)
	kCardPresent   = []byte(`"card_present":`)
	kKmFromHome    = []byte(`"km_from_home":`)
	kTimestamp     = []byte(`"timestamp":"`)
	kKmFromCurrent = []byte(`"km_from_current":`)
)

// --- primitives ---

func findFrom(b, needle []byte, start int) int {
	if start < 0 || start >= len(b) {
		return -1
	}
	if i := bytes.Index(b[start:], needle); i >= 0 {
		return start + i
	}
	return -1
}

func findByteFrom(b []byte, c byte, start int) int {
	if start < 0 || start >= len(b) {
		return -1
	}
	if i := bytes.IndexByte(b[start:], c); i >= 0 {
		return start + i
	}
	return -1
}

func numAfter(b, key []byte, start int) (float64, int, bool) {
	pos := findFrom(b, key, start)
	if pos < 0 {
		return 0, 0, false
	}
	pos += len(key)
	val := 0.0
	div := 0.0
	for pos < len(b) {
		c := b[pos]
		if c >= '0' && c <= '9' {
			if div == 0 {
				val = val*10 + float64(c-'0')
			} else {
				div *= 10
				val += float64(c-'0') / div
			}
			pos++
			continue
		}
		if c == '.' {
			div = 1
			pos++
			continue
		}
		break
	}
	return val, pos, true
}

func intAfter(b, key []byte, start int) (int, int, bool) {
	pos := findFrom(b, key, start)
	if pos < 0 {
		return 0, 0, false
	}
	pos += len(key)
	val := 0
	for pos < len(b) {
		c := b[pos]
		if c < '0' || c > '9' {
			break
		}
		val = val*10 + int(c-'0')
		pos++
	}
	return val, pos, true
}

func strAfter(b, key []byte, start int) ([]byte, int, bool) {
	pos := findFrom(b, key, start)
	if pos < 0 {
		return nil, 0, false
	}
	pos += len(key)
	end := findByteFrom(b, '"', pos)
	if end < 0 {
		return nil, 0, false
	}
	return b[pos:end], end + 1, true
}

func boolAfter(b, key []byte, start int) (bool, int, bool) {
	pos := findFrom(b, key, start)
	if pos < 0 {
		return false, 0, false
	}
	pos += len(key)
	if pos+4 <= len(b) && b[pos] == 't' {
		return true, pos + 4, true
	}
	if pos+5 <= len(b) && b[pos] == 'f' {
		return false, pos + 5, true
	}
	return false, 0, false
}

func containsQuoted(haystack, needle []byte) bool {
	for {
		i := bytes.Index(haystack, needle)
		if i < 0 {
			return false
		}
		if i > 0 && haystack[i-1] == '"' &&
			i+len(needle) < len(haystack) && haystack[i+len(needle)] == '"' {
			return true
		}
		haystack = haystack[i+1:]
	}
}

// parseRFC3339Bytes parses a []byte timestamp without allocating a string.
func parseRFC3339Bytes(s []byte) (hour, dow, epochMinute int, ok bool) {
	if len(s) < 19 {
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

// mccIdxBytes converts a []byte MCC code to index without allocating a string.
func mccIdxBytes(s []byte) int {
	if len(s) != 4 {
		return -1
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}
