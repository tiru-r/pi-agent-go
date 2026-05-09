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

// Manager discovers and holds all loaded JS extensions.
type Manager struct {
	extensions []Extension
}

// New discovers and loads all .js extensions from dir.
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

// Close releases resources held by all extensions.
func (m *Manager) Close() error {
	for _, e := range m.extensions {
		_ = e.Close()
	}
	return nil
}
