package runtime

import (
	"math"
	"sync"
)

const (
	cusumK      = 0.5
	cusumH      = 5.0
	bocpdLambda = 50.0
	windowSize  = 60
	maxRunLen   = 500
)

// runningStats maintains a sliding-window mean and variance for z-score
// standardization using a ring buffer of the last windowSize observations.
type runningStats struct {
	buf  [windowSize]float64
	n    int   // total observations seen
	head int   // next write position
	sum  float64
	sum2 float64
}

func (r *runningStats) push(x float64) {
	if r.n >= windowSize {
		// Evict oldest value.
		old := r.buf[r.head]
		r.sum -= old
		r.sum2 -= old * old
	}
	r.buf[r.head] = x
	r.head = (r.head + 1) % windowSize
	r.sum += x
	r.sum2 += x * x
	if r.n < windowSize {
		r.n++
	}
}

func (r *runningStats) meanStd() (mean, std float64) {
	if r.n == 0 {
		return 0, 1
	}
	n := float64(r.n)
	mean = r.sum / n
	variance := r.sum2/n - mean*mean
	if variance < 0 {
		variance = 0
	}
	std = math.Sqrt(variance)
	if std < 1e-9 {
		std = 1
	}
	return mean, std
}

// ── CUSUM ────────────────────────────────────────────────────────────────────

type cusum struct {
	sPlus  float64
	sMinus float64
}

// update applies one observation (already z-scored) and returns true if alarm.
func (c *cusum) update(z float64) bool {
	c.sPlus = math.Max(0, c.sPlus+(z-cusumK))
	c.sMinus = math.Max(0, c.sMinus+(-z-cusumK))
	return c.sPlus > cusumH || c.sMinus > cusumH
}

func (c *cusum) reset() {
	c.sPlus = 0
	c.sMinus = 0
}

// ── Normal-Gamma conjugate prior for BOCPD ───────────────────────────────────

type normalGamma struct {
	m   float64 // mean
	kap float64 // pseudo-count
	a   float64 // shape
	b   float64 // rate
}

// update returns posterior parameters after observing x.
func (ng normalGamma) update(x float64) normalGamma {
	kap1 := ng.kap + 1
	m1 := (ng.kap*ng.m + x) / kap1
	a1 := ng.a + 0.5
	b1 := ng.b + ng.kap*(x-ng.m)*(x-ng.m)/(2*kap1)
	return normalGamma{m: m1, kap: kap1, a: a1, b: b1}
}

// logPredictive returns the log Student-t predictive density at x.
func (ng normalGamma) logPredictive(x float64) float64 {
	nu := 2 * ng.a
	scale2 := ng.b * (ng.kap + 1) / (ng.a * ng.kap)
	z := (x - ng.m) / math.Sqrt(scale2)
	// log Student-t density
	lgHalf1, _ := math.Lgamma((nu + 1) / 2)
	lgHalf2, _ := math.Lgamma(nu / 2)
	return lgHalf1 - lgHalf2 - 0.5*math.Log(nu*math.Pi*scale2) - (nu+1)/2*math.Log(1+z*z/nu)
}

// ── BOCPD ─────────────────────────────────────────────────────────────────────

type bocpd struct {
	logProb []float64    // log run-length posterior P(r_t = i)
	stats   []normalGamma
	prior   normalGamma

	// Most probable change-point index from last alarm.
	lastChangePoint int
}

func newBOCPD() *bocpd {
	prior := normalGamma{m: 0, kap: 1, a: 1, b: 1}
	b := &bocpd{
		prior:   prior,
		logProb: []float64{0}, // log P(r=0) = log(1) = 0
		stats:   []normalGamma{prior},
	}
	return b
}

// logSumExp computes log(Σ exp(a_i)) in a numerically stable way.
func logSumExp(a []float64) float64 {
	if len(a) == 0 {
		return math.Inf(-1)
	}
	maxVal := a[0]
	for _, v := range a[1:] {
		if v > maxVal {
			maxVal = v
		}
	}
	if math.IsInf(maxVal, -1) {
		return math.Inf(-1)
	}
	sum := 0.0
	for _, v := range a {
		sum += math.Exp(v - maxVal)
	}
	return maxVal + math.Log(sum)
}

// update processes one new observation x and returns P(r_t = 0) (changepoint probability).
func (b *bocpd) update(x float64) float64 {
	n := len(b.logProb)
	logH := math.Log(1.0 / bocpdLambda)
	logNoH := math.Log(1.0 - 1.0/bocpdLambda)

	// Compute predictive log-likelihoods.
	logPred := make([]float64, n)
	for i := range n {
		logPred[i] = b.stats[i].logPredictive(x)
	}

	// New log-probability array (length n+1).
	newLogProb := make([]float64, n+1)
	newStats := make([]normalGamma, n+1)

	// New segment (r = 0): sum over all current run lengths of
	// P(r_{t-1}=i) * H * p(x | prior)
	terms := make([]float64, n)
	for i := range n {
		terms[i] = b.logProb[i] + logH + logPred[i]
	}
	newLogProb[0] = logSumExp(terms)
	newStats[0] = b.prior

	// Continue existing runs (r_{t} = r_{t-1} + 1).
	for i := range n {
		newLogProb[i+1] = b.logProb[i] + logNoH + logPred[i]
		newStats[i+1] = b.stats[i].update(x)
	}

	// Normalize.
	lse := logSumExp(newLogProb)
	for i := range newLogProb {
		newLogProb[i] -= lse
	}

	// Prune to maxRunLen.
	if len(newLogProb) > maxRunLen {
		newLogProb = newLogProb[:maxRunLen]
		newStats = newStats[:maxRunLen]
	}

	b.logProb = newLogProb
	b.stats = newStats

	cp0 := math.Exp(newLogProb[0])

	// Track most probable run length index for change-point reporting.
	if cp0 > 0.5 {
		// Find the previous MAP run length (index of max in old distribution).
		b.lastChangePoint = 0
	}

	return cp0
}

// ── RegimeDetector ────────────────────────────────────────────────────────────

// RegimeDetector combines CUSUM and BOCPD to signal regime changes.
type RegimeDetector struct {
	mu     sync.Mutex
	stats  runningStats
	c      cusum
	b      *bocpd
	alarm  bool
	cpMode int
}

// NewRegimeDetector constructs a ready-to-use RegimeDetector.
func NewRegimeDetector() *RegimeDetector {
	return &RegimeDetector{b: newBOCPD()}
}

// Update ingests a new latency observation (in seconds) and returns alarm state and
// the BOCPD posterior mode change-point index.
func (d *RegimeDetector) Update(x float64) (alarm bool, changePoint int) {
	d.mu.Lock()
	defer d.mu.Unlock()

	mean, std := d.stats.meanStd()
	d.stats.push(x)

	z := (x - mean) / std
	cusumAlarm := d.c.update(z)
	if cusumAlarm {
		d.c.reset()
	}

	cp0 := d.b.update(x)
	bocpdAlarm := cp0 > 0.5

	d.alarm = cusumAlarm || bocpdAlarm
	if d.alarm {
		d.cpMode = d.b.lastChangePoint
	}
	return d.alarm, d.cpMode
}

// State returns the current alarm state and last change-point index.
func (d *RegimeDetector) State() (alarm bool, changePoint int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.alarm, d.cpMode
}

// ── OutputDriftDetector ───────────────────────────────────────────────────────

// QualityObservation carries proxy quality signals for one agent turn.
type QualityObservation struct {
	TokenLength  int
	ToolCallRate float64
}

// OutputDriftDetector runs CUSUM on response token length and tool call rate
// to detect output / concept drift independent of latency drift.
type OutputDriftDetector struct {
	mu sync.Mutex

	tokenStats  runningStats
	tokenCUSUM  cusum
	tokenAlarm  bool

	rateStats runningStats
	rateCUSUM cusum
	rateAlarm bool
}

// NewOutputDriftDetector returns a ready-to-use OutputDriftDetector.
func NewOutputDriftDetector() *OutputDriftDetector {
	return &OutputDriftDetector{}
}

// Update ingests one quality observation and updates alarm state.
func (o *OutputDriftDetector) Update(q QualityObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// Token length CUSUM.
	tMean, tStd := o.tokenStats.meanStd()
	o.tokenStats.push(float64(q.TokenLength))
	tz := (float64(q.TokenLength) - tMean) / tStd
	if o.tokenCUSUM.update(tz) {
		o.tokenAlarm = true
		o.tokenCUSUM.reset()
	}

	// Tool call rate CUSUM.
	rMean, rStd := o.rateStats.meanStd()
	o.rateStats.push(q.ToolCallRate)
	rz := (q.ToolCallRate - rMean) / rStd
	if o.rateCUSUM.update(rz) {
		o.rateAlarm = true
		o.rateCUSUM.reset()
	}
}

// Alarm returns true when either the token-length or tool-call-rate CUSUM has
// fired since the last call to ClearAlarm.
func (o *OutputDriftDetector) Alarm() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.tokenAlarm || o.rateAlarm
}

// ClearAlarm resets both alarm flags.
func (o *OutputDriftDetector) ClearAlarm() {
	o.mu.Lock()
	o.tokenAlarm = false
	o.rateAlarm = false
	o.mu.Unlock()
}
