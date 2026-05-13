package provider

import (
	"context"
	"sort"

	"github.com/tiru-r/pi-agent-go/internal/model"
)

// Provider is the interface every LLM backend must implement.
type Provider interface {
	// Name returns the canonical provider identifier (e.g. "anthropic").
	Name() string
	// Stream sends a request and returns a channel of events.
	// The channel is closed when the response is complete or an error occurs.
	Stream(ctx context.Context, req *Request) (<-chan Event, error)
}

// Request is a normalised LLM request understood by all providers.
type Request struct {
	Model         string
	Messages      []model.Message
	System        string
	Tools         []model.ToolDefinition
	MaxTokens     int
	Temperature   *float64
	ThinkingLevel model.ThinkingLevel
	StopSequences []string
	// ToolChoice controls how the model uses tools: "auto" (default), "none",
	// or "required" (model must call at least one tool this turn).
	ToolChoice string
	// Extra provider-specific fields passed through verbatim.
	Extra map[string]any
}

// EventType tags each event from a streaming response.
type EventType string

const (
	EventTextDelta     EventType = "text_delta"
	EventThinkingDelta EventType = "thinking_delta"
	EventToolCallStart EventType = "tool_call_start"
	EventToolCallDelta EventType = "tool_call_delta"
	EventToolCallDone  EventType = "tool_call_done"
	EventMessageStop   EventType = "message_stop"
	EventError         EventType = "error"
)

// Event carries streaming data from the provider.
// Only fields relevant to the EventType are populated.
type Event struct {
	Type EventType

	// EventTextDelta / EventThinkingDelta
	Text string

	// EventToolCallStart
	ToolID   string
	ToolName string

	// EventToolCallDelta — partial JSON accumulation
	ToolIndex   int
	PartialJSON string

	// EventMessageStop
	StopReason model.StopReason
	Usage      model.Usage

	// EventError
	Err error
}

// Response is the fully assembled response after draining a stream.
type Response struct {
	Message    model.Message
	StopReason model.StopReason
	Usage      model.Usage
}

// Collect drains the event channel and assembles a complete Response.
func Collect(events <-chan Event) (*Response, error) {
	var (
		blocks      []model.ContentBlock
		textIdx     = -1
		thinkingIdx = -1
		toolBlocks  = map[int]*model.ContentBlock{}
		toolOrder   []int
		usage       model.Usage
		stop        model.StopReason
	)

	for ev := range events {
		switch ev.Type {
		case EventError:
			return nil, ev.Err

		case EventTextDelta:
			if textIdx < 0 {
				blocks = append(blocks, model.ContentBlock{Type: model.ContentTypeText})
				textIdx = len(blocks) - 1
			}
			blocks[textIdx].Text += ev.Text

		case EventThinkingDelta:
			if thinkingIdx < 0 {
				blocks = append(blocks, model.ContentBlock{Type: model.ContentTypeThinking})
				thinkingIdx = len(blocks) - 1
			}
			blocks[thinkingIdx].Thinking += ev.Text

		case EventToolCallStart:
			tb := &model.ContentBlock{
				Type: model.ContentTypeToolUse,
				ID:   ev.ToolID,
				Name: ev.ToolName,
			}
			toolBlocks[ev.ToolIndex] = tb

		case EventToolCallDelta:
			if tb, ok := toolBlocks[ev.ToolIndex]; ok {
				tb.Input = append(tb.Input, []byte(ev.PartialJSON)...)
			}

		case EventToolCallDone:
			if _, ok := toolBlocks[ev.ToolIndex]; ok {
				toolOrder = append(toolOrder, ev.ToolIndex)
			}

		case EventMessageStop:
			stop = ev.StopReason
			usage = ev.Usage
		}
	}

	// Append tool blocks sorted by stream index for stable ordering.
	sort.Ints(toolOrder)
	for _, idx := range toolOrder {
		blocks = append(blocks, *toolBlocks[idx])
	}

	return &Response{
		Message:    model.Message{Role: model.RoleAssistant, Content: blocks},
		StopReason: stop,
		Usage:      usage,
	}, nil
}
