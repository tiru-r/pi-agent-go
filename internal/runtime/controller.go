package runtime

import (
	"math"
	"sync"
)

const (
	ocoTauMin        = 0.1
	ocoTauMax        = 10.0
	ocoEta           = 0.05
	ocoRollbackThresh = 2.0
)

// OCOController implements online gradient descent with clipping and rollback.
type OCOController struct {
	mu            sync.Mutex
	tau           float64
	tauMin        float64
	tauMax        float64
	eta           float64
	rollbackThresh float64
	safeVal       float64
	lastLoss      float64
}

// NewOCOController constructs an OCOController with default parameters.
// Initial tau = (tauMin + tauMax) / 2 = 5.05.
func NewOCOController() *OCOController {
	initTau := (ocoTauMin + ocoTauMax) / 2
	return &OCOController{
		tau:            initTau,
		tauMin:         ocoTauMin,
		tauMax:         ocoTauMax,
		eta:            ocoEta,
		rollbackThresh: ocoRollbackThresh,
		safeVal:        initTau,
		lastLoss:       0,
	}
}

// clip clamps v to [lo, hi].
func clip(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

// Update applies one gradient step and returns the new tau.
// If loss > rollbackThresh, restores tau to the last safe value.
// If loss < lastLoss, records tau as the new safe value.
func (c *OCOController) Update(loss, grad float64) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	if loss > c.rollbackThresh {
		c.tau = c.safeVal
		return c.tau
	}

	c.tau = clip(c.tau-c.eta*grad, c.tauMin, c.tauMax)

	if loss < c.lastLoss {
		c.safeVal = c.tau
	}
	c.lastLoss = loss

	return c.tau
}

// Param returns the current tau without modifying state.
func (c *OCOController) Param() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tau
}
