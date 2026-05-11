package runtime

import (
	"math"
	"sync"
)

const (
	hdrBins    = 200
	hdrMinSecs = 100e-6 // 100 µs
	hdrMaxSecs = 30.0   // 30 s
)

// HDRHistogram is a log-spaced histogram covering [100µs, 30s] in 200 bins.
// All methods are safe for concurrent use.
type HDRHistogram struct {
	mu     sync.Mutex
	counts [hdrBins]int64
	total  int64
	logMin float64
	logMax float64
}

// NewHDRHistogram returns an initialised HDRHistogram.
func NewHDRHistogram() *HDRHistogram {
	return &HDRHistogram{
		logMin: math.Log(hdrMinSecs),
		logMax: math.Log(hdrMaxSecs),
	}
}

// binIndex maps a value (seconds) to a bucket index, clamped to [0, hdrBins-1].
func (h *HDRHistogram) binIndex(v float64) int {
	if v <= hdrMinSecs {
		return 0
	}
	if v >= hdrMaxSecs {
		return hdrBins - 1
	}
	ratio := (math.Log(v) - h.logMin) / (h.logMax - h.logMin)
	idx := int(ratio * float64(hdrBins))
	if idx >= hdrBins {
		idx = hdrBins - 1
	}
	return idx
}

// Record adds one observation (in seconds).
func (h *HDRHistogram) Record(v float64) {
	idx := h.binIndex(v)
	h.mu.Lock()
	h.counts[idx]++
	h.total++
	h.mu.Unlock()
}

// Percentile returns the value (seconds) at the given quantile p ∈ [0, 1].
// Returns 0 when no data has been recorded.
func (h *HDRHistogram) Percentile(p float64) float64 {
	h.mu.Lock()
	counts := h.counts
	total := h.total
	h.mu.Unlock()

	if total == 0 {
		return 0
	}
	target := int64(math.Ceil(p * float64(total)))
	var cum int64
	for i, c := range counts {
		cum += c
		if cum >= target {
			// Return the midpoint of the log-spaced bin.
			lo := math.Exp(h.logMin + float64(i)*(h.logMax-h.logMin)/float64(hdrBins))
			hi := math.Exp(h.logMin + float64(i+1)*(h.logMax-h.logMin)/float64(hdrBins))
			return (lo + hi) / 2
		}
	}
	return hdrMaxSecs
}

// Snapshot returns four key percentiles in one locked read.
func (h *HDRHistogram) Snapshot() (p50, p95, p99, p999 float64) {
	h.mu.Lock()
	counts := h.counts
	total := h.total
	h.mu.Unlock()

	if total == 0 {
		return
	}

	targets := [4]int64{
		int64(math.Ceil(0.500 * float64(total))),
		int64(math.Ceil(0.950 * float64(total))),
		int64(math.Ceil(0.990 * float64(total))),
		int64(math.Ceil(0.999 * float64(total))),
	}
	results := [4]float64{}
	filled := 0
	var cum int64
	for i, c := range counts {
		cum += c
		lo := math.Exp(h.logMin + float64(i)*(h.logMax-h.logMin)/float64(hdrBins))
		hi := math.Exp(h.logMin + float64(i+1)*(h.logMax-h.logMin)/float64(hdrBins))
		mid := (lo + hi) / 2
		for j, t := range targets {
			if results[j] == 0 && cum >= t {
				results[j] = mid
				filled++
			}
		}
		if filled == 4 {
			break
		}
	}
	return results[0], results[1], results[2], results[3]
}
