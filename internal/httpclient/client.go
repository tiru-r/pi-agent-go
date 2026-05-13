package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

var defaultClient = &http.Client{
	// No global timeout — callers manage lifetime via context.
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	},
}

// HTTPError represents a non-2xx response.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

func (e *HTTPError) IsRateLimit() bool { return e.StatusCode == 429 }
func (e *HTTPError) IsAuth() bool      { return e.StatusCode == 401 || e.StatusCode == 403 }

// DoStream performs a request and returns the open response body for streaming.
// The caller must close the body when done.
func DoStream(ctx context.Context, req *http.Request) (io.ReadCloser, error) {
	resp, err := defaultClient.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	return resp.Body, nil
}

// Do performs a request and reads the full response body.
func Do(ctx context.Context, req *http.Request) ([]byte, error) {
	resp, err := defaultClient.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	return io.ReadAll(resp.Body)
}

// PostJSON serialises body as JSON, sends a POST, and returns the raw response bytes.
func PostJSON(ctx context.Context, url string, headers map[string]string, body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return Do(ctx, req)
}

// PostJSONStream serialises body as JSON, sends a POST, and returns the streaming body.
// Retries are handled by the caller (agent.streamWithRetry) to avoid double retry compounding.
func PostJSONStream(ctx context.Context, url string, headers map[string]string, body any) (io.ReadCloser, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return DoStream(ctx, req)
}
