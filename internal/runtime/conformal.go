package runtime

import (
	"math"
	"sort"
	"sync"
)

const (
	conformalCap  = 200
	conformalMinN = 20
	conformalAlpha = 0.95
)

// ConformalEnvelope maintains a sliding window of nonconformity scores and
// provides anomaly detection via conformal prediction.
type ConformalEnvelope struct {
	mu sync.Mutex

	// Calibration ring buffer of nonconformity scores |x - mu|.
	scores  [conformalCap]float64
	sHead   int
	sCount  int

	// Anomaly decision ring buffer.
	anomalies  [conformalCap]bool
	aHead      int
	aCount     int
	anomCount  int // number of true entries in the anomaly buffer

	// Running mean of raw observations (Welford).
	runMean float64
	runN    int
}

// NewConformalEnvelope constructs a ready-to-use ConformalEnvelope.
func NewConformalEnvelope() *ConformalEnvelope {
	return &ConformalEnvelope{}
}

// Update feeds a new observation x, returns (anomaly, threshold).
func (c *ConformalEnvelope) Update(x float64) (anomaly bool, threshold float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Step 1: compute mu from running mean BEFORE adding x.
	mu := c.runMean

	// Step 2: nonconformity score.
	score := math.Abs(x - mu)

	// Step 3: compute threshold from current calibration window.
	threshold = c.computeThreshold()

	// Step 4: anomaly decision (only flag if we have enough calibration data).
	anomaly = c.sCount >= conformalMinN && score > threshold

	// Step 5: add score to calibration buffer.
	if c.sCount == conformalCap {
		// Evict oldest score (no adjustment needed, just overwrite).
		_ = c.scores[c.sHead]
	}
	c.scores[c.sHead] = score
	c.sHead = (c.sHead + 1) % conformalCap
	if c.sCount < conformalCap {
		c.sCount++
	}

	// Step 5b: add anomaly to anomaly ring buffer.
	if c.aCount == conformalCap {
		// Evict oldest anomaly decision.
		oldest := c.anomalies[c.aHead]
		if oldest {
			c.anomCount--
		}
	}
	c.anomalies[c.aHead] = anomaly
	if anomaly {
		c.anomCount++
	}
	c.aHead = (c.aHead + 1) % conformalCap
	if c.aCount < conformalCap {
		c.aCount++
	}

	// Step 6: push x into running mean.
	c.runN++
	c.runMean += (x - c.runMean) / float64(c.runN)

	return anomaly, threshold
}

// computeThreshold computes the conformal quantile from the calibration window.
// Caller must hold c.mu.
func (c *ConformalEnvelope) computeThreshold() float64 {
	if c.sCount == 0 {
		return math.Inf(1)
	}
	n := c.sCount

	// Copy scores to a sorted slice.
	sorted := make([]float64, n)
	for i := range n {
		sorted[i] = c.scores[i]
	}
	sort.Float64s(sorted)

	// q = score[ceil((n+1)*0.95) - 1], clamped to valid index.
	idx := int(math.Ceil(float64(n+1)*conformalAlpha)) - 1
	idx = max(idx, 0)
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

// AnomalyRate returns the fraction of recent anomaly decisions that were true.
func (c *ConformalEnvelope) AnomalyRate() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.aCount == 0 {
		return 0
	}
	return float64(c.anomCount) / float64(c.aCount)
}

// LastThreshold returns the current threshold without updating state.
func (c *ConformalEnvelope) LastThreshold() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.computeThreshold()
}
