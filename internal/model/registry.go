package model

// ModelInfo describes a model's capabilities and pricing.
// Populated dynamically from OpenRouter's /api/v1/models endpoint.
type ModelInfo struct {
	ID               string
	Provider         string
	DisplayName      string
	ContextWindow    int // total context length in tokens (input + output)
	MaxTokens        int // max output tokens
	SupportsTools    bool
	SupportsVision   bool
	SupportsThinking bool
	InputCostPer1M   float64
	OutputCostPer1M  float64
}
