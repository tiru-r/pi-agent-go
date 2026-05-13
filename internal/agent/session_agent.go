package agent

import (
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
	defaultMaxIter  = 20
	maxBashWorkers  = 4 // bash spawns real OS processes; cap to avoid CPU saturation
)

// SessionAgent is a session-aware agent that persists conversation history
// to a Session and drives the full agentic loop.
//
// Static configuration (provider, tools, session, compactors) lives here.
// Runtime capabilities (lifecycle, budget, telemetry) travel in the AgentCx
// passed to Run — construct the AgentCx at the entry point.
type SessionAgent struct {
	Provider  provider.Provider
	Tools     []tools.Tool
	Session   *session.Session
	Config    *config.Config
	MaxIter   int // default 20, prevents infinite loops
	Compactor *Compactor

	// BGCompactor is optional; when set, compaction runs asynchronously so it
	// does not block the foreground turn. Requires Compactor to also be set.
	BGCompactor *BackgroundCompactor

	// Hooks is optional; wire an extensions.Manager to broadcast tool lifecycle
	// events to JS extensions that define before_tool / after_tool.
	Hooks HookRunner

	// RetryAttempts is the number of times to retry a failed provider.Stream call
	// for retriable errors (429, 5xx, timeouts). 0 defaults to defaultRetryAttempts (3).
	RetryAttempts int
}

// SessionRunOptions configures a single SessionAgent.Run call.
type SessionRunOptions struct {
	System    string
	Model     string
	MaxTokens int
	Thinking  model.ThinkingLevel
	Mode      AgentMode
}

// Run executes one full agent turn: sends input, streams response, executes tools,
// repeats until stop reason is end_turn or MaxIter is exceeded.
// cx carries the lifecycle context, token budget, and runtime monitor for this
// run — construct it at the entry point and pass it unchanged here.
// onEvent is called for each streaming event so callers can update the UI.
func (a *SessionAgent) Run(
	cx *AgentCx,
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
		maxTokens = DefaultMaxTokens
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
	toolDefsTokens := EstimateToolDefsTokens(toolDefs)

	for iter := 0; iter < maxIter; iter++ {
		select {
		case <-cx.Done():
			return cx.Err()
		default:
		}

		msgs := a.Session.Messages()

		// Apply any pending background compaction result. Rewinds the session to
		// the trigger head and replays: compaction marker → kept messages → new
		// messages added since the trigger. This keeps the JSONL tree consistent so
		// session restarts correctly reconstruct [summary, keptMsgs, futureMsgs].
		if a.BGCompactor != nil {
			if result := a.BGCompactor.Take(); result != nil {
				newMsgs := msgs[result.SnapLen:]
				_ = a.Session.SetHead(result.HeadID)
				_ = a.Session.Append(session.Entry{Type: session.EntryCompaction, Summary: result.Summary})
				for _, keptMsg := range result.Compacted[1:] { // Compacted[0] is the summary placeholder
					_ = a.Session.AppendMessage(keptMsg, nil)
				}
				for _, newMsg := range newMsgs {
					_ = a.Session.AppendMessage(newMsg, nil)
				}
				msgs = append(result.Compacted, newMsgs...)
				lastMeasuredTokens = estimateTokens(msgs)
			}
		}
		// Compact history synchronously (or trigger background compaction) when needed.
		if a.Compactor != nil && a.Compactor.ShouldCompact(msgs, lastMeasuredTokens, toolDefsTokens) {
			if a.BGCompactor != nil {
				a.BGCompactor.Trigger(msgs, opts.System, a.Session.HeadID())
			} else if compacted, summary, compactErr := a.Compactor.Compact(cx.Context(), msgs, opts.System); compactErr == nil {
				msgs = compacted
				lastMeasuredTokens = estimateTokens(compacted)
				// Persist: append compaction marker then kept messages so session
				// restarts reconstruct [summary, keptMsgs, futureMsgs] via buildMessages.
				_ = a.Session.Append(session.Entry{Type: session.EntryCompaction, Summary: summary})
				for _, keptMsg := range compacted[1:] {
					_ = a.Session.AppendMessage(keptMsg, nil)
				}
			}
		}

		// Pre-turn: enforce context budget before spending the round-trip.
		est := lastMeasuredTokens
		if est <= 0 {
			est = estimateTokens(msgs) + toolDefsTokens
		}
		if err := cx.CheckPreTurn(iter, est); err != nil {
			return err
		}

		toolChoice := ""
		if len(toolDefs) > 0 {
			switch opts.Mode {
			case AgentModeAct, AgentModeHandoff, "":
				if iter == 0 {
					toolChoice = "required"
				} else {
					toolChoice = "auto"
				}
			default:
				toolChoice = "auto"
			}
		}

		req := &provider.Request{
			Model:         modelName,
			Messages:      msgs,
			System:        opts.System,
			Tools:         toolDefs,
			MaxTokens:     maxTokens,
			ThinkingLevel: thinking,
			ToolChoice:    toolChoice,
		}

		t0 := time.Now()
		events, err := streamWithRetry(cx.Context(), a.Provider, req, a.RetryAttempts)
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
			return fmt.Errorf("session_agent: stream: %w", err)
		}

		// Tee events to caller while collecting.
		fanned := teeEvents(events, onEvent)
		resp, err := provider.Collect(fanned)
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
			return fmt.Errorf("session_agent: collect: %w", err)
		}

		// Post-turn: record usage and enforce both budget limits with accurate counts.
		if resp.Usage.InputTokens > 0 {
			lastMeasuredTokens = resp.Usage.InputTokens
			if err := cx.RecordTurn(iter, resp.Usage); err != nil {
				return fmt.Errorf("session_agent: token budget: %w", err)
			}
		}

		// Append assistant response to session.
		if err := a.Session.AppendMessage(resp.Message, &resp.Usage); err != nil {
			return fmt.Errorf("session_agent: append assistant message: %w", err)
		}

		if resp.StopReason != model.StopReasonToolUse {
			return nil // end_turn, max_tokens, or stop_sequence
		}

		// Halt before running tools if the runtime monitor signals a safety veto.
		if cx.Monitor != nil && cx.Monitor.ShouldVeto() {
			return fmt.Errorf("session_agent: runtime safety veto — error rate exceeded threshold, halting tool execution")
		}

		// Execute all tool calls in parallel (up to maxToolConcurrency).
		toolUses := resp.Message.ToolUses()
		if len(toolUses) == 0 {
			return nil
		}

		// Capture before goroutines start so runTool doesn't call Session.Messages()
		// (which acquires a mutex and rebuilds the list) from every tool goroutine.
		msgCount := len(msgs)
		results := a.runTools(cx, toolUses, msgCount)

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

// bashSem is shared across all SessionAgent and Agent instances in the process.
// The cap of maxBashWorkers applies system-wide, intentionally preventing
// multi-session workloads from saturating OS process and CPU limits.
// I/O-bound tools (read, write, grep, find, ls, edit, hashline_edit) run
// without a cap — Go parks blocked goroutines for free.
// Adjust maxBashWorkers at program startup if a different cap is needed.
var bashSem = make(chan struct{}, maxBashWorkers)

// runTools executes all tool uses concurrently and returns result
// ContentBlocks in the same order as uses. msgCount is the message count
// captured before the goroutines start; it is passed to runTool so it can
// record monitoring observations without rebuilding the session message list.
func (a *SessionAgent) runTools(cx *AgentCx, uses []model.ContentBlock, msgCount int) []model.ContentBlock {
	// Build a name→tool map once so each goroutine does an O(1) lookup.
	toolMap := make(map[string]tools.Tool, len(a.Tools))
	for _, t := range a.Tools {
		toolMap[t.Name()] = t
	}

	results := make([]model.ContentBlock, len(uses))
	var wg sync.WaitGroup
	for i, use := range uses {
		wg.Add(1)
		go func(i int, block model.ContentBlock) {
			defer wg.Done()
			if block.Name == "bash" {
				select {
				case bashSem <- struct{}{}:
					defer func() { <-bashSem }()
				case <-cx.Done():
					results[i] = model.ContentBlock{
						Type:      model.ContentTypeToolResult,
						ToolUseID: block.ID,
						IsError:   true,
						Content:   []model.ContentBlock{{Type: model.ContentTypeText, Text: cx.Err().Error()}},
					}
					return
				}
			}
			results[i] = a.runTool(cx, block, toolMap, msgCount)
		}(i, use)
	}
	wg.Wait()
	return results
}

// runTool executes a single tool call and returns a tool_result ContentBlock.
// toolMap is the pre-built name→tool index from the caller's a.Tools slice.
// msgCount is the message count at the start of the tool-execution batch; it is
// used for monitoring weight without acquiring the session mutex mid-goroutine.
func (a *SessionAgent) runTool(cx *AgentCx, block model.ContentBlock, toolMap map[string]tools.Tool, msgCount int) model.ContentBlock {
	result := model.ContentBlock{
		Type:      model.ContentTypeToolResult,
		ToolUseID: block.ID,
	}

	// Resolve the tool: check caller-provided tools first, then built-ins.
	t, ok := toolMap[block.Name]
	if !ok {
		t2, ok2 := tools.Get(block.Name)
		if !ok2 {
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
		a.Hooks.RunBeforeTool(cx.Context(), block.Name, params)
	}

	t0tool := time.Now()
	res, err := t.Execute(cx.Context(), params)
	toolLatency := time.Since(t0tool)

	if err != nil {
		if cx.Monitor != nil {
			cx.Monitor.Observe(runtime.Observation{
				Time:    t0tool,
				Stage:   "tool:" + block.Name,
				Latency: toolLatency,
				Weight:  float64(msgCount),
				Success: false,
			})
		}
		if a.Hooks != nil {
			a.Hooks.RunAfterTool(cx.Context(), block.Name, "tool execution error: "+err.Error(), true)
		}
		result.IsError = true
		result.Content = []model.ContentBlock{{
			Type: model.ContentTypeText,
			Text: "tool execution error: " + err.Error(),
		}}
		return result
	}

	if cx.Monitor != nil {
		cx.Monitor.Observe(runtime.Observation{
			Time:    t0tool,
			Stage:   "tool:" + block.Name,
			Latency: toolLatency,
			Weight:  float64(msgCount),
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
		a.Hooks.RunAfterTool(cx.Context(), block.Name, sb.String(), res.IsError)
	}

	result.IsError = res.IsError
	result.Content = res.Content
	return result
}

// teeEvents fans an event channel to the optional onEvent callback and
// returns a new channel that can be drained by provider.Collect.
// The collector channel is fed first (non-blocking while buffer has space) so
// provider.Collect can process events without waiting for the UI callback.
func teeEvents(in <-chan provider.Event, onEvent func(provider.Event)) <-chan provider.Event {
	out := make(chan provider.Event, 64)
	go func() {
		defer close(out)
		for ev := range in {
			out <- ev
			if onEvent != nil {
				onEvent(ev)
			}
		}
	}()
	return out
}
