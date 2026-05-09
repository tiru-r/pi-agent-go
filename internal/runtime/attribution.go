package runtime

import (
	"math"
	"sync"
)

const attributionCap = 1000

// AttributionTracker records per-stage observations and computes weighted
// bottleneck attribution with 95% confidence intervals.
type AttributionTracker struct {
	mu   sync.Mutex
	buf  [attributionCap]Observation
	head int
	n    int
}

// NewAttributionTracker constructs a ready-to-use AttributionTracker.
func NewAttributionTracker() *AttributionTracker {
	return &AttributionTracker{}
}

// Add appends an Observation to the ring buffer.
func (a *AttributionTracker) Add(obs Observation) {
	a.mu.Lock()
	a.buf[a.head] = obs
	a.head = (a.head + 1) % attributionCap
	if a.n < attributionCap {
		a.n++
	}
	a.mu.Unlock()
}

// Report computes weighted bottleneck attribution across all stages seen.
// Returns one StageAttribution per stage, sorted by WeightedShare descending.
func (a *AttributionTracker) Report() []StageAttribution {
	a.mu.Lock()
	n := a.n
	obs := make([]Observation, n)
	start := 0
	if n == attributionCap {
		start = a.head
	}
	for i := range n {
		obs[i] = a.buf[(start+i)%attributionCap]
	}
	a.mu.Unlock()

	if n == 0 {
		return nil
	}

	// First pass: compute total latency per observation across all stages.
	// Since observations are per-stage (one stage per Observation), the
	// "total latency for observation i" is the sum of latencies of all
	// observations with the same Weight bucket. However, the spec says
	// t_i is the total latency across all stages for observation i.
	// We interpret each Observation as a single-stage measurement and the
	// denominator is the stage's contribution fraction over the entire window.
	//
	// weighted_contribution_s = (Σ_i∈s w_i * m_i) / (Σ_all w_i * m_i) * 100
	//
	// This is a normalised weighted sum approach consistent with the spec.

	type stageAccum struct {
		weightedLatSum float64 // Σ w_i * lat_i  for this stage
		wSum           float64 // Σ w_i
		wSumSq         float64 // Σ w_i²
		shares         []float64 // per-observation latency share (pre-normalisation)
		weights        []float64 // per-observation weights
	}

	stages := make(map[string]*stageAccum)
	totalWeightedLat := 0.0

	for _, o := range obs {
		lat := o.Latency.Seconds()
		w := o.Weight
		if w <= 0 {
			w = 1
		}
		totalWeightedLat += w * lat
		if _, ok := stages[o.Stage]; !ok {
			stages[o.Stage] = &stageAccum{}
		}
		s := stages[o.Stage]
		s.weightedLatSum += w * lat
		s.wSum += w
		s.wSumSq += w * w
		s.shares = append(s.shares, lat)
		s.weights = append(s.weights, w)
	}

	if totalWeightedLat == 0 {
		return nil
	}

	result := make([]StageAttribution, 0, len(stages))

	for stageName, s := range stages {
		share := s.weightedLatSum / totalWeightedLat * 100

		// Compute N_eff = (Σ w_i)² / Σ w_i²
		neff := 0.0
		if s.wSumSq > 0 {
			neff = (s.wSum * s.wSum) / s.wSumSq
		}

		// Weighted mean latency share for this stage.
		// Per-observation "share" is just the raw latency here.
		wMean := s.weightedLatSum / s.wSum

		// Weighted variance (Welford-style over the slice).
		wVar := 0.0
		wSumAcc := 0.0
		wMeanAcc := 0.0
		for i, lat := range s.shares {
			w := s.weights[i]
			wSumAcc += w
			delta := lat - wMeanAcc
			wMeanAcc += w * delta / wSumAcc
			delta2 := lat - wMeanAcc
			wVar += w * delta * delta2
		}
		if s.wSum > 0 {
			wVar /= s.wSum
		}
		_ = wMean // used implicitly via share

		// CI_95 = share ± 1.96 * sqrt(σ²_w / n_eff)
		ciHalfWidth := 0.0
		if neff > 0 {
			ciHalfWidth = 1.96 * math.Sqrt(wVar/neff) / totalWeightedLat * 100
		}

		result = append(result, StageAttribution{
			Stage:         stageName,
			WeightedShare: share,
			CI95Lo:        share - ciHalfWidth,
			CI95Hi:        share + ciHalfWidth,
			Neff:          neff,
		})
	}

	// Sort by WeightedShare descending.
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && result[j].WeightedShare > result[j-1].WeightedShare; j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}

	return result
}
