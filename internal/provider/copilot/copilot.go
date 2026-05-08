// Package copilot implements the GitHub Copilot provider via OpenAI-compatible chat API.
package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tiru-r/pi-agent-go/internal/httpclient"
	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/provider"
	"github.com/tiru-r/pi-agent-go/internal/sse"
)

const (
	tokenEndpoint  = "https://api.github.com/copilot_internal/v2/token"
	chatEndpoint   = "https://api.githubcopilot.com/chat/completions"
	editorVersion  = "pi-agent/0.1.0"
	defaultModel   = "gpt-4o"
)

// Provider implements provider.Provider for GitHub Copilot.
type Provider struct {
	githubToken string

	mu          sync.Mutex
	cachedToken cachedToken
}

type cachedToken struct {
	token       string
	expiresAt   time.Time
	apiEndpoint string // chat completions endpoint from token response; may be empty
}

// New creates a new Copilot provider.
// If githubToken is empty, the token is discovered from the environment or gh config.
func New(githubToken string) *Provider {
	return &Provider{githubToken: githubToken}
}

// Name returns the provider identifier.
func (p *Provider) Name() string { return "copilot" }

// ─── Token resolution ────────────────────────────────────────────────────────

// resolveGitHubToken returns the best available GitHub token.
func (p *Provider) resolveGitHubToken() (string, error) {
	if p.githubToken != "" {
		return p.githubToken, nil
	}
	if v := os.Getenv("GITHUB_TOKEN"); v != "" {
		return v, nil
	}
	if v := os.Getenv("GITHUB_OAUTH_TOKEN"); v != "" {
		return v, nil
	}
	// Try ~/.config/gh/hosts.yml
	if tok, err := readGHHostsToken(); err == nil && tok != "" {
		return tok, nil
	}
	return "", fmt.Errorf("copilot: no GitHub token found; set GITHUB_TOKEN or log in with `gh auth login`")
}

// readGHHostsToken parses ~/.config/gh/hosts.yml for github.com oauth_token.
// Parses only the fields we need without a full YAML library.
func readGHHostsToken() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".config", "gh", "hosts.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	// Simple line-by-line parser — avoids pulling in a YAML dependency.
	// The file looks like:
	//   github.com:
	//       oauth_token: gho_xxx
	//       ...
	inGithubSection := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "github.com:" {
			inGithubSection = true
			continue
		}
		if inGithubSection {
			// A new top-level key ends the section.
			if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
				break
			}
			if strings.HasPrefix(trimmed, "oauth_token:") {
				tok := strings.TrimSpace(strings.TrimPrefix(trimmed, "oauth_token:"))
				tok = strings.Trim(tok, `"'`)
				if tok != "" {
					return tok, nil
				}
			}
		}
	}
	return "", fmt.Errorf("oauth_token not found in %s", path)
}

// copilotTokenResponse is the response from the Copilot token endpoint.
type copilotTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"` // Unix timestamp
	Endpoints struct {
		API string `json:"api"`
	} `json:"endpoints"`
}

// getCopilotSession returns a valid Copilot session token and API endpoint.
func (p *Provider) getCopilotSession(ctx context.Context) (token, endpoint string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Refresh 60 seconds before actual expiry.
	if p.cachedToken.token != "" && time.Now().Add(60*time.Second).Before(p.cachedToken.expiresAt) {
		ep := p.cachedToken.apiEndpoint
		if ep == "" {
			ep = chatEndpoint
		}
		return p.cachedToken.token, ep, nil
	}

	ghToken, err := p.resolveGitHubToken()
	if err != nil {
		return "", "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenEndpoint, nil)
	if err != nil {
		return "", "", fmt.Errorf("copilot: build token request: %w", err)
	}
	req.Header.Set("Authorization", "token "+ghToken)
	req.Header.Set("Editor-Version", editorVersion)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")

	respBytes, err := httpclient.Do(ctx, req)
	if err != nil {
		return "", "", fmt.Errorf("copilot: token exchange: %w", err)
	}

	var resp copilotTokenResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return "", "", fmt.Errorf("copilot: decode token response: %w", err)
	}
	if resp.Token == "" {
		return "", "", fmt.Errorf("copilot: empty token in response")
	}

	expiresAt := time.Now().Add(30 * time.Minute) // safe default
	if resp.ExpiresAt > 0 {
		expiresAt = time.Unix(resp.ExpiresAt, 0)
	}
	endpoint = resp.Endpoints.API
	if endpoint == "" {
		endpoint = chatEndpoint
	} else {
		endpoint = strings.TrimRight(endpoint, "/")
		if !strings.HasSuffix(endpoint, "/chat/completions") {
			endpoint += "/chat/completions"
		}
	}
	p.cachedToken = cachedToken{token: resp.Token, expiresAt: expiresAt, apiEndpoint: endpoint}
	return resp.Token, endpoint, nil
}

// ─── Wire types (OpenAI-compatible) ─────────────────────────────────────────

type copilotRequest struct {
	Model         string           `json:"model"`
	Messages      []copilotMessage `json:"messages"`
	Tools         []copilotTool    `json:"tools,omitempty"`
	MaxTokens     int              `json:"max_tokens,omitempty"`
	Stream        bool             `json:"stream"`
	StreamOptions *streamOptions   `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type copilotMessage struct {
	Role       string           `json:"role"`
	Content    interface{}      `json:"content"` // string or []copilotContentPart
	ToolCalls  []copilotToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type copilotContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *copilotImgURL  `json:"image_url,omitempty"`
}

type copilotImgURL struct {
	URL string `json:"url"`
}

type copilotToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type copilotTool struct {
	Type     string          `json:"type"`
	Function copilotToolFunc `json:"function"`
}

type copilotToolFunc struct {
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

func mapMessages(msgs []model.Message, system string) []copilotMessage {
	var out []copilotMessage
	if system != "" {
		out = append(out, copilotMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		switch m.Role {
		case model.RoleSystem:
			out = append(out, copilotMessage{Role: "system", Content: m.Text()})

		case model.RoleUser:
			if hasImages(m) {
				out = append(out, copilotMessage{Role: "user", Content: buildUserParts(m)})
			} else {
				for _, cb := range m.Content {
					if cb.Type == model.ContentTypeToolResult {
						out = append(out, copilotMessage{
							Role:       "tool",
							Content:    extractToolResultText(cb),
							ToolCallID: cb.ToolUseID,
						})
					}
				}
				if text := m.Text(); text != "" {
					out = append(out, copilotMessage{Role: "user", Content: text})
				}
			}

		case model.RoleAssistant:
			am := copilotMessage{Role: "assistant"}
			if text := m.Text(); text != "" {
				am.Content = text
			}
			for _, cb := range m.Content {
				if cb.Type == model.ContentTypeToolUse {
					tc := copilotToolCall{ID: cb.ID, Type: "function"}
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
					out = append(out, copilotMessage{
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

func buildUserParts(m model.Message) []copilotContentPart {
	var parts []copilotContentPart
	for _, cb := range m.Content {
		switch cb.Type {
		case model.ContentTypeText:
			parts = append(parts, copilotContentPart{Type: "text", Text: cb.Text})
		case model.ContentTypeImage:
			if cb.Source != nil {
				var url string
				if cb.Source.Type == "url" {
					url = cb.Source.URL
				} else {
					url = fmt.Sprintf("data:%s;base64,%s", cb.Source.MediaType, cb.Source.Data)
				}
				parts = append(parts, copilotContentPart{
					Type:     "image_url",
					ImageURL: &copilotImgURL{URL: url},
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

func mapTools(tools []model.ToolDefinition) []copilotTool {
	out := make([]copilotTool, len(tools))
	for i, t := range tools {
		params := t.InputSchema
		if params == nil {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out[i] = copilotTool{
			Type: "function",
			Function: copilotToolFunc{
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

// modelName returns the model to use, stripping any "copilot/" prefix.
func modelName(m string) string {
	m = strings.TrimPrefix(m, "copilot/")
	if m == "" {
		return defaultModel
	}
	return m
}

// ─── Streaming ───────────────────────────────────────────────────────────────

// Stream sends a chat completions request and emits provider.Event values.
func (p *Provider) Stream(ctx context.Context, req *provider.Request) (<-chan provider.Event, error) {
	copilotToken, apiEndpoint, err := p.getCopilotSession(ctx)
	if err != nil {
		return nil, err
	}

	body := copilotRequest{
		Model:         modelName(req.Model),
		Messages:      mapMessages(req.Messages, req.System),
		MaxTokens:     req.MaxTokens,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	if len(req.Tools) > 0 {
		body.Tools = mapTools(req.Tools)
	}

	headers := map[string]string{
		"Authorization": "Bearer " + copilotToken,
		"Editor-Version": editorVersion,
	}

	ch := make(chan provider.Event, 64)

	go func() {
		defer close(ch)

		rc, err := httpclient.PostJSONStream(ctx, apiEndpoint, headers, body)
		if err != nil {
			ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("copilot: %w", err)}
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

