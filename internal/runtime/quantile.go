package runtime

import (
	"math"
	"sort"
	"sync"
)

const (
	cpAlpha      = 0.05  // 95% coverage (5% error rate)
	cpWindowSize = 500   // sliding calibration window
)

// ConformalPredictor provides distribution-free prediction intervals using
// split conformal prediction over a sliding window of residuals.
// Safe for concurrent use.
type ConformalPredictor struct {
	mu        sync.Mutex
	residuals []float64 // circular window of |obs - center|
	head      int
	n         int
}

// NewConformalPredictor returns a ready-to-use ConformalPredictor.
func NewConformalPredictor() *ConformalPredictor {
	return &ConformalPredictor{
		residuals: make([]float64, cpWindowSize),
	}
}

// Update adds one (obs, center) pair to the calibration window.
func (cp *ConformalPredictor) Update(obs, center float64) {
	r := math.Abs(obs - center)
	cp.mu.Lock()
	cp.residuals[cp.head] = r
	cp.head = (cp.head + 1) % cpWindowSize
	if cp.n < cpWindowSize {
		cp.n++
	}
	cp.mu.Unlock()
}

// Interval returns the 95% conformal prediction interval centred on center.
// Returns (center, center) when fewer than 2 calibration points are available.
func (cp *ConformalPredictor) Interval(center float64) (lo, hi float64) {
	cp.mu.Lock()
	n := cp.n
	buf := make([]float64, n)
	start := 0
	if n == cpWindowSize {
		start = cp.head
	}
	for i := range n {
		buf[i] = cp.residuals[(start+i)%cpWindowSize]
	}
	cp.mu.Unlock()

	if n < 2 {
		return center, center
	}

	sort.Float64s(buf)
	// Conformal quantile: smallest q such that at least ⌈(n+1)(1-alpha)⌉/n of
	// residuals fall below q. Use the standard finite-sample correction.
	idx := int(math.Ceil(float64(n+1)*(1-cpAlpha))) - 1
	if idx >= n {
		idx = n - 1
	}
	q := buf[idx]
	return center - q, center + q
}
