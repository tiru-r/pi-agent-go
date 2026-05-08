package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/provider"
)

const (
	// charsPerToken is a conservative estimate used for rough token counting.
	charsPerToken = 4
	// imageTokenEstimate is the estimated token cost of an image block.
	imageTokenEstimate = 1200
)

// Compactor decides when and how to compact (summarise) a conversation's
// message history so it stays within the provider's context window.
type Compactor struct {
	Provider  provider.Provider
	MaxTokens int // trigger compaction above this threshold
	KeepLast  int // always keep the last N messages intact (default 10)
}

// ShouldCompact returns true when the estimated token count exceeds MaxTokens.
func (c *Compactor) ShouldCompact(msgs []model.Message, estimatedTokens int) bool {
	if estimatedTokens <= 0 {
		estimatedTokens = estimateTokens(msgs)
	}
	threshold := c.MaxTokens
	if threshold <= 0 {
		threshold = 100_000
	}
	return estimatedTokens > threshold
}

// Compact summarises the older portion of msgs and returns a shortened list.
//
// It keeps the last c.KeepLast messages intact and asks the provider to
// summarise everything before them.  The returned message list is:
//
//	[{role:user, content:"[Previous conversation summary]\n\n<summary>"}, ...keptMessages]
//
// The returned string is the summary text; the caller may persist it in a
// session compaction entry.
func (c *Compactor) Compact(ctx context.Context, msgs []model.Message, system string) ([]model.Message, string, error) {
	keepLast := c.KeepLast
	if keepLast <= 0 {
		keepLast = 10
	}

	if len(msgs) <= keepLast {
		// Nothing to compact.
		return msgs, "", nil
	}

	toSummarise := msgs[:len(msgs)-keepLast]
	kept := msgs[len(msgs)-keepLast:]

	summary, err := c.summarise(ctx, toSummarise, system)
	if err != nil {
		return nil, "", fmt.Errorf("compactor: summarise: %w", err)
	}

	summaryMsg := model.NewTextMessage(model.RoleUser,
		"[Previous conversation summary]\n\n"+summary)

	compacted := make([]model.Message, 0, 1+len(kept))
	compacted = append(compacted, summaryMsg)
	compacted = append(compacted, kept...)
	return compacted, summary, nil
}

// summarise calls the provider with a summarisation request for msgs.
func (c *Compactor) summarise(ctx context.Context, msgs []model.Message, system string) (string, error) {
	// Append a summarisation instruction as the final user message.
	instruction := model.NewTextMessage(model.RoleUser,
		"Summarize this conversation so far. Be thorough. Include all important context, "+
			"decisions made, files modified, and any ongoing tasks.")

	reqMsgs := make([]model.Message, 0, len(msgs)+1)
	reqMsgs = append(reqMsgs, msgs...)
	reqMsgs = append(reqMsgs, instruction)

	req := &provider.Request{
		Messages:  reqMsgs,
		System:    system,
		MaxTokens: 4096,
	}
	if c.Provider != nil {
		// Use the first available model name from the provider.
		req.Model = ""
	}

	events, err := c.Provider.Stream(ctx, req)
	if err != nil {
		return "", err
	}

	resp, err := provider.Collect(events)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	for _, block := range resp.Message.Content {
		if block.Type == model.ContentTypeText {
			sb.WriteString(block.Text)
		}
	}
	return sb.String(), nil
}

// estimateTokens produces a rough token count for a slice of messages.
// Uses 4 chars-per-token for text and 1200 tokens for each image.
func estimateTokens(msgs []model.Message) int {
	total := 0
	for _, msg := range msgs {
		for _, block := range msg.Content {
			switch block.Type {
			case model.ContentTypeText:
				total += len(block.Text) / charsPerToken
			case model.ContentTypeImage:
				total += imageTokenEstimate
			case model.ContentTypeToolUse:
				total += len(block.Input) / charsPerToken
			case model.ContentTypeToolResult:
				for _, sub := range block.Content {
					total += len(sub.Text) / charsPerToken
				}
			case model.ContentTypeThinking:
				total += len(block.Thinking) / charsPerToken
			}
		}
	}
	return total
}
