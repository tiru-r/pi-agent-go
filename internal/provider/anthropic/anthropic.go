// Package anthropic implements the Anthropic Messages API provider.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/pi-agent/pi/internal/httpclient"
	"github.com/pi-agent/pi/internal/model"
	"github.com/pi-agent/pi/internal/provider"
	"github.com/pi-agent/pi/internal/sse"
)

const (
	baseURL    = "https://api.anthropic.com/v1/messages"
	apiVersion = "2023-06-01"
)

// Provider implements provider.Provider for the Anthropic Messages API.
type Provider struct {
	apiKey string
}

// New creates a new Anthropic provider.
func New(apiKey string) *Provider {
	return &Provider{apiKey: apiKey}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "anthropic" }

// ─── Wire types ─────────────────────────────────────────────────────────────

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
	System    string             `json:"system,omitempty"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Thinking  *thinkingConfig    `json:"thinking,omitempty"`
	Stream    bool               `json:"stream"`
}

type thinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicMessage struct {
	Role    string              `json:"role"`
	Content []anthropicContent  `json:"content"`
}

type anthropicContent struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// image
	Source *anthropicImageSource `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string             `json:"tool_use_id,omitempty"`
	Content   []anthropicContent `json:"content,omitempty"`
	IsError   bool               `json:"is_error,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ─── SSE event shapes ────────────────────────────────────────────────────────

type messageStartEvent struct {
	Type    string `json:"type"`
	Message struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type contentBlockStartEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
}

type contentBlockDeltaEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
	} `json:"delta"`
}

type messageDeltaEvent struct {
	Type  string `json:"type"`
	Delta struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// ─── Request mapping ─────────────────────────────────────────────────────────

func mapMessages(msgs []model.Message) []anthropicMessage {
	out := make([]anthropicMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == model.RoleSystem {
			// System is sent as top-level field; skip in messages array.
			continue
		}
		am := anthropicMessage{Role: string(m.Role)}
		for _, cb := range m.Content {
			am.Content = append(am.Content, mapContentBlock(cb))
		}
		out = append(out, am)
	}
	return out
}

func mapContentBlock(cb model.ContentBlock) anthropicContent {
	switch cb.Type {
	case model.ContentTypeText:
		return anthropicContent{Type: "text", Text: cb.Text}
	case model.ContentTypeImage:
		ac := anthropicContent{Type: "image"}
		if cb.Source != nil {
			ac.Source = &anthropicImageSource{
				Type:      cb.Source.Type,
				MediaType: cb.Source.MediaType,
				Data:      cb.Source.Data,
				URL:       cb.Source.URL,
			}
		}
		return ac
	case model.ContentTypeToolUse:
		input := cb.Input
		if input == nil {
			input = json.RawMessage("{}")
		}
		return anthropicContent{
			Type:  "tool_use",
			ID:    cb.ID,
			Name:  cb.Name,
			Input: input,
		}
	case model.ContentTypeToolResult:
		ac := anthropicContent{
			Type:      "tool_result",
			ToolUseID: cb.ToolUseID,
			IsError:   cb.IsError,
		}
		for _, c := range cb.Content {
			ac.Content = append(ac.Content, mapContentBlock(c))
		}
		return ac
	case model.ContentTypeThinking:
		return anthropicContent{
			Type:      "thinking",
			Thinking:  cb.Thinking,
			Signature: cb.Signature,
		}
	default:
		return anthropicContent{Type: string(cb.Type), Text: cb.Text}
	}
}

func mapTools(tools []model.ToolDefinition) []anthropicTool {
	out := make([]anthropicTool, len(tools))
	for i, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out[i] = anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		}
	}
	return out
}

func systemText(msgs []model.Message) string {
	var parts []string
	for _, m := range msgs {
		if m.Role == model.RoleSystem {
			parts = append(parts, m.Text())
		}
	}
	return strings.Join(parts, "\n")
}

func buildRequest(req *provider.Request, stream bool) anthropicRequest {
	ar := anthropicRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		Messages:  mapMessages(req.Messages),
		System:    systemText(req.Messages),
		Stream:    stream,
	}
	if len(req.Tools) > 0 {
		ar.Tools = mapTools(req.Tools)
	}
	if req.System != "" {
		if ar.System != "" {
			ar.System = req.System + "\n" + ar.System
		} else {
			ar.System = req.System
		}
	}
	if req.ThinkingLevel == model.ThinkingLevelAuto || req.ThinkingLevel == model.ThinkingLevelFull {
		budget := req.MaxTokens / 2
		if budget < 1024 {
			budget = 1024
		}
		ar.Thinking = &thinkingConfig{Type: "enabled", BudgetTokens: budget}
	}
	return ar
}

func headers(apiKey string, thinking bool) map[string]string {
	h := map[string]string{
		"x-api-key":         apiKey,
		"anthropic-version": apiVersion,
		"content-type":      "application/json",
	}
	if thinking {
		h["anthropic-beta"] = "interleaved-thinking-2025-05-14"
	}
	return h
}

// ─── Streaming ───────────────────────────────────────────────────────────────

// Stream sends a streaming request and returns a channel of events.
func (p *Provider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.Event, error) {
	useThinking := req.ThinkingLevel == model.ThinkingLevelAuto || req.ThinkingLevel == model.ThinkingLevelFull
	body := buildRequest(req, true)

	body2, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}
	_ = body2

	ch := make(chan provider.Event, 64)

	go func() {
		defer close(ch)

		rc, err := httpclient.PostJSONStream(ctx, baseURL, headers(p.apiKey, useThinking), body)
		if err != nil {
			ch <- provider.Event{Type: provider.EventError, Err: err}
			return
		}
		defer rc.Close()

		done := make(chan struct{})
		defer close(done)

		// Track per-block metadata (index → type).
		blockType := map[int]string{}
		blockID := map[int]string{}
		blockName := map[int]string{}

		var inputTokens int
		var outputTokens int

		sseCh := sse.Chan(rc, done)
		for ev := range sseCh {
			if ev.Data == "" {
				continue
			}
			switch ev.Type {
			case "message_start":
				var e messageStartEvent
				if err := json.Unmarshal([]byte(ev.Data), &e); err == nil {
					inputTokens = e.Message.Usage.InputTokens
					outputTokens = e.Message.Usage.OutputTokens
				}

			case "content_block_start":
				var e contentBlockStartEvent
				if err := json.Unmarshal([]byte(ev.Data), &e); err == nil {
					blockType[e.Index] = e.ContentBlock.Type
					blockID[e.Index] = e.ContentBlock.ID
					blockName[e.Index] = e.ContentBlock.Name
					if e.ContentBlock.Type == "tool_use" {
						ch <- provider.Event{
							Type:      provider.EventToolCallStart,
							ToolID:    e.ContentBlock.ID,
							ToolName:  e.ContentBlock.Name,
							ToolIndex: e.Index,
						}
					}
				}

			case "content_block_delta":
				var e contentBlockDeltaEvent
				if err := json.Unmarshal([]byte(ev.Data), &e); err == nil {
					switch e.Delta.Type {
					case "text_delta":
						ch <- provider.Event{Type: provider.EventTextDelta, Text: e.Delta.Text}
					case "input_json_delta":
						ch <- provider.Event{
							Type:        provider.EventToolCallDelta,
							ToolIndex:   e.Index,
							PartialJSON: e.Delta.PartialJSON,
						}
					case "thinking_delta":
						ch <- provider.Event{Type: provider.EventThinkingDelta, Text: e.Delta.Thinking}
					}
				}

			case "content_block_stop":
				// Parse to get index.
				var raw struct {
					Index int `json:"index"`
				}
				if err := json.Unmarshal([]byte(ev.Data), &raw); err == nil {
					if blockType[raw.Index] == "tool_use" {
						ch <- provider.Event{
							Type:      provider.EventToolCallDone,
							ToolIndex: raw.Index,
						}
					}
				}
				// Clean up tracking maps.
				delete(blockType, raw.Index)
				delete(blockID, raw.Index)
				delete(blockName, raw.Index)

			case "message_delta":
				var e messageDeltaEvent
				if err := json.Unmarshal([]byte(ev.Data), &e); err == nil {
					outputTokens += e.Usage.OutputTokens
					ch <- provider.Event{
						Type:       provider.EventMessageStop,
						StopReason: mapStopReason(e.Delta.StopReason),
						Usage: model.Usage{
							InputTokens:  inputTokens,
							OutputTokens: outputTokens,
						},
					}
				}

			case "message_stop":
				// Final signal — we already sent MessageStop above.

			case "error":
				var raw struct {
					Error struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				msg := ev.Data
				if err2 := json.Unmarshal([]byte(ev.Data), &raw); err2 == nil && raw.Error.Message != "" {
					msg = raw.Error.Message
				}
				ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("anthropic: %s", msg)}
				return
			}
		}
	}()

	return ch, nil
}

// Complete performs a non-streaming request and returns the full response.
func (p *Provider) Complete(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	useThinking := req.ThinkingLevel == model.ThinkingLevelAuto || req.ThinkingLevel == model.ThinkingLevelFull
	body := buildRequest(req, false)

	data, err := httpclient.PostJSON(ctx, baseURL, headers(p.apiKey, useThinking), body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}

	var resp struct {
		Content    []anthropicContent `json:"content"`
		StopReason string             `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}

	blocks := make([]model.ContentBlock, 0, len(resp.Content))
	for _, c := range resp.Content {
		blocks = append(blocks, unmapContent(c))
	}

	return &provider.Response{
		Message:    model.Message{Role: model.RoleAssistant, Content: blocks},
		StopReason: mapStopReason(resp.StopReason),
		Usage: model.Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		},
	}, nil
}

func unmapContent(c anthropicContent) model.ContentBlock {
	switch c.Type {
	case "text":
		return model.ContentBlock{Type: model.ContentTypeText, Text: c.Text}
	case "tool_use":
		return model.ContentBlock{
			Type:  model.ContentTypeToolUse,
			ID:    c.ID,
			Name:  c.Name,
			Input: c.Input,
		}
	case "thinking":
		return model.ContentBlock{
			Type:      model.ContentTypeThinking,
			Thinking:  c.Thinking,
			Signature: c.Signature,
		}
	default:
		return model.ContentBlock{Type: model.ContentType(c.Type), Text: c.Text}
	}
}

func mapStopReason(r string) model.StopReason {
	switch r {
	case "end_turn":
		return model.StopReasonEndTurn
	case "tool_use":
		return model.StopReasonToolUse
	case "max_tokens":
		return model.StopReasonMaxTokens
	case "stop_sequence":
		return model.StopReasonStopSeq
	default:
		return model.StopReasonEndTurn
	}
}

// Ensure io import is used via io.EOF reference in sse package (kept for clarity).
var _ = io.EOF
