package runtime

import (
	"math"
	"sync"
)

// Monitor is the top-level runtime intelligence system.
// It wires all subsystems together and provides a single integration point.
type Monitor struct {
	mu          sync.RWMutex
	detector    *RegimeDetector
	conformal   *ConformalEnvelope
	safety      *PACBayesSafety
	ope         *OffPolicyEvaluator
	voi         *VOIPlanner
	attribution *AttributionTracker
	controller  *LoadController

	// Per-stage in-flight counters for queue depth estimation.
	queueDepth map[string]int

	// EMA of KL(π||μ) estimated from log importance ratios in policy traces.
	// Used to keep the PAC-Bayes safety bound calibrated to the actual policy gap.
	klEMA   float64
	klCount int

	lastReport RuntimeReport
}

// NewMonitor constructs a Monitor with all subsystems initialised.
func NewMonitor() *Monitor {
	return &Monitor{
		detector:    NewRegimeDetector(),
		conformal:   NewConformalEnvelope(),
		safety:      NewPACBayesSafety(),
		ope:         NewOffPolicyEvaluator(),
		voi:         NewVOIPlanner(),
		attribution: NewAttributionTracker(),
		controller:  NewLoadController(),
		queueDepth:  make(map[string]int),
	}
}

// ShouldVeto returns true when the safety subsystem signals that continued
// tool execution poses unacceptable risk. It is a cheap read — it does not
// recompute the full RuntimeReport, so callers can invoke it before every
// tool batch without performance concern.
//
// Veto conditions:
//   - PAC-Bayes upper bound on error rate > 30%
//   - Median OPE regret across IPS/WIS/DR > 0.05 with N_eff ≥ opeMinNeff
func (m *Monitor) ShouldVeto() bool {
	return m.safety.Veto(0.3) || m.ope.ShouldVeto(0.05, opeMinNeff)
}

// RecordQueued adjusts the in-flight operation counter for a stage by delta
// (+1 when a task starts, -1 when it finishes). The counter is used as the
// queue depth signal fed to the shard load controller.
func (m *Monitor) RecordQueued(stage string, delta int) {
	m.mu.Lock()
	m.queueDepth[stage] += delta
	if m.queueDepth[stage] < 0 {
		m.queueDepth[stage] = 0
	}
	m.mu.Unlock()
}

// Observe feeds a new measurement into all relevant subsystems.
func (m *Monitor) Observe(obs Observation) {
	lat := obs.Latency.Seconds()

	m.detector.Update(lat)
	m.conformal.Update(lat)
	m.safety.Update(!obs.Success)
	m.attribution.Add(obs)

	m.mu.RLock()
	qd := m.queueDepth[obs.Stage]
	m.mu.RUnlock()

	m.controller.Update(obs.Stage, qd, lat, obs.Success)
}

// AddTrace forwards a PolicyTrace to the OPE subsystem and updates the
// PAC-Bayes KL term from the log importance ratio of the trace. This keeps
// the safety bound calibrated to the true gap between target and behavior
// policy without requiring external KL computation.
func (m *Monitor) AddTrace(t PolicyTrace) {
	m.ope.Add(t)

	if t.TargetProb > 0 && t.BehaviorProb > 0 {
		logRatio := math.Log(t.TargetProb / t.BehaviorProb)
		// KL(π||μ) = E_π[log π/μ] ≥ 0; negative log ratios (target < behavior)
		// contribute zero to the KL and are not used to tighten the bound.
		if logRatio > 0 {
			const klDecay = 0.1 // slow EMA for a stable KL estimate
			m.mu.Lock()
			if m.klCount == 0 {
				m.klEMA = logRatio
			} else {
				m.klEMA = klDecay*logRatio + (1-klDecay)*m.klEMA
			}
			m.klCount++
			kl := m.klEMA
			m.mu.Unlock()
			m.safety.SetKLQP(kl)
		}
	}
}

// RegisterProbe forwards a ProbeSpec to the VOI planner.
func (m *Monitor) RegisterProbe(p ProbeSpec) {
	m.voi.Register(p)
}

// MarkProbeRun marks a probe as executed in the VOI planner.
func (m *Monitor) MarkProbeRun(id string) {
	m.voi.MarkRun(id)
}

// Report collects state from all subsystems and returns a RuntimeReport.
func (m *Monitor) Report() RuntimeReport {
	alarm, cp := m.detector.State()

	anomalyRate := m.conformal.AnomalyRate()

	_, pacBoundHi := m.safety.Bound()

	opeVetoed := m.ope.ShouldVeto(0.05, opeMinNeff)
	safetyVetoed := m.safety.Veto(0.3)
	policyVetoed := safetyVetoed || opeVetoed

	var opeValue, opeRegret, effSS float64
	if res, ok := m.ope.Evaluate(); ok {
		opeValue = res.DR
		opeRegret = res.MedianRegret
		effSS = res.Neff
	}

	shardStates := m.controller.Status()
	attribution := m.attribution.Report()
	nextProbe := m.voi.Next(1.0)

	report := RuntimeReport{
		RegimeShift:  alarm,
		ChangePoint:  cp,
		AnomalyRate:  anomalyRate,
		PACBoundHi:   pacBoundHi,
		PolicyVetoed: policyVetoed,
		OPEValue:     opeValue,
		OPERegret:    opeRegret,
		EffectiveSS:  effSS,
		ShardStates:  shardStates,
		Attribution:  attribution,
		NextProbe:    nextProbe,
	}

	m.mu.Lock()
	m.lastReport = report
	m.mu.Unlock()

	return report
}
