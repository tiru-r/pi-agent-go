// Package cohere implements the Cohere Chat API v2 provider.
package cohere

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

const baseURL = "https://api.cohere.com/v2/chat"

// Provider implements provider.Provider for the Cohere Chat API.
type Provider struct {
	apiKey string
}

// New creates a new Cohere provider.
func New(apiKey string) *Provider {
	return &Provider{apiKey: apiKey}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "cohere" }

// ─── Wire types ─────────────────────────────────────────────────────────────

type cohereRequest struct {
	Model    string          `json:"model"`
	Messages []cohereMessage `json:"messages"`
	Tools    []cohereTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
}

type cohereMessage struct {
	Role      string              `json:"role"`
	Content   interface{}         `json:"content"` // string or []cohereContentPart
	ToolCalls []cohereToolCall    `json:"tool_calls,omitempty"`
	ToolPlan  string              `json:"tool_plan,omitempty"`
}

type cohereContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type cohereToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type cohereTool struct {
	Type     string          `json:"type"`
	Function cohereToolFunc  `json:"function"`
}

type cohereToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ─── SSE event shapes ────────────────────────────────────────────────────────

type cohereSSEEvent struct {
	Type  string          `json:"type"`
	Index int             `json:"index"`
	Delta *cohereDelta    `json:"delta,omitempty"`
}

type cohereDelta struct {
	Message *cohereDeltaMsg   `json:"message,omitempty"`
	FinishReason string        `json:"finish_reason,omitempty"`
	Usage   *cohereUsage      `json:"usage,omitempty"`
}

type cohereDeltaMsg struct {
	Content   *cohereDeltaContent  `json:"content,omitempty"`
	ToolCalls *cohereToolCallChunk `json:"tool_calls,omitempty"`
}

type cohereDeltaContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type cohereToolCallChunk struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type cohereUsage struct {
	BilledUnits struct {
		InputTokens  float64 `json:"input_tokens"`
		OutputTokens float64 `json:"output_tokens"`
	} `json:"billed_units"`
	Tokens struct {
		InputTokens  float64 `json:"input_tokens"`
		OutputTokens float64 `json:"output_tokens"`
	} `json:"tokens"`
}

// ─── Request mapping ─────────────────────────────────────────────────────────

func mapMessages(msgs []model.Message, system string) []cohereMessage {
	var out []cohereMessage

	// Prepend system message.
	var sysTexts []string
	if system != "" {
		sysTexts = append(sysTexts, system)
	}
	for _, m := range msgs {
		if m.Role == model.RoleSystem {
			sysTexts = append(sysTexts, m.Text())
		}
	}
	if len(sysTexts) > 0 {
		out = append(out, cohereMessage{
			Role:    "system",
			Content: strings.Join(sysTexts, "\n"),
		})
	}

	for _, m := range msgs {
		switch m.Role {
		case model.RoleSystem:
			// Already handled above.
			continue

		case model.RoleUser:
			// Check for tool results.
			for _, cb := range m.Content {
				if cb.Type == model.ContentTypeToolResult {
					txt := extractToolResultText(cb)
					out = append(out, cohereMessage{
						Role:    "tool",
						Content: []cohereContentPart{{Type: "text", Text: txt}},
						ToolCalls: []cohereToolCall{{
							ID:   cb.ToolUseID,
							Type: "function",
						}},
					})
				}
			}
			text := m.Text()
			if text != "" {
				out = append(out, cohereMessage{Role: "user", Content: text})
			}

		case model.RoleAssistant:
			am := cohereMessage{Role: "assistant"}
			text := m.Text()
			if text != "" {
				am.Content = text
			}
			for _, cb := range m.Content {
				if cb.Type == model.ContentTypeToolUse {
					tc := cohereToolCall{ID: cb.ID, Type: "function"}
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
					txt := extractToolResultText(cb)
					out = append(out, cohereMessage{
						Role:    "tool",
						Content: []cohereContentPart{{Type: "text", Text: txt}},
						ToolCalls: []cohereToolCall{{
							ID:   cb.ToolUseID,
							Type: "function",
						}},
					})
				}
			}
		}
	}
	return out
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

func mapTools(tools []model.ToolDefinition) []cohereTool {
	out := make([]cohereTool, len(tools))
	for i, t := range tools {
		params := t.InputSchema
		if params == nil {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out[i] = cohereTool{
			Type: "function",
			Function: cohereToolFunc{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		}
	}
	return out
}

func mapStopReason(r string) model.StopReason {
	switch strings.ToUpper(r) {
	case "COMPLETE":
		return model.StopReasonEndTurn
	case "TOOL_CALL", "MAX_TOKENS":
		if strings.ToUpper(r) == "MAX_TOKENS" {
			return model.StopReasonMaxTokens
		}
		return model.StopReasonToolUse
	default:
		return model.StopReasonEndTurn
	}
}

// ─── Streaming ───────────────────────────────────────────────────────────────

// Stream sends a streaming request and returns a channel of events.
func (p *Provider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.Event, error) {
	body := cohereRequest{
		Model:    req.Model,
		Messages: mapMessages(req.Messages, req.System),
		Stream:   true,
	}
	if len(req.Tools) > 0 {
		body.Tools = mapTools(req.Tools)
	}

	headers := map[string]string{
		"Authorization": "Bearer " + p.apiKey,
	}

	ch := make(chan provider.Event, 64)

	go func() {
		defer close(ch)

		rc, err := httpclient.PostJSONStream(ctx, baseURL, headers, body)
		if err != nil {
			ch <- provider.Event{Type: provider.EventError, Err: err}
			return
		}
		defer rc.Close()

		done := make(chan struct{})
		defer close(done)

		var finishReason string
		var inputTokens, outputTokens int

		// Track tool call states by index.
		type toolState struct {
			id   string
			name string
		}
		toolStates := map[int]toolState{}

		sseCh := sse.Chan(rc, done)
		for ev := range sseCh {
			if ev.Data == "" {
				continue
			}

			var e cohereSSEEvent
			if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
				continue
			}

			switch e.Type {
			case "message-start":
				// id arrived; nothing to emit yet.

			case "content-start":
				// content block opened.

			case "content-delta":
				if e.Delta != nil && e.Delta.Message != nil {
					if e.Delta.Message.Content != nil && e.Delta.Message.Content.Text != "" {
						ch <- provider.Event{
							Type: provider.EventTextDelta,
							Text: e.Delta.Message.Content.Text,
						}
					}
				}

			case "content-end":
				// content block closed.

			case "tool-call-start":
				if e.Delta != nil && e.Delta.Message != nil && e.Delta.Message.ToolCalls != nil {
					tc := e.Delta.Message.ToolCalls
					st := toolState{id: tc.ID, name: tc.Function.Name}
					toolStates[e.Index] = st
					ch <- provider.Event{
						Type:      provider.EventToolCallStart,
						ToolID:    tc.ID,
						ToolName:  tc.Function.Name,
						ToolIndex: e.Index,
					}
				}

			case "tool-call-delta":
				if e.Delta != nil && e.Delta.Message != nil && e.Delta.Message.ToolCalls != nil {
					args := e.Delta.Message.ToolCalls.Function.Arguments
					if args != "" {
						ch <- provider.Event{
							Type:        provider.EventToolCallDelta,
							ToolIndex:   e.Index,
							PartialJSON: args,
						}
					}
				}

			case "tool-call-end":
				ch <- provider.Event{
					Type:      provider.EventToolCallDone,
					ToolIndex: e.Index,
				}
				delete(toolStates, e.Index)

			case "message-end":
				if e.Delta != nil {
					finishReason = e.Delta.FinishReason
					if e.Delta.Usage != nil {
						inputTokens = int(e.Delta.Usage.Tokens.InputTokens)
						outputTokens = int(e.Delta.Usage.Tokens.OutputTokens)
						if inputTokens == 0 {
							inputTokens = int(e.Delta.Usage.BilledUnits.InputTokens)
							outputTokens = int(e.Delta.Usage.BilledUnits.OutputTokens)
						}
					}
				}
			}
		}

		// Close any unclosed tool calls.
		for idx := range toolStates {
			ch <- provider.Event{Type: provider.EventToolCallDone, ToolIndex: idx}
		}

		ch <- provider.Event{
			Type:       provider.EventMessageStop,
			StopReason: mapStopReason(finishReason),
			Usage: model.Usage{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
		}

		_ = fmt.Sprintf // keep fmt import used
	}()

	return ch, nil
}
