// Package vertex implements the Google Vertex AI provider for Gemini models.
package vertex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/oauth2/google"

	"github.com/pi-agent/pi/internal/httpclient"
	"github.com/pi-agent/pi/internal/model"
	"github.com/pi-agent/pi/internal/provider"
	"github.com/pi-agent/pi/internal/sse"
)

// Provider implements provider.Provider for Google Vertex AI (Gemini models).
type Provider struct {
	project  string
	location string
}

// New creates a new Vertex AI provider.
// project and location are required; credentials are loaded from Application Default Credentials.
func New(project, location string) (*Provider, error) {
	if project == "" {
		return nil, fmt.Errorf("vertex: project must be set (set GOOGLE_CLOUD_PROJECT or vertex_project in config)")
	}
	if location == "" {
		location = "us-central1"
	}
	return &Provider{project: project, location: location}, nil
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "vertex" }

// ─── Wire types (Gemini-compatible) ─────────────────────────────────────────

type vertexRequest struct {
	Contents          []vertexContent  `json:"contents"`
	Tools             []vertexToolGroup `json:"tools,omitempty"`
	GenerationConfig  *vertexGenConfig `json:"generationConfig,omitempty"`
	SystemInstruction *vertexContent   `json:"systemInstruction,omitempty"`
}

type vertexContent struct {
	Role  string        `json:"role,omitempty"`
	Parts []vertexPart  `json:"parts"`
}

type vertexPart struct {
	Text             string            `json:"text,omitempty"`
	InlineData       *vertexInlineData `json:"inlineData,omitempty"`
	FunctionCall     *vertexFuncCall   `json:"functionCall,omitempty"`
	FunctionResponse *vertexFuncResp   `json:"functionResponse,omitempty"`
}

type vertexInlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type vertexFuncCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type vertexFuncResp struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type vertexToolGroup struct {
	FunctionDeclarations []vertexFuncDecl `json:"functionDeclarations"`
}

type vertexFuncDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type vertexGenConfig struct {
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
	Temperature     float64 `json:"temperature,omitempty"`
}

// ─── SSE response shape ───────────────────────────────────────────────────────

type vertexStreamResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text         string          `json:"text"`
				FunctionCall *vertexFuncCall `json:"functionCall"`
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

func mapMessages(msgs []model.Message, system string) ([]vertexContent, *vertexContent) {
	var systemInstr *vertexContent
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
		systemInstr = &vertexContent{
			Parts: []vertexPart{{Text: strings.Join(sysTexts, "\n")}},
		}
	}

	var contents []vertexContent
	for _, m := range msgs {
		if m.Role == model.RoleSystem {
			continue
		}
		role := vertexRole(m.Role)
		var parts []vertexPart
		for _, cb := range m.Content {
			parts = append(parts, mapContentBlock(cb)...)
		}
		if len(parts) == 0 {
			continue
		}
		contents = append(contents, vertexContent{Role: role, Parts: parts})
	}
	return contents, systemInstr
}

func vertexRole(r model.Role) string {
	switch r {
	case model.RoleUser, model.RoleTool:
		return "user"
	case model.RoleAssistant:
		return "model"
	default:
		return "user"
	}
}

func mapContentBlock(cb model.ContentBlock) []vertexPart {
	switch cb.Type {
	case model.ContentTypeText:
		return []vertexPart{{Text: cb.Text}}
	case model.ContentTypeImage:
		if cb.Source == nil {
			return nil
		}
		if cb.Source.Type == "url" {
			return []vertexPart{{Text: cb.Source.URL}}
		}
		return []vertexPart{{
			InlineData: &vertexInlineData{
				MIMEType: cb.Source.MediaType,
				Data:     cb.Source.Data,
			},
		}}
	case model.ContentTypeToolUse:
		input := cb.Input
		if input == nil {
			input = json.RawMessage("{}")
		}
		return []vertexPart{{
			FunctionCall: &vertexFuncCall{
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
		return []vertexPart{{
			FunctionResponse: &vertexFuncResp{
				Name:     cb.ToolUseID,
				Response: resp,
			},
		}}
	default:
		if cb.Text != "" {
			return []vertexPart{{Text: cb.Text}}
		}
		return nil
	}
}

func mapTools(tools []model.ToolDefinition) []vertexToolGroup {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]vertexFuncDecl, len(tools))
	for i, t := range tools {
		decls[i] = vertexFuncDecl{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		}
	}
	return []vertexToolGroup{{FunctionDeclarations: decls}}
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

// streamURL builds the Vertex AI streaming endpoint.
func (p *Provider) streamURL(modelID string) string {
	m := strings.TrimPrefix(modelID, "vertex/")
	return fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:streamGenerateContent?alt=sse",
		p.location, p.project, p.location, m,
	)
}

// getAccessToken retrieves a short-lived OAuth2 bearer token using ADC.
func getAccessToken(ctx context.Context) (string, error) {
	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return "", fmt.Errorf("vertex: find default credentials: %w", err)
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("vertex: get OAuth2 token: %w", err)
	}
	return tok.AccessToken, nil
}

// ─── Streaming ───────────────────────────────────────────────────────────────

// Stream sends a streamGenerateContent request and emits provider.Event values.
func (p *Provider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.Event, error) {
	accessToken, err := getAccessToken(ctx)
	if err != nil {
		return nil, err
	}

	contents, systemInstr := mapMessages(req.Messages, req.System)

	genCfg := &vertexGenConfig{
		MaxOutputTokens: req.MaxTokens,
	}
	if req.Temperature != nil {
		genCfg.Temperature = *req.Temperature
	}

	body := vertexRequest{
		Contents:          contents,
		SystemInstruction: systemInstr,
		GenerationConfig:  genCfg,
	}
	if len(req.Tools) > 0 {
		body.Tools = mapTools(req.Tools)
	}

	headers := map[string]string{
		"Authorization": "Bearer " + accessToken,
	}

	url := p.streamURL(req.Model)
	ch := make(chan provider.Event, 64)

	go func() {
		defer close(ch)

		rc, err := httpclient.PostJSONStream(ctx, url, headers, body)
		if err != nil {
			ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("vertex: %w", err)}
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

			var resp vertexStreamResponse
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
