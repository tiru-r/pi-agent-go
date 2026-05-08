// Package gemini implements the Google Gemini API provider.
package gemini

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

const baseURL = "https://generativelanguage.googleapis.com/v1beta/models"

// Provider implements provider.Provider for the Google Gemini API.
type Provider struct {
	apiKey string
}

// New creates a new Gemini provider.
func New(apiKey string) *Provider {
	return &Provider{apiKey: apiKey}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "gemini" }

// ─── Wire types ─────────────────────────────────────────────────────────────

type geminiRequest struct {
	Contents        []geminiContent  `json:"contents"`
	Tools           []geminiTools    `json:"tools,omitempty"`
	SystemInstruction *geminiContent `json:"systemInstruction,omitempty"`
	GenerationConfig geminiGenConfig  `json:"generationConfig"`
}

type geminiContent struct {
	Role  string      `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text         string             `json:"text,omitempty"`
	InlineData   *geminiInlineData  `json:"inlineData,omitempty"`
	FunctionCall *geminiFuncCall    `json:"functionCall,omitempty"`
	FunctionResponse *geminiFuncResp `json:"functionResponse,omitempty"`
}

type geminiInlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiFuncCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type geminiFuncResp struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type geminiTools struct {
	FunctionDeclarations []geminiFuncDecl `json:"functionDeclarations"`
}

type geminiFuncDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type geminiGenConfig struct {
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
	Temperature     float64 `json:"temperature,omitempty"`
}

// ─── SSE response shapes ─────────────────────────────────────────────────────

type geminiStreamResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text         string          `json:"text"`
				FunctionCall *geminiFuncCall `json:"functionCall"`
			} `json:"parts"`
			Role string `json:"role"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
		Index        int    `json:"index"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// ─── Request mapping ─────────────────────────────────────────────────────────

func mapMessages(msgs []model.Message, system string) ([]geminiContent, *geminiContent) {
	var systemInstr *geminiContent
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
		systemInstr = &geminiContent{
			Parts: []geminiPart{{Text: strings.Join(sysTexts, "\n")}},
		}
	}

	var contents []geminiContent
	for _, m := range msgs {
		if m.Role == model.RoleSystem {
			continue
		}
		role := geminiRole(m.Role)
		var parts []geminiPart
		for _, cb := range m.Content {
			p := mapContentBlock(cb)
			parts = append(parts, p...)
		}
		if len(parts) == 0 {
			continue
		}
		contents = append(contents, geminiContent{Role: role, Parts: parts})
	}
	return contents, systemInstr
}

func geminiRole(r model.Role) string {
	switch r {
	case model.RoleUser, model.RoleTool:
		return "user"
	case model.RoleAssistant:
		return "model"
	default:
		return "user"
	}
}

func mapContentBlock(cb model.ContentBlock) []geminiPart {
	switch cb.Type {
	case model.ContentTypeText:
		return []geminiPart{{Text: cb.Text}}
	case model.ContentTypeImage:
		if cb.Source == nil {
			return nil
		}
		if cb.Source.Type == "url" {
			// Gemini doesn't support URL images natively; embed as text reference.
			return []geminiPart{{Text: cb.Source.URL}}
		}
		return []geminiPart{{
			InlineData: &geminiInlineData{
				MIMEType: cb.Source.MediaType,
				Data:     cb.Source.Data,
			},
		}}
	case model.ContentTypeToolUse:
		input := cb.Input
		if input == nil {
			input = json.RawMessage("{}")
		}
		return []geminiPart{{
			FunctionCall: &geminiFuncCall{
				Name: cb.Name,
				Args: input,
			},
		}}
	case model.ContentTypeToolResult:
		resp := json.RawMessage(`{"output":""}`)
		for _, c := range cb.Content {
			if c.Type == model.ContentTypeText {
				b, _ := json.Marshal(map[string]string{"output": c.Text})
				resp = b
				break
			}
		}
		return []geminiPart{{
			FunctionResponse: &geminiFuncResp{
				Name:     cb.ToolUseID,
				Response: resp,
			},
		}}
	default:
		if cb.Text != "" {
			return []geminiPart{{Text: cb.Text}}
		}
		return nil
	}
}

func mapTools(tools []model.ToolDefinition) []geminiTools {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]geminiFuncDecl, len(tools))
	for i, t := range tools {
		decls[i] = geminiFuncDecl{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		}
	}
	return []geminiTools{{FunctionDeclarations: decls}}
}

func mapStopReason(r string) model.StopReason {
	switch r {
	case "STOP", "":
		return model.StopReasonEndTurn
	case "MAX_TOKENS":
		return model.StopReasonMaxTokens
	case "TOOL_CODE", "FUNCTION_CALL":
		return model.StopReasonToolUse
	default:
		return model.StopReasonEndTurn
	}
}

// streamURL builds the Gemini streaming endpoint URL.
func streamURL(modelID, apiKey string) string {
	// Strip any "gemini/" prefix used internally.
	m := strings.TrimPrefix(modelID, "gemini/")
	return fmt.Sprintf("%s/%s:streamGenerateContent?key=%s&alt=sse", baseURL, m, apiKey)
}

// ─── Streaming ───────────────────────────────────────────────────────────────

// Stream sends a streaming request and returns a channel of events.
func (p *Provider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.Event, error) {
	contents, systemInstr := mapMessages(req.Messages, req.System)
	body := geminiRequest{
		Contents:          contents,
		SystemInstruction: systemInstr,
		GenerationConfig: geminiGenConfig{
			MaxOutputTokens: req.MaxTokens,
		},
	}
	if req.Temperature != nil {
		body.GenerationConfig.Temperature = *req.Temperature
	}
	if len(req.Tools) > 0 {
		body.Tools = mapTools(req.Tools)
	}

	url := streamURL(req.Model, p.apiKey)
	ch := make(chan provider.Event, 64)

	go func() {
		defer close(ch)

		rc, err := httpclient.PostJSONStream(ctx, url, nil, body)
		if err != nil {
			ch <- provider.Event{Type: provider.EventError, Err: err}
			return
		}
		defer rc.Close()

		done := make(chan struct{})
		defer close(done)

		var promptTokens, candidateTokens int
		var finishReason string
		toolIndex := 0

		sseCh := sse.Chan(rc, done)
		for ev := range sseCh {
			if ev.Data == "" {
				continue
			}

			var resp geminiStreamResponse
			if err := json.Unmarshal([]byte(ev.Data), &resp); err != nil {
				continue
			}

			if resp.UsageMetadata.PromptTokenCount > 0 {
				promptTokens = resp.UsageMetadata.PromptTokenCount
			}
			if resp.UsageMetadata.CandidatesTokenCount > 0 {
				candidateTokens = resp.UsageMetadata.CandidatesTokenCount
			}

			for _, cand := range resp.Candidates {
				if cand.FinishReason != "" {
					finishReason = cand.FinishReason
				}
				for _, part := range cand.Content.Parts {
					if part.FunctionCall != nil {
						args := part.FunctionCall.Args
						if args == nil {
							args = json.RawMessage("{}")
						}
						ch <- provider.Event{
							Type:      provider.EventToolCallStart,
							ToolID:    fmt.Sprintf("call_%d", toolIndex),
							ToolName:  part.FunctionCall.Name,
							ToolIndex: toolIndex,
						}
						ch <- provider.Event{
							Type:        provider.EventToolCallDelta,
							ToolIndex:   toolIndex,
							PartialJSON: string(args),
						}
						ch <- provider.Event{
							Type:      provider.EventToolCallDone,
							ToolIndex: toolIndex,
						}
						toolIndex++
					} else if part.Text != "" {
						ch <- provider.Event{Type: provider.EventTextDelta, Text: part.Text}
					}
				}
			}
		}

		ch <- provider.Event{
			Type:       provider.EventMessageStop,
			StopReason: mapStopReason(finishReason),
			Usage: model.Usage{
				InputTokens:  promptTokens,
				OutputTokens: candidateTokens,
			},
		}
	}()

	return ch, nil
}
