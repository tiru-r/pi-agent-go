package runtime

import (
	"sync"
	"time"
)

// VOIPlanner maintains a list of ProbeSpec entries and schedules the
// highest-value experiment within an overhead budget.
type VOIPlanner struct {
	mu     sync.RWMutex
	probes map[string]*ProbeSpec
}

// NewVOIPlanner constructs a ready-to-use VOIPlanner.
func NewVOIPlanner() *VOIPlanner {
	return &VOIPlanner{
		probes: make(map[string]*ProbeSpec),
	}
}

// Register adds or updates a ProbeSpec by ID.
func (v *VOIPlanner) Register(p ProbeSpec) {
	v.mu.Lock()
	clone := p
	v.probes[p.ID] = &clone
	v.mu.Unlock()
}

// isStale returns true if the probe's TTL has expired or it has never been run.
func isStale(p *ProbeSpec) bool {
	if p.TTL == 0 {
		return true // zero TTL means always stale
	}
	if p.LastRun.IsZero() {
		return true // never run
	}
	return time.Since(p.LastRun) >= p.TTL
}

// Next returns the stale probe with the highest Utility/Overhead ratio among
// those whose Overhead ≤ overheadBudget. Returns nil if no probe qualifies.
func (v *VOIPlanner) Next(overheadBudget float64) *ProbeSpec {
	v.mu.RLock()
	defer v.mu.RUnlock()

	var best *ProbeSpec
	bestRatio := -1.0

	for _, p := range v.probes {
		if !isStale(p) {
			continue
		}
		if p.Overhead > overheadBudget {
			continue
		}
		if p.Overhead <= 0 {
			continue
		}
		ratio := p.Utility / p.Overhead
		if ratio > bestRatio {
			bestRatio = ratio
			clone := *p
			best = &clone
		}
	}

	return best
}

// MarkRun sets the LastRun timestamp of the identified probe to now.
func (v *VOIPlanner) MarkRun(id string) {
	v.mu.Lock()
	if p, ok := v.probes[id]; ok {
		p.LastRun = time.Now()
	}
	v.mu.Unlock()
}
