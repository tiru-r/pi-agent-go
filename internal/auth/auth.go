// Package auth provides credential storage and API key management.
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Credentials holds all stored API keys and OAuth tokens.
type Credentials struct {
	AnthropicAPIKey string `json:"anthropic_api_key,omitempty"`
	OpenAIAPIKey    string `json:"openai_api_key,omitempty"`
	GeminiAPIKey    string `json:"gemini_api_key,omitempty"`
	CohereAPIKey    string `json:"cohere_api_key,omitempty"`
	GitHubToken     string `json:"github_token,omitempty"`
	GitLabToken     string `json:"gitlab_token,omitempty"`
	// OAuth refresh tokens
	GoogleRefreshToken string `json:"google_refresh_token,omitempty"`
}

// authPath returns the path to ~/.pi/agent/auth.json.
func authPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "agent", "auth.json")
}

// Load reads credentials from ~/.pi/agent/auth.json.
// Returns an empty Credentials if the file does not exist.
func Load() (*Credentials, error) {
	data, err := os.ReadFile(authPath())
	if os.IsNotExist(err) {
		return &Credentials{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("auth: unmarshal: %w", err)
	}
	return &c, nil
}

// Save writes credentials to ~/.pi/agent/auth.json with mode 0600.
func (c *Credentials) Save() error {
	path := authPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("auth: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("auth: marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("auth: write: %w", err)
	}
	return nil
}

// SetKey stores the given API key for the named provider.
func (c *Credentials) SetKey(prov, key string) error {
	switch strings.ToLower(prov) {
	case "anthropic":
		c.AnthropicAPIKey = key
	case "openai":
		c.OpenAIAPIKey = key
	case "gemini", "google":
		c.GeminiAPIKey = key
	case "cohere":
		c.CohereAPIKey = key
	case "github":
		c.GitHubToken = key
	case "gitlab":
		c.GitLabToken = key
	default:
		return fmt.Errorf("auth: unknown provider %q", prov)
	}
	return nil
}

// GetKey retrieves the API key for the named provider.
func (c *Credentials) GetKey(prov string) string {
	switch strings.ToLower(prov) {
	case "anthropic":
		return c.AnthropicAPIKey
	case "openai":
		return c.OpenAIAPIKey
	case "gemini", "google":
		return c.GeminiAPIKey
	case "cohere":
		return c.CohereAPIKey
	case "github":
		return c.GitHubToken
	case "gitlab":
		return c.GitLabToken
	}
	return ""
}

// ValidateAPIKey makes a minimal test request to verify the key works.
func ValidateAPIKey(ctx context.Context, prov, key string) error {
	switch strings.ToLower(prov) {
	case "anthropic":
		return validateAnthropic(ctx, key)
	case "openai":
		return validateOpenAI(ctx, key)
	case "gemini", "google":
		return validateGemini(ctx, key)
	case "cohere":
		return validateCohere(ctx, key)
	default:
		return fmt.Errorf("auth: validation not supported for provider %q", prov)
	}
}

func validateAnthropic(ctx context.Context, key string) error {
	body := []byte(`{"model":"claude-haiku-3-5-20241022","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("auth: anthropic validation request: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("auth: anthropic key is invalid (HTTP %d)", resp.StatusCode)
	}
	return nil
}

func validateOpenAI(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.openai.com/v1/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("auth: openai validation request: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("auth: openai key is invalid (HTTP %d)", resp.StatusCode)
	}
	return nil
}

func validateGemini(ctx context.Context, key string) error {
	url := "https://generativelanguage.googleapis.com/v1beta/models?key=" + key
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("auth: gemini validation request: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("auth: gemini key is invalid (HTTP %d)", resp.StatusCode)
	}
	return nil
}

func validateCohere(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.cohere.com/v1/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("auth: cohere validation request: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("auth: cohere key is invalid (HTTP %d)", resp.StatusCode)
	}
	return nil
}
