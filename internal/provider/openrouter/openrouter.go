// Package openrouter implements the OpenRouter unified LLM API provider.
//
// OpenRouter proxies to 200+ models from Anthropic, OpenAI, Google, Meta,
// Mistral, DeepSeek and others through a single OpenAI-compatible endpoint.
//
// Docs: https://openrouter.ai/docs
package openrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/tiru-r/pi-agent-go/internal/httpclient"
	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/provider"
	"github.com/tiru-r/pi-agent-go/internal/sse"
)

const baseURL = "https://openrouter.ai/api/v1/chat/completions"

// Provider implements provider.Provider for OpenRouter.
type Provider struct {
	apiKey  string
	siteURL string // HTTP-Referer header value — shown on openrouter.ai dashboard
	appName string // X-Title header value — shown on openrouter.ai dashboard
}

// New creates an OpenRouter provider.
//
//   - siteURL and appName are optional; they appear in your OpenRouter dashboard
//     and can improve rate-limit handling.  Pass empty strings to omit them.
func New(apiKey, siteURL, appName string) *Provider {
	return &Provider{apiKey: apiKey, siteURL: siteURL, appName: appName}
}

func (p *Provider) Name() string { return "openrouter" }

// ── Wire types ────────────────────────────────────────────────────────────────

type orRequest struct {
	Model       string     `json:"model"`
	Messages    []orMsg    `json:"messages"`
	Tools       []orTool   `json:"tools,omitempty"`
	MaxTokens   int        `json:"max_tokens,omitempty"`
	Temperature *float64   `json:"temperature,omitempty"`
	Stop        []string   `json:"stop,omitempty"`
	Stream      bool       `json:"stream"`

	// OpenRouter-specific extensions — populated from Request.Extra.
	// See: https://openrouter.ai/docs/provider-routing
	Provider *providerPrefs `json:"provider,omitempty"`
	// Fallback model list — if primary model fails, try these in order.
	Models []string `json:"models,omitempty"`
	// Route is "fallback" to enable the models list.
	Route string `json:"route,omitempty"`
}

// providerPrefs controls how OpenRouter routes to upstream providers.
type providerPrefs struct {
	// Order lists preferred upstream providers by name (e.g. "Anthropic", "Together").
	Order []string `json:"order,omitempty"`
	// AllowFallbacks permits OpenRouter to use other providers if preferred ones
	// are unavailable.
	AllowFallbacks *bool `json:"allow_fallbacks,omitempty"`
	// RequireParameters rejects providers that don't support all request params.
	RequireParameters bool `json:"require_parameters,omitempty"`
	// DataCollection: "deny" opts out of training data use; "allow" is default.
	DataCollection string `json:"data_collection,omitempty"`
	// IgnoreProviders lists providers to explicitly skip.
	IgnoreProviders []string `json:"ignore,omitempty"`
	// QuantizationLevel: "int4", "int8", "fp8", "fp16", "bf16", "unknown".
	QuantizationLevel string `json:"quantization,omitempty"`
}

type orMsg struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"` // string or []orContentPart
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []orToolCall `json:"tool_calls,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type orContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *orImageURL     `json:"image_url,omitempty"`
}

type orImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type orTool struct {
	Type     string       `json:"type"`
	Function orToolFunc   `json:"function"`
}

type orToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type orToolCall struct {
	Index    int         `json:"index"`
	ID       string      `json:"id,omitempty"`
	Type     string      `json:"type,omitempty"`
	Function orFuncCall  `json:"function"`
}

type orFuncCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// SSE chunk shapes
type orChunk struct {
	ID      string      `json:"id"`
	Model   string      `json:"model"`
	Choices []orChoice  `json:"choices"`
	Usage   *orUsage    `json:"usage,omitempty"`
	// OpenRouter-specific: cost in USD for this request
	GenerationID string  `json:"generation_id,omitempty"`
}

type orChoice struct {
	Index        int      `json:"index"`
	Delta        orDelta  `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

type orDelta struct {
	Role      string       `json:"role,omitempty"`
	Content   *string      `json:"content"`
	ToolCalls []orToolCall `json:"tool_calls,omitempty"`
}

type orUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ── Stream ────────────────────────────────────────────────────────────────────

func (p *Provider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.Event, error) {
	body, err := p.buildRequest(req)
	if err != nil {
		return nil, err
	}

	headers := p.headers()
	rc, err := httpclient.PostJSONStream(ctx, baseURL, headers, body)
	if err != nil {
		return nil, fmt.Errorf("openrouter: %w", err)
	}

	ch := make(chan provider.Event, 64)
	go func() {
		defer close(ch)
		defer rc.Close()
		p.parseStream(ctx, rc, ch)
	}()
	return ch, nil
}

// parseStream reads the SSE body and emits provider.Event values.
func (p *Provider) parseStream(ctx context.Context, r io.Reader, ch chan<- provider.Event) {
	// Per-tool-call accumulator: index → partial state
	type toolState struct {
		id   string
		name string
		sent bool // EventToolCallStart emitted
		done bool // EventToolCallDone emitted
	}
	tools := map[int]*toolState{}

	var usage model.Usage
	var stopReason model.StopReason

	parser := sse.NewParser(r)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		ev, err := parser.Next()
		if err != nil {
			// Stream closed without [DONE] — emit a clean stop so the agent
			// doesn't hang waiting for EventMessageStop.
			ch <- provider.Event{
				Type:       provider.EventMessageStop,
				StopReason: stopReason,
				Usage:      usage,
			}
			return
		}
		if ev.Data == "[DONE]" {
			// Flush any tool calls not yet closed by a finish_reason chunk.
			for idx, ts := range tools {
				if ts.sent && !ts.done {
					ts.done = true
					ch <- provider.Event{
						Type:      provider.EventToolCallDone,
						ToolIndex: idx,
					}
				}
			}
			ch <- provider.Event{
				Type:       provider.EventMessageStop,
				StopReason: stopReason,
				Usage:      usage,
			}
			return
		}
		if len(ev.Data) == 0 {
			continue
		}

		var chunk orChunk
		if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
			continue
		}

		// Capture usage when OpenRouter sends it (often in the final chunk).
		if chunk.Usage != nil {
			usage.InputTokens = chunk.Usage.PromptTokens
			usage.OutputTokens = chunk.Usage.CompletionTokens
		}

		for _, choice := range chunk.Choices {
			d := choice.Delta

			// Text delta
			if d.Content != nil && *d.Content != "" {
				ch <- provider.Event{Type: provider.EventTextDelta, Text: *d.Content}
			}

			// Tool-call deltas
			for _, tc := range d.ToolCalls {
				ts, exists := tools[tc.Index]
				if !exists {
					ts = &toolState{}
					tools[tc.Index] = ts
				}
				if tc.ID != "" {
					ts.id = tc.ID
				}
				if tc.Function.Name != "" {
					ts.name = tc.Function.Name
				}
				// Emit start once we have both id and name.
				if !ts.sent && ts.id != "" && ts.name != "" {
					ts.sent = true
					ch <- provider.Event{
						Type:      provider.EventToolCallStart,
						ToolIndex: tc.Index,
						ToolID:    ts.id,
						ToolName:  ts.name,
					}
				}
				if tc.Function.Arguments != "" {
					ch <- provider.Event{
						Type:        provider.EventToolCallDelta,
						ToolIndex:   tc.Index,
						PartialJSON: tc.Function.Arguments,
					}
				}
			}

			// Finish reason — close open tool calls when stop is tool_calls.
			if choice.FinishReason != nil {
				stopReason = mapStopReason(*choice.FinishReason)
				if stopReason == model.StopReasonToolUse {
					for idx, ts := range tools {
						if ts.sent && !ts.done {
							ts.done = true
							ch <- provider.Event{
								Type:      provider.EventToolCallDone,
								ToolIndex: idx,
							}
						}
					}
				}
			}
		}
	}
}

// ── Request builder ───────────────────────────────────────────────────────────

func (p *Provider) buildRequest(req *provider.Request) (*orRequest, error) {
	msgs, err := convertMessages(req.Messages)
	if err != nil {
		return nil, err
	}

	// Prepend system prompt as a system message if provided.
	if req.System != "" {
		msgs = append([]orMsg{{Role: "system", Content: req.System}}, msgs...)
	}

	out := &orRequest{
		Model:     strings.TrimPrefix(req.Model, "openrouter/"),
		Messages:  msgs,
		MaxTokens: req.MaxTokens,
		Temperature: req.Temperature,
		Stop:      req.StopSequences,
		Stream:    true,
	}

	if len(req.Tools) > 0 {
		out.Tools = convertTools(req.Tools)
	}

	// OpenRouter extensions from Request.Extra
	if req.Extra != nil {
		if pp, ok := req.Extra["provider"]; ok {
			b, _ := json.Marshal(pp)
			var prefs providerPrefs
			if json.Unmarshal(b, &prefs) == nil {
				out.Provider = &prefs
			}
		}
		if models, ok := req.Extra["models"]; ok {
			if ms, ok := models.([]string); ok {
				out.Models = ms
				out.Route = "fallback"
			}
		}
		if route, ok := req.Extra["route"].(string); ok {
			out.Route = route
		}
	}

	return out, nil
}

func (p *Provider) headers() map[string]string {
	h := map[string]string{
		"Authorization": "Bearer " + p.apiKey,
	}
	if p.siteURL != "" {
		h["HTTP-Referer"] = p.siteURL
	}
	if p.appName != "" {
		h["X-Title"] = p.appName
	}
	return h
}

// ── Message conversion ────────────────────────────────────────────────────────

func convertMessages(msgs []model.Message) ([]orMsg, error) {
	out := make([]orMsg, 0, len(msgs))
	for _, m := range msgs {
		converted, err := convertMessage(m)
		if err != nil {
			return nil, err
		}
		out = append(out, converted...)
	}
	return out, nil
}

func convertMessage(m model.Message) ([]orMsg, error) {
	switch m.Role {
	case model.RoleUser:
		return convertUserMessage(m)
	case model.RoleAssistant:
		return convertAssistantMessage(m)
	case model.RoleTool:
		return convertToolResultMessages(m)
	case model.RoleSystem:
		return []orMsg{{Role: "system", Content: m.Text()}}, nil
	default:
		return []orMsg{{Role: string(m.Role), Content: m.Text()}}, nil
	}
}

func convertUserMessage(m model.Message) ([]orMsg, error) {
	// Tool results from the agent loop arrive as RoleUser with ContentTypeToolResult
	// blocks. OpenAI-compat APIs require these as separate role=tool messages.
	hasToolResults := false
	for _, b := range m.Content {
		if b.Type == model.ContentTypeToolResult {
			hasToolResults = true
			break
		}
	}
	if hasToolResults {
		var out []orMsg
		for _, b := range m.Content {
			if b.Type == model.ContentTypeToolResult {
				content := b.Text
				if content == "" {
					for _, c := range b.Content {
						if c.Type == model.ContentTypeText {
							content += c.Text
						}
					}
				}
				out = append(out, orMsg{
					Role:       "tool",
					ToolCallID: b.ToolUseID,
					Content:    content,
				})
			}
		}
		// Include any accompanying plain text as a user follow-up.
		if text := m.Text(); text != "" {
			out = append(out, orMsg{Role: "user", Content: text})
		}
		return out, nil
	}

	// If all content is text, send as a plain string (simpler, wider compat).
	allText := true
	for _, b := range m.Content {
		if b.Type != model.ContentTypeText {
			allText = false
			break
		}
	}
	if allText {
		return []orMsg{{Role: "user", Content: m.Text()}}, nil
	}

	// Mixed content (text + images) → content-parts array.
	parts := make([]orContentPart, 0, len(m.Content))
	for _, b := range m.Content {
		switch b.Type {
		case model.ContentTypeText:
			parts = append(parts, orContentPart{Type: "text", Text: b.Text})
		case model.ContentTypeImage:
			if b.Source != nil {
				var url string
				if b.Source.Type == "base64" {
					url = "data:" + b.Source.MediaType + ";base64," + b.Source.Data
				} else {
					url = b.Source.URL
				}
				parts = append(parts, orContentPart{
					Type:     "image_url",
					ImageURL: &orImageURL{URL: url},
				})
			}
		}
	}
	return []orMsg{{Role: "user", Content: parts}}, nil
}

func convertAssistantMessage(m model.Message) ([]orMsg, error) {
	var text strings.Builder
	var toolCalls []orToolCall

	for _, b := range m.Content {
		switch b.Type {
		case model.ContentTypeText:
			text.WriteString(b.Text)
		case model.ContentTypeToolUse:
			input := string(b.Input)
			if input == "" {
				input = "{}"
			}
			toolCalls = append(toolCalls, orToolCall{
				ID:   b.ID,
				Type: "function",
				Function: orFuncCall{
					Name:      b.Name,
					Arguments: input,
				},
			})
		}
	}

	msg := orMsg{Role: "assistant"}
	t := text.String()
	if t != "" {
		msg.Content = t
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	return []orMsg{msg}, nil
}

// convertToolResultMessages turns tool_result blocks into OpenAI tool messages
// (one per tool call result).
func convertToolResultMessages(m model.Message) ([]orMsg, error) {
	var out []orMsg
	for _, b := range m.Content {
		if b.Type != model.ContentTypeToolResult {
			continue
		}
		content := b.Text
		if content == "" {
			for _, c := range b.Content {
				if c.Type == model.ContentTypeText {
					content += c.Text
				}
			}
		}
		out = append(out, orMsg{
			Role:       "tool",
			ToolCallID: b.ToolUseID,
			Content:    content,
		})
	}
	return out, nil
}

func convertTools(tools []model.ToolDefinition) []orTool {
	out := make([]orTool, 0, len(tools))
	for _, t := range tools {
		params := t.InputSchema
		if params == nil {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, orTool{
			Type: "function",
			Function: orToolFunc{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return out
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func mapStopReason(r string) model.StopReason {
	switch r {
	case "stop":
		return model.StopReasonEndTurn
	case "tool_calls":
		return model.StopReasonToolUse
	case "length":
		return model.StopReasonMaxTokens
	case "content_filter":
		return model.StopReasonStopSeq
	default:
		return model.StopReasonEndTurn
	}
}

// Models returns a curated list of popular OpenRouter model IDs for use in
// the model registry.
func Models() []string {
	return popularModels
}

// popularModels is a curated list of the most widely-used OpenRouter models.
// Full list: https://openrouter.ai/models
var popularModels = []string{
	// Anthropic (via OpenRouter)
	"anthropic/claude-opus-4-7",
	"anthropic/claude-sonnet-4-6",
	"anthropic/claude-haiku-4-5",
	// OpenAI (via OpenRouter)
	"openai/gpt-4o",
	"openai/gpt-4o-mini",
	"openai/o3",
	"openai/o4-mini",
	// Google (via OpenRouter)
	"google/gemini-2.0-flash-001",
	"google/gemini-1.5-pro",
	"google/gemini-1.5-flash",
	// Meta (via OpenRouter)
	"meta-llama/llama-3.3-70b-instruct",
	"meta-llama/llama-3.1-405b-instruct",
	"meta-llama/llama-3.1-8b-instruct",
	// Mistral (via OpenRouter)
	"mistralai/mistral-large-2411",
	"mistralai/mistral-small-3.1-24b-instruct",
	"mistralai/codestral-2501",
	// DeepSeek (via OpenRouter)
	"deepseek/deepseek-chat-v3-0324",
	"deepseek/deepseek-r1",
	"deepseek/deepseek-r1-distill-llama-70b",
	// Qwen (via OpenRouter)
	"qwen/qwen-2.5-72b-instruct",
	"qwen/qwen-2.5-coder-32b-instruct",
	"qwen/qwq-32b",
	// xAI (via OpenRouter)
	"x-ai/grok-3-beta",
	"x-ai/grok-3-mini-beta",
	// Cohere (via OpenRouter)
	"cohere/command-r-plus-08-2024",
	"cohere/command-r7b-12-2024",
	// Amazon (via OpenRouter)
	"amazon/nova-pro-v1",
	"amazon/nova-lite-v1",
	// Microsoft (via OpenRouter)
	"microsoft/phi-4",
	"microsoft/phi-4-multimodal-instruct",
	// NovaSky (via OpenRouter)
	"novasky-ai/sky-t1-32b-preview",
}
