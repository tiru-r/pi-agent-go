package runtime

import (
	"math"
	"sync"
)

// PACBayesSafety tracks observations and computes a PAC-Bayes upper bound on
// the true error rate.
type PACBayesSafety struct {
	mu    sync.Mutex
	n     int
	errors int
	klQP  float64 // KL(Q||P), set externally
	delta float64 // confidence parameter
	minN  int     // minimum observations before Veto makes a decision
}

// NewPACBayesSafety constructs a PACBayesSafety with default parameters.
func NewPACBayesSafety() *PACBayesSafety {
	return &PACBayesSafety{
		klQP:  0.0,
		delta: 0.05,
		minN:  50,
	}
}

// SetKLQP sets the KL divergence term KL(Q||P) externally.
func (p *PACBayesSafety) SetKLQP(kl float64) {
	p.mu.Lock()
	p.klQP = kl
	p.mu.Unlock()
}

// Update records one observation. isError=true counts as an error event.
func (p *PACBayesSafety) Update(isError bool) {
	p.mu.Lock()
	p.n++
	if isError {
		p.errors++
	}
	p.mu.Unlock()
}

// binaryKL computes kl(q, p) = q*ln(q/p) + (1-q)*ln((1-q)/(1-p)).
// Handles edge cases p=0, p=1, q=0, q=1.
func binaryKL(q, pVal float64) float64 {
	if q <= 0 && pVal <= 0 {
		return 0
	}
	if q >= 1 && pVal >= 1 {
		return 0
	}
	result := 0.0
	if q > 0 && pVal > 0 {
		result += q * math.Log(q/pVal)
	}
	if q < 1 && pVal < 1 {
		result += (1 - q) * math.Log((1-q)/(1-pVal))
	}
	return result
}

// Bound returns the empirical error rate and the PAC-Bayes upper bound.
// Returns (0, 1) if n == 0.
func (p *PACBayesSafety) Bound() (empiricalErr, upperBound float64) {
	p.mu.Lock()
	n := p.n
	errors := p.errors
	klQP := p.klQP
	delta := p.delta
	p.mu.Unlock()

	if n == 0 {
		return 0, 1
	}

	qHat := float64(errors) / float64(n)
	rhs := (klQP + math.Log(2*math.Sqrt(float64(n))/delta)) / float64(n)

	// Solve kl(qHat, upperBound) = rhs via bisection on [qHat, 1-1e-10].
	lo := qHat
	hi := 1.0 - 1e-10

	// If kl(qHat, hi) < rhs, the bound saturates at 1.
	if binaryKL(qHat, hi) < rhs {
		return qHat, hi
	}

	for range 60 {
		mid := (lo + hi) / 2
		if binaryKL(qHat, mid) < rhs {
			lo = mid
		} else {
			hi = mid
		}
		if hi-lo < 1e-9 {
			break
		}
	}

	return qHat, (lo + hi) / 2
}

// Veto returns true if the PAC-Bayes upper bound exceeds maxErrRate.
// Abstains (returns false) when fewer than minN observations have been recorded,
// since the bound is not meaningful with too little data.
func (p *PACBayesSafety) Veto(maxErrRate float64) bool {
	p.mu.Lock()
	n := p.n
	errors := p.errors
	minN := p.minN
	p.mu.Unlock()

	if n < minN || errors == 0 {
		return false
	}
	_, upper := p.Bound()
	return upper > maxErrRate
}
