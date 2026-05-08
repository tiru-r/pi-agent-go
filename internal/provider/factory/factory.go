// Package factory constructs provider implementations from a Config.
//
// It lives in its own package (rather than in internal/provider) to avoid
// the import cycle that would arise if the parent provider package imported
// its own sub-packages (anthropic, openai, …), each of which already imports
// the parent package for the Provider interface and request/event types.
package factory

import (
	"fmt"

	"github.com/tiru-r/pi-agent-go/internal/config"
	"github.com/tiru-r/pi-agent-go/internal/provider"
	"github.com/tiru-r/pi-agent-go/internal/provider/anthropic"
	"github.com/tiru-r/pi-agent-go/internal/provider/azure"
	"github.com/tiru-r/pi-agent-go/internal/provider/bedrock"
	"github.com/tiru-r/pi-agent-go/internal/provider/cohere"
	"github.com/tiru-r/pi-agent-go/internal/provider/copilot"
	"github.com/tiru-r/pi-agent-go/internal/provider/gemini"
	"github.com/tiru-r/pi-agent-go/internal/provider/gitlab"
	"github.com/tiru-r/pi-agent-go/internal/provider/openai"
	"github.com/tiru-r/pi-agent-go/internal/provider/openrouter"
	"github.com/tiru-r/pi-agent-go/internal/provider/vertex"
)

// New constructs the appropriate Provider based on cfg.Provider.
func New(cfg *config.Config) (provider.Provider, error) {
	switch cfg.Provider {
	case "anthropic":
		if cfg.AnthropicAPIKey == "" {
			return nil, fmt.Errorf("anthropic: ANTHROPIC_API_KEY not set")
		}
		return anthropic.New(cfg.AnthropicAPIKey), nil

	case "openai":
		if cfg.OpenAIAPIKey == "" {
			return nil, fmt.Errorf("openai: OPENAI_API_KEY not set")
		}
		return openai.New(cfg.OpenAIAPIKey), nil

	case "gemini":
		if cfg.GeminiAPIKey == "" {
			return nil, fmt.Errorf("gemini: GEMINI_API_KEY not set")
		}
		return gemini.New(cfg.GeminiAPIKey), nil

	case "azure":
		return azure.New(cfg.AzureAPIKey, cfg.AzureEndpoint, cfg.AzureDeployment, cfg.AzureAPIVersion), nil

	case "cohere":
		if cfg.CohereAPIKey == "" {
			return nil, fmt.Errorf("cohere: COHERE_API_KEY not set")
		}
		return cohere.New(cfg.CohereAPIKey), nil

	case "bedrock":
		region := cfg.BedrockRegion
		if region == "" {
			region = "us-east-1"
		}
		return bedrock.New(region)

	case "vertex":
		return vertex.New(cfg.VertexProject, cfg.VertexLocation)

	case "copilot":
		// GitHub token is resolved lazily from env / gh config.
		return copilot.New(""), nil

	case "gitlab":
		// Base URL and token are resolved lazily from env / glab config.
		return gitlab.New("", ""), nil

	case "openrouter":
		if cfg.OpenRouterAPIKey == "" {
			return nil, fmt.Errorf("openrouter: OPENROUTER_API_KEY not set")
		}
		return openrouter.New(cfg.OpenRouterAPIKey, cfg.OpenRouterSiteURL, cfg.OpenRouterAppName), nil

	default:
		return nil, fmt.Errorf(
			"unknown provider: %q (supported: anthropic, openai, gemini, azure, cohere, bedrock, vertex, copilot, gitlab, openrouter)",
			cfg.Provider,
		)
	}
}
