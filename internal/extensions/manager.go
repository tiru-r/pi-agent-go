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
// in the agent tool lifecycle. Both JS and future extension types may implement it.
type HookedExtension interface {
	// BeforeTool is called before any tool (not just this extension's) executes.
	BeforeTool(ctx context.Context, name string, params json.RawMessage)
	// AfterTool is called after any tool executes.
	AfterTool(ctx context.Context, name string, result string, isError bool)
}

// Manager discovers and holds all loaded extensions.
type Manager struct {
	extensions []Extension
}

// New discovers and loads all extensions from dir:
//   - .js files are loaded as in-process goja extensions
//   - .json files without a sibling .js are loaded as native subprocess extensions
//
// Returns an empty manager (no error) if dir is empty or does not exist.
func New(ctx context.Context, dir string) (*Manager, error) {
	m := &Manager{}
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

	// Track which base names have a .js file so we can skip their sidecar .json manifests.
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
		m.extensions = append(m.extensions, e)
	}

	// Load native descriptor extensions (.json without a sibling .js).
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.ToLower(filepath.Ext(entry.Name())) != ".json" {
			continue
		}
		base := entry.Name()[:len(entry.Name())-len(".json")]
		if jsBasenames[base] {
			continue // sidecar manifest for a JS extension
		}
		path := filepath.Join(dir, entry.Name())
		e, err := loadNative(path)
		if err != nil {
			// Silently skip: .json files may be non-extension config.
			continue
		}
		m.extensions = append(m.extensions, e)
	}

	return m, nil
}

// All returns all loaded extensions.
func (m *Manager) All() []Extension { return m.extensions }

// Get returns the extension with the given name.
func (m *Manager) Get(name string) (Extension, bool) {
	for _, e := range m.extensions {
		if e.Info().Name == name {
			return e, true
		}
	}
	return nil, false
}

// RunBeforeTool calls BeforeTool on every extension that implements HookedExtension.
// Calls are best-effort and happen synchronously before the tool executes.
func (m *Manager) RunBeforeTool(ctx context.Context, name string, params json.RawMessage) {
	for _, e := range m.extensions {
		if h, ok := e.(HookedExtension); ok {
			h.BeforeTool(ctx, name, params)
		}
	}
}

// RunAfterTool calls AfterTool on every extension that implements HookedExtension.
// Calls are best-effort and happen synchronously after the tool executes.
func (m *Manager) RunAfterTool(ctx context.Context, name string, result string, isError bool) {
	for _, e := range m.extensions {
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
