package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config holds all pi-agent settings, loaded from ~/.pi/agent/settings.json
// and overridable via environment variables.
type Config struct {
	// OpenRouter
	OpenRouterAPIKey  string `json:"openrouter_api_key,omitempty"`
	OpenRouterSiteURL string `json:"openrouter_site_url,omitempty"`
	OpenRouterAppName string `json:"openrouter_app_name,omitempty"`

	// Model selection — any OpenRouter model ID, e.g. "tencent/hy3-preview:free"
	Model string `json:"model,omitempty"`

	// Behaviour
	MaxTokens     int     `json:"max_tokens,omitempty"`
	Temperature   float64 `json:"temperature,omitempty"`
	SystemPrompt  string  `json:"system_prompt,omitempty"`
	ThinkingLevel string  `json:"thinking_level,omitempty"` // "off"|"auto"|"full"

	// Session
	SessionDir string `json:"session_dir,omitempty"`
	SQLite     bool   `json:"sqlite,omitempty"`
}

var defaultCfg = Config{
	Model:     "tencent/hy3-preview:free",
	MaxTokens: 8096,
	SQLite:    true,
}

// Load returns the effective config, merging file settings and env vars.
func Load() (*Config, error) {
	cfg := defaultCfg

	if data, err := os.ReadFile(settingsPath()); err == nil {
		var fileCfg Config
		if err := json.Unmarshal(data, &fileCfg); err == nil {
			merge(&cfg, &fileCfg)
		}
	}

	applyEnv(&cfg)
	setDefaults(&cfg)
	return &cfg, nil
}

// Save writes the config to disk.
func (c *Config) Save() error {
	path := settingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func settingsPath() string {
	if v := os.Getenv("PI_CONFIG"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "agent", "settings.json")
}

func merge(base, override *Config) {
	if override.OpenRouterAPIKey != "" {
		base.OpenRouterAPIKey = override.OpenRouterAPIKey
	}
	if override.OpenRouterSiteURL != "" {
		base.OpenRouterSiteURL = override.OpenRouterSiteURL
	}
	if override.OpenRouterAppName != "" {
		base.OpenRouterAppName = override.OpenRouterAppName
	}
	if override.Model != "" {
		base.Model = override.Model
	}
	if override.MaxTokens != 0 {
		base.MaxTokens = override.MaxTokens
	}
	if override.Temperature != 0 {
		base.Temperature = override.Temperature
	}
	if override.SystemPrompt != "" {
		base.SystemPrompt = override.SystemPrompt
	}
	if override.ThinkingLevel != "" {
		base.ThinkingLevel = override.ThinkingLevel
	}
	if override.SessionDir != "" {
		base.SessionDir = override.SessionDir
	}
	base.SQLite = override.SQLite
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("OPENROUTER_API_KEY"); v != "" {
		cfg.OpenRouterAPIKey = v
	}
	if v := os.Getenv("PI_MODEL"); v != "" {
		cfg.Model = v
	}
}

func setDefaults(cfg *Config) {
	if cfg.SessionDir == "" {
		home, _ := os.UserHomeDir()
		cfg.SessionDir = filepath.Join(home, ".pi", "agent", "sessions")
	}
}
