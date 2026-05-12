package runtime

import (
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// SkipReason describes why a probe was excluded from a plan.
type SkipReason string

const (
	SkipNotStale     SkipReason = "not_stale"
	SkipOverBudget   SkipReason = "over_budget"
	SkipZeroOverhead SkipReason = "zero_overhead"
)

// SkipEntry pairs a probe with the reason it was excluded.
type SkipEntry struct {
	Probe  ProbeSpec
	Reason SkipReason
}

// ProbePlan is the output of the VOI planner: the greedy-optimal set of probes
// to run within the budget and the full list of excluded probes with reasons.
type ProbePlan struct {
	Selected []ProbeSpec
	Skipped  []SkipEntry
}

// probeState holds the mutable runtime state for a registered probe, including
// a Beta posterior for Thompson / Bayesian bandit selection.
type probeState struct {
	ProbeSpec
	// Beta posterior parameters: high learning → Alpha, low → Beta.
	Alpha float64
	Beta  float64
}

// betaSample draws one sample from Beta(alpha, beta) via the ratio-of-gammas
// method using a Kinderman–Ramage approximation of the Gamma distribution.
// It falls back to the mean when both parameters are zero.
func betaSample(alpha, beta float64, rng *rand.Rand) float64 {
	if alpha <= 0 || beta <= 0 {
		a := alpha
		b := beta
		if a <= 0 {
			a = 1
		}
		if b <= 0 {
			b = 1
		}
		return a / (a + b)
	}
	// Use the regularised incomplete beta inverse via a Newton approximation
	// on a uniform draw U ∈ (0,1).  For small alpha/beta the mean is used as
	// the starting point; convergence is typically 5-10 iterations.
	u := rng.Float64()
	// Protect against degenerate u values.
	if u <= 0 {
		u = 1e-9
	}
	if u >= 1 {
		u = 1 - 1e-9
	}
	return betaInvCDF(u, alpha, beta)
}

// betaInvCDF approximates the quantile function of Beta(a, b) at p ∈ (0,1)
// using Newton–Raphson with a mode-biased starting guess.
func betaInvCDF(p, a, b float64) float64 {
	// Starting guess: mode when the distribution is unimodal (a>1, b>1),
	// otherwise the mean. The mode is a better seed than the mean for
	// skewed Betas because it lies closer to the high-density region.
	var x float64
	if a > 1 && b > 1 {
		x = (a - 1) / (a + b - 2)
	} else {
		x = a / (a + b)
	}
	// For very extreme quantiles, bias the seed further toward the tail so the
	// Newton step converges in fewer iterations.
	if p < 0.05 && a >= 1 {
		x = math.Min(x, math.Pow(p, 1/a))
	} else if p > 0.95 && b >= 1 {
		x = math.Max(x, 1-math.Pow(1-p, 1/b))
	}
	x = math.Max(1e-9, math.Min(1-1e-9, x))

	logB, _ := math.Lgamma(a + b)
	lgA, _ := math.Lgamma(a)
	lgB, _ := math.Lgamma(b)
	logB -= lgA + lgB

	for range 30 {
		// CDF value at x.
		cdf := regularisedIncompleteBeta(x, a, b)
		// PDF at x: exp(logB + (a-1)*ln(x) + (b-1)*ln(1-x))
		var pdf float64
		if x > 0 && x < 1 {
			logPDF := logB + (a-1)*math.Log(x) + (b-1)*math.Log(1-x)
			pdf = math.Exp(logPDF)
		}
		if pdf < 1e-300 {
			break
		}
		dx := (cdf - p) / pdf
		x -= dx
		x = math.Max(1e-9, math.Min(1-1e-9, x))
		if math.Abs(dx) < 1e-8 {
			break
		}
	}
	return x
}

// regularisedIncompleteBeta computes I_x(a,b) via the continued fraction
// representation (Lentz's algorithm).  Accurate to ~1e-7 for most inputs.
func regularisedIncompleteBeta(x, a, b float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	// Use the symmetry relation when x > (a+1)/(a+b+2) for faster convergence.
	if x > (a+1)/(a+b+2) {
		return 1 - regularisedIncompleteBeta(1-x, b, a)
	}
	logBeta, _ := math.Lgamma(a + b)
	logBeta -= func() float64 { v, _ := math.Lgamma(a); return v }()
	logBeta -= func() float64 { v, _ := math.Lgamma(b); return v }()
	front := math.Exp(math.Log(x)*a+math.Log(1-x)*b+logBeta) / a

	// Lentz continued fraction.
	const maxIter = 200
	const eps = 1e-9
	f := 1.0
	C := f
	D := 0.0
	for m := 0; m <= maxIter; m++ {
		for t := 0; t <= 1; t++ {
			var d float64
			if t == 0 {
				if m == 0 {
					d = 1
				} else {
					d = -((a + float64(m)) * (a + b + float64(m)) * x) /
						((a + 2*float64(m)) * (a + 2*float64(m) + 1))
				}
			} else {
				d = (float64(m+1) * (b - float64(m)) * x) /
					((a + 2*float64(m) + 1) * (a + 2*float64(m) + 2))
			}
			D = 1 + d*D
			if math.Abs(D) < 1e-30 {
				D = 1e-30
			}
			C = 1 + d/C
			if math.Abs(C) < 1e-30 {
				C = 1e-30
			}
			D = 1 / D
			delta := C * D
			f *= delta
			if math.Abs(delta-1) < eps {
				return front * f
			}
		}
	}
	return front * f
}

// VOIPlanner maintains a set of ProbeSpec entries and schedules the
// highest-value set of experiments within an overhead budget using Thompson
// sampling over Beta posteriors for probe selection.
type VOIPlanner struct {
	mu     sync.RWMutex
	probes map[string]*probeState
	rng    *rand.Rand
}

// NewVOIPlanner constructs a ready-to-use VOIPlanner.
func NewVOIPlanner() *VOIPlanner {
	return &VOIPlanner{
		probes: make(map[string]*probeState),
		rng:    rand.New(rand.NewSource(42)), //nolint:gosec
	}
}

// Register adds or updates a ProbeSpec by ID.  New probes start with a
// uniform Beta(1,1) prior.
func (v *VOIPlanner) Register(p ProbeSpec) {
	v.mu.Lock()
	if existing, ok := v.probes[p.ID]; ok {
		existing.ProbeSpec = p
	} else {
		v.probes[p.ID] = &probeState{ProbeSpec: p, Alpha: 1, Beta: 1}
	}
	v.mu.Unlock()
}

// UpdateUtility adjusts a probe's Utility with an EMA (alpha=0.3) and updates
// its Beta posterior: high learning increments Alpha, low increments Beta.
func (v *VOIPlanner) UpdateUtility(id string, observedLearning float64) {
	const emaAlpha = 0.3
	// Threshold: treat learning above mean as "high".
	const highThreshold = 0.5
	v.mu.Lock()
	if ps, ok := v.probes[id]; ok {
		ps.Utility = emaAlpha*observedLearning + (1-emaAlpha)*ps.Utility
		if observedLearning >= highThreshold {
			ps.Alpha++
		} else {
			ps.Beta++
		}
	}
	v.mu.Unlock()
}

// isStale returns true if the probe's TTL has expired or it has never been run.
func isStale(p *ProbeSpec) bool {
	if p.TTL == 0 {
		return true
	}
	if p.LastRun.IsZero() {
		return true
	}
	return time.Since(p.LastRun) >= p.TTL
}

// thompsonScore returns a sample from Beta(Alpha, Beta) * (1/Overhead) for
// probe selection.  Overhead-zero probes return 0 to avoid division by zero.
func (v *VOIPlanner) thompsonScore(ps *probeState) float64 {
	if ps.Overhead <= 0 {
		return 0
	}
	sample := betaSample(ps.Alpha, ps.Beta, v.rng)
	return sample / ps.Overhead
}

// Plan returns the greedy-optimal set of stale probes whose combined overhead
// fits within overheadBudget, ranked by Thompson-sampled Beta score.
func (v *VOIPlanner) Plan(overheadBudget float64) ProbePlan {
	v.mu.Lock()
	defer v.mu.Unlock()

	type candidate struct {
		p     ProbeSpec
		score float64
	}

	var plan ProbePlan
	var candidates []candidate

	for _, ps := range v.probes {
		clone := ps.ProbeSpec
		switch {
		case !isStale(&ps.ProbeSpec):
			plan.Skipped = append(plan.Skipped, SkipEntry{Probe: clone, Reason: SkipNotStale})
		case ps.Overhead <= 0:
			plan.Skipped = append(plan.Skipped, SkipEntry{Probe: clone, Reason: SkipZeroOverhead})
		case ps.Overhead > overheadBudget:
			plan.Skipped = append(plan.Skipped, SkipEntry{Probe: clone, Reason: SkipOverBudget})
		default:
			candidates = append(candidates, candidate{p: clone, score: v.thompsonScore(ps)})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	remaining := overheadBudget
	for _, c := range candidates {
		if c.p.Overhead <= remaining {
			plan.Selected = append(plan.Selected, c.p)
			remaining -= c.p.Overhead
		} else {
			plan.Skipped = append(plan.Skipped, SkipEntry{Probe: c.p, Reason: SkipOverBudget})
		}
	}

	return plan
}

// Next returns the single stale probe with the highest Thompson score within
// overheadBudget.
func (v *VOIPlanner) Next(overheadBudget float64) *ProbeSpec {
	plan := v.Plan(overheadBudget)
	if len(plan.Selected) == 0 {
		return nil
	}
	clone := plan.Selected[0]
	return &clone
}

// MarkRun sets the LastRun timestamp of the identified probe and triggers
// lightweight DAgger-style policy transfer: if the probe's observed utility
// significantly exceeds the prior mean, all other probes' Utility values are
// nudged upward by δ to imitate the superior policy.
func (v *VOIPlanner) MarkRun(id string) {
	v.mu.Lock()
	defer v.mu.Unlock()

	ps, ok := v.probes[id]
	if !ok {
		return
	}
	ps.LastRun = time.Now()

	// Compute the prior mean from the Beta posterior.
	priorMean := ps.Alpha / (ps.Alpha + ps.Beta)
	// Significant = utility more than one standard deviation above the Beta mean.
	betaVar := (ps.Alpha * ps.Beta) / ((ps.Alpha + ps.Beta) * (ps.Alpha + ps.Beta) * (ps.Alpha + ps.Beta + 1))
	threshold := priorMean + math.Sqrt(betaVar)

	if ps.Utility > threshold {
		v.distillPolicy(id)
	}
}

// distillPolicy transfers a positive δ to all other probe Utilities to
// imitate a "better" policy discovered by the identified probe.
func (v *VOIPlanner) distillPolicy(fromID string) {
	const delta = 0.01
	for id, ps := range v.probes {
		if id != fromID {
			ps.Utility += delta
		}
	}
}
