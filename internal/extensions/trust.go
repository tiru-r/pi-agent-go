package extensions

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// TrustState is the lifecycle state of a loaded extension.
type TrustState string

const (
	TrustPending      TrustState = "pending"      // loaded but not yet reviewed
	TrustAcknowledged TrustState = "acknowledged" // operator has seen it; not yet fully trusted
	TrustTrusted      TrustState = "trusted"      // fully trusted by operator
	TrustKilled       TrustState = "killed"        // kill-switched; quarantined
)

// AuditRecord is written for every trust state transition.
type AuditRecord struct {
	Time    time.Time  `json:"time"`
	ExtName string     `json:"ext_name"`
	Action  string     `json:"action"`
	Reason  string     `json:"reason,omitempty"`
	State   TrustState `json:"state"`
}

// TrustRegistry tracks per-extension trust state and the audit trail.
// All methods are safe for concurrent use.
type TrustRegistry struct {
	mu         sync.RWMutex
	states     map[string]TrustState
	quarantine map[string]bool
	audits     []AuditRecord
}

// NewTrustRegistry returns an empty registry; extensions start at TrustPending.
func NewTrustRegistry() *TrustRegistry {
	return &TrustRegistry{
		states:     make(map[string]TrustState),
		quarantine: make(map[string]bool),
	}
}

// State returns the current trust state for name (TrustPending if never set).
func (r *TrustRegistry) State(name string) TrustState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.states[name]; ok {
		return s
	}
	return TrustPending
}

// Acknowledge marks the extension as reviewed by the operator.
func (r *TrustRegistry) Acknowledge(name string) {
	r.transition(name, TrustAcknowledged, "acknowledged", "")
}

// Trust elevates the extension to fully trusted.
func (r *TrustRegistry) Trust(name string) {
	r.transition(name, TrustTrusted, "trust", "")
}

// Kill activates the kill switch: the extension is quarantined, a critical
// alert is emitted to stderr, and an audit record is written.
// Lifting requires an explicit Lift call.
func (r *TrustRegistry) Kill(name, reason string) {
	r.mu.Lock()
	r.states[name] = TrustKilled
	r.quarantine[name] = true
	r.audits = append(r.audits, AuditRecord{
		Time:    time.Now(),
		ExtName: name,
		Action:  "kill",
		Reason:  reason,
		State:   TrustKilled,
	})
	r.mu.Unlock()
	fmt.Fprintf(os.Stderr, "[ext:CRITICAL] extension %q killed: %s\n", name, reason)
}

// Lift removes the kill switch and moves the extension back to Acknowledged.
// Returns an error if the extension is not currently killed.
func (r *TrustRegistry) Lift(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.states[name] != TrustKilled {
		return fmt.Errorf("extension %q is not killed (current state: %s)", name, r.states[name])
	}
	delete(r.quarantine, name)
	r.states[name] = TrustAcknowledged
	r.audits = append(r.audits, AuditRecord{
		Time:    time.Now(),
		ExtName: name,
		Action:  "lift",
		Reason:  "operator lift",
		State:   TrustAcknowledged,
	})
	return nil
}

// IsQuarantined reports whether the extension is currently quarantined.
func (r *TrustRegistry) IsQuarantined(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.quarantine[name]
}

// Audits returns a copy of the full audit trail.
func (r *TrustRegistry) Audits() []AuditRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AuditRecord, len(r.audits))
	copy(out, r.audits)
	return out
}

// MarshalJSON serializes trust state for diagnostics.
func (r *TrustRegistry) MarshalJSON() ([]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return json.Marshal(struct {
		States     map[string]TrustState `json:"states"`
		Quarantine map[string]bool       `json:"quarantine"`
	}{r.states, r.quarantine})
}

func (r *TrustRegistry) transition(name string, state TrustState, action, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states[name] = state
	r.audits = append(r.audits, AuditRecord{
		Time:    time.Now(),
		ExtName: name,
		Action:  action,
		Reason:  reason,
		State:   state,
	})
}
