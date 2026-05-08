// Package migrations handles idempotent startup migrations from legacy pi-mono layouts.
package migrations

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tiru-r/pi-agent-go/internal/config"
)

const migrationsFile = ".migrations"

// Run applies all pending migrations idempotently.
// It reads the list of already-applied migrations from ~/.pi/agent/.migrations
// and only runs migrations that are not yet listed there.
func Run(cfg *config.Config) error {
	agentDir := agentDir()

	applied, err := loadApplied(agentDir)
	if err != nil {
		// Non-fatal: if we can't read the file, try all migrations.
		applied = map[string]bool{}
	}

	migrations := []struct {
		name string
		fn   func(agentDir string, cfg *config.Config) error
	}{
		{"session_dir", migrateSessionDir},
		{"auth_file", migrateAuthFile},
		{"settings_file", migrateSettingsFile},
	}

	for _, m := range migrations {
		if applied[m.name] {
			continue
		}
		if err := m.fn(agentDir, cfg); err != nil {
			// Log the error but don't abort — migrations are best-effort.
			fmt.Fprintf(os.Stderr, "migration %s: %v\n", m.name, err)
			continue
		}
		applied[m.name] = true
	}

	return saveApplied(agentDir, applied)
}

// agentDir returns the canonical ~/.pi/agent directory.
func agentDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "agent")
}

// loadApplied reads the set of already-applied migration names.
func loadApplied(agentDir string) (map[string]bool, error) {
	path := filepath.Join(agentDir, migrationsFile)
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	applied := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			applied[line] = true
		}
	}
	return applied, scanner.Err()
}

// saveApplied persists the set of applied migrations.
func saveApplied(agentDir string, applied map[string]bool) error {
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(agentDir, migrationsFile)
	var sb strings.Builder
	for name := range applied {
		sb.WriteString(name)
		sb.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(sb.String()), 0o600)
}

// ============================================================================
// Migration: session_dir
// Move ~/.pi/sessions → ~/.pi/agent/sessions if the latter doesn't exist.
// ============================================================================

func migrateSessionDir(agentDir string, _ *config.Config) error {
	home, _ := os.UserHomeDir()
	oldDir := filepath.Join(home, ".pi", "sessions")
	newDir := filepath.Join(agentDir, "sessions")

	oldExists, _ := dirExists(oldDir)
	newExists, _ := dirExists(newDir)

	if !oldExists || newExists {
		return nil // nothing to do
	}

	if err := os.MkdirAll(filepath.Dir(newDir), 0o700); err != nil {
		return fmt.Errorf("session_dir: mkdir parent: %w", err)
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		return fmt.Errorf("session_dir: rename: %w", err)
	}
	return nil
}

// ============================================================================
// Migration: auth_file
// Move ~/.pi/auth.json → ~/.pi/agent/auth.json if the latter doesn't exist.
// ============================================================================

func migrateAuthFile(agentDir string, _ *config.Config) error {
	home, _ := os.UserHomeDir()
	oldPath := filepath.Join(home, ".pi", "auth.json")
	newPath := filepath.Join(agentDir, "auth.json")

	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		return nil
	}
	if _, err := os.Stat(newPath); err == nil {
		return nil // already present
	}

	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return fmt.Errorf("auth_file: mkdir: %w", err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("auth_file: rename: %w", err)
	}
	return nil
}

// ============================================================================
// Migration: settings_file
// Move ~/.pi/settings.json → ~/.pi/agent/settings.json if the latter doesn't exist,
// and convert legacy key names to the current format.
// ============================================================================

func migrateSettingsFile(agentDir string, _ *config.Config) error {
	home, _ := os.UserHomeDir()
	oldPath := filepath.Join(home, ".pi", "settings.json")
	newPath := filepath.Join(agentDir, "settings.json")

	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		return nil
	}
	if _, err := os.Stat(newPath); err == nil {
		return nil // already present
	}

	data, err := os.ReadFile(oldPath)
	if err != nil {
		return fmt.Errorf("settings_file: read old: %w", err)
	}

	// Parse as generic map so we can rename legacy keys.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		// If we can't parse, copy as-is.
		if err2 := os.MkdirAll(agentDir, 0o700); err2 != nil {
			return err2
		}
		return os.Rename(oldPath, newPath)
	}

	// Convert legacy camelCase keys to snake_case equivalents.
	legacyKeyMap := map[string]string{
		"apiKey":       "anthropic_api_key",
		"defaultModel": "model",
		"systemPrompt": "system_prompt",
		"maxTokens":    "max_tokens",
		"sessionDir":   "session_dir",
	}
	for oldKey, newKey := range legacyKeyMap {
		if val, ok := raw[oldKey]; ok {
			delete(raw, oldKey)
			if _, exists := raw[newKey]; !exists {
				raw[newKey] = val
			}
		}
	}

	converted, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("settings_file: marshal: %w", err)
	}

	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return fmt.Errorf("settings_file: mkdir: %w", err)
	}
	if err := os.WriteFile(newPath, converted, 0o600); err != nil {
		return fmt.Errorf("settings_file: write new: %w", err)
	}

	// Remove old file only after successfully writing the new one.
	_ = os.Remove(oldPath)
	return nil
}

// dirExists returns true if path exists and is a directory.
func dirExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}
