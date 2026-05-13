package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Config holds all pi-agent settings, loaded from ~/.pi/agent/settings.json
// and overridable via environment variables.
type Config struct {
	// OpenRouter
	OpenRouterAPIKey  string `json:"openrouter_api_key,omitempty"`
	OpenRouterSiteURL string `json:"openrouter_site_url,omitempty"`
	OpenRouterAppName string `json:"openrouter_app_name,omitempty"`

	// Model selection — any OpenRouter model ID, e.g. "openai/gpt-oss-120b:free"
	Model string `json:"model,omitempty"`

	// Behaviour
	MaxTokens     int     `json:"max_tokens,omitempty"`
	Temperature   float64 `json:"temperature,omitempty"`
	SystemPrompt  string  `json:"system_prompt,omitempty"`
	ThinkingLevel string  `json:"thinking_level,omitempty"` // "off"|"minimal"|"low"|"medium"|"high"|"xhigh"

	// Session
	SessionDir string `json:"session_dir,omitempty"`
	SQLite     bool   `json:"sqlite,omitempty"`

	// Extensions
	ExtensionsDir string `json:"extensions_dir,omitempty"`

	// Caching / distillation
	SemanticCache bool   `json:"semantic_cache,omitempty"` // enable Jaccard-similarity response cache
	DistillFile   string `json:"distill_file,omitempty"`   // JSONL path for knowledge-distillation output
}

var defaultCfg = Config{
	Model:     "openai/gpt-oss-120b:free",
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
			// Only override bool fields when the key is explicitly present in the
			// file, because a bool zero value (false) is indistinguishable from
			// "not set" after JSON unmarshalling.
			if strings.Contains(string(data), `"sqlite"`) {
				cfg.SQLite = fileCfg.SQLite
			}
			if strings.Contains(string(data), `"semantic_cache"`) {
				cfg.SemanticCache = fileCfg.SemanticCache
			}
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
	if override.ExtensionsDir != "" {
		base.ExtensionsDir = override.ExtensionsDir
	}
	if override.DistillFile != "" {
		base.DistillFile = override.DistillFile
	}
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("OPENROUTER_API_KEY"); v != "" {
		cfg.OpenRouterAPIKey = v
	}
	if v := os.Getenv("PI_MODEL"); v != "" {
		cfg.Model = v
	}
	if v := os.Getenv("PI_EXTENSIONS_DIR"); v != "" {
		cfg.ExtensionsDir = v
	}
}

func setDefaults(cfg *Config) {
	home, _ := os.UserHomeDir()
	if cfg.SessionDir == "" {
		cfg.SessionDir = filepath.Join(home, ".pi", "agent", "sessions")
	}
	if cfg.ExtensionsDir == "" {
		cfg.ExtensionsDir = filepath.Join(home, ".pi", "extensions")
	}
}
