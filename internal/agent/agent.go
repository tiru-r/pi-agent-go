// Package agent orchestrates the agentic loop: call the LLM, run tools,
// feed results back, and repeat until the model stops or the context is cancelled.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/provider"
	"github.com/tiru-r/pi-agent-go/internal/runtime"
	"github.com/tiru-r/pi-agent-go/internal/tools"
)

// Options controls a single agent run.
type Options struct {
	// System overrides the system prompt from config for this run only.
	System string
	// ThinkingLevel overrides the thinking level for this run only.
	ThinkingLevel model.ThinkingLevel
	// MaxTurns caps the number of tool-use / response cycles (0 = unlimited).
	MaxTurns int
	// Tools lists which tool names are enabled. nil = all built-in tools.
	Tools []string
}

// EventKind tags the kind of event emitted by the agent.
type EventKind string

const (
	EventKindText        EventKind = "text"
	EventKindThinking    EventKind = "thinking"
	EventKindToolStart   EventKind = "tool_start"
	EventKindToolDone    EventKind = "tool_done"
	EventKindError       EventKind = "error"
	EventKindDone        EventKind = "done"
)

// AgentEvent is emitted by the agent during a Run.
type AgentEvent struct {
	Kind EventKind

	// EventKindText / EventKindThinking
	Delta string

	// EventKindToolStart
	ToolID   string
	ToolName string

	// EventKindToolDone
	ToolResult model.ContentBlock

	// EventKindDone
	Usage      model.Usage
	StopReason model.StopReason

	// EventKindError
	Err error
}

// Agent drives the LLM ↔ tool loop.
type Agent struct {
	prov   provider.Provider
	model  string
	system string
	maxTok int

	// Monitor is optional; attach one to enable runtime intelligence.
	Monitor *runtime.Monitor

	// Compactor is optional; when set it compacts message history before each
	// LLM call if the estimated token count exceeds the threshold.
	Compactor *Compactor
}

// New constructs an Agent backed by the given provider.
func New(prov provider.Provider, modelName, system string, maxTokens int) *Agent {
	return &Agent{
		prov:   prov,
		model:  modelName,
		system: system,
		maxTok: maxTokens,
	}
}

// Run executes the agent loop for the given input, appending to history.
// Events are dispatched to onEvent synchronously as they arrive.
// history is the prior conversation; the new user message is prepended automatically.
// Returns the updated message history (including the new turn).
func (a *Agent) Run(
	ctx context.Context,
	input string,
	history []model.Message,
	opts Options,
	onEvent func(AgentEvent),
) ([]model.Message, error) {
	systemPrompt := a.system
	if opts.System != "" {
		systemPrompt = opts.System
	}
	thinkLevel := model.ThinkingLevelOff
	if opts.ThinkingLevel != "" {
		thinkLevel = opts.ThinkingLevel
	}
	maxTurns := opts.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 20
	}

	// Build initial message list.
	msgs := make([]model.Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	msgs = append(msgs, model.NewTextMessage(model.RoleUser, input))

	// Resolve tool set.
	toolDefs := resolveTools(opts.Tools)

	for turn := 0; turn < maxTurns; turn++ {
		select {
		case <-ctx.Done():
			return msgs, ctx.Err()
		default:
		}

		// Compact history before calling the LLM if it's grown too large.
		if a.Compactor != nil && a.Compactor.ShouldCompact(msgs, 0) {
			if compacted, _, compactErr := a.Compactor.Compact(ctx, msgs, systemPrompt); compactErr == nil {
				msgs = compacted
			}
		}

		req := &provider.Request{
			Model:         a.model,
			Messages:      msgs,
			System:        systemPrompt,
			Tools:         toolDefs,
			MaxTokens:     a.maxTok,
			ThinkingLevel: thinkLevel,
		}

		t0 := time.Now()
		eventCh, err := a.prov.Stream(ctx, req)
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
			onEvent(AgentEvent{Kind: EventKindError, Err: err})
			return msgs, err
		}

		// Drain the stream, collecting a complete response message while
		// forwarding incremental events to the caller.
		resp, err := drainStream(ctx, eventCh, onEvent)
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
			return msgs, err
		}

		// Add the assistant message to history.
		msgs = append(msgs, resp.Message)

		// If there are no tool calls we're done.
		if resp.StopReason != model.StopReasonToolUse {
			onEvent(AgentEvent{
				Kind:       EventKindDone,
				Usage:      resp.Usage,
				StopReason: resp.StopReason,
			})
			return msgs, nil
		}

		// Halt before running tools if the runtime monitor signals a safety veto.
		if a.Monitor != nil && a.Monitor.ShouldVeto() {
			err := fmt.Errorf("agent: runtime safety veto — error rate exceeded threshold, halting tool execution")
			onEvent(AgentEvent{Kind: EventKindError, Err: err})
			return msgs, err
		}

		// Execute tool calls and collect results.
		toolResults, err := executeTools(ctx, resp.Message.Content, onEvent, a.Monitor, len(msgs))
		if err != nil {
			return msgs, err
		}
		if len(toolResults) > 0 {
			msgs = append(msgs, model.Message{
				Role:    model.RoleUser,
				Content: toolResults,
			})
		}
	}

	err := fmt.Errorf("agent: max turns (%d) reached without completion", maxTurns)
	onEvent(AgentEvent{Kind: EventKindError, Err: err})
	return msgs, err
}

// drainStream reads from the event channel forwarding events and accumulates
// the full response.
func drainStream(
	ctx context.Context,
	ch <-chan provider.Event,
	onEvent func(AgentEvent),
) (*provider.Response, error) {
	var (
		blocks      []model.ContentBlock
		textIdx     = -1
		thinkingIdx = -1
		toolBlocks  = map[int]*model.ContentBlock{}
		toolOrder   []int // indices in completion order for stable sorting
		usage       model.Usage
		stop        model.StopReason
	)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				goto done
			}
			switch ev.Type {
			case provider.EventError:
				onEvent(AgentEvent{Kind: EventKindError, Err: ev.Err})
				return nil, ev.Err

			case provider.EventTextDelta:
				if textIdx < 0 {
					blocks = append(blocks, model.ContentBlock{Type: model.ContentTypeText})
					textIdx = len(blocks) - 1
				}
				blocks[textIdx].Text += ev.Text
				onEvent(AgentEvent{Kind: EventKindText, Delta: ev.Text})

			case provider.EventThinkingDelta:
				if thinkingIdx < 0 {
					blocks = append(blocks, model.ContentBlock{Type: model.ContentTypeThinking})
					thinkingIdx = len(blocks) - 1
				}
				blocks[thinkingIdx].Thinking += ev.Text
				onEvent(AgentEvent{Kind: EventKindThinking, Delta: ev.Text})

			case provider.EventToolCallStart:
				tb := &model.ContentBlock{
					Type: model.ContentTypeToolUse,
					ID:   ev.ToolID,
					Name: ev.ToolName,
				}
				toolBlocks[ev.ToolIndex] = tb
				onEvent(AgentEvent{
					Kind:     EventKindToolStart,
					ToolID:   ev.ToolID,
					ToolName: ev.ToolName,
				})

			case provider.EventToolCallDelta:
				if tb, ok := toolBlocks[ev.ToolIndex]; ok {
					tb.Input = append(tb.Input, []byte(ev.PartialJSON)...)
				}

			case provider.EventToolCallDone:
				if _, ok := toolBlocks[ev.ToolIndex]; ok {
					toolOrder = append(toolOrder, ev.ToolIndex)
				}

			case provider.EventMessageStop:
				stop = ev.StopReason
				usage = ev.Usage
			}
		}
	}
done:
	// Append tool blocks sorted by their stream index so ordering is stable
	// even if EventToolCallDone events arrive out of sequence.
	sort.Ints(toolOrder)
	for _, idx := range toolOrder {
		blocks = append(blocks, *toolBlocks[idx])
	}
	return &provider.Response{
		Message:    model.Message{Role: model.RoleAssistant, Content: blocks},
		StopReason: stop,
		Usage:      usage,
	}, nil
}

// executeTools runs all tool_use blocks from the assistant message.
func executeTools(
	ctx context.Context,
	blocks []model.ContentBlock,
	onEvent func(AgentEvent),
	mon *runtime.Monitor,
	msgCount int,
) ([]model.ContentBlock, error) {
	var results []model.ContentBlock

	for _, block := range blocks {
		if block.Type != model.ContentTypeToolUse {
			continue
		}

		t, ok := tools.Get(block.Name)
		if !ok {
			// Return an error result for unknown tools.
			errText := fmt.Sprintf("unknown tool: %s", block.Name)
			result := model.ContentBlock{
				Type:      model.ContentTypeToolResult,
				ToolUseID: block.ID,
				Content: []model.ContentBlock{
					{Type: model.ContentTypeText, Text: errText},
				},
				IsError: true,
			}
			if mon != nil {
				mon.Observe(runtime.Observation{
					Time:    time.Now(),
					Stage:   "tool:" + block.Name,
					Latency: 0,
					Weight:  float64(msgCount),
					Success: false,
				})
			}
			onEvent(AgentEvent{Kind: EventKindToolDone, ToolResult: result})
			results = append(results, result)
			continue
		}

		params := block.Input
		if params == nil {
			params = json.RawMessage("{}")
		}

		t0tool := time.Now()
		toolResult, err := t.Execute(ctx, params)
		toolLatency := time.Since(t0tool)

		var resultContent []model.ContentBlock
		isError := false
		if err != nil {
			isError = true
			resultContent = []model.ContentBlock{
				{Type: model.ContentTypeText, Text: "tool error: " + err.Error()},
			}
		} else if toolResult != nil {
			isError = toolResult.IsError
			resultContent = toolResult.Content
		}

		if mon != nil {
			mon.Observe(runtime.Observation{
				Time:    t0tool,
				Stage:   "tool:" + block.Name,
				Latency: toolLatency,
				Weight:  float64(msgCount),
				Success: !isError,
			})
		}

		result := model.ContentBlock{
			Type:      model.ContentTypeToolResult,
			ToolUseID: block.ID,
			Content:   resultContent,
			IsError:   isError,
		}
		onEvent(AgentEvent{Kind: EventKindToolDone, ToolResult: result})
		results = append(results, result)
	}

	return results, nil
}

// resolveTools returns the tool definitions for the given name list.
// A nil list returns all built-in tools.
func resolveTools(names []string) []model.ToolDefinition {
	if names == nil {
		return tools.ToDefinitions()
	}
	var defs []model.ToolDefinition
	for _, name := range names {
		if t, ok := tools.Get(name); ok {
			defs = append(defs, model.ToolDefinition{
				Name:        t.Name(),
				Description: t.Description(),
				InputSchema: t.Schema(),
			})
		}
	}
	return defs
}
