package model

// ModelInfo describes a registered model's capabilities and pricing.
type ModelInfo struct {
	ID               string
	Provider         string
	DisplayName      string
	MaxTokens        int
	SupportsTools    bool
	SupportsVision   bool
	SupportsThinking bool
	InputCostPer1M   float64
	OutputCostPer1M  float64
}

var Registry = []ModelInfo{
	// Anthropic
	{ID: "claude-opus-4-7", Provider: "anthropic", DisplayName: "Claude Opus 4.7", MaxTokens: 32000, SupportsTools: true, SupportsVision: true, SupportsThinking: true, InputCostPer1M: 15, OutputCostPer1M: 75},
	{ID: "claude-sonnet-4-6", Provider: "anthropic", DisplayName: "Claude Sonnet 4.6", MaxTokens: 16000, SupportsTools: true, SupportsVision: true, SupportsThinking: true, InputCostPer1M: 3, OutputCostPer1M: 15},
	{ID: "claude-haiku-4-5-20251001", Provider: "anthropic", DisplayName: "Claude Haiku 4.5", MaxTokens: 8096, SupportsTools: true, SupportsVision: true, InputCostPer1M: 0.25, OutputCostPer1M: 1.25},
	// OpenAI
	{ID: "gpt-4o", Provider: "openai", DisplayName: "GPT-4o", MaxTokens: 16384, SupportsTools: true, SupportsVision: true, InputCostPer1M: 2.5, OutputCostPer1M: 10},
	{ID: "gpt-4o-mini", Provider: "openai", DisplayName: "GPT-4o Mini", MaxTokens: 16384, SupportsTools: true, SupportsVision: true, InputCostPer1M: 0.15, OutputCostPer1M: 0.6},
	{ID: "o3", Provider: "openai", DisplayName: "o3", MaxTokens: 100000, SupportsTools: true, InputCostPer1M: 10, OutputCostPer1M: 40},
	{ID: "o4-mini", Provider: "openai", DisplayName: "o4-mini", MaxTokens: 100000, SupportsTools: true, InputCostPer1M: 1.1, OutputCostPer1M: 4.4},
	// Gemini
	{ID: "gemini-2.0-flash", Provider: "gemini", DisplayName: "Gemini 2.0 Flash", MaxTokens: 8192, SupportsTools: true, SupportsVision: true},
	{ID: "gemini-1.5-pro", Provider: "gemini", DisplayName: "Gemini 1.5 Pro", MaxTokens: 8192, SupportsTools: true, SupportsVision: true},
	{ID: "gemini-1.5-flash", Provider: "gemini", DisplayName: "Gemini 1.5 Flash", MaxTokens: 8192, SupportsTools: true, SupportsVision: true},
	// Azure OpenAI (deployment IDs vary)
	{ID: "azure/gpt-4o", Provider: "azure", DisplayName: "Azure GPT-4o", MaxTokens: 16384, SupportsTools: true, SupportsVision: true},
	{ID: "azure/gpt-4o-mini", Provider: "azure", DisplayName: "Azure GPT-4o Mini", MaxTokens: 16384, SupportsTools: true, SupportsVision: true},
	// Cohere
	{ID: "command-r-plus", Provider: "cohere", DisplayName: "Command R+", MaxTokens: 4096, SupportsTools: true, InputCostPer1M: 2.5, OutputCostPer1M: 10},
	{ID: "command-r", Provider: "cohere", DisplayName: "Command R", MaxTokens: 4096, SupportsTools: true, InputCostPer1M: 0.15, OutputCostPer1M: 0.6},
	// Bedrock (Claude via AWS)
	{ID: "bedrock/claude-opus-4-7", Provider: "bedrock", DisplayName: "Bedrock Claude Opus 4.7", MaxTokens: 32000, SupportsTools: true, SupportsVision: true},
	{ID: "bedrock/claude-sonnet-4-6", Provider: "bedrock", DisplayName: "Bedrock Claude Sonnet 4.6", MaxTokens: 16000, SupportsTools: true, SupportsVision: true},
	// Vertex AI (Gemini via Google Cloud)
	{ID: "vertex/gemini-1.5-pro", Provider: "vertex", DisplayName: "Vertex Gemini 1.5 Pro", MaxTokens: 8192, SupportsTools: true, SupportsVision: true},
	// GitHub Copilot
	{ID: "copilot/gpt-4o", Provider: "copilot", DisplayName: "Copilot GPT-4o", MaxTokens: 16384, SupportsTools: true, SupportsVision: true},
	// GitLab Duo
	{ID: "gitlab/claude-3-5-sonnet", Provider: "gitlab", DisplayName: "GitLab Duo Sonnet", MaxTokens: 16000, SupportsTools: true},

	// OpenRouter — curated popular models
	// Anthropic via OpenRouter
	{ID: "anthropic/claude-opus-4-7", Provider: "openrouter", DisplayName: "OR: Claude Opus 4.7", MaxTokens: 32000, SupportsTools: true, SupportsVision: true, SupportsThinking: true},
	{ID: "anthropic/claude-sonnet-4-6", Provider: "openrouter", DisplayName: "OR: Claude Sonnet 4.6", MaxTokens: 16000, SupportsTools: true, SupportsVision: true, SupportsThinking: true},
	{ID: "anthropic/claude-haiku-4-5", Provider: "openrouter", DisplayName: "OR: Claude Haiku 4.5", MaxTokens: 8096, SupportsTools: true, SupportsVision: true},
	// OpenAI via OpenRouter
	{ID: "openai/gpt-4o", Provider: "openrouter", DisplayName: "OR: GPT-4o", MaxTokens: 16384, SupportsTools: true, SupportsVision: true},
	{ID: "openai/gpt-4o-mini", Provider: "openrouter", DisplayName: "OR: GPT-4o Mini", MaxTokens: 16384, SupportsTools: true, SupportsVision: true},
	{ID: "openai/o3", Provider: "openrouter", DisplayName: "OR: o3", MaxTokens: 100000, SupportsTools: true},
	{ID: "openai/o4-mini", Provider: "openrouter", DisplayName: "OR: o4-mini", MaxTokens: 100000, SupportsTools: true},
	// Google via OpenRouter
	{ID: "google/gemini-2.0-flash-001", Provider: "openrouter", DisplayName: "OR: Gemini 2.0 Flash", MaxTokens: 8192, SupportsTools: true, SupportsVision: true},
	{ID: "google/gemini-1.5-pro", Provider: "openrouter", DisplayName: "OR: Gemini 1.5 Pro", MaxTokens: 8192, SupportsTools: true, SupportsVision: true},
	// Meta via OpenRouter
	{ID: "meta-llama/llama-3.3-70b-instruct", Provider: "openrouter", DisplayName: "OR: Llama 3.3 70B", MaxTokens: 8192, SupportsTools: true},
	{ID: "meta-llama/llama-3.1-405b-instruct", Provider: "openrouter", DisplayName: "OR: Llama 3.1 405B", MaxTokens: 8192, SupportsTools: true},
	// Mistral via OpenRouter
	{ID: "mistralai/mistral-large-2411", Provider: "openrouter", DisplayName: "OR: Mistral Large", MaxTokens: 8192, SupportsTools: true},
	{ID: "mistralai/codestral-2501", Provider: "openrouter", DisplayName: "OR: Codestral", MaxTokens: 32000, SupportsTools: true},
	// DeepSeek via OpenRouter
	{ID: "deepseek/deepseek-chat-v3-0324", Provider: "openrouter", DisplayName: "OR: DeepSeek Chat v3", MaxTokens: 8192, SupportsTools: true},
	{ID: "deepseek/deepseek-r1", Provider: "openrouter", DisplayName: "OR: DeepSeek R1", MaxTokens: 8192, SupportsThinking: true},
	// Qwen via OpenRouter
	{ID: "qwen/qwen-2.5-72b-instruct", Provider: "openrouter", DisplayName: "OR: Qwen 2.5 72B", MaxTokens: 8192, SupportsTools: true},
	{ID: "qwen/qwen-2.5-coder-32b-instruct", Provider: "openrouter", DisplayName: "OR: Qwen 2.5 Coder 32B", MaxTokens: 8192, SupportsTools: true},
	{ID: "qwen/qwq-32b", Provider: "openrouter", DisplayName: "OR: QwQ 32B", MaxTokens: 8192, SupportsThinking: true},
	// xAI via OpenRouter
	{ID: "x-ai/grok-3-beta", Provider: "openrouter", DisplayName: "OR: Grok 3", MaxTokens: 131072, SupportsTools: true, SupportsVision: true},
	{ID: "x-ai/grok-3-mini-beta", Provider: "openrouter", DisplayName: "OR: Grok 3 Mini", MaxTokens: 131072, SupportsTools: true},
}

func Lookup(id string) (ModelInfo, bool) {
	for _, m := range Registry {
		if m.ID == id {
			return m, true
		}
	}
	return ModelInfo{}, false
}

func ByProvider(provider string) []ModelInfo {
	var out []ModelInfo
	for _, m := range Registry {
		if m.Provider == provider {
			out = append(out, m)
		}
	}
	return out
}

func DefaultModel() ModelInfo {
	m, _ := Lookup("claude-sonnet-4-6")
	return m
}
