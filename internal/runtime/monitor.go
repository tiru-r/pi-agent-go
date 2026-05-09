package runtime

import "sync"

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
	controller  *OCOController
	lastReport  RuntimeReport
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
		controller:  NewOCOController(),
	}
}

// ShouldVeto returns true when the safety subsystem signals that continued
// tool execution poses unacceptable risk. It is a cheap read — it does not
// recompute the full RuntimeReport, so callers can invoke it before every
// tool batch without performance concern.
//
// Veto conditions (mirrors Report logic):
//   - PAC-Bayes upper bound on error rate > 30%
//   - OPE doubly-robust estimator signals policy degradation
func (m *Monitor) ShouldVeto() bool {
	return m.safety.Veto(0.3) || m.ope.ShouldVeto(0.05, 5.0)
}

// Observe feeds a new measurement into all relevant subsystems.
func (m *Monitor) Observe(obs Observation) {
	lat := obs.Latency.Seconds()

	m.detector.Update(lat)

	anomaly, thresh := m.conformal.Update(lat)

	m.safety.Update(!obs.Success)

	m.attribution.Add(obs)

	// OCO: use latency/threshold as loss; gradient from anomaly signal.
	loss := 1.0 // default when thresh is zero/invalid
	if thresh > 0 {
		loss = lat / thresh
	}

	grad := 0.0
	if anomaly {
		grad = 1.0
	} else if thresh > 0 && lat < thresh*0.5 {
		grad = -0.5
	}

	m.controller.Update(loss, grad)
}

// AddTrace forwards a PolicyTrace to the OPE subsystem.
func (m *Monitor) AddTrace(t PolicyTrace) {
	m.ope.Add(t)
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

	opeVetoed := m.ope.ShouldVeto(0.05, 5.0)
	safetyVetoed := m.safety.Veto(0.3)
	policyVetoed := safetyVetoed || opeVetoed

	var opeValue, opeRegret, effSS float64
	if res, ok := m.ope.Evaluate(); ok {
		opeValue = res.DR
		opeRegret = res.Regret
		effSS = res.Neff
	}

	controlParam := m.controller.Param()
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
		ControlParam: controlParam,
		Attribution:  attribution,
		NextProbe:    nextProbe,
	}

	m.mu.Lock()
	m.lastReport = report
	m.mu.Unlock()

	return report
}
