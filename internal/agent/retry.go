package agent

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/tiru-r/pi-agent-go/internal/httpclient"
	"github.com/tiru-r/pi-agent-go/internal/provider"
)

const (
	defaultRetryAttempts = 3
	retryBaseDelay       = 500 * time.Millisecond
	retryMaxDelay        = 30 * time.Second
)

// streamWithRetry calls prov.Stream with exponential backoff on retriable errors
// (429, 5xx, network timeouts). maxAttempts ≤ 0 defaults to defaultRetryAttempts.
// Context cancellation stops retrying immediately.
func streamWithRetry(ctx context.Context, prov provider.Provider, req *provider.Request, maxAttempts int) (<-chan provider.Event, error) {
	if maxAttempts <= 0 {
		maxAttempts = defaultRetryAttempts
	}
	delay := retryBaseDelay
	for attempt := 1; ; attempt++ {
		ch, err := prov.Stream(ctx, req)
		if err == nil {
			return ch, nil
		}
		if attempt >= maxAttempts || !isRetriableErr(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < retryMaxDelay {
			delay *= 2
			if delay > retryMaxDelay {
				delay = retryMaxDelay
			}
		}
	}
}

// isRetriableErr reports whether err warrants a retry: HTTP rate-limits (429),
// transient server errors (5xx), or network-level timeouts.
func isRetriableErr(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *httpclient.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == 429 || httpErr.StatusCode >= 500
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}
