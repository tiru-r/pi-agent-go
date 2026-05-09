package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Info holds the metadata an extension advertises via its describe function.
type Info struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// Extension is the interface each loaded extension must satisfy.
type Extension interface {
	Info() Info
	// Execute calls the extension with JSON params and returns (content, isError, err).
	Execute(ctx context.Context, params json.RawMessage) (string, bool, error)
	Close() error
}

// HookedExtension is an optional interface extensions can implement to participate
// in the agent tool lifecycle.
type HookedExtension interface {
	BeforeTool(ctx context.Context, name string, params json.RawMessage)
	AfterTool(ctx context.Context, name string, result string, isError bool)
}

// Manager discovers, loads, and governs all extensions.
// It owns the TrustRegistry and enforces quarantine on killed extensions.
type Manager struct {
	extensions []Extension
	trust      *TrustRegistry
	repairMode RepairMode
}

// New discovers and loads all extensions from dir.
// repairMode controls how aggressively load errors are auto-repaired.
// Returns an empty manager (no error) if dir is empty or does not exist.
func New(ctx context.Context, dir string) (*Manager, error) {
	return NewWithRepair(ctx, dir, RepairAutoSafe)
}

// NewWithRepair is like New but allows specifying the repair mode.
func NewWithRepair(ctx context.Context, dir string, mode RepairMode) (*Manager, error) {
	m := &Manager{
		trust:      NewTrustRegistry(),
		repairMode: mode,
	}
	if dir == "" {
		return m, nil
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("extensions: read %s: %w", dir, err)
	}

	jsBasenames := make(map[string]bool, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.ToLower(filepath.Ext(entry.Name())) != ".js" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		e, err := loadJS(ctx, path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ext] skip %s: %v\n", entry.Name(), err)
			continue
		}
		base := entry.Name()[:len(entry.Name())-len(".js")]
		jsBasenames[base] = true
		m.trust.Acknowledge(e.Info().Name)
		m.extensions = append(m.extensions, e)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.ToLower(filepath.Ext(entry.Name())) != ".json" {
			continue
		}
		base := entry.Name()[:len(entry.Name())-len(".json")]
		if jsBasenames[base] {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		e, err := loadNative(path)
		if err != nil {
			continue
		}
		m.trust.Acknowledge(e.Info().Name)
		m.extensions = append(m.extensions, e)
	}

	return m, nil
}

// All returns all loaded, non-quarantined extensions.
func (m *Manager) All() []Extension {
	var active []Extension
	for _, e := range m.extensions {
		if !m.trust.IsQuarantined(e.Info().Name) {
			active = append(active, e)
		}
	}
	return active
}

// Get returns the extension with the given name if it is not quarantined.
func (m *Manager) Get(name string) (Extension, bool) {
	for _, e := range m.extensions {
		if e.Info().Name == name {
			if m.trust.IsQuarantined(name) {
				return nil, false
			}
			return e, true
		}
	}
	return nil, false
}

// Kill activates the kill switch for the named extension.
// The extension is quarantined: it will not be invoked until Lift is called.
func (m *Manager) Kill(name, reason string) {
	m.trust.Kill(name, reason)
}

// Lift removes the kill switch, moving the extension back to Acknowledged.
func (m *Manager) Lift(name string) error {
	return m.trust.Lift(name)
}

// TrustState returns the current trust state for the named extension.
func (m *Manager) TrustState(name string) TrustState {
	return m.trust.State(name)
}

// Trust elevates an extension to fully trusted.
func (m *Manager) Trust(name string) {
	m.trust.Trust(name)
}

// Audits returns the full audit trail from the trust registry.
func (m *Manager) Audits() []AuditRecord {
	return m.trust.Audits()
}

// RunBeforeTool calls BeforeTool on every active hooked extension.
// Quarantined extensions are skipped.
func (m *Manager) RunBeforeTool(ctx context.Context, name string, params json.RawMessage) {
	for _, e := range m.extensions {
		if m.trust.IsQuarantined(e.Info().Name) {
			continue
		}
		if h, ok := e.(HookedExtension); ok {
			h.BeforeTool(ctx, name, params)
		}
	}
}

// RunAfterTool calls AfterTool on every active hooked extension.
// Quarantined extensions are skipped.
func (m *Manager) RunAfterTool(ctx context.Context, name string, result string, isError bool) {
	for _, e := range m.extensions {
		if m.trust.IsQuarantined(e.Info().Name) {
			continue
		}
		if h, ok := e.(HookedExtension); ok {
			h.AfterTool(ctx, name, result, isError)
		}
	}
}

// Close releases resources held by all extensions.
func (m *Manager) Close() error {
	for _, e := range m.extensions {
		_ = e.Close()
	}
	return nil
}
