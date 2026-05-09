package runtime

import (
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

// VOIPlanner maintains a set of ProbeSpec entries and schedules the
// highest-value set of experiments within an overhead budget.
type VOIPlanner struct {
	mu     sync.RWMutex
	probes map[string]*ProbeSpec
}

// NewVOIPlanner constructs a ready-to-use VOIPlanner.
func NewVOIPlanner() *VOIPlanner {
	return &VOIPlanner{probes: make(map[string]*ProbeSpec)}
}

// Register adds or updates a ProbeSpec by ID.
func (v *VOIPlanner) Register(p ProbeSpec) {
	v.mu.Lock()
	clone := p
	v.probes[p.ID] = &clone
	v.mu.Unlock()
}

// UpdateUtility adjusts a probe's Utility with an exponential moving average
// of observed learning (alpha=0.3 → 30% weight on the new observation).
// Call this after a probe runs with the actual information gained.
func (v *VOIPlanner) UpdateUtility(id string, observedLearning float64) {
	const alpha = 0.3
	v.mu.Lock()
	if p, ok := v.probes[id]; ok {
		p.Utility = alpha*observedLearning + (1-alpha)*p.Utility
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

// Plan returns the greedy-optimal set of stale probes whose combined overhead
// fits within overheadBudget, ranked by Utility/Overhead ratio (expected
// learning per unit cost). All excluded probes are returned in Skipped with an
// explicit SkipReason so callers can log or act on the omissions.
func (v *VOIPlanner) Plan(overheadBudget float64) ProbePlan {
	v.mu.RLock()
	defer v.mu.RUnlock()

	type candidate struct {
		p     ProbeSpec
		ratio float64
	}

	var plan ProbePlan
	var candidates []candidate

	for _, p := range v.probes {
		clone := *p
		switch {
		case !isStale(p):
			plan.Skipped = append(plan.Skipped, SkipEntry{Probe: clone, Reason: SkipNotStale})
		case p.Overhead <= 0:
			plan.Skipped = append(plan.Skipped, SkipEntry{Probe: clone, Reason: SkipZeroOverhead})
		case p.Overhead > overheadBudget:
			plan.Skipped = append(plan.Skipped, SkipEntry{Probe: clone, Reason: SkipOverBudget})
		default:
			candidates = append(candidates, candidate{p: clone, ratio: p.Utility / p.Overhead})
		}
	}

	// Greedy knapsack: sort by utility/overhead ratio descending, then add each
	// probe to the plan while it fits in the remaining budget.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ratio > candidates[j].ratio
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

// Next returns the single stale probe with the highest Utility/Overhead ratio
// within overheadBudget. Convenience wrapper around Plan for callers that
// execute one probe at a time.
func (v *VOIPlanner) Next(overheadBudget float64) *ProbeSpec {
	plan := v.Plan(overheadBudget)
	if len(plan.Selected) == 0 {
		return nil
	}
	clone := plan.Selected[0]
	return &clone
}

// MarkRun sets the LastRun timestamp of the identified probe to now.
func (v *VOIPlanner) MarkRun(id string) {
	v.mu.Lock()
	if p, ok := v.probes[id]; ok {
		p.LastRun = time.Now()
	}
	v.mu.Unlock()
}
