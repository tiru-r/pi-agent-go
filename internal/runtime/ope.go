package runtime

import (
	"math"
	"sort"
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
	IPS       float64
	WIS       float64
	DR        float64
	Neff      float64
	// Per-estimator regret: Δ = r̄_baseline − V̂_estimator.
	// MedianRegret is the robust summary used for veto decisions.
	RegretIPS    float64
	RegretWIS    float64
	RegretDR     float64
	MedianRegret float64
	// Kept for backward compatibility — equals RegretDR.
	Regret float64
}

// OffPolicyEvaluator stores a ring buffer of PolicyTrace entries and computes
// IPS, WIS, DR estimators along with ESS and per-estimator regret.
type OffPolicyEvaluator struct {
	mu    sync.Mutex
	buf   [opeCapacity]PolicyTrace
	head  int
	count int
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

// Evaluate computes OPE estimates. Returns (result, false) if insufficient data
// or effective sample size is below opeMinNeff.
func (e *OffPolicyEvaluator) Evaluate() (OPEResult, bool) {
	e.mu.Lock()
	n := e.count
	traces := make([]PolicyTrace, n)
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

	// Compute clipped importance weights w_i = π(a|x) / μ(a|x).
	weights := make([]float64, n)
	wSum := 0.0
	w2Sum := 0.0
	for i, t := range traces {
		w := 0.0
		if t.BehaviorProb > 0 {
			w = math.Min(t.TargetProb/t.BehaviorProb, opeMaxWeight)
		}
		weights[i] = w
		wSum += w
		w2Sum += w * w
	}

	// Baseline reward: simple mean of r_i under the behavior policy.
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

	// V̂_WIS = Σ w_i r_i / Σ w_i  (self-normalised; falls back to IPS if wSum==0)
	vWIS := vIPS
	wrSum := 0.0
	for i, t := range traces {
		wrSum += weights[i] * t.Reward
	}
	if wSum > 0 {
		vWIS = wrSum / wSum
	}

	// V̂_DR = (1/n) Σ (r̂_i + w_i*(r_i − r̂_i))
	drSum := 0.0
	for i, t := range traces {
		drSum += t.ModelEst + weights[i]*(t.Reward-t.ModelEst)
	}
	vDR := drSum / float64(n)

	// N_eff = (Σ w_i)² / Σ w_i²
	neff := 0.0
	if w2Sum > 0 {
		neff = (wSum * wSum) / w2Sum
	}

	if neff < opeMinNeff {
		return OPEResult{}, false
	}

	// Per-estimator regret = r̄_baseline − V̂_estimator.
	regretIPS := baseline - vIPS
	regretWIS := baseline - vWIS
	regretDR := baseline - vDR

	// Median regret across the three estimators provides robustness: a single
	// mis-specified estimator cannot trigger or suppress a veto on its own.
	medianRegret := medianOfThree(regretIPS, regretWIS, regretDR)

	return OPEResult{
		IPS:          vIPS,
		WIS:          vWIS,
		DR:           vDR,
		Neff:         neff,
		RegretIPS:    regretIPS,
		RegretWIS:    regretWIS,
		RegretDR:     regretDR,
		MedianRegret: medianRegret,
		Regret:       regretDR, // backward compat
	}, true
}

// ShouldVeto returns true when the median regret across all three estimators
// exceeds regretThresh or effective sample size is below neffMin.
// Using the median means a single outlier estimator cannot force or block a veto.
func (e *OffPolicyEvaluator) ShouldVeto(regretThresh, neffMin float64) bool {
	res, ok := e.Evaluate()
	if !ok {
		return false
	}
	if res.Neff < neffMin {
		return true
	}
	return res.MedianRegret > regretThresh
}

// medianOfThree returns the median of three float64 values via a sorting network.
func medianOfThree(a, b, c float64) float64 {
	vals := [3]float64{a, b, c}
	sort.Slice(vals[:], func(i, j int) bool { return vals[i] < vals[j] })
	return vals[1]
}
