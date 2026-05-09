package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/provider"
)

const (
	charsPerToken              = 4
	imageTokenEstimate         = 1200
	defaultMaxCompactionTokens = 100_000
	defaultReserveTokens       = 5_000
	defaultKeepLast            = 10
)

// Compactor decides when and how to compact (summarise) a conversation's
// message history so it stays within the provider's context window.
type Compactor struct {
	Provider      provider.Provider
	Model         string // model to use for summarisation (defaults to provider default)
	ContextWindow int    // model's actual context window in tokens (0 = unknown)
	ReserveTokens int    // tokens to keep free for new output (default 5000)
	MaxTokens     int    // fallback trigger threshold when ContextWindow is 0 (default 100_000)
	KeepLast      int    // always keep the last N messages intact (default 10)
}

// threshold returns the token count above which compaction is triggered.
// When ContextWindow is known the trigger is ContextWindow - ReserveTokens;
// otherwise it falls back to MaxTokens (or 100 000).
func (c *Compactor) threshold() int {
	if c.ContextWindow > 0 {
		reserve := c.ReserveTokens
		if reserve <= 0 {
			reserve = defaultReserveTokens
		}
		return c.ContextWindow - reserve
	}
	if c.MaxTokens > 0 {
		return c.MaxTokens
	}
	return defaultMaxCompactionTokens
}

// ShouldCompact returns true when the token count exceeds the threshold.
// measuredTokens should be the InputTokens value from the last API response
// Usage field; pass 0 to fall back to the chars÷4 heuristic.
func (c *Compactor) ShouldCompact(msgs []model.Message, measuredTokens int) bool {
	tokens := measuredTokens
	if tokens <= 0 {
		tokens = estimateTokens(msgs)
	}
	return tokens > c.threshold()
}

// Compact summarises the older portion of msgs and returns a shortened list.
//
// The cut point is aligned to a clean turn boundary: it falls after an
// assistant message and before a real user prompt (never inside a
// tool-use / tool-result pair). If no clean boundary can be found the
// original slice is returned unchanged.
//
// The returned list is:
//
//	[{role:user, "[Previous conversation summary]\n\n<summary>"}, ...keptMsgs]
//
// The second return value is the summary text; the caller may persist it as
// a session compaction entry.
func (c *Compactor) Compact(ctx context.Context, msgs []model.Message, system string) ([]model.Message, string, error) {
	keepLast := c.KeepLast
	if keepLast <= 0 {
		keepLast = defaultKeepLast
	}

	cut := findCutPoint(msgs, keepLast)
	if cut == 0 {
		return msgs, "", nil
	}

	toSummarise := msgs[:cut]
	kept := msgs[cut:]

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

// findCutPoint returns the index of the first message to keep (msgs[:cut] is
// summarised). The cut lands on a clean turn boundary — after an assistant
// message, before a real user prompt (not a tool_result). It starts at
// len(msgs)-keepLast and walks backward to avoid splitting a tool exchange.
// Returns 0 if no clean boundary exists (caller should skip compaction).
func findCutPoint(msgs []model.Message, keepLast int) int {
	target := len(msgs) - keepLast
	if target <= 0 {
		return 0
	}
	for i := target; i > 0; i-- {
		if msgs[i-1].Role == model.RoleAssistant && isRealUserMessage(msgs[i]) {
			return i
		}
	}
	return 0
}

// isRealUserMessage returns true when msg is a genuine user prompt, not a
// tool_result reply injected by the agent loop.
func isRealUserMessage(msg model.Message) bool {
	if msg.Role != model.RoleUser {
		return false
	}
	for _, b := range msg.Content {
		if b.Type == model.ContentTypeToolResult {
			return false
		}
	}
	return true
}

// summarise calls the provider with a summarisation request for toSummarise.
// File paths touched by Read / Write / Edit tool calls are extracted and
// appended to the prompt so the summary preserves file-operation context.
func (c *Compactor) summarise(ctx context.Context, msgs []model.Message, system string) (string, error) {
	readFiles, modFiles := extractFilePaths(msgs)

	var promptSB strings.Builder
	promptSB.WriteString("Summarize this conversation so far. Be thorough. Include all important context, " +
		"decisions made, files modified, and any ongoing tasks.")
	if len(readFiles) > 0 {
		promptSB.WriteString("\n\n<read-files>\n")
		for _, p := range readFiles {
			promptSB.WriteString(p)
			promptSB.WriteByte('\n')
		}
		promptSB.WriteString("</read-files>")
	}
	if len(modFiles) > 0 {
		promptSB.WriteString("\n\n<modified-files>\n")
		for _, p := range modFiles {
			promptSB.WriteString(p)
			promptSB.WriteByte('\n')
		}
		promptSB.WriteString("</modified-files>")
	}

	reqMsgs := make([]model.Message, 0, len(msgs)+1)
	reqMsgs = append(reqMsgs, msgs...)
	reqMsgs = append(reqMsgs, model.NewTextMessage(model.RoleUser, promptSB.String()))

	req := &provider.Request{
		Model:     c.Model,
		Messages:  reqMsgs,
		System:    system,
		MaxTokens: 4096,
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

// extractFilePaths scans tool_use blocks in msgs and returns sorted slices of
// read and modified file paths, recognised via the standard "file_path"
// parameter of the Read, Write, and Edit tools.
func extractFilePaths(msgs []model.Message) (read, modified []string) {
	readSet := make(map[string]bool)
	modSet := make(map[string]bool)

	for _, msg := range msgs {
		for _, block := range msg.Content {
			if block.Type != model.ContentTypeToolUse || len(block.Input) == 0 {
				continue
			}
			var params map[string]any
			if json.Unmarshal(block.Input, &params) != nil {
				continue
			}
			path, _ := params["file_path"].(string)
			if path == "" {
				continue
			}
			switch strings.ToLower(block.Name) {
			case "read":
				readSet[path] = true
			case "write", "edit":
				modSet[path] = true
			}
		}
	}

	for p := range readSet {
		read = append(read, p)
	}
	for p := range modSet {
		modified = append(modified, p)
	}
	sort.Strings(read)
	sort.Strings(modified)
	return
}

// estimateTokens produces a rough token count for a slice of messages using
// the chars÷4 heuristic for text and a flat 1 200 tokens per image.
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
