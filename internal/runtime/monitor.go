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

	// New subsystems.
	histograms     map[string]*HDRHistogram // per-stage HDR histograms
	globalHisto    *HDRHistogram            // aggregate across all stages
	ece            *ECECalibrator
	breakers       *circuitBreakerRegistry
	conformalPred  *ConformalPredictor
	outputDrift    *OutputDriftDetector

	// Per-stage in-flight counters for queue depth estimation.
	queueDepth map[string]int

	// EMA of KL(π||μ) estimated from log importance ratios in policy traces.
	klEMA   float64
	klCount int

	lastReport RuntimeReport
}

// NewMonitor constructs a Monitor with all subsystems initialised.
func NewMonitor() *Monitor {
	return &Monitor{
		detector:      NewRegimeDetector(),
		conformal:     NewConformalEnvelope(),
		safety:        NewPACBayesSafety(),
		ope:           NewOffPolicyEvaluator(),
		voi:           NewVOIPlanner(),
		attribution:   NewAttributionTracker(),
		controller:    NewLoadController(),
		histograms:    make(map[string]*HDRHistogram),
		globalHisto:   NewHDRHistogram(),
		ece:           NewECECalibrator(),
		breakers:      newCircuitBreakerRegistry(),
		conformalPred: NewConformalPredictor(),
		outputDrift:   NewOutputDriftDetector(),
		queueDepth:    make(map[string]int),
	}
}

// ShouldVeto returns true when the safety subsystem signals that continued
// tool execution poses unacceptable risk.
func (m *Monitor) ShouldVeto() bool {
	return m.safety.Veto(0.8) || m.ope.ShouldVeto(0.05, opeMinNeff)
}

// RecordQueued adjusts the in-flight operation counter for a stage by delta
// (+1 when a task starts, -1 when it finishes).
func (m *Monitor) RecordQueued(stage string, delta int) {
	m.mu.Lock()
	m.queueDepth[stage] += delta
	if m.queueDepth[stage] < 0 {
		m.queueDepth[stage] = 0
	}
	m.mu.Unlock()
}

// stageHistogram returns (creating if necessary) the HDR histogram for a stage.
func (m *Monitor) stageHistogram(stage string) *HDRHistogram {
	m.mu.Lock()
	h, ok := m.histograms[stage]
	if !ok {
		h = NewHDRHistogram()
		m.histograms[stage] = h
	}
	m.mu.Unlock()
	return h
}

// Observe feeds a new measurement into all relevant subsystems.
func (m *Monitor) Observe(obs Observation) {
	lat := obs.Latency.Seconds()

	m.detector.Update(lat)
	m.conformal.Update(lat)
	m.safety.Update(!obs.Success)
	m.attribution.Add(obs)

	// Circuit breaker gate.
	cb := m.breakers.get(obs.Stage)
	if obs.Success {
		cb.RecordSuccess()
	} else {
		cb.RecordFailure()
	}

	// HDR histograms.
	m.stageHistogram(obs.Stage).Record(lat)
	m.globalHisto.Record(lat)

	// Conformal predictor update.
	m.conformalPred.Update(lat, lat) // self-calibration with prior mean = current obs

	m.mu.RLock()
	qd := m.queueDepth[obs.Stage]
	m.mu.RUnlock()

	m.controller.Update(obs.Stage, qd, lat, obs.Success)
}

// UpdateQuality feeds a QualityObservation to the output drift detector.
func (m *Monitor) UpdateQuality(q QualityObservation) {
	m.outputDrift.Update(q)
}

// RecordCalibration records a (probability, correct) pair for ECE computation.
func (m *Monitor) RecordCalibration(prob float64, correct bool) {
	m.ece.Update(prob, correct)
}

// AddTrace forwards a PolicyTrace to the OPE subsystem and updates the
// PAC-Bayes KL term from the log importance ratio of the trace.
func (m *Monitor) AddTrace(t PolicyTrace) {
	m.ope.Add(t)

	if t.TargetProb > 0 && t.BehaviorProb > 0 {
		logRatio := math.Log(t.TargetProb / t.BehaviorProb)
		if logRatio > 0 {
			const klDecay = 0.1
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

// PlanProbes returns the recommended set of probes to run within budget.
// Uses MCTS when ≥2 stale probes are available, falls back to greedy otherwise.
func (m *Monitor) PlanProbes(budget float64) []ProbeSpec {
	plan := m.voi.Plan(budget)
	stale := plan.Selected
	if len(stale) >= 2 {
		return MCTSPlan(stale, budget, 500)
	}
	return stale
}

// CircuitBreakerFor returns the circuit breaker for a specific stage.
// Useful for callers that want to check Allow() before dispatching work.
func (m *Monitor) CircuitBreakerFor(stage string) *CircuitBreaker {
	return m.breakers.get(stage)
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

	p50, p95, p99, p999 := m.globalHisto.Snapshot()

	ece := m.ece.ECE()

	cbStates := m.breakers.States()

	// Conformal interval for a hypothetical next observation at the EMA latency.
	// Use a rough center estimate from the p50.
	center := p50
	if center == 0 {
		center = 0.1 // 100ms default if no data
	}
	intervalLo, intervalHi := m.conformalPred.Interval(center)

	driftAlarm := m.outputDrift.Alarm()

	report := RuntimeReport{
		RegimeShift:     alarm,
		ChangePoint:     cp,
		AnomalyRate:     anomalyRate,
		PACBoundHi:      pacBoundHi,
		PolicyVetoed:    policyVetoed,
		OPEValue:        opeValue,
		OPERegret:       opeRegret,
		EffectiveSS:     effSS,
		ShardStates:     shardStates,
		Attribution:     attribution,
		NextProbe:       nextProbe,
		HDRP50:          p50,
		HDRP95:          p95,
		HDRP99:          p99,
		HDRP999:         p999,
		ECE:             ece,
		CircuitBreakers: cbStates,
		LatencyInterval: QuantileInterval{Lo: intervalLo, Hi: intervalHi},
		OutputDriftAlarm: driftAlarm,
	}

	m.mu.Lock()
	m.lastReport = report
	m.mu.Unlock()

	return report
}
