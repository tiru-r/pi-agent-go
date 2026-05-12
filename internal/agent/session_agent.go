package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tiru-r/pi-agent-go/internal/config"
	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/provider"
	"github.com/tiru-r/pi-agent-go/internal/runtime"
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
	Provider  provider.Provider
	Tools     []tools.Tool
	Session   *session.Session
	Config    *config.Config
	MaxIter   int // default 20, prevents infinite loops
	Compactor *Compactor

	// Monitor is optional; attach one to enable runtime intelligence.
	Monitor *runtime.Monitor

	// Hooks is optional; wire an extensions.Manager to broadcast tool lifecycle
	// events to JS extensions that define before_tool / after_tool.
	Hooks HookRunner
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

	// lastMeasuredTokens holds the InputTokens value from the previous API
	// response. When non-zero it is used instead of the heuristic estimator.
	var lastMeasuredTokens int

	for iter := 0; iter < maxIter; iter++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msgs := a.Session.Messages()

		// Compact history if it has grown past the threshold.
		if a.Compactor != nil && a.Compactor.ShouldCompact(msgs, lastMeasuredTokens) {
			if compacted, summary, compactErr := a.Compactor.Compact(ctx, msgs, opts.System); compactErr == nil {
				msgs = compacted
				lastMeasuredTokens = estimateTokens(compacted) // prime heuristic from compacted size
				_ = a.Session.Append(session.Entry{
					Type:    session.EntryCompaction,
					Summary: summary,
				})
			}
		}

		req := &provider.Request{
			Model:         modelName,
			Messages:      msgs,
			System:        opts.System,
			Tools:         toolDefs,
			MaxTokens:     maxTokens,
			ThinkingLevel: thinking,
		}

		t0 := time.Now()
		events, err := a.Provider.Stream(ctx, req)
		if err != nil {
			if a.Monitor != nil {
				a.Monitor.Observe(runtime.Observation{
					Time:    t0,
					Stage:   "llm",
					Latency: time.Since(t0),
					Weight:  float64(len(msgs)),
					Success: false,
				})
			}
			return fmt.Errorf("session_agent: stream: %w", err)
		}

		// Tee events to caller while collecting.
		fanned := teeEvents(events, onEvent)
		resp, err := provider.Collect(fanned)
		if a.Monitor != nil {
			a.Monitor.Observe(runtime.Observation{
				Time:    t0,
				Stage:   "llm",
				Latency: time.Since(t0),
				Weight:  float64(len(msgs)),
				Success: err == nil,
			})
		}
		if err != nil {
			return fmt.Errorf("session_agent: collect: %w", err)
		}

		// Update measured token count for the next compaction check.
		if resp.Usage.InputTokens > 0 {
			lastMeasuredTokens = resp.Usage.InputTokens
		}

		// Append assistant response to session.
		if err := a.Session.AppendMessage(resp.Message, &resp.Usage); err != nil {
			return fmt.Errorf("session_agent: append assistant message: %w", err)
		}

		if resp.StopReason != model.StopReasonToolUse {
			return nil // end_turn, max_tokens, or stop_sequence
		}

		// Halt before running tools if the runtime monitor signals a safety veto.
		if a.Monitor != nil && a.Monitor.ShouldVeto() {
			return fmt.Errorf("session_agent: runtime safety veto — error rate exceeded threshold, halting tool execution")
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

// runTools executes all tool uses concurrently (up to maxToolConcurrency at a
// time) and returns result ContentBlocks in the same order as uses.
func (a *SessionAgent) runTools(ctx context.Context, uses []model.ContentBlock) []model.ContentBlock {
	results := make([]model.ContentBlock, len(uses))
	sem := make(chan struct{}, maxToolConcurrency)
	var wg sync.WaitGroup
	for i, use := range uses {
		wg.Add(1)
		go func(i int, block model.ContentBlock) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = model.ContentBlock{
					Type:      model.ContentTypeToolResult,
					ToolUseID: block.ID,
					IsError:   true,
					Content:   []model.ContentBlock{{Type: model.ContentTypeText, Text: ctx.Err().Error()}},
				}
				return
			}
			results[i] = a.runTool(ctx, block)
		}(i, use)
	}
	wg.Wait()
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

	if a.Hooks != nil {
		a.Hooks.RunBeforeTool(ctx, block.Name, params)
	}

	t0tool := time.Now()
	res, err := t.Execute(ctx, params)
	toolLatency := time.Since(t0tool)

	if err != nil {
		if a.Monitor != nil {
			a.Monitor.Observe(runtime.Observation{
				Time:    t0tool,
				Stage:   "tool:" + block.Name,
				Latency: toolLatency,
				Weight:  float64(len(a.Session.Messages())),
				Success: false,
			})
		}
		if a.Hooks != nil {
			a.Hooks.RunAfterTool(ctx, block.Name, "tool execution error: "+err.Error(), true)
		}
		result.IsError = true
		result.Content = []model.ContentBlock{{
			Type: model.ContentTypeText,
			Text: "tool execution error: " + err.Error(),
		}}
		return result
	}

	if a.Monitor != nil {
		a.Monitor.Observe(runtime.Observation{
			Time:    t0tool,
			Stage:   "tool:" + block.Name,
			Latency: toolLatency,
			Weight:  float64(len(a.Session.Messages())),
			Success: !res.IsError,
		})
	}

	if a.Hooks != nil {
		var sb strings.Builder
		for _, rc := range res.Content {
			if rc.Type == model.ContentTypeText {
				sb.WriteString(rc.Text)
			}
		}
		a.Hooks.RunAfterTool(ctx, block.Name, sb.String(), res.IsError)
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
