// Package agent — AgentCx: capability-scoped context for the agent loop.
//
// AgentCx is threaded at every API boundary (agent loop ↔ tools ↔ sessions ↔
// RPC handler). It carries the three cross-cutting concerns together so each
// callsite makes its capability requirements explicit and auditable:
//
//   - Lifecycle:  cancellation and deadlines (wraps a context.Context)
//   - Budget:     token limits (run-total and per-turn)
//   - Telemetry:  runtime monitor observations
//
// context.Context and budget integers are NOT separate parameters in the
// agent loop; AgentCx is the single plumbing type passed downward unchanged
// from the RPC or CLI entry point.
package agent

import (
	"context"
	"fmt"

	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/runtime"
)

// AgentCx is the capability context for one agent Run. Construct it at the
// entry point (RPC handler, CLI command) with NewAgentCx, then pass it
// unchanged into Run, executeTools, and any other function that needs
// cancellation, budget enforcement, or telemetry.
type AgentCx struct {
	ctx context.Context // lifecycle: cancellation + deadlines

	// Budget limits (immutable after construction).
	totalBudget int // cap on cumulative InputTokens (0 = unlimited)
	turnBudget  int // cap on InputTokens per turn   (0 = unlimited)

	// cumulative tracks token usage; mutated only by the owning Run goroutine
	// (RecordTurn is not safe to call concurrently).
	cumulative model.Usage

	// Monitor is optional. Its methods are safe to call concurrently — tool
	// goroutines in executeTools call Observe while Run is blocked on wg.Wait.
	Monitor *runtime.Monitor
}

// NewAgentCx constructs an AgentCx from the entry-point context and limits.
// totalBudget caps cumulative InputTokens for the whole Run (0 = unlimited).
// turnBudget caps InputTokens in a single turn (0 = unlimited).
func NewAgentCx(ctx context.Context, totalBudget, turnBudget int, mon *runtime.Monitor) *AgentCx {
	return &AgentCx{
		ctx:         ctx,
		totalBudget: totalBudget,
		turnBudget:  turnBudget,
		Monitor:     mon,
	}
}

// Context returns the underlying lifecycle context. Use this when calling
// functions that require a raw context.Context (provider.Stream, tool.Execute).
func (cx *AgentCx) Context() context.Context { return cx.ctx }

// Done returns the lifecycle cancellation channel, mirroring context.Context.
func (cx *AgentCx) Done() <-chan struct{} { return cx.ctx.Done() }

// Err returns the lifecycle context error, mirroring context.Context.
func (cx *AgentCx) Err() error { return cx.ctx.Err() }

// WithContext returns a shallow copy of cx with a derived context. Use this
// when a sub-operation needs a tighter deadline without changing budgets.
func (cx *AgentCx) WithContext(ctx context.Context) *AgentCx {
	cp := *cx
	cp.ctx = ctx
	return &cp
}

// CheckPreTurn checks the per-turn budget against an estimated token count
// before the LLM round-trip fires. Returns nil when the budget is inactive
// or the estimate is within the cap.
func (cx *AgentCx) CheckPreTurn(turn, estimated int) error {
	if cx.turnBudget > 0 && estimated > cx.turnBudget {
		return fmt.Errorf("agent: context budget exceeded before turn %d (%d/%d tokens estimated)",
			turn+1, estimated, cx.turnBudget)
	}
	return nil
}

// RecordTurn adds measured usage for a completed LLM turn and checks both
// limits with accurate token counts. Returns an error if either cap is exceeded.
func (cx *AgentCx) RecordTurn(turn int, usage model.Usage) error {
	cx.cumulative = cx.cumulative.Add(usage)
	if cx.totalBudget > 0 && cx.cumulative.InputTokens > cx.totalBudget {
		return fmt.Errorf("agent: token budget exceeded (%d/%d input tokens consumed)",
			cx.cumulative.InputTokens, cx.totalBudget)
	}
	if cx.turnBudget > 0 && usage.InputTokens > cx.turnBudget {
		return fmt.Errorf("agent: context budget exceeded in turn %d (%d/%d input tokens)",
			turn+1, usage.InputTokens, cx.turnBudget)
	}
	return nil
}

// Usage returns the cumulative token usage recorded across all turns so far.
func (cx *AgentCx) Usage() model.Usage { return cx.cumulative }
