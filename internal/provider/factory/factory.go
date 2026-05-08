package factory

import (
	"fmt"

	"github.com/tiru-r/pi-agent-go/internal/config"
	"github.com/tiru-r/pi-agent-go/internal/provider"
	"github.com/tiru-r/pi-agent-go/internal/provider/openrouter"
)

// New constructs the OpenRouter provider from cfg.
func New(cfg *config.Config) (provider.Provider, error) {
	if cfg.OpenRouterAPIKey == "" {
		return nil, fmt.Errorf("OPENROUTER_API_KEY is not set")
	}
	return openrouter.New(cfg.OpenRouterAPIKey, cfg.OpenRouterSiteURL, cfg.OpenRouterAppName), nil
}
