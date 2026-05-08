package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config holds all pi-agent settings, loaded from ~/.pi/agent/settings.json
// and overridable via environment variables.
type Config struct {
	// Provider selection
	Provider string `json:"provider,omitempty"` // "anthropic" | "openai" | ...
	Model    string `json:"model,omitempty"`

	// Provider API keys (also read from env)
	AnthropicAPIKey   string `json:"anthropic_api_key,omitempty"`
	OpenAIAPIKey      string `json:"openai_api_key,omitempty"`
	GeminiAPIKey      string `json:"gemini_api_key,omitempty"`
	CohereAPIKey      string `json:"cohere_api_key,omitempty"`
	OpenRouterAPIKey  string `json:"openrouter_api_key,omitempty"`
	// OpenRouter optional branding headers (shown on openrouter.ai dashboard).
	OpenRouterSiteURL string `json:"openrouter_site_url,omitempty"`
	OpenRouterAppName string `json:"openrouter_app_name,omitempty"`

	// Azure-specific
	AzureEndpoint   string `json:"azure_endpoint,omitempty"`
	AzureAPIKey     string `json:"azure_api_key,omitempty"`
	AzureAPIVersion string `json:"azure_api_version,omitempty"`
	AzureDeployment string `json:"azure_deployment,omitempty"`

	// Vertex AI
	VertexProject  string `json:"vertex_project,omitempty"`
	VertexLocation string `json:"vertex_location,omitempty"`

	// Bedrock
	BedrockRegion string `json:"bedrock_region,omitempty"`

	// Behaviour
	MaxTokens     int     `json:"max_tokens,omitempty"`
	Temperature   float64 `json:"temperature,omitempty"`
	SystemPrompt  string  `json:"system_prompt,omitempty"`
	ThinkingLevel string  `json:"thinking_level,omitempty"` // "off"|"auto"|"full"

	// Session
	SessionDir string `json:"session_dir,omitempty"`
	SQLite     bool   `json:"sqlite,omitempty"`

	// UI
	Theme       string `json:"theme,omitempty"`
	SyntaxTheme string `json:"syntax_theme,omitempty"`
}

var defaultCfg = Config{
	Provider:      "anthropic",
	Model:         "claude-sonnet-4-6",
	MaxTokens:     8096,
	ThinkingLevel: "off",
	Theme:         "dark",
	SyntaxTheme:   "monokai",
	SQLite:        true,
}

// Load returns the effective config, merging file settings and env vars.
func Load() (*Config, error) {
	cfg := defaultCfg

	path := settingsPath()
	if data, err := os.ReadFile(path); err == nil {
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
	if override.Provider != "" {
		base.Provider = override.Provider
	}
	if override.Model != "" {
		base.Model = override.Model
	}
	if override.AnthropicAPIKey != "" {
		base.AnthropicAPIKey = override.AnthropicAPIKey
	}
	if override.OpenAIAPIKey != "" {
		base.OpenAIAPIKey = override.OpenAIAPIKey
	}
	if override.GeminiAPIKey != "" {
		base.GeminiAPIKey = override.GeminiAPIKey
	}
	if override.CohereAPIKey != "" {
		base.CohereAPIKey = override.CohereAPIKey
	}
	if override.AzureEndpoint != "" {
		base.AzureEndpoint = override.AzureEndpoint
	}
	if override.AzureAPIKey != "" {
		base.AzureAPIKey = override.AzureAPIKey
	}
	if override.AzureAPIVersion != "" {
		base.AzureAPIVersion = override.AzureAPIVersion
	}
	if override.AzureDeployment != "" {
		base.AzureDeployment = override.AzureDeployment
	}
	if override.VertexProject != "" {
		base.VertexProject = override.VertexProject
	}
	if override.VertexLocation != "" {
		base.VertexLocation = override.VertexLocation
	}
	if override.BedrockRegion != "" {
		base.BedrockRegion = override.BedrockRegion
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
	if override.Theme != "" {
		base.Theme = override.Theme
	}
	if override.SyntaxTheme != "" {
		base.SyntaxTheme = override.SyntaxTheme
	}
	base.SQLite = override.SQLite
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
		cfg.AnthropicAPIKey = v
	}
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		cfg.OpenAIAPIKey = v
	}
	if v := os.Getenv("GEMINI_API_KEY"); v != "" || os.Getenv("GOOGLE_API_KEY") != "" {
		if v == "" {
			v = os.Getenv("GOOGLE_API_KEY")
		}
		cfg.GeminiAPIKey = v
	}
	if v := os.Getenv("COHERE_API_KEY"); v != "" {
		cfg.CohereAPIKey = v
	}
	if v := os.Getenv("OPENROUTER_API_KEY"); v != "" {
		cfg.OpenRouterAPIKey = v
	}
	if v := os.Getenv("AZURE_OPENAI_API_KEY"); v != "" {
		cfg.AzureAPIKey = v
	}
	if v := os.Getenv("AZURE_OPENAI_ENDPOINT"); v != "" {
		cfg.AzureEndpoint = v
	}
	if v := os.Getenv("GOOGLE_CLOUD_PROJECT"); v != "" {
		cfg.VertexProject = v
	}
	if v := os.Getenv("PI_MODEL"); v != "" {
		cfg.Model = v
	}
	if v := os.Getenv("PI_PROVIDER"); v != "" {
		cfg.Provider = v
	}
}

func setDefaults(cfg *Config) {
	if cfg.SessionDir == "" {
		home, _ := os.UserHomeDir()
		cfg.SessionDir = filepath.Join(home, ".pi", "agent", "sessions")
	}
}
