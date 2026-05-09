// Package extensions manages WASM and Node.js tool extensions for the pi agent.
package extensions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// extensionCallTimeout is the per-call deadline applied to every extension invocation.
const extensionCallTimeout = 30 * time.Second

// Capability names an operation an extension is allowed to perform.
type Capability string

const (
	CapFSRead  Capability = "fs_read"  // read-only filesystem access
	CapFSWrite Capability = "fs_write" // read-write filesystem access
	CapNetwork Capability = "network"  // outbound HTTP requests
	CapExec    Capability = "exec"     // shell command execution
	CapEnv     Capability = "env"      // read environment variables
	CapSession Capability = "session"  // session read/write operations
	CapUI      Capability = "ui"       // UI interaction operations
)

// PolicyDecision is the outcome of a capability policy check.
type PolicyDecision int

const (
	PolicyAllow PolicyDecision = iota
	PolicyDeny
	PolicyPrompt // interactive approval required
)

// toolCapability maps a Pi tool name to the capability it requires.
func toolCapability(toolName string) Capability {
	switch toolName {
	case "bash":
		return CapExec
	case "write", "edit", "hashline_edit":
		return CapFSWrite
	case "read", "grep", "find", "ls":
		return CapFSRead
	default:
		return CapFSRead // conservative default
	}
}

// toolDomain returns a stable domain label for telemetry lane keys.
func toolDomain(toolName string) string {
	switch toolName {
	case "bash":
		return "exec"
	case "write", "edit", "hashline_edit", "read", "grep", "find", "ls":
		return "filesystem"
	default:
		return "other"
	}
}

// isReadOnlyTool reports whether a tool performs only read operations.
func isReadOnlyTool(toolName string) bool {
	switch toolName {
	case "read", "grep", "find", "ls":
		return true
	}
	return false
}

// ── Env var blocklist ─────────────────────────────────────────────────────────

// envExactBlock is the set of environment variable names that are always blocked.
var envExactBlock = map[string]bool{
	"OPENROUTER_API_KEY":    true,
	"AWS_SECRET_ACCESS_KEY": true,
	"AWS_SESSION_TOKEN":     true,
	"GITHUB_TOKEN":          true,
	"NPM_TOKEN":             true,
	"PYPI_TOKEN":            true,
	"DATABASE_URL":          true,
	"REDIS_URL":             true,
}

// envSuffixBlock lists suffixes that, when matched case-insensitively, indicate a blocked var.
var envSuffixBlock = []string{
	"_API_KEY",
	"_SECRET",
	"_TOKEN",
	"_PASSWORD",
	"_PASSWD",
	"_PRIVATE_KEY",
	"_SECRET_KEY",
}

// envPrefixBlock lists prefixes that, when matched case-insensitively, indicate a blocked var.
var envPrefixBlock = []string{
	"AWS_SECRET_",
	"AWS_SESSION_",
}

// IsEnvBlocked reports whether the named environment variable is blocked from
// extension access. PI_* variables are unconditionally allowed.
func IsEnvBlocked(key string) bool {
	if strings.HasPrefix(key, "PI_") {
		return false
	}
	upper := strings.ToUpper(key)
	if envExactBlock[upper] {
		return true
	}
	for _, sfx := range envSuffixBlock {
		if strings.HasSuffix(upper, sfx) {
			return true
		}
	}
	for _, pfx := range envPrefixBlock {
		if strings.HasPrefix(upper, pfx) {
			return true
		}
	}
	return false
}

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
	return slices.Contains(m.Capabilities, cap)
}

func (m Manifest) allowedPaths() []string {
	if len(m.AllowPaths) > 0 {
		return m.AllowPaths
	}
	home, _ := os.UserHomeDir()
	return []string{home}
}
