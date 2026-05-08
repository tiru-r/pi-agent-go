package agent

import (
	"context"
	"fmt"

	"github.com/tiru-r/pi-agent-go/internal/config"
	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/provider"
	"github.com/tiru-r/pi-agent-go/internal/session"
	"github.com/tiru-r/pi-agent-go/internal/tools"
)

const (
	defaultMaxIter    = 20
	maxToolConcurrency = 4
)

// SessionAgent is a session-aware agent that persists conversation history
// to a Session and drives the full agentic loop.
type SessionAgent struct {
	Provider provider.Provider
	Tools    []tools.Tool
	Session  *session.Session
	Config   *config.Config
	MaxIter  int // default 20, prevents infinite loops
}

// SessionRunOptions configures a single SessionAgent.Run call.
type SessionRunOptions struct {
	System    string
	Model     string
	MaxTokens int
	Thinking  model.ThinkingLevel
}

// Run executes one full agent turn: sends input, streams response, executes tools,
// repeats until stop reason is end_turn or MaxIter is exceeded.
// onEvent is called for each streaming event so callers can update the UI.
func (a *SessionAgent) Run(
	ctx context.Context,
	input string,
	opts SessionRunOptions,
	onEvent func(provider.Event),
) error {
	maxIter := a.MaxIter
	if maxIter <= 0 {
		maxIter = defaultMaxIter
	}

	// Append user message to session.
	userMsg := model.NewTextMessage(model.RoleUser, input)
	if err := a.Session.AppendMessage(userMsg, nil); err != nil {
		return fmt.Errorf("session_agent: append user message: %w", err)
	}

	modelName := opts.Model
	if modelName == "" && a.Config != nil {
		modelName = a.Config.Model
	}
	maxTokens := opts.MaxTokens
	if maxTokens == 0 && a.Config != nil {
		maxTokens = a.Config.MaxTokens
	}
	if maxTokens == 0 {
		maxTokens = 8096
	}
	thinking := opts.Thinking
	if thinking == "" {
		thinking = model.ThinkingLevelOff
	}

	// Build tool definitions from the agent's tool set (or all built-ins).
	var toolDefs []model.ToolDefinition
	if len(a.Tools) > 0 {
		for _, t := range a.Tools {
			toolDefs = append(toolDefs, model.ToolDefinition{
				Name:        t.Name(),
				Description: t.Description(),
				InputSchema: t.Schema(),
			})
		}
	} else {
		toolDefs = tools.ToDefinitions()
	}

	for iter := 0; iter < maxIter; iter++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msgs := a.Session.Messages()

		req := &provider.Request{
			Model:         modelName,
			Messages:      msgs,
			System:        opts.System,
			Tools:         toolDefs,
			MaxTokens:     maxTokens,
			ThinkingLevel: thinking,
		}

		events, err := a.Provider.Stream(ctx, req)
		if err != nil {
			return fmt.Errorf("session_agent: stream: %w", err)
		}

		// Tee events to caller while collecting.
		fanned := teeEvents(events, onEvent)
		resp, err := provider.Collect(fanned)
		if err != nil {
			return fmt.Errorf("session_agent: collect: %w", err)
		}

		// Append assistant response to session.
		if err := a.Session.AppendMessage(resp.Message, &resp.Usage); err != nil {
			return fmt.Errorf("session_agent: append assistant message: %w", err)
		}

		if resp.StopReason != model.StopReasonToolUse {
			return nil // end_turn, max_tokens, or stop_sequence
		}

		// Execute all tool calls in parallel (up to maxToolConcurrency).
		toolUses := resp.Message.ToolUses()
		if len(toolUses) == 0 {
			return nil
		}

		results := a.runTools(ctx, toolUses)

		// Build and persist the tool_result message.
		toolResultMsg := model.Message{
			Role:    model.RoleUser,
			Content: results,
		}
		if err := a.Session.AppendMessage(toolResultMsg, nil); err != nil {
			return fmt.Errorf("session_agent: append tool results: %w", err)
		}
	}

	return fmt.Errorf("session_agent: max iterations (%d) exceeded", maxIter)
}

// runTools executes all tool uses concurrently and returns result ContentBlocks.
func (a *SessionAgent) runTools(ctx context.Context, uses []model.ContentBlock) []model.ContentBlock {
	results := make([]model.ContentBlock, len(uses))
	sem := make(chan struct{}, maxToolConcurrency)

	done := make(chan struct{})
	for i, use := range uses {
		i, use := i, use
		go func() {
			sem <- struct{}{}
			defer func() { <-sem; done <- struct{}{} }()
			results[i] = a.runTool(ctx, use)
		}()
	}
	for range uses {
		<-done
	}
	return results
}

// runTool executes a single tool call and returns a tool_result ContentBlock.
func (a *SessionAgent) runTool(ctx context.Context, block model.ContentBlock) model.ContentBlock {
	result := model.ContentBlock{
		Type:      model.ContentTypeToolResult,
		ToolUseID: block.ID,
	}

	// Resolve the tool.
	var t tools.Tool
	for _, candidate := range a.Tools {
		if candidate.Name() == block.Name {
			t = candidate
			break
		}
	}
	if t == nil {
		// Fall back to built-in tools.
		t2, ok := tools.Get(block.Name)
		if !ok {
			result.IsError = true
			result.Content = []model.ContentBlock{{
				Type: model.ContentTypeText,
				Text: fmt.Sprintf("unknown tool: %s", block.Name),
			}}
			return result
		}
		t = t2
	}

	params := block.Input
	if params == nil {
		params = []byte("{}")
	}

	res, err := t.Execute(ctx, params)
	if err != nil {
		result.IsError = true
		result.Content = []model.ContentBlock{{
			Type: model.ContentTypeText,
			Text: "tool execution error: " + err.Error(),
		}}
		return result
	}

	result.IsError = res.IsError
	result.Content = res.Content
	return result
}

// teeEvents fans an event channel to the optional onEvent callback and
// returns a new channel that can be drained by provider.Collect.
func teeEvents(in <-chan provider.Event, onEvent func(provider.Event)) <-chan provider.Event {
	out := make(chan provider.Event, 64)
	go func() {
		defer close(out)
		for ev := range in {
			if onEvent != nil {
				onEvent(ev)
			}
			out <- ev
		}
	}()
	return out
}
