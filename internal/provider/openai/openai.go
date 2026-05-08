// Package openai implements the OpenAI Chat Completions API provider.
package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tiru-r/pi-agent-go/internal/httpclient"
	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/provider"
	"github.com/tiru-r/pi-agent-go/internal/sse"
)

const defaultBaseURL = "https://api.openai.com/v1/chat/completions"

// Provider implements provider.Provider for the OpenAI Chat Completions API.
type Provider struct {
	apiKey  string
	baseURL string
	name    string
}

// New creates a new OpenAI provider.
func New(apiKey string) *Provider {
	return &Provider{
		apiKey:  apiKey,
		baseURL: defaultBaseURL,
		name:    "openai",
	}
}

// NewWithBaseURL creates a provider with a custom base URL (Azure, proxies, etc.).
func NewWithBaseURL(apiKey, baseURL, providerName string) *Provider {
	return &Provider{
		apiKey:  apiKey,
		baseURL: baseURL,
		name:    providerName,
	}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return p.name }

// ─── Wire types ─────────────────────────────────────────────────────────────

type openAIRequest struct {
	Model     string          `json:"model"`
	Messages  []openAIMessage `json:"messages"`
	Tools     []openAITool    `json:"tools,omitempty"`
	MaxTokens int             `json:"max_tokens,omitempty"`
	Stream    bool            `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role       string             `json:"role"`
	Content    interface{}        `json:"content"`           // string or []openAIContentPart
	ToolCalls  []openAIToolCall   `json:"tool_calls,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
	Name       string             `json:"name,omitempty"`
}

type openAIContentPart struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	ImageURL *openAIImageURL   `json:"image_url,omitempty"`
}

type openAIImageURL struct {
	URL string `json:"url"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string           `json:"type"`
	Function openAIToolFunc   `json:"function"`
}

type openAIToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ─── SSE event shapes ────────────────────────────────────────────────────────

type chunkDelta struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	ToolCalls []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type streamChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Delta        chunkDelta `json:"delta"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// ─── Request mapping ─────────────────────────────────────────────────────────

func mapMessages(msgs []model.Message, system string) []openAIMessage {
	var out []openAIMessage

	// Prepend explicit system if set.
	if system != "" {
		out = append(out, openAIMessage{Role: "system", Content: system})
	}

	for _, m := range msgs {
		switch m.Role {
		case model.RoleSystem:
			out = append(out, openAIMessage{Role: "system", Content: m.Text()})

		case model.RoleUser:
			// May include images.
			if hasImages(m) {
				parts := buildUserParts(m)
				out = append(out, openAIMessage{Role: "user", Content: parts})
			} else {
				// Also handle tool_result blocks.
				for _, cb := range m.Content {
					if cb.Type == model.ContentTypeToolResult {
						out = append(out, openAIMessage{
							Role:       "tool",
							Content:    extractToolResultText(cb),
							ToolCallID: cb.ToolUseID,
						})
					}
				}
				text := m.Text()
				if text != "" {
					out = append(out, openAIMessage{Role: "user", Content: text})
				}
			}

		case model.RoleAssistant:
			am := openAIMessage{Role: "assistant"}
			text := m.Text()
			if text != "" {
				am.Content = text
			}
			for _, cb := range m.Content {
				if cb.Type == model.ContentTypeToolUse {
					tc := openAIToolCall{
						ID:   cb.ID,
						Type: "function",
					}
					tc.Function.Name = cb.Name
					if cb.Input != nil {
						tc.Function.Arguments = string(cb.Input)
					} else {
						tc.Function.Arguments = "{}"
					}
					am.ToolCalls = append(am.ToolCalls, tc)
				}
			}
			out = append(out, am)

		case model.RoleTool:
			for _, cb := range m.Content {
				if cb.Type == model.ContentTypeToolResult {
					out = append(out, openAIMessage{
						Role:       "tool",
						Content:    extractToolResultText(cb),
						ToolCallID: cb.ToolUseID,
					})
				}
			}
		}
	}
	return out
}

func hasImages(m model.Message) bool {
	for _, cb := range m.Content {
		if cb.Type == model.ContentTypeImage {
			return true
		}
	}
	return false
}

func buildUserParts(m model.Message) []openAIContentPart {
	var parts []openAIContentPart
	for _, cb := range m.Content {
		switch cb.Type {
		case model.ContentTypeText:
			parts = append(parts, openAIContentPart{Type: "text", Text: cb.Text})
		case model.ContentTypeImage:
			if cb.Source != nil {
				var url string
				if cb.Source.Type == "url" {
					url = cb.Source.URL
				} else {
					url = fmt.Sprintf("data:%s;base64,%s", cb.Source.MediaType, cb.Source.Data)
				}
				parts = append(parts, openAIContentPart{
					Type:     "image_url",
					ImageURL: &openAIImageURL{URL: url},
				})
			}
		}
	}
	return parts
}

func extractToolResultText(cb model.ContentBlock) string {
	var parts []string
	for _, c := range cb.Content {
		if c.Type == model.ContentTypeText {
			parts = append(parts, c.Text)
		}
	}
	if len(parts) == 0 {
		return cb.Text
	}
	return strings.Join(parts, "\n")
}

func mapTools(tools []model.ToolDefinition) []openAITool {
	out := make([]openAITool, len(tools))
	for i, t := range tools {
		params := t.InputSchema
		if params == nil {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out[i] = openAITool{
			Type: "function",
			Function: openAIToolFunc{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		}
	}
	return out
}

func mapStopReason(r string) model.StopReason {
	switch r {
	case "stop":
		return model.StopReasonEndTurn
	case "tool_calls":
		return model.StopReasonToolUse
	case "length":
		return model.StopReasonMaxTokens
	default:
		return model.StopReasonEndTurn
	}
}

// ─── Headers ─────────────────────────────────────────────────────────────────

func (p *Provider) authHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + p.apiKey,
	}
}

// ─── Streaming ───────────────────────────────────────────────────────────────

// Stream sends a streaming request and returns a channel of events.
func (p *Provider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.Event, error) {
	body := openAIRequest{
		Model:    req.Model,
		Messages: mapMessages(req.Messages, req.System),
		MaxTokens: req.MaxTokens,
		Stream:   true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	if len(req.Tools) > 0 {
		body.Tools = mapTools(req.Tools)
	}

	ch := make(chan provider.Event, 64)

	go func() {
		defer close(ch)

		rc, err := httpclient.PostJSONStream(ctx, p.baseURL, p.authHeaders(), body)
		if err != nil {
			ch <- provider.Event{Type: provider.EventError, Err: err}
			return
		}
		defer rc.Close()

		done := make(chan struct{})
		defer close(done)

		// Track tool calls by index: index → {id, name, argsBuilder}
		type toolState struct {
			id   string
			name string
		}
		toolStates := map[int]toolState{}

		var finishReason string
		var promptTokens, completionTokens int

		sseCh := sse.Chan(rc, done)
		for ev := range sseCh {
			if ev.Data == "[DONE]" {
				break
			}
			if ev.Data == "" {
				continue
			}

			var chunk streamChunk
			if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
				continue
			}

			if chunk.Usage != nil {
				promptTokens = chunk.Usage.PromptTokens
				completionTokens = chunk.Usage.CompletionTokens
			}

			for _, choice := range chunk.Choices {
				if choice.FinishReason != "" {
					finishReason = choice.FinishReason
				}

				delta := choice.Delta
				if delta.Content != "" {
					ch <- provider.Event{Type: provider.EventTextDelta, Text: delta.Content}
				}

				for _, tc := range delta.ToolCalls {
					idx := tc.Index
					st, seen := toolStates[idx]
					if !seen {
						st = toolState{id: tc.ID, name: tc.Function.Name}
						toolStates[idx] = st
						ch <- provider.Event{
							Type:      provider.EventToolCallStart,
							ToolID:    tc.ID,
							ToolName:  tc.Function.Name,
							ToolIndex: idx,
						}
					}
					// Update id/name if they arrive later.
					if tc.ID != "" && st.id == "" {
						st.id = tc.ID
						toolStates[idx] = st
					}
					if tc.Function.Name != "" && st.name == "" {
						st.name = tc.Function.Name
						toolStates[idx] = st
					}
					if tc.Function.Arguments != "" {
						ch <- provider.Event{
							Type:        provider.EventToolCallDelta,
							ToolIndex:   idx,
							PartialJSON: tc.Function.Arguments,
						}
					}
				}
			}
		}

		// Emit tool done events.
		for idx := range toolStates {
			ch <- provider.Event{Type: provider.EventToolCallDone, ToolIndex: idx}
		}

		ch <- provider.Event{
			Type:       provider.EventMessageStop,
			StopReason: mapStopReason(finishReason),
			Usage: model.Usage{
				InputTokens:  promptTokens,
				OutputTokens: completionTokens,
			},
		}
	}()

	return ch, nil
}
