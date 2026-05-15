package factory

import (
	"fmt"

	"github.com/tiru-r/pi-agent-go/internal/config"
	"github.com/tiru-r/pi-agent-go/internal/provider"
	"github.com/tiru-r/pi-agent-go/internal/provider/openrouter"
	"github.com/tiru-r/pi-agent-go/internal/runtime"
)

// New constructs the OpenRouter provider from cfg, optionally wiring the
// semantic response cache.
func New(cfg *config.Config) (provider.Provider, error) {
	if cfg.OpenRouterAPIKey == "" {
		return nil, fmt.Errorf("OPENROUTER_API_KEY is not set")
	}
	p := openrouter.New(cfg.OpenRouterAPIKey, cfg.OpenRouterSiteURL, cfg.OpenRouterAppName)
	if cfg.SemanticCache {
		p.SemanticCache = runtime.NewSemanticCache(0, 0) // defaults: threshold=0.85, cap=512
	}
	return p, nil
}
