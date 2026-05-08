// Package azure implements the Azure OpenAI provider.
// It reuses the OpenAI-compatible SSE format with Azure-specific URL and auth.
package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pi-agent/pi/internal/httpclient"
	"github.com/pi-agent/pi/internal/model"
	"github.com/pi-agent/pi/internal/provider"
	"github.com/pi-agent/pi/internal/sse"
)

const defaultAPIVersion = "2024-08-01-preview"

// Provider implements provider.Provider for Azure OpenAI.
type Provider struct {
	apiKey     string
	endpoint   string
	deployment string
	apiVersion string
}

// New creates a new Azure OpenAI provider.
func New(apiKey, endpoint, deployment, apiVersion string) *Provider {
	if apiVersion == "" {
		apiVersion = defaultAPIVersion
	}
	return &Provider{
		apiKey:     apiKey,
		endpoint:   strings.TrimRight(endpoint, "/"),
		deployment: deployment,
		apiVersion: apiVersion,
	}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "azure" }

func (p *Provider) baseURL(deploymentOverride string) string {
	dep := p.deployment
	if deploymentOverride != "" && deploymentOverride != dep {
		dep = deploymentOverride
	}
	// Strip "azure/" prefix from model ID if used as deployment.
	dep = strings.TrimPrefix(dep, "azure/")
	return fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s",
		p.endpoint, dep, p.apiVersion)
}

func (p *Provider) authHeaders() map[string]string {
	return map[string]string{
		"api-key": p.apiKey,
	}
}

// ─── Reuse OpenAI wire types ──────────────────────────────────────────────────

type azureRequest struct {
	Messages  []azureMessage `json:"messages"`
	Tools     []azureTool    `json:"tools,omitempty"`
	MaxTokens int            `json:"max_tokens,omitempty"`
	Stream    bool           `json:"stream"`
	StreamOptions *streamOpts `json:"stream_options,omitempty"`
}

type streamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type azureMessage struct {
	Role       string           `json:"role"`
	Content    interface{}      `json:"content"`
	ToolCalls  []azureToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type azureToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type azureTool struct {
	Type     string         `json:"type"`
	Function azureToolFunc  `json:"function"`
}

type azureToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type azureContentPart struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	ImageURL *azureImageURL `json:"image_url,omitempty"`
}

type azureImageURL struct {
	URL string `json:"url"`
}

// ─── Request mapping ─────────────────────────────────────────────────────────

func mapMessages(msgs []model.Message, system string) []azureMessage {
	var out []azureMessage
	if system != "" {
		out = append(out, azureMessage{Role: "system", Content: system})
	}

	for _, m := range msgs {
		switch m.Role {
		case model.RoleSystem:
			out = append(out, azureMessage{Role: "system", Content: m.Text()})

		case model.RoleUser:
			if hasImages(m) {
				parts := buildUserParts(m)
				out = append(out, azureMessage{Role: "user", Content: parts})
			} else {
				for _, cb := range m.Content {
					if cb.Type == model.ContentTypeToolResult {
						out = append(out, azureMessage{
							Role:       "tool",
							Content:    extractToolResultText(cb),
							ToolCallID: cb.ToolUseID,
						})
					}
				}
				text := m.Text()
				if text != "" {
					out = append(out, azureMessage{Role: "user", Content: text})
				}
			}

		case model.RoleAssistant:
			am := azureMessage{Role: "assistant"}
			text := m.Text()
			if text != "" {
				am.Content = text
			}
			for _, cb := range m.Content {
				if cb.Type == model.ContentTypeToolUse {
					tc := azureToolCall{ID: cb.ID, Type: "function"}
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
					out = append(out, azureMessage{
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

func buildUserParts(m model.Message) []azureContentPart {
	var parts []azureContentPart
	for _, cb := range m.Content {
		switch cb.Type {
		case model.ContentTypeText:
			parts = append(parts, azureContentPart{Type: "text", Text: cb.Text})
		case model.ContentTypeImage:
			if cb.Source != nil {
				var url string
				if cb.Source.Type == "url" {
					url = cb.Source.URL
				} else {
					url = fmt.Sprintf("data:%s;base64,%s", cb.Source.MediaType, cb.Source.Data)
				}
				parts = append(parts, azureContentPart{
					Type:     "image_url",
					ImageURL: &azureImageURL{URL: url},
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

func mapTools(tools []model.ToolDefinition) []azureTool {
	out := make([]azureTool, len(tools))
	for i, t := range tools {
		params := t.InputSchema
		if params == nil {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out[i] = azureTool{
			Type: "function",
			Function: azureToolFunc{
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

// ─── SSE event shapes ────────────────────────────────────────────────────────

type streamChunk struct {
	Choices []struct {
		Delta struct {
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
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// ─── Streaming ───────────────────────────────────────────────────────────────

// Stream sends a streaming request and returns a channel of events.
func (p *Provider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.Event, error) {
	// Use model as deployment name if not explicitly set.
	dep := p.deployment
	if dep == "" {
		dep = req.Model
	}
	url := p.baseURL(dep)

	body := azureRequest{
		Messages:  mapMessages(req.Messages, req.System),
		MaxTokens: req.MaxTokens,
		Stream:    true,
		StreamOptions: &streamOpts{IncludeUsage: true},
	}
	if len(req.Tools) > 0 {
		body.Tools = mapTools(req.Tools)
	}

	ch := make(chan provider.Event, 64)

	go func() {
		defer close(ch)

		rc, err := httpclient.PostJSONStream(ctx, url, p.authHeaders(), body)
		if err != nil {
			ch <- provider.Event{Type: provider.EventError, Err: err}
			return
		}
		defer rc.Close()

		done := make(chan struct{})
		defer close(done)

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
				if choice.Delta.Content != "" {
					ch <- provider.Event{Type: provider.EventTextDelta, Text: choice.Delta.Content}
				}
				for _, tc := range choice.Delta.ToolCalls {
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
