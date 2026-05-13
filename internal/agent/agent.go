// Package agent orchestrates the agentic loop: call the LLM, run tools,
// feed results back, and repeat until the model stops or the context is cancelled.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/provider"
	"github.com/tiru-r/pi-agent-go/internal/runtime"
	"github.com/tiru-r/pi-agent-go/internal/tools"
)

// AgentMode controls the execution strategy for a Run.
type AgentMode string

const (
	// AgentModeAct is the default: full agentic loop with all tools.
	AgentModeAct AgentMode = "act"
	// AgentModePlan outputs a plan only — no tools are executed.
	AgentModePlan AgentMode = "plan"
	// AgentModePlanAct plans first, then executes with tools.
	AgentModePlanAct AgentMode = "plan_act"
	// AgentModeInteractive describes each tool action before running it.
	AgentModeInteractive AgentMode = "interactive"
	// AgentModePipe is single-turn, no tools — pure Q&A / stdin→stdout.
	AgentModePipe AgentMode = "pipe"
	// AgentModeHandoff executes normally then emits a HANDOFF summary.
	AgentModeHandoff AgentMode = "handoff"
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
	// TurnContextBudget caps context size (InputTokens) in a single turn (0 = unlimited).
	// Checked pre-turn via estimate and post-turn via measured value.
	TurnContextBudget int
	// Mode selects the execution strategy (default: AgentModeAct).
	Mode AgentMode
}

// EventKind tags the kind of event emitted by the agent.
type EventKind string

const (
	EventKindText        EventKind = "text"
	EventKindThinking    EventKind = "thinking"
	EventKindToolStart   EventKind = "tool_start"
	EventKindToolExec    EventKind = "tool_exec" // full input ready, about to execute
	EventKindToolDone    EventKind = "tool_done"
	EventKindError       EventKind = "error"
	EventKindDone        EventKind = "done"
)

// AgentEvent is emitted by the agent during a Run.
type AgentEvent struct {
	Kind EventKind

	// EventKindText / EventKindThinking
	Delta string

	// EventKindToolStart / EventKindToolExec
	ToolID    string
	ToolName  string
	ToolInput json.RawMessage // populated for EventKindToolExec only

	// EventKindToolDone
	ToolResult model.ContentBlock

	// EventKindDone
	Usage      model.Usage
	StopReason model.StopReason

	// EventKindError
	Err error
}

// HookRunner is implemented by the extension manager to broadcast tool lifecycle
// events to all loaded extensions that define before_tool / after_tool hooks.
type HookRunner interface {
	RunBeforeTool(ctx context.Context, name string, params json.RawMessage)
	RunAfterTool(ctx context.Context, name string, result string, isError bool)
}

// Agent drives the LLM ↔ tool loop.
type Agent struct {
	prov   provider.Provider
	model  string
	system string
	maxTok int

	// Monitor is optional; attach one to enable runtime intelligence.
	Monitor *runtime.Monitor

	// Hooks is optional; wire an extensions.Manager to broadcast tool lifecycle
	// events to JS extensions that define before_tool / after_tool.
	Hooks HookRunner

	// Compactor is optional; when set it compacts message history when the
	// estimated token count exceeds the threshold.
	Compactor *Compactor

	// BGCompactor is optional; when set, compaction runs asynchronously so it
	// does not block the foreground turn. The result is applied at the start of
	// the next turn. Requires Compactor to also be set.
	BGCompactor *BackgroundCompactor

	// TokenBudget caps total cumulative InputTokens for a Run (0 = unlimited).
	TokenBudget int

	// RetryAttempts is the number of times to retry a failed provider.Stream call
	// for retriable errors (429, 5xx, timeouts). 0 defaults to defaultRetryAttempts (3).
	RetryAttempts int
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
	// Wrap onEvent once so tool goroutines can call it concurrently without
	// the caller needing to worry about thread safety.
	var evMu sync.Mutex
	safeEmit := func(ev AgentEvent) {
		evMu.Lock()
		onEvent(ev)
		evMu.Unlock()
	}

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

	// Apply mode overrides before resolving tools so Plan/Pipe skip the lookup.
	var toolDefs []model.ToolDefinition
	switch opts.Mode {
	case AgentModePlan:
		// No tools — forces end_turn after the plan text.
		maxTurns = 1
		systemPrompt = modePrefix("You are in PLAN MODE. Do not use any tools. Output a detailed numbered plan of exactly what you would do to complete this task.", systemPrompt)
	case AgentModePipe:
		// No tools — single-turn pass-through.
		maxTurns = 1
	case AgentModePlanAct:
		toolDefs = resolveTools(opts.Tools)
		systemPrompt = modePrefix("First write a concise numbered plan of your approach. Then execute each step using the available tools.", systemPrompt)
	case AgentModeInteractive:
		toolDefs = resolveTools(opts.Tools)
		systemPrompt = modePrefix("You are in INTERACTIVE MODE. Before invoking any tool, briefly describe what you are about to do and why, then proceed.", systemPrompt)
	case AgentModeHandoff:
		toolDefs = resolveTools(opts.Tools)
		systemPrompt = modeSuffix(systemPrompt, "When your task is complete, output a HANDOFF section with a concise state summary so another agent can continue from where you left off.")
	default: // AgentModeAct and unset ("")
		toolDefs = resolveTools(opts.Tools)
	}

	// lastMeasuredTokens holds the InputTokens value from the previous API
	// response. When non-zero it is used instead of the heuristic estimator.
	var lastMeasuredTokens int
	var cumulativeUsage model.Usage
	toolDefsTokens := EstimateToolDefsTokens(toolDefs)

	for turn := 0; turn < maxTurns; turn++ {
		select {
		case <-ctx.Done():
			return msgs, ctx.Err()
		default:
		}

		// Apply any pending background compaction result (non-blocking).
		if a.BGCompactor != nil {
			if result := a.BGCompactor.Take(); result != nil {
				newMsgs := msgs[result.SnapLen:]
				msgs = append(result.Compacted, newMsgs...)
				lastMeasuredTokens = estimateTokens(msgs)
			}
		}
		// Compact history before calling the LLM; use background worker if available.
		if a.Compactor != nil && a.Compactor.ShouldCompact(msgs, lastMeasuredTokens, toolDefsTokens) {
			if a.BGCompactor != nil {
				a.BGCompactor.Trigger(msgs, systemPrompt, "")
			} else if compacted, _, compactErr := a.Compactor.Compact(ctx, msgs, systemPrompt); compactErr == nil {
				msgs = compacted
				lastMeasuredTokens = estimateTokens(compacted)
			}
		}

		// Pre-turn: enforce context budget before spending a round-trip.
		if opts.TurnContextBudget > 0 {
			est := lastMeasuredTokens
			if est <= 0 {
				est = estimateTokens(msgs) + toolDefsTokens
			}
			if est > opts.TurnContextBudget {
				budgetErr := fmt.Errorf("agent: context budget exceeded before turn %d (%d/%d tokens estimated)",
					turn+1, est, opts.TurnContextBudget)
				onEvent(AgentEvent{Kind: EventKindError, Err: budgetErr})
				return msgs, budgetErr
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
		eventCh, err := streamWithRetry(ctx, a.prov, req, a.RetryAttempts)
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

		// Update measured token count and cumulative usage for budget tracking.
		if resp.Usage.InputTokens > 0 {
			lastMeasuredTokens = resp.Usage.InputTokens
			cumulativeUsage = cumulativeUsage.Add(resp.Usage)

			// Enforce cumulative token budget.
			if a.TokenBudget > 0 && cumulativeUsage.InputTokens > a.TokenBudget {
				budgetErr := fmt.Errorf("agent: token budget exceeded (%d/%d input tokens consumed)",
					cumulativeUsage.InputTokens, a.TokenBudget)
				onEvent(AgentEvent{Kind: EventKindError, Err: budgetErr})
				return msgs, budgetErr
			}

			// Post-turn: verify context budget with accurate measured tokens.
			if opts.TurnContextBudget > 0 && resp.Usage.InputTokens > opts.TurnContextBudget {
				budgetErr := fmt.Errorf("agent: context budget exceeded in turn %d (%d/%d input tokens)",
					turn+1, resp.Usage.InputTokens, opts.TurnContextBudget)
				onEvent(AgentEvent{Kind: EventKindError, Err: budgetErr})
				return msgs, budgetErr
			}
		}

		// Add the assistant message to history.
		msgs = append(msgs, resp.Message)

		// If there are no tool calls we're done.
		if resp.StopReason != model.StopReasonToolUse {
			onEvent(AgentEvent{
				Kind:       EventKindDone,
				Usage:      cumulativeUsage,
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
		// safeEmit is passed so goroutines inside executeTools can emit events
		// without the caller needing to handle concurrent access.
		toolResults, err := executeTools(ctx, resp.Message.Content, safeEmit, a.Monitor, a.Hooks, len(msgs))
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

	// Emit final usage before the error so callers can account for consumed tokens.
	onEvent(AgentEvent{Kind: EventKindDone, Usage: cumulativeUsage})
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

loop:
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				break loop
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

// executeTools runs all tool_use blocks from the assistant message in parallel
// (up to maxToolConcurrency goroutines) and returns results in original order.
func executeTools(
	ctx context.Context,
	blocks []model.ContentBlock,
	onEvent func(AgentEvent),
	mon *runtime.Monitor,
	hooks HookRunner,
	msgCount int,
) ([]model.ContentBlock, error) {
	// Collect tool-use blocks preserving original order.
	type call struct{ block model.ContentBlock }
	var calls []call
	for _, b := range blocks {
		if b.Type == model.ContentTypeToolUse {
			calls = append(calls, call{b})
		}
	}
	if len(calls) == 0 {
		return nil, nil
	}

	results := make([]model.ContentBlock, len(calls))

	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func(i int, block model.ContentBlock) {
			defer wg.Done()

			// bash spawns real OS processes; cap concurrency to avoid saturation.
			// All other tools are I/O-bound and run without a semaphore.
			if block.Name == "bash" {
				// bashSem is declared in session_agent.go; caps concurrent bash process spawning.
				select {
				case bashSem <- struct{}{}:
					defer func() { <-bashSem }()
				case <-ctx.Done():
					result := model.ContentBlock{
						Type:      model.ContentTypeToolResult,
						ToolUseID: block.ID,
						Content:   []model.ContentBlock{{Type: model.ContentTypeText, Text: ctx.Err().Error()}},
						IsError:   true,
					}
					onEvent(AgentEvent{Kind: EventKindToolDone, ToolResult: result})
					results[i] = result
					return
				}
			} else if err := ctx.Err(); err != nil {
				result := model.ContentBlock{
					Type:      model.ContentTypeToolResult,
					ToolUseID: block.ID,
					Content:   []model.ContentBlock{{Type: model.ContentTypeText, Text: err.Error()}},
					IsError:   true,
				}
				onEvent(AgentEvent{Kind: EventKindToolDone, ToolResult: result})
				results[i] = result
				return
			}

			t, ok := tools.Get(block.Name)
			if !ok {
				result := model.ContentBlock{
					Type:      model.ContentTypeToolResult,
					ToolUseID: block.ID,
					Content:   []model.ContentBlock{{Type: model.ContentTypeText, Text: fmt.Sprintf("unknown tool: %s", block.Name)}},
					IsError:   true,
				}
				if mon != nil {
					mon.Observe(runtime.Observation{
						Time:    time.Now(),
						Stage:   "tool:" + block.Name,
						Weight:  float64(msgCount),
						Success: false,
					})
				}
				onEvent(AgentEvent{Kind: EventKindToolDone, ToolResult: result})
				results[i] = result
				return
			}

			params := block.Input
			if params == nil {
				params = json.RawMessage("{}")
			}

			if hooks != nil {
				hooks.RunBeforeTool(ctx, block.Name, params)
			}

			onEvent(AgentEvent{
				Kind:      EventKindToolExec,
				ToolID:    block.ID,
				ToolName:  block.Name,
				ToolInput: params,
			})

			t0 := time.Now()
			toolResult, err := t.Execute(ctx, params)
			latency := time.Since(t0)

			var resultContent []model.ContentBlock
			isError := false
			if err != nil {
				isError = true
				resultContent = []model.ContentBlock{{Type: model.ContentTypeText, Text: "tool error: " + err.Error()}}
			} else if toolResult != nil {
				isError = toolResult.IsError
				resultContent = toolResult.Content
			}

			if hooks != nil {
				var sb strings.Builder
				for _, rc := range resultContent {
					if rc.Type == model.ContentTypeText {
						sb.WriteString(rc.Text)
					}
				}
				hooks.RunAfterTool(ctx, block.Name, sb.String(), isError)
			}

			if mon != nil {
				mon.Observe(runtime.Observation{
					Time:    t0,
					Stage:   "tool:" + block.Name,
					Latency: latency,
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
			results[i] = result
		}(i, c.block)
	}

	wg.Wait()
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

// modePrefix prepends instruction to system, separated by a blank line.
func modePrefix(instruction, system string) string {
	if system == "" {
		return instruction
	}
	return instruction + "\n\n" + system
}

// modeSuffix appends instruction to system, separated by a blank line.
func modeSuffix(system, instruction string) string {
	if system == "" {
		return instruction
	}
	return system + "\n\n" + instruction
}
