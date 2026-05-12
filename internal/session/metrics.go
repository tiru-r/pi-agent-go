package session

import (
	"fmt"
	"time"

	"github.com/tiru-r/pi-agent-go/internal/model"
)

// Metrics holds aggregated statistics for a session.
type Metrics struct {
	TotalMessages     int
	UserMessages      int
	AssistantMessages int
	ToolCalls         int
	TotalTokens       int
	InputTokens       int
	OutputTokens      int
	CacheReadTokens   int
	Duration          time.Duration
	StartedAt         time.Time
}

// Compute calculates Metrics from a Session's entries.
func Compute(s *Session) Metrics {
	entries := s.Snapshot()

	var m Metrics

	var first, last time.Time
	for _, e := range entries {
		if e.Type != EntryMessage || e.Message == nil {
			continue
		}

		if first.IsZero() {
			first = e.Timestamp
		}
		last = e.Timestamp

		m.TotalMessages++
		switch e.Message.Role {
		case model.RoleUser:
			m.UserMessages++
		case model.RoleAssistant:
			m.AssistantMessages++
			for _, block := range e.Message.Content {
				if block.Type == model.ContentTypeToolUse {
					m.ToolCalls++
				}
			}
		}

		if e.Usage != nil {
			m.InputTokens += e.Usage.InputTokens
			m.OutputTokens += e.Usage.OutputTokens
			m.CacheReadTokens += e.Usage.CacheReadTokens
		}
	}

	m.TotalTokens = m.InputTokens + m.OutputTokens
	m.StartedAt = first
	if !first.IsZero() && !last.IsZero() {
		m.Duration = last.Sub(first)
	}

	return m
}

// String returns a human-readable summary of the metrics.
func (m Metrics) String() string {
	dur := m.Duration.Round(time.Second)
	return fmt.Sprintf(
		"Messages: %d (user: %d, assistant: %d) | Tool calls: %d | Tokens: %d (in: %d, out: %d, cached: %d) | Duration: %s",
		m.TotalMessages, m.UserMessages, m.AssistantMessages,
		m.ToolCalls,
		m.TotalTokens, m.InputTokens, m.OutputTokens, m.CacheReadTokens,
		dur,
	)
}
