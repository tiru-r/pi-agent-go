package runtime

import (
	"math"
	"sync"
)

const (
	// EMA decay for latency, error rate, and queue depth signals.
	lcEMAAlpha = 0.2
	// Base gradient step size.
	lcEtaBase = 0.05
	// Oscillation guard: window size and max allowed sign flips before dampening.
	lcOscWindow   = 8
	lcOscMaxFlips = 3
	// Routing weight bounds.
	lcWeightMin = 0.01
	lcWeightMax = 1.0
	// Batch budget bounds (concurrent operations per shard).
	lcBatchMin = 1
	lcBatchMax = 32
	// Retry backoff bounds (milliseconds).
	lcBackoffMin = 10.0
	lcBackoffMax = 5000.0
	// Queue depth at which pressure begins saturating.
	lcQueueSaturation = 10.0
	// Starvation threshold: queue below this with low errors triggers upweighting.
	lcStarvationQueue = 0.5
	lcStarvationErr   = 0.05
	lcStarvationBoost = 0.3
)

// shardState tracks per-shard EMA signals, control outputs, and oscillation
// history for one execution stage (e.g. "llm", "tool:bash").
type shardState struct {
	// Control outputs — read by external callers.
	weight      float64
	batchBudget int
	backoffMs   float64

	// EMA-smoothed input signals.
	emaLatency float64
	emaErrRate float64
	emaQueue   float64

	// Damping: EMA-smoothed gradient from previous update.
	smoothedGrad float64

	// Oscillation guard: circular buffer of recent gradient signs.
	gradSigns [lcOscWindow]int8
	gradIdx   int
	gradCount int
}

// signOf returns -1, 0, or +1.
func signOf(v float64) int8 {
	if v > 0 {
		return 1
	}
	if v < 0 {
		return -1
	}
	return 0
}

// recordGradSign appends the sign of grad to the circular buffer and returns
// the number of sign flips observed across the window.
func (s *shardState) recordGradSign(grad float64) int {
	sign := signOf(grad)
	s.gradSigns[s.gradIdx] = sign
	s.gradIdx = (s.gradIdx + 1) % lcOscWindow
	if s.gradCount < lcOscWindow {
		s.gradCount++
	}

	flips := 0
	n := s.gradCount
	for i := 1; i < n; i++ {
		prev := s.gradSigns[(s.gradIdx-i-1+lcOscWindow)%lcOscWindow]
		cur := s.gradSigns[(s.gradIdx-i+lcOscWindow)%lcOscWindow]
		if prev != 0 && cur != 0 && prev != cur {
			flips++
		}
	}
	return flips
}

// effectiveEta returns a dampened step size when the gradient is oscillating.
func effectiveEta(flips int) float64 {
	if flips <= lcOscMaxFlips {
		return lcEtaBase
	}
	// Halve the step size for each flip above the threshold.
	return lcEtaBase * math.Pow(0.5, float64(flips-lcOscMaxFlips))
}

// update incorporates one measurement and adjusts all control outputs for this
// shard. queueDepth is the number of pending operations at observation time.
func (s *shardState) update(queueDepth int, latency float64, success bool) {
	errSignal := 0.0
	if !success {
		errSignal = 1.0
	}

	// EMA-update input signals.
	s.emaLatency = lcEMAAlpha*latency + (1-lcEMAAlpha)*s.emaLatency
	s.emaErrRate = lcEMAAlpha*errSignal + (1-lcEMAAlpha)*s.emaErrRate
	s.emaQueue = lcEMAAlpha*float64(queueDepth) + (1-lcEMAAlpha)*s.emaQueue

	// Raw gradient: positive = shard is struggling (weight should drop),
	// negative = shard is starving/healthy (weight should rise).
	queuePressure := math.Min(1, s.emaQueue/lcQueueSaturation)
	rawGrad := s.emaErrRate + queuePressure*0.5
	if s.emaQueue < lcStarvationQueue && s.emaErrRate < lcStarvationErr {
		rawGrad -= lcStarvationBoost
	}

	// Damping: blend raw gradient with the smoothed history (momentum).
	const gradMomentum = 0.7
	s.smoothedGrad = (1-gradMomentum)*rawGrad + gradMomentum*s.smoothedGrad

	// Oscillation guard: reduce eta when gradient sign has been flipping.
	flips := s.recordGradSign(s.smoothedGrad)
	eta := effectiveEta(flips)

	// Projected gradient descent on weight.
	s.weight = clamp(s.weight-eta*s.smoothedGrad, lcWeightMin, lcWeightMax)

	// Batch budget: shrinks under pressure, recovers when shard is healthy.
	pressure := s.emaErrRate + queuePressure
	targetBatch := float64(lcBatchMax) * math.Max(0, 1-pressure)
	s.batchBudget = int(math.Max(float64(lcBatchMin), math.Round(targetBatch)))

	// Backoff: grows exponentially with pressure.
	s.backoffMs = lcBackoffMin + (lcBackoffMax-lcBackoffMin)*math.Min(1, pressure)
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

// ShardStatus is a point-in-time snapshot of one shard's control state.
type ShardStatus struct {
	Weight      float64
	BatchBudget int
	BackoffMs   float64
	AvgLatency  float64
	AvgErrRate  float64
	AvgQueue    float64
}

// LoadController manages per-shard routing weights, batch budgets, and backoff
// factors. It is safe for concurrent use.
type LoadController struct {
	mu     sync.Mutex
	shards map[string]*shardState
}

// NewLoadController constructs a LoadController with no shards registered.
func NewLoadController() *LoadController {
	return &LoadController{shards: make(map[string]*shardState)}
}

func (c *LoadController) getOrCreate(id string) *shardState {
	s, ok := c.shards[id]
	if !ok {
		s = &shardState{
			weight:      1.0,
			batchBudget: lcBatchMax / 2,
			backoffMs:   lcBackoffMin,
		}
		c.shards[id] = s
	}
	return s
}

// Update records one observation for the given shard and recomputes its routing
// weight, batch budget, and backoff factor. queueDepth is the number of pending
// operations for this shard at the time the observation was made.
func (c *LoadController) Update(shardID string, queueDepth int, latency float64, success bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.getOrCreate(shardID).update(queueDepth, latency, success)
}

// RoutingWeights returns a normalized weight map (values sum to 1.0) suitable
// for weighted-random shard selection. Unknown shards are not included.
func (c *LoadController) RoutingWeights() map[string]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	total := 0.0
	for _, s := range c.shards {
		total += s.weight
	}
	out := make(map[string]float64, len(c.shards))
	if total == 0 {
		return out
	}
	for id, s := range c.shards {
		out[id] = s.weight / total
	}
	return out
}

// BatchBudget returns the maximum number of concurrent operations for the given
// shard. Returns lcBatchMax/2 for unknown shards.
func (c *LoadController) BatchBudget(shardID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.shards[shardID]; ok {
		return s.batchBudget
	}
	return lcBatchMax / 2
}

// BackoffFactor returns the recommended retry delay in milliseconds for the
// given shard. Returns lcBackoffMin for unknown shards.
func (c *LoadController) BackoffFactor(shardID string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.shards[shardID]; ok {
		return s.backoffMs
	}
	return lcBackoffMin
}

// Status returns a point-in-time snapshot of every shard's control state.
func (c *LoadController) Status() map[string]ShardStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]ShardStatus, len(c.shards))
	for id, s := range c.shards {
		out[id] = ShardStatus{
			Weight:      s.weight,
			BatchBudget: s.batchBudget,
			BackoffMs:   s.backoffMs,
			AvgLatency:  s.emaLatency,
			AvgErrRate:  s.emaErrRate,
			AvgQueue:    s.emaQueue,
		}
	}
	return out
}
