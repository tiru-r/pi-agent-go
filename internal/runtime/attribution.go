package runtime

import (
	"math"
	"math/rand"
	"sort"
	"sync"
)

const attributionCap = 1000

// StageShapley is a stage's Shapley-value attribution.
type StageShapley struct {
	Stage   string
	Shapley float64 // fraction of total latency attributed via Shapley
}

// AttributionTracker records per-stage observations and computes weighted
// bottleneck attribution with 95% confidence intervals.
// Reservoir sampling (Algorithm R) replaces the FIFO ring buffer so that
// high-load stages get unbiased random samples rather than recency-biased ones.
type AttributionTracker struct {
	mu      sync.Mutex
	buf     [attributionCap]Observation
	n       int // total observations seen (not capped)
	filled  int // number of valid entries in buf (≤ attributionCap)
	rng     *rand.Rand
}

// NewAttributionTracker constructs a ready-to-use AttributionTracker.
func NewAttributionTracker() *AttributionTracker {
	return &AttributionTracker{
		rng: rand.New(rand.NewSource(42)), //nolint:gosec — deterministic seed for reproducibility
	}
}

// Add appends an Observation using Algorithm R reservoir sampling.
func (a *AttributionTracker) Add(obs Observation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	if a.filled < attributionCap {
		a.buf[a.filled] = obs
		a.filled++
	} else {
		// Replace a random slot with probability cap/n.
		j := a.rng.Intn(a.n)
		if j < attributionCap {
			a.buf[j] = obs
		}
	}
}

// snapshot returns a copy of the current reservoir under the lock.
func (a *AttributionTracker) snapshot() []Observation {
	a.mu.Lock()
	out := make([]Observation, a.filled)
	copy(out, a.buf[:a.filled])
	a.mu.Unlock()
	return out
}

// Report computes weighted bottleneck attribution across all stages seen.
// Returns one StageAttribution per stage, sorted by WeightedShare descending.
func (a *AttributionTracker) Report() []StageAttribution {
	obs := a.snapshot()
	if len(obs) == 0 {
		return nil
	}

	type stageAccum struct {
		weightedLatSum float64
		wSum           float64
		wSumSq         float64
		shares         []float64
		weights        []float64
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

		neff := 0.0
		if s.wSumSq > 0 {
			neff = (s.wSum * s.wSum) / s.wSumSq
		}

		// Weighted variance (Welford-style).
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

	sort.Slice(result, func(i, j int) bool {
		return result[i].WeightedShare > result[j].WeightedShare
	})

	return result
}

// ShapleyReport computes Shapley-value attribution for each stage.
//
// For additive systems the Shapley value equals the weighted mean latency
// share, which coincides with the marginal contribution averaged over all
// insertion orderings.  For correlated stages (same Weight bucket) we use a
// sampling-based approximation over random orderings.
func (a *AttributionTracker) ShapleyReport() []StageShapley {
	obs := a.snapshot()
	if len(obs) == 0 {
		return nil
	}

	// Group by Weight bucket (rounded to nearest 10) to detect correlation.
	type stageData struct {
		weightedLatSum float64
		wSum           float64
		bucket         int
	}
	stages := make(map[string]*stageData)
	totalWLat := 0.0

	for _, o := range obs {
		lat := o.Latency.Seconds()
		w := o.Weight
		if w <= 0 {
			w = 1
		}
		totalWLat += w * lat
		if _, ok := stages[o.Stage]; !ok {
			bucket := int(w/10) * 10
			stages[o.Stage] = &stageData{bucket: bucket}
		}
		s := stages[o.Stage]
		s.weightedLatSum += w * lat
		s.wSum += w
	}

	if totalWLat == 0 {
		return nil
	}

	// Check for correlated buckets.
	bucketCount := make(map[int]int)
	for _, s := range stages {
		bucketCount[s.bucket]++
	}
	correlated := false
	for _, c := range bucketCount {
		if c > 1 {
			correlated = true
			break
		}
	}

	result := make([]StageShapley, 0, len(stages))

	if !correlated {
		// Additive case: Shapley = weighted mean latency share.
		for name, s := range stages {
			shapley := 0.0
			if s.wSum > 0 {
				shapley = (s.weightedLatSum / totalWLat)
			}
			result = append(result, StageShapley{Stage: name, Shapley: shapley})
		}
	} else {
		// Sampling-based Shapley over 200 random coalition orderings.
		stageNames := make([]string, 0, len(stages))
		for name := range stages {
			stageNames = append(stageNames, name)
		}
		m := len(stageNames)
		marginals := make(map[string]float64, m)

		// Seed a local RNG from the shared source under lock, then release the lock.
		// Using the shared rng pointer outside the lock would race with Add().
		a.mu.Lock()
		seed := a.rng.Int63()
		a.mu.Unlock()
		localRng := rand.New(rand.NewSource(seed))

		const numSamples = 200
		for range numSamples {
			// Random permutation.
			perm := localRng.Perm(m)
			var cumLat float64
			for _, idx := range perm {
				name := stageNames[idx]
				s := stages[name]
				share := s.weightedLatSum / totalWLat
				marginals[name] += share - cumLat
				cumLat += share
			}
		}
		for name, sum := range marginals {
			shapley := sum / numSamples
			if shapley < 0 {
				shapley = 0
			}
			result = append(result, StageShapley{Stage: name, Shapley: shapley})
		}
	}

	// Normalise so values sum to 1.
	total := 0.0
	for _, r := range result {
		total += r.Shapley
	}
	if total > 0 {
		for i := range result {
			result[i].Shapley /= total
		}
	}

	return result
}
