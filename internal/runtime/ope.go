package runtime

import (
	"math"
	"sync"
)

const (
	opeCapacity  = 500
	opeMaxWeight = 20.0
	opeMinN      = 10
	opeMinNeff   = 5.0
)

// OPEResult holds the results of an off-policy evaluation.
type OPEResult struct {
	IPS    float64
	WIS    float64
	DR     float64
	Neff   float64
	Regret float64
}

// OffPolicyEvaluator stores a ring buffer of PolicyTrace entries and computes
// IPS, WIS, DR estimators along with ESS and regret.
type OffPolicyEvaluator struct {
	mu     sync.Mutex
	buf    [opeCapacity]PolicyTrace
	head   int
	count  int
}

// NewOffPolicyEvaluator constructs a ready-to-use OffPolicyEvaluator.
func NewOffPolicyEvaluator() *OffPolicyEvaluator {
	return &OffPolicyEvaluator{}
}

// Add appends a PolicyTrace to the ring buffer.
func (e *OffPolicyEvaluator) Add(t PolicyTrace) {
	e.mu.Lock()
	e.buf[e.head] = t
	e.head = (e.head + 1) % opeCapacity
	if e.count < opeCapacity {
		e.count++
	}
	e.mu.Unlock()
}

// Evaluate computes OPE estimates. Returns (result, false) if insufficient data.
func (e *OffPolicyEvaluator) Evaluate() (OPEResult, bool) {
	e.mu.Lock()
	n := e.count
	traces := make([]PolicyTrace, n)
	// Copy in order from oldest to newest.
	start := 0
	if n == opeCapacity {
		start = e.head
	}
	for i := range n {
		traces[i] = e.buf[(start+i)%opeCapacity]
	}
	e.mu.Unlock()

	if n < opeMinN {
		return OPEResult{}, false
	}

	// Compute weights.
	weights := make([]float64, n)
	for i, t := range traces {
		var w float64
		if t.BehaviorProb > 0 {
			w = t.TargetProb / t.BehaviorProb
		}
		if w > opeMaxWeight {
			w = opeMaxWeight
		}
		weights[i] = w
	}

	// Baseline reward = mean of r_i.
	baselineSum := 0.0
	for _, t := range traces {
		baselineSum += t.Reward
	}
	baseline := baselineSum / float64(n)

	// V̂_IPS = (1/n) Σ w_i r_i
	ipsSum := 0.0
	for i, t := range traces {
		ipsSum += weights[i] * t.Reward
	}
	vIPS := ipsSum / float64(n)

	// V̂_WIS = Σ w_i r_i / Σ w_i
	wSum := 0.0
	wrSum := 0.0
	for i, t := range traces {
		wSum += weights[i]
		wrSum += weights[i] * t.Reward
	}
	var vWIS float64
	if wSum > 0 {
		vWIS = wrSum / wSum
	}

	// V̂_DR = (1/n) Σ (r̂_i + w_i*(r_i - r̂_i))
	drSum := 0.0
	for i, t := range traces {
		drSum += t.ModelEst + weights[i]*(t.Reward-t.ModelEst)
	}
	vDR := drSum / float64(n)

	// N_eff = (Σ w_i)² / Σ w_i²
	w2Sum := 0.0
	for _, w := range weights {
		w2Sum += w * w
	}
	var neff float64
	if w2Sum > 0 {
		neff = (wSum * wSum) / w2Sum
	}

	if neff < opeMinNeff {
		return OPEResult{}, false
	}

	// Δ_regret = r̄_baseline - V̂_DR
	regret := baseline - vDR

	return OPEResult{
		IPS:    vIPS,
		WIS:    vWIS,
		DR:     vDR,
		Neff:   neff,
		Regret: regret,
	}, true
}

// ShouldVeto returns true if regret > regretThresh or Neff < neffMin.
func (e *OffPolicyEvaluator) ShouldVeto(regretThresh, neffMin float64) bool {
	res, ok := e.Evaluate()
	if !ok {
		return false
	}
	return res.Regret > regretThresh || res.Neff < neffMin
}

// ensure math is imported (used for opeMaxWeight clamp via math.Min alternative)
var _ = math.MaxFloat64
