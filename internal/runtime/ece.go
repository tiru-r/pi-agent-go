package runtime

import (
	"math"
	"sync"
)

const eceBins = 10

// ECECalibrator bins (probability, outcome) pairs into 10 equal-width buckets
// and computes Expected Calibration Error = Σ |acc(b) - conf(b)| * |b|/n.
// Safe for concurrent use.
type ECECalibrator struct {
	mu      sync.Mutex
	correct [eceBins]float64 // sum of correct outcomes per bin
	total   [eceBins]float64 // count of samples per bin
	conf    [eceBins]float64 // sum of confidence values per bin
	n       int
}

// NewECECalibrator returns a ready-to-use ECECalibrator.
func NewECECalibrator() *ECECalibrator {
	return &ECECalibrator{}
}

// Update records one (prob, correct) pair. prob ∈ [0, 1]; correct is 1 for a
// correct prediction, 0 for an incorrect one.
func (e *ECECalibrator) Update(prob float64, correct bool) {
	if prob < 0 {
		prob = 0
	}
	if prob > 1 {
		prob = 1
	}
	bin := int(prob * eceBins)
	if bin >= eceBins {
		bin = eceBins - 1
	}
	c := 0.0
	if correct {
		c = 1.0
	}
	e.mu.Lock()
	e.correct[bin] += c
	e.conf[bin] += prob
	e.total[bin]++
	e.n++
	e.mu.Unlock()
}

// ECE returns the current Expected Calibration Error. Returns 0 when no data.
func (e *ECECalibrator) ECE() float64 {
	e.mu.Lock()
	correct := e.correct
	conf := e.conf
	total := e.total
	n := e.n
	e.mu.Unlock()

	if n == 0 {
		return 0
	}
	var ece float64
	for b := range eceBins {
		nb := total[b]
		if nb == 0 {
			continue
		}
		acc := correct[b] / nb
		avgConf := conf[b] / nb
		ece += math.Abs(acc-avgConf) * nb / float64(n)
	}
	return ece
}
