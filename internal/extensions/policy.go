// Package extensions manages WASM and Node.js tool extensions for the pi agent.
package extensions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// extensionCallTimeout is the per-call deadline applied to every extension invocation.
const extensionCallTimeout = 30 * time.Second

// Capability names an operation an extension is allowed to perform.
type Capability string

const (
	CapFSRead  Capability = "fs_read"  // read-only filesystem access
	CapFSWrite Capability = "fs_write" // read-write filesystem access
	CapNetwork Capability = "network"  // outbound network (WASM only; node always has it)
	CapEnv     Capability = "env"      // read environment variables
)

// Manifest is loaded from a sibling .json file next to each extension binary.
// If absent, the extension runs with no extra capabilities.
type Manifest struct {
	// Capabilities lists what the extension is permitted to do.
	Capabilities []Capability `json:"capabilities"`
	// AllowPaths restricts FS access to these host paths (empty = user home dir).
	AllowPaths []string `json:"allow_paths,omitempty"`
}

// loadManifest reads the sibling <base>.json file next to extensionPath.
// Returns a zero-value Manifest on any error (no capabilities granted).
func loadManifest(extensionPath string) Manifest {
	base := extensionPath[:len(extensionPath)-len(filepath.Ext(extensionPath))]
	data, err := os.ReadFile(base + ".json")
	if err != nil {
		return Manifest{}
	}
	var m Manifest
	_ = json.Unmarshal(data, &m)
	return m
}

func (m Manifest) has(cap Capability) bool {
	for _, c := range m.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

func (m Manifest) allowedPaths() []string {
	if len(m.AllowPaths) > 0 {
		return m.AllowPaths
	}
	home, _ := os.UserHomeDir()
	return []string{home}
}
