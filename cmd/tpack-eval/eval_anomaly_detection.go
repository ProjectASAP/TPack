package main

import (
	"math"
	"runtime"
	"sort"
	"sync"
)

// CUSUM (Page 1954, Biometrika 41:100-115) parameters. Two-sided form:
//
//	S+[t] = max(0, S+[t-1] + z[t] - k)
//	S-[t] = min(0, S-[t-1] + z[t] + k)
//	alarm at bucket t iff |S+[t]| > h or |S-[t]| > h
const (
	cusumK  = 0.5
	cusumH  = 5.0
	cusumW0 = 11
)

type alarmRecord struct {
	Bucket int     `json:"bucket"`
	Signal string  `json:"signal"`
	S      float64 `json:"S"`
}

// evaluateAnomalyDetection runs per-service two-sided CUSUM on log-mean and
// log-p95 duration time series; alarms are OR'd across the two signals. The
// detector consumes only the time series, never injectTimeUs — that field is
// used solely to annotate the JSON output (inject_bucket) for downstream
// scoring.
func evaluateAnomalyDetection(dir string, traces []evalTrace, injectTimeUs *int64) error {
	durations, minBucket, maxBucket := collectDurationsByServiceBucket(traces)
	if len(durations) == 0 {
		return writeEvalResult(dir, "anomaly_detection_results.json", emptyAnomalyResult())
	}
	numBuckets := int(maxBucket - minBucket + 1)

	services := make([]string, 0, len(durations))
	for svc := range durations {
		services = append(services, svc)
	}
	sort.Strings(services)

	allAlarms := make(map[string][]alarmRecord)
	for _, svc := range services {
		buckets := durations[svc]
		for _, sig := range []struct {
			name string
			fn   func([]float64) (float64, bool)
		}{
			{"logmean", signalLogMean},
			{"logp95", signalLogP95},
		} {
			series, present := buildSeries(buckets, minBucket, numBuckets, sig.fn)
			alarms := runCUSUM(series, present, sig.name)
			if len(alarms) > 0 {
				allAlarms[svc] = append(allAlarms[svc], alarms...)
			}
		}
		// Sort this service's alarms by bucket for deterministic output.
		sort.Slice(allAlarms[svc], func(i, j int) bool {
			return allAlarms[svc][i].Bucket < allAlarms[svc][j].Bucket
		})
	}

	firstBucket, firstService := firstAlarm(allAlarms)
	rankedServices := localize(allAlarms, firstBucket)

	var injectBucket any = nil
	if injectTimeUs != nil {
		ib := *injectTimeUs/60_000_000 - minBucket
		if ib >= 0 && ib < int64(numBuckets) {
			injectBucket = int(ib)
		}
	}

	var firstBucketJSON any = nil
	var firstServiceJSON any = nil
	if firstBucket >= 0 {
		firstBucketJSON = firstBucket
		firstServiceJSON = firstService
	}

	result := map[string]any{
		"params": map[string]any{
			"k":       cusumK,
			"h":       cusumH,
			"W0":      cusumW0,
			"signals": []string{"logmean", "logp95"},
		},
		"alarms":              allAlarms,
		"first_alarm_bucket":  firstBucketJSON,
		"first_alarm_service": firstServiceJSON,
		"ranked_services":     rankedServices,
		"inject_bucket":       injectBucket,
		"num_buckets":         numBuckets,
	}
	return writeEvalResult(dir, "anomaly_detection_results.json", result)
}

func emptyAnomalyResult() map[string]any {
	return map[string]any{
		"params": map[string]any{
			"k":       cusumK,
			"h":       cusumH,
			"W0":      cusumW0,
			"signals": []string{"logmean", "logp95"},
		},
		"alarms":              map[string][]alarmRecord{},
		"first_alarm_bucket":  nil,
		"first_alarm_service": nil,
		"ranked_services":     []string{},
		"inject_bucket":       nil,
		"num_buckets":         0,
	}
}

// collectDurationsByServiceBucket shards traces across CPUs, accumulating
// per-(service, minute-bucket) duration slices. Returns the map plus the
// global min/max bucket indices.
func collectDurationsByServiceBucket(traces []evalTrace) (map[string]map[int64][]float64, int64, int64) {
	nw := max(min(runtime.NumCPU(), len(traces)), 1)

	partials := make([]map[string]map[int64][]float64, nw)
	var wg sync.WaitGroup
	for w := range nw {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			local := make(map[string]map[int64][]float64)
			for i := w; i < len(traces); i += nw {
				for _, span := range traces[i] {
					svc := span.Feature.ServiceName()
					if svc == "" {
						continue
					}
					bucket := timeBucketMinute(span.StartTime)
					m, ok := local[svc]
					if !ok {
						m = make(map[int64][]float64)
						local[svc] = m
					}
					m[bucket] = append(m[bucket], float64(span.Duration))
				}
			}
			partials[w] = local
		}(w)
	}
	wg.Wait()

	merged := make(map[string]map[int64][]float64)
	var minBucket int64 = math.MaxInt64
	var maxBucket int64 = math.MinInt64
	for _, local := range partials {
		for svc, buckets := range local {
			m, ok := merged[svc]
			if !ok {
				m = make(map[int64][]float64)
				merged[svc] = m
			}
			for b, durs := range buckets {
				m[b] = append(m[b], durs...)
				if b < minBucket {
					minBucket = b
				}
				if b > maxBucket {
					maxBucket = b
				}
			}
		}
	}
	if len(merged) == 0 {
		return merged, 0, 0
	}
	return merged, minBucket, maxBucket
}

// buildSeries materializes a per-bucket signal value for one service.
// Returns parallel arrays (value, present) of length numBuckets.
func buildSeries(buckets map[int64][]float64, minBucket int64, numBuckets int, sig func([]float64) (float64, bool)) ([]float64, []bool) {
	series := make([]float64, numBuckets)
	present := make([]bool, numBuckets)
	for b, durs := range buckets {
		v, ok := sig(durs)
		if ok {
			series[b-minBucket] = v
			present[b-minBucket] = true
		}
	}
	return series, present
}

// runCUSUM consumes the first cusumW0 *present* buckets as a baseline,
// then runs two-sided CUSUM on the remainder. Resets on every alarm.
func runCUSUM(series []float64, present []bool, signalName string) []alarmRecord {
	var baseline []float64
	baselineEnd := -1
	for i := range series {
		if present[i] {
			baseline = append(baseline, series[i])
			if len(baseline) == cusumW0 {
				baselineEnd = i
				break
			}
		}
	}
	if len(baseline) < cusumW0 {
		return nil
	}
	mu0, sigma0 := meanStdev(baseline)
	if sigma0 <= 0 {
		return nil
	}

	var alarms []alarmRecord
	var sPlus, sMinus float64
	for i := baselineEnd + 1; i < len(series); i++ {
		if !present[i] {
			continue
		}
		z := (series[i] - mu0) / sigma0
		sPlus = math.Max(0, sPlus+z-cusumK)
		sMinus = math.Min(0, sMinus+z+cusumK)
		mag := math.Max(sPlus, -sMinus)
		if mag > cusumH {
			alarms = append(alarms, alarmRecord{Bucket: i, Signal: signalName, S: mag})
			sPlus = 0
			sMinus = 0
		}
	}
	return alarms
}

func firstAlarm(allAlarms map[string][]alarmRecord) (int, string) {
	bestBucket := -1
	bestService := ""
	for svc, alarms := range allAlarms {
		for _, a := range alarms {
			if bestBucket < 0 || a.Bucket < bestBucket ||
				(a.Bucket == bestBucket && svc < bestService) {
				bestBucket = a.Bucket
				bestService = svc
			}
		}
	}
	return bestBucket, bestService
}

// localize ranks services by max |S| across all alarms in the full window.
// At detection time, an operator inspects the alarm history; this corresponds
// to "which service's CUSUM showed the strongest deviation at any point." A
// strict ±1-bucket window misses cases where a downstream service amplifies
// faster than the injected upstream service (common with CPU/mem faults that
// propagate via synchronous calls). Returns an empty slice if no alarms.
func localize(allAlarms map[string][]alarmRecord, firstBucket int) []string {
	if firstBucket < 0 {
		return []string{}
	}
	type svcMag struct {
		name string
		mag  float64
	}
	var ranked []svcMag
	for svc, alarms := range allAlarms {
		var maxMag float64
		for _, a := range alarms {
			if a.S > maxMag {
				maxMag = a.S
			}
		}
		if maxMag > 0 {
			ranked = append(ranked, svcMag{svc, maxMag})
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].mag != ranked[j].mag {
			return ranked[i].mag > ranked[j].mag
		}
		return ranked[i].name < ranked[j].name
	})
	out := make([]string, len(ranked))
	for i, r := range ranked {
		out[i] = r.name
	}
	return out
}

func signalLogMean(durs []float64) (float64, bool) {
	if len(durs) == 0 {
		return 0, false
	}
	var sum float64
	for _, d := range durs {
		sum += d
	}
	mean := sum / float64(len(durs))
	if mean <= 0 {
		return 0, false
	}
	return math.Log(mean), true
}

func signalLogP95(durs []float64) (float64, bool) {
	if len(durs) == 0 {
		return 0, false
	}
	sorted := append([]float64(nil), durs...)
	sort.Float64s(sorted)
	p95 := percentile(sorted, 95)
	if p95 <= 0 {
		return 0, false
	}
	return math.Log(p95), true
}

// meanStdev returns (mean, Bessel-corrected stdev). Returns (mean, 0) for n<=1.
func meanStdev(x []float64) (float64, float64) {
	if len(x) == 0 {
		return 0, 0
	}
	if len(x) == 1 {
		return x[0], 0
	}
	var sum float64
	for _, v := range x {
		sum += v
	}
	mean := sum / float64(len(x))
	var ss float64
	for _, v := range x {
		ss += (v - mean) * (v - mean)
	}
	return mean, math.Sqrt(ss / float64(len(x)-1))
}
