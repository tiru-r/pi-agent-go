package openrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/tiru-r/pi-agent-go/internal/httpclient"
	"github.com/tiru-r/pi-agent-go/internal/model"
)

const modelsURL = "https://openrouter.ai/api/v1/models"

type orModelsResponse struct {
	Data []orModelEntry `json:"data"`
}

type orModelEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContextLen  int    `json:"context_length"`
	Architecture struct {
		Modality string `json:"modality"`
	} `json:"architecture"`
	TopProvider struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
	Pricing struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
	SupportedParameters []string `json:"supported_parameters"`
}

// FetchModels retrieves the live model list from OpenRouter's /api/v1/models endpoint.
// The apiKey is optional; unauthenticated requests return all public models.
func FetchModels(ctx context.Context, apiKey string) ([]model.ModelInfo, error) {
	req, err := http.NewRequest(http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("openrouter: models: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	data, err := httpclient.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("openrouter: models: %w", err)
	}
	var resp orModelsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("openrouter: models: decode: %w", err)
	}
	out := make([]model.ModelInfo, 0, len(resp.Data))
	for _, e := range resp.Data {
		out = append(out, toModelInfo(e))
	}
	return out, nil
}

func toModelInfo(e orModelEntry) model.ModelInfo {
	maxTok := e.TopProvider.MaxCompletionTokens
	if maxTok == 0 {
		maxTok = 4096
	}

	supportsVision := strings.Contains(e.Architecture.Modality, "image")

	supportsTools := false
	for _, p := range e.SupportedParameters {
		if p == "tools" {
			supportsTools = true
			break
		}
	}

	id := e.ID
	supportsThinking := strings.Contains(id, "claude-3-7") ||
		strings.Contains(id, "claude-opus-4") ||
		strings.Contains(id, "claude-sonnet-4") ||
		strings.Contains(id, "deepseek-r1") ||
		strings.Contains(id, "qwq") ||
		strings.HasSuffix(id, ":thinking")

	return model.ModelInfo{
		ID:               e.ID,
		Provider:         "openrouter",
		DisplayName:      e.Name,
		ContextWindow:    e.ContextLen,
		MaxTokens:        maxTok,
		SupportsTools:    supportsTools,
		SupportsVision:   supportsVision,
		SupportsThinking: supportsThinking,
		InputCostPer1M:   parsePrice(e.Pricing.Prompt) * 1e6,
		OutputCostPer1M:  parsePrice(e.Pricing.Completion) * 1e6,
	}
}

func parsePrice(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
