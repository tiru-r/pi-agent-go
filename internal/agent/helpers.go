package agent

import (
	"encoding/json"
	"time"

	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/runtime"
)

// acquireSem acquires one slot from sem, blocking until a slot is free or cx
// is cancelled. sem may be nil (no cap), in which case it returns immediately.
// The caller must call the returned release function exactly once when the slot
// can be freed; it is safe to defer.
func acquireSem(cx *AgentCx, sem chan struct{}) (release func(), err error) {
	if sem == nil {
		return func() {}, nil
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-cx.Done():
		return nil, cx.Err()
	}
}

// errorToolResult builds a tool_result ContentBlock carrying an error message
// to be fed back to the model. Used for cancellations, unknown tools, and
// tool execution failures.
func errorToolResult(toolUseID, text string) model.ContentBlock {
	return model.ContentBlock{
		Type:      model.ContentTypeToolResult,
		ToolUseID: toolUseID,
		IsError:   true,
		Content:   []model.ContentBlock{{Type: model.ContentTypeText, Text: text}},
	}
}

// maybeRepairToolJSON returns repaired bytes when the profile enables JSON
// repair and the input is invalid. Records a monitor observation on successful
// repair so operators can see how often repair fires per tool/tier.
// Returns the original bytes when repair is disabled or fails.
func maybeRepairToolJSON(profile model.Profile, params []byte, mon *runtime.Monitor, toolName string) []byte {
	if !profile.RepairToolJSON || json.Valid(params) {
		return params
	}
	repaired, ok := repairToolJSON(params)
	if !ok {
		return params
	}
	if mon != nil {
		mon.Observe(runtime.Observation{
			Time:    time.Now(),
			Stage:   "tool:" + toolName + ":json_repair",
			Weight:  1,
			Success: true,
		})
	}
	return repaired
}
