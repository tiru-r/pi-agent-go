package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/tiru-r/pi-agent-go/internal/model"
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
	MaxTokens     int      `json:"max_tokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"` // nil = unset; pointer so 0.0 is distinguishable from "not set"
	SystemPrompt  string   `json:"system_prompt,omitempty"`
	ThinkingLevel string  `json:"thinking_level,omitempty"` // "off"|"minimal"|"low"|"medium"|"high"|"xhigh"

	// Session
	SessionDir string `json:"session_dir,omitempty"`
	SQLite     bool   `json:"sqlite,omitempty"`

	// Extensions
	ExtensionsDir string `json:"extensions_dir,omitempty"`

	// Caching
	SemanticCache bool `json:"semantic_cache,omitempty"` // enable Jaccard-similarity response cache

	// ModelProfileOverrides pins specific model IDs to a fixed model.Profile,
	// overriding the automatic tier classification. Useful when a model is
	// misclassified (e.g. a high-priced model that behaves like a small one).
	// Keys are full OpenRouter model IDs (e.g. "openai/gpt-oss-120b:free").
	ModelProfileOverrides map[string]model.Profile `json:"model_profile_overrides,omitempty"`
}

// defaultSystemPromptTemplate is the built-in system prompt. "{{TOOLS}}" is
// replaced at startup with the live tool-registry names via ExpandSystemPrompt.
const defaultSystemPromptTemplate = `You are Pi, an expert AI coding assistant running inside the Zed editor. You have access to the following tools: {{TOOLS}}. Never call any other tool name.

Core rules:
1. Complete the task in full. If asked to "write tests", create new test files with real, meaningful test functions — do not stop just because existing tests already pass.
2. When a tool call fails, read the error carefully, fix the argument, and retry. Do not give up after one failure.
3. Explore before acting. Use print_tree or ls to understand the project structure, then read the relevant source files before writing or editing anything.
4. Paths: prefer absolute paths. Relative paths (e.g. ".", "src/") are automatically resolved from the working directory shown in your context.
5. Verify non-trivial changes by running the affected tests or build commands via bash.
6. Be concise in explanations but thorough in execution — do the work, don't just describe it.`

// ExpandSystemPrompt replaces "{{TOOLS}}" in s with a comma-joined list of
// toolNames. Call this after the tool registry is initialised (e.g. in the
// ACP server or CLI) rather than at config-load time, which avoids a
// circular import between the config and tools packages.
// If s contains no placeholder it is returned unchanged, so custom
// user-supplied prompts are never mutated.
func ExpandSystemPrompt(s string, toolNames []string) string {
	return strings.ReplaceAll(s, "{{TOOLS}}", strings.Join(toolNames, ", "))
}

var defaultCfg = Config{
	Model:        "openai/gpt-oss-120b:free",
	MaxTokens:    8096,
	SQLite:       true,
	SystemPrompt: defaultSystemPromptTemplate,
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

// ConfigDir returns the directory that holds pi-agent configuration files.
// It is derived from settingsPath so that PI_CONFIG (which sets the full
// settings file path) is always the single source of truth.
func ConfigDir() string {
	return filepath.Dir(settingsPath())
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
	if override.Temperature != nil {
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
	if len(override.ModelProfileOverrides) > 0 {
		if base.ModelProfileOverrides == nil {
			base.ModelProfileOverrides = make(map[string]model.Profile, len(override.ModelProfileOverrides))
		}
		for k, v := range override.ModelProfileOverrides {
			base.ModelProfileOverrides[k] = v
		}
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
