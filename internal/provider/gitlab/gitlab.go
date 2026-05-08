// Package gitlab implements the GitLab Duo provider via OpenAI-compatible chat API.
package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pi-agent/pi/internal/httpclient"
	"github.com/pi-agent/pi/internal/model"
	"github.com/pi-agent/pi/internal/provider"
	"github.com/pi-agent/pi/internal/sse"
)

const (
	defaultBaseURL = "https://gitlab.com"
	chatPath       = "/api/v4/openai/chat/completions"
	defaultModel   = "claude-3-5-sonnet-20241022"
)

// Provider implements provider.Provider for GitLab Duo.
type Provider struct {
	baseURL string
	token   string
}

// New creates a new GitLab Duo provider.
// If baseURL or token are empty, they are resolved from environment variables or config files.
func New(baseURL, token string) *Provider {
	return &Provider{baseURL: baseURL, token: token}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "gitlab" }

// ─── Credential resolution ───────────────────────────────────────────────────

func (p *Provider) resolveToken() (string, error) {
	if p.token != "" {
		return p.token, nil
	}
	if v := os.Getenv("GITLAB_TOKEN"); v != "" {
		return v, nil
	}
	if v := os.Getenv("GITLAB_PERSONAL_ACCESS_TOKEN"); v != "" {
		return v, nil
	}
	// Try glab config files.
	if tok, err := readGlabToken(); err == nil && tok != "" {
		return tok, nil
	}
	return "", fmt.Errorf("gitlab: no token found; set GITLAB_TOKEN or GITLAB_PERSONAL_ACCESS_TOKEN")
}

func (p *Provider) resolveBaseURL() string {
	if p.baseURL != "" {
		return strings.TrimRight(p.baseURL, "/")
	}
	if v := os.Getenv("GITLAB_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultBaseURL
}

// readGlabToken tries to read an auth token from common glab config file locations.
// It supports both ~/.config/glab-cli/config.yml and ~/.config/gl/config.toml.
func readGlabToken() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	// Try glab-cli YAML config first.
	ymlPath := filepath.Join(home, ".config", "glab-cli", "config.yml")
	if tok, err := parseGlabYAML(ymlPath); err == nil && tok != "" {
		return tok, nil
	}

	// Fall back to gl TOML config.
	tomlPath := filepath.Join(home, ".config", "gl", "config.toml")
	if tok, err := parseGlabTOML(tomlPath); err == nil && tok != "" {
		return tok, nil
	}

	return "", fmt.Errorf("no glab config found")
}

// parseGlabYAML does a minimal parse of glab-cli's config.yml to extract a token.
// Format example:
//
//	hosts:
//	  gitlab.com:
//	    token: glpat-xxx
func parseGlabYAML(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	inHosts := false
	inSection := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "hosts:" {
			inHosts = true
			continue
		}
		if inHosts {
			// Detect sub-key (a hostname section, indented once).
			if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "    ") && strings.HasSuffix(trimmed, ":") {
				inSection = true
				continue
			}
			if inSection {
				if strings.HasPrefix(trimmed, "token:") {
					tok := strings.TrimSpace(strings.TrimPrefix(trimmed, "token:"))
					tok = strings.Trim(tok, `"'`)
					if tok != "" {
						return tok, nil
					}
				}
			}
			// New top-level key ends hosts section.
			if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
				break
			}
		}
	}
	return "", fmt.Errorf("token not found in %s", path)
}

// parseGlabTOML does a minimal parse of a TOML config for a token value.
func parseGlabTOML(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "token") {
			// token = "glpat-xxx"  or  token = 'glpat-xxx'
			parts := strings.SplitN(trimmed, "=", 2)
			if len(parts) == 2 {
				tok := strings.TrimSpace(parts[1])
				tok = strings.Trim(tok, `"'`)
				if tok != "" {
					return tok, nil
				}
			}
		}
	}
	return "", fmt.Errorf("token not found in %s", path)
}

// ─── Wire types (OpenAI-compatible) ─────────────────────────────────────────

type gitlabRequest struct {
	Model         string          `json:"model"`
	Messages      []gitlabMessage `json:"messages"`
	Tools         []gitlabTool    `json:"tools,omitempty"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Stream        bool            `json:"stream"`
	StreamOptions *streamOptions  `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type gitlabMessage struct {
	Role       string            `json:"role"`
	Content    interface{}       `json:"content"` // string or []gitlabContentPart
	ToolCalls  []gitlabToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
}

type gitlabContentPart struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	ImageURL *gitlabImgURL  `json:"image_url,omitempty"`
}

type gitlabImgURL struct {
	URL string `json:"url"`
}

type gitlabToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type gitlabTool struct {
	Type     string          `json:"type"`
	Function gitlabToolFunc  `json:"function"`
}

type gitlabToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ─── SSE chunk shapes ─────────────────────────────────────────────────────────

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

// ─── Request mapping ─────────────────────────────────────────────────────────

func mapMessages(msgs []model.Message, system string) []gitlabMessage {
	var out []gitlabMessage
	if system != "" {
		out = append(out, gitlabMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		switch m.Role {
		case model.RoleSystem:
			out = append(out, gitlabMessage{Role: "system", Content: m.Text()})

		case model.RoleUser:
			if hasImages(m) {
				out = append(out, gitlabMessage{Role: "user", Content: buildUserParts(m)})
			} else {
				for _, cb := range m.Content {
					if cb.Type == model.ContentTypeToolResult {
						out = append(out, gitlabMessage{
							Role:       "tool",
							Content:    extractToolResultText(cb),
							ToolCallID: cb.ToolUseID,
						})
					}
				}
				if text := m.Text(); text != "" {
					out = append(out, gitlabMessage{Role: "user", Content: text})
				}
			}

		case model.RoleAssistant:
			am := gitlabMessage{Role: "assistant"}
			if text := m.Text(); text != "" {
				am.Content = text
			}
			for _, cb := range m.Content {
				if cb.Type == model.ContentTypeToolUse {
					tc := gitlabToolCall{ID: cb.ID, Type: "function"}
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
					out = append(out, gitlabMessage{
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

func buildUserParts(m model.Message) []gitlabContentPart {
	var parts []gitlabContentPart
	for _, cb := range m.Content {
		switch cb.Type {
		case model.ContentTypeText:
			parts = append(parts, gitlabContentPart{Type: "text", Text: cb.Text})
		case model.ContentTypeImage:
			if cb.Source != nil {
				var url string
				if cb.Source.Type == "url" {
					url = cb.Source.URL
				} else {
					url = fmt.Sprintf("data:%s;base64,%s", cb.Source.MediaType, cb.Source.Data)
				}
				parts = append(parts, gitlabContentPart{
					Type:     "image_url",
					ImageURL: &gitlabImgURL{URL: url},
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

func mapTools(tools []model.ToolDefinition) []gitlabTool {
	out := make([]gitlabTool, len(tools))
	for i, t := range tools {
		params := t.InputSchema
		if params == nil {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out[i] = gitlabTool{
			Type: "function",
			Function: gitlabToolFunc{
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

func resolveModel(m string) string {
	m = strings.TrimPrefix(m, "gitlab/")
	if m == "" {
		return defaultModel
	}
	return m
}

// ─── Streaming ───────────────────────────────────────────────────────────────

// Stream sends a GitLab Duo chat completions request and emits provider.Event values.
func (p *Provider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.Event, error) {
	token, err := p.resolveToken()
	if err != nil {
		return nil, err
	}

	base := p.resolveBaseURL()
	url := base + chatPath

	body := gitlabRequest{
		Model:         resolveModel(req.Model),
		Messages:      mapMessages(req.Messages, req.System),
		MaxTokens:     req.MaxTokens,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	if len(req.Tools) > 0 {
		body.Tools = mapTools(req.Tools)
	}

	headers := map[string]string{
		"Authorization": "Bearer " + token,
	}

	ch := make(chan provider.Event, 64)

	go func() {
		defer close(ch)

		rc, err := httpclient.PostJSONStream(ctx, url, headers, body)
		if err != nil {
			ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("gitlab: %w", err)}
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
