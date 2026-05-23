// Package agent orchestrates the agentic loop: call the LLM, run tools,
// feed results back, and repeat until the model stops or the context is cancelled.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
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

// DefaultMaxTokens is the output token cap used when no explicit limit is configured.
const DefaultMaxTokens = 8096

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
	// MaxTurns caps the number of tool-use / response cycles.
	// 0 defers to Profile.RecommendedMaxTurns (0 profile = unlimited).
	// -1 forces unlimited regardless of the profile.
	// Set a positive value to enforce a hard limit.
	MaxTurns int
	// Tools lists which tool names are enabled. nil = all built-in tools.
	Tools []string
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
//
// Static configuration (model, prompts, tools) lives here.
// Runtime capabilities (lifecycle, budget, telemetry) travel in the AgentCx
// passed to Run — construct the AgentCx at the RPC/CLI entry point and pass
// it unchanged into every subsystem boundary.
type Agent struct {
	prov   provider.Provider
	model  string
	system string
	maxTok int

	// Profile holds tier-derived defaults for the active model. A zero-value
	// Profile reproduces the original one-size-fits-all harness behavior.
	Profile model.Profile

	// ConfigTemp is the user's explicit temperature setting (from config file or
	// env). When non-nil it overrides Profile.TemperatureDefault on all
	// non-thinking requests. Nil means use the profile's tier-based default.
	ConfigTemp *float64

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
// cx carries the lifecycle context, token budget, and runtime monitor for
// this run — construct it at the entry point and pass it unchanged here.
// Events are dispatched to onEvent synchronously as they arrive.
// history is the prior conversation; the new user message is prepended automatically.
// Returns the updated message history (including the new turn).
func (a *Agent) Run(
	cx *AgentCx,
	input []model.ContentBlock,
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
	maxTurns := a.Profile.MaxTurnsOr(opts.MaxTurns)
	maxTok := a.Profile.ApplyMaxTokens(a.maxTok, DefaultMaxTokens)
	// Temperature and retry count are loop-invariant; derive once.
	temp := a.Profile.TemperatureFor(thinkLevel, a.ConfigTemp)
	retryAttempts := a.Profile.RetryAttemptsOr(a.RetryAttempts)

	// Build initial message list.
	msgs := make([]model.Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	msgs = append(msgs, model.Message{Role: model.RoleUser, Content: input})

	// Apply mode overrides. TierToolless models don't support function calling —
	// never send tool definitions regardless of mode (provider rejects them).
	toolless := a.Profile.Tier == model.TierToolless
	var toolDefs []model.ToolDefinition
	switch opts.Mode {
	case AgentModePlan:
		maxTurns = 1
		systemPrompt = modePrefix("You are in PLAN MODE. Do not use any tools. Output a detailed numbered plan of exactly what you would do to complete this task.", systemPrompt)
	case AgentModePipe:
		maxTurns = 1
	case AgentModePlanAct:
		if !toolless {
			toolDefs = resolveTools(opts.Tools)
		}
		systemPrompt = modePrefix("First write a concise numbered plan of your approach. Then execute each step using the available tools.", systemPrompt)
	case AgentModeInteractive:
		if !toolless {
			toolDefs = resolveTools(opts.Tools)
		}
		systemPrompt = modePrefix("You are in INTERACTIVE MODE. Before invoking any tool, briefly describe what you are about to do and why, then proceed.", systemPrompt)
	case AgentModeHandoff:
		if !toolless {
			toolDefs = resolveTools(opts.Tools)
		}
		systemPrompt = modeSuffix(systemPrompt, "When your task is complete, output a HANDOFF section with a concise state summary so another agent can continue from where you left off.")
	default:
		if !toolless {
			toolDefs = resolveTools(opts.Tools)
		}
	}

	// lastMeasuredTokens holds the InputTokens value from the previous API
	// response. When non-zero it is used instead of the heuristic estimator.
	var lastMeasuredTokens int
	toolDefsTokens := EstimateToolDefsTokens(toolDefs)

	for turn := 0; maxTurns <= 0 || turn < maxTurns; turn++ {
		select {
		case <-cx.Done():
			return msgs, cx.Err()
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
			} else if compacted, _, compactErr := a.Compactor.Compact(cx.Context(), msgs, systemPrompt); compactErr == nil {
				msgs = compacted
				lastMeasuredTokens = estimateTokens(compacted)
			}
		}

		// Pre-turn: check context budget via estimate before spending a round-trip.
		est := lastMeasuredTokens
		if est <= 0 {
			est = estimateTokens(msgs) + toolDefsTokens
		}
		if err := cx.CheckPreTurn(turn, est); err != nil {
			onEvent(AgentEvent{Kind: EventKindError, Err: err})
			return msgs, err
		}

		// "required" on turn 0 forces the model to act immediately. Only applies
		// to act/handoff modes; plan_act/interactive need a free-text first turn.
		toolChoice := ""
		if len(toolDefs) > 0 {
			switch opts.Mode {
			case AgentModeAct, AgentModeHandoff, "":
				if turn == 0 {
					toolChoice = a.Profile.ToolChoiceOrDefault()
				} else {
					toolChoice = "auto"
				}
			default:
				toolChoice = "auto"
			}
		}

		req := &provider.Request{
			Model:         a.model,
			Messages:      msgs,
			System:        systemPrompt,
			Tools:         toolDefs,
			MaxTokens:     maxTok,
			Temperature:   temp,
			ThinkingLevel: thinkLevel,
			ToolChoice:    toolChoice,
		}

		t0 := time.Now()
		eventCh, err := streamWithRetry(cx.Context(), a.prov, req, retryAttempts)
		if err != nil {
			if cx.Monitor != nil {
				cx.Monitor.Observe(runtime.Observation{
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
		resp, err := drainStream(cx.Context(), eventCh, onEvent)
		if cx.Monitor != nil {
			cx.Monitor.Observe(runtime.Observation{
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

		// Post-turn: record usage and verify both budget limits with accurate counts.
		if resp.Usage.InputTokens > 0 {
			lastMeasuredTokens = resp.Usage.InputTokens
			if err := cx.RecordTurn(turn, resp.Usage); err != nil {
				onEvent(AgentEvent{Kind: EventKindError, Err: err})
				return msgs, err
			}
		}

		// Add the assistant message to history.
		msgs = append(msgs, resp.Message)

		// Content is the authoritative signal for tool use — stop_reason is
		// unreliable across models (some send "stop" with tool blocks, others
		// send "tool_calls" with none).
		hasToolUse := false
		for _, b := range resp.Message.Content {
			if b.Type == model.ContentTypeToolUse {
				hasToolUse = true
				break
			}
		}
		if !hasToolUse {
			onEvent(AgentEvent{
				Kind:       EventKindDone,
				Usage:      cx.Usage(),
				StopReason: resp.StopReason,
			})
			return msgs, nil
		}

		// Halt before running tools if the runtime monitor signals a safety veto.
		if cx.Monitor != nil && cx.Monitor.ShouldVeto() {
			err := fmt.Errorf("agent: runtime safety veto — error rate exceeded threshold, halting tool execution")
			onEvent(AgentEvent{Kind: EventKindError, Err: err})
			return msgs, err
		}

		// Execute tool calls and collect results.
		// safeEmit is passed so goroutines inside executeTools can emit events
		// without the caller needing to handle concurrent access.
		toolResults := executeTools(cx, resp.Message.Content, safeEmit, a.Hooks, len(msgs), a.Profile)
		if len(toolResults) > 0 {
			msgs = append(msgs, model.Message{
				Role:    model.RoleUser,
				Content: toolResults,
			})
		}
	}

	// Reachable when MaxTurns cap (from explicit opts or profile) was exhausted.
	err := fmt.Errorf("agent: max turns (%d) reached without completion", maxTurns)
	onEvent(AgentEvent{Kind: EventKindError, Err: err, Usage: cx.Usage()})
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
	// Some models (e.g. gpt-oss-120b) return multiple tool calls with the same
	// ID, or omit IDs entirely. Two-pass fix: first assign unique fallback IDs
	// to empty slots without colliding with existing IDs, then suffix-disambiguate
	// any remaining duplicates.
	existingIDs := make(map[string]struct{}, len(blocks))
	for i := range blocks {
		if blocks[i].Type == model.ContentTypeToolUse && blocks[i].ID != "" {
			existingIDs[blocks[i].ID] = struct{}{}
		}
	}
	fallback := 0
	for i := range blocks {
		if blocks[i].Type != model.ContentTypeToolUse || blocks[i].ID != "" {
			continue
		}
		for {
			candidate := "call_" + strconv.Itoa(fallback)
			fallback++
			if _, taken := existingIDs[candidate]; !taken {
				blocks[i].ID = candidate
				existingIDs[candidate] = struct{}{}
				break
			}
		}
	}
	seenIDs := make(map[string]int, len(blocks))
	for i := range blocks {
		if blocks[i].Type != model.ContentTypeToolUse {
			continue
		}
		seenIDs[blocks[i].ID]++
		if seenIDs[blocks[i].ID] > 1 {
			blocks[i].ID = blocks[i].ID + "_" + strconv.Itoa(seenIDs[blocks[i].ID])
		}
	}
	return &provider.Response{
		Message:    model.Message{Role: model.RoleAssistant, Content: blocks},
		StopReason: stop,
		Usage:      usage,
	}, nil
}

// executeTools runs all tool_use blocks from the assistant message in parallel
// and returns results in original order. Non-bash concurrency is capped by
// profile.ParallelToolBudget (0 = unlimited). Tool-level errors are embedded in
// the returned ContentBlocks (IsError=true).
func executeTools(
	cx *AgentCx,
	blocks []model.ContentBlock,
	onEvent func(AgentEvent),
	hooks HookRunner,
	msgCount int,
	profile model.Profile,
) []model.ContentBlock {
	// Collect tool-use blocks preserving original order.
	var calls []model.ContentBlock
	for _, b := range blocks {
		if b.Type == model.ContentTypeToolUse {
			calls = append(calls, b)
		}
	}
	if len(calls) == 0 {
		return nil
	}

	results := make([]model.ContentBlock, len(calls))

	// Per-profile semaphore for non-bash tool parallelism.
	// 0 = unlimited; allocate a buffered channel only when a cap is configured.
	var nonBashSem chan struct{}
	if profile.ParallelToolBudget > 0 {
		nonBashSem = make(chan struct{}, profile.ParallelToolBudget)
	}

	// Short-circuit: if the context is already cancelled, emit in_progress + done
	// for every pending call so Zed never sees a done event without a prior exec
	// event ("Tool call not found"), then return without spawning goroutines.
	if err := cx.Err(); err != nil {
		for i, c := range calls {
			params := c.Input
			if params == nil {
				params = json.RawMessage("{}")
			}
			onEvent(AgentEvent{Kind: EventKindToolExec, ToolID: c.ID, ToolName: c.Name, ToolInput: params})
			result := errorToolResult(c.ID, err.Error())
			onEvent(AgentEvent{Kind: EventKindToolDone, ToolResult: result})
			results[i] = result
		}
		return results
	}

	var wg sync.WaitGroup
	for i, block := range calls {
		wg.Add(1)
		go func(i int, block model.ContentBlock) {
			defer wg.Done()

			params := block.Input
			if params == nil {
				params = json.RawMessage("{}")
			}

			// Always emit in_progress before any outcome so Zed sees an exec
			// event before the matching done event (prevents "Tool call not found").
			onEvent(AgentEvent{
				Kind:      EventKindToolExec,
				ToolID:    block.ID,
				ToolName:  block.Name,
				ToolInput: params,
			})

			// Acquire a concurrency slot. bash uses a global cap (real processes);
			// other tools use the per-profile cap when set.
			var sem chan struct{}
			if block.Name == tools.ToolNameBash {
				sem = bashSem
			} else {
				sem = nonBashSem
			}
			release, err := acquireSem(cx, sem)
			if err != nil {
				result := errorToolResult(block.ID, err.Error())
				onEvent(AgentEvent{Kind: EventKindToolDone, ToolResult: result})
				results[i] = result
				return
			}
			defer release()

			t, ok := tools.Get(block.Name)
			if !ok {
				if cx.Monitor != nil {
					cx.Monitor.Observe(runtime.Observation{
						Time:    time.Now(),
						Stage:   "tool:" + block.Name,
						Weight:  float64(msgCount),
						Success: false,
					})
				}
				result := errorToolResult(block.ID, fmt.Sprintf("unknown tool: %s", block.Name))
				onEvent(AgentEvent{Kind: EventKindToolDone, ToolResult: result})
				results[i] = result
				return
			}

			params = maybeRepairToolJSON(profile, params, cx.Monitor, block.Name)

			if hooks != nil {
				hooks.RunBeforeTool(cx.Context(), block.Name, params)
			}

			t0 := time.Now()
			toolResult, err := t.Execute(cx.Context(), params)
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
				hooks.RunAfterTool(cx.Context(), block.Name, sb.String(), isError)
			}

			if cx.Monitor != nil {
				cx.Monitor.Observe(runtime.Observation{
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
		}(i, block)
	}

	wg.Wait()
	return results
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
