package runtime

import "time"

// Observation is a single measured event.
type Observation struct {
	Time    time.Time
	Stage   string        // "llm", "tool:bash", "tool:read", etc.
	Latency time.Duration
	Weight  float64       // session message count for attribution weighting
	Success bool
}

// PolicyTrace is one logged interaction for off-policy evaluation.
type PolicyTrace struct {
	StateKey     string
	ActionID     string
	BehaviorProb float64 // μ(a|x) — probability under logging policy
	TargetProb   float64 // π(a|x) — probability under target policy
	Reward       float64
	ModelEst     float64 // direct model reward estimate r̂_i for DR estimator
}

// ProbeSpec describes a schedulable experiment for the VOI planner.
type ProbeSpec struct {
	ID       string
	Utility  float64
	Overhead float64
	LastRun  time.Time
	TTL      time.Duration // zero means always stale
}

// StageAttribution is per-stage weighted latency attribution with CI.
type StageAttribution struct {
	Stage         string
	WeightedShare float64 // percent of total weighted latency
	CI95Lo        float64
	CI95Hi        float64
	Neff          float64
}

// RuntimeReport is a full snapshot of all subsystem states.
type RuntimeReport struct {
	RegimeShift  bool
	ChangePoint  int     // BOCPD posterior mode at last alarm
	AnomalyRate  float64
	PACBoundHi   float64 // upper bound on true error rate
	PolicyVetoed bool
	OPEValue     float64 // doubly-robust estimate V̂_DR
	OPERegret    float64 // median regret across IPS, WIS, DR estimators
	EffectiveSS  float64 // N_eff
	ShardStates  map[string]ShardStatus // per-stage routing/batch/backoff state
	Attribution  []StageAttribution
	NextProbe    *ProbeSpec
}
