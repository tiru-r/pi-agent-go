package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"time"
)

// ExtShape classifies the primary behavioral shape of a loaded extension.
type ExtShape int

const (
	ShapeTool          ExtShape = iota // generic tool invocation
	ShapeCommand                       // slash-command or CLI dispatch
	ShapeProvider                      // data / model provider
	ShapeEventHook                     // lifecycle event subscriber
	ShapeUIComponent                   // UI rendering or interaction
	ShapeConfiguration                 // settings / config read-write
	ShapeMulti                         // declares multiple shapes
	ShapeGeneral                       // uncategorized / catch-all
)

func (s ExtShape) String() string {
	switch s {
	case ShapeTool:
		return "tool"
	case ShapeCommand:
		return "command"
	case ShapeProvider:
		return "provider"
	case ShapeEventHook:
		return "event_hook"
	case ShapeUIComponent:
		return "ui_component"
	case ShapeConfiguration:
		return "configuration"
	case ShapeMulti:
		return "multi"
	default:
		return "general"
	}
}

// LifecycleStep names a harness lifecycle phase.
type LifecycleStep string

const (
	StepLoad     LifecycleStep = "load"
	StepVerify   LifecycleStep = "verify_registrations"
	StepInvoke   LifecycleStep = "invoke"
	StepShutdown LifecycleStep = "shutdown"
)

// ErrorCategory classifies a lifecycle failure into an actionable bucket.
type ErrorCategory string

const (
	ErrCatLoad         ErrorCategory = "load_failure"
	ErrCatRegistration ErrorCategory = "registration_mismatch"
	ErrCatInvoke       ErrorCategory = "invoke_failure"
	ErrCatShutdown     ErrorCategory = "shutdown_failure"
	ErrCatTimeout      ErrorCategory = "timeout"
	ErrCatCapability   ErrorCategory = "capability_denied"
	ErrCatPanic        ErrorCategory = "panic"
)

// LifecycleError pairs an actionable category with a human-readable message.
type LifecycleError struct {
	Category ErrorCategory `json:"category"`
	Message  string        `json:"message"`
}

func (e *LifecycleError) Error() string {
	return fmt.Sprintf("[%s] %s", e.Category, e.Message)
}

// LifecycleEvent is one structured log line emitted per harness step.
// Written to the caller-supplied sink as newline-terminated JSON (JSONL).
type LifecycleEvent struct {
	Step    LifecycleStep   `json:"step"`
	Shape   string          `json:"shape"`
	ExtName string          `json:"ext_name"`
	OK      bool            `json:"ok"`
	Detail  string          `json:"detail,omitempty"`
	Err     *LifecycleError `json:"error,omitempty"`
	TS      time.Time       `json:"timestamp"`
}

// LifecycleReport collects all events produced by RunLifecycle.
type LifecycleReport struct {
	ExtName string
	Shape   ExtShape
	Events  []LifecycleEvent
	// Failed is true if any step produced an error event.
	Failed bool
}

// RunLifecycle exercises ext through the standard four-step lifecycle and
// emits one JSONL event per step to sink (pass nil to suppress output).
//
// Steps:
//  1. load                 — validates Info() field completeness
//  2. verify_registrations — checks Info() is consistent with the declared shape
//  3. invoke               — calls Execute with a shape-appropriate probe payload
//  4. shutdown             — calls Close(), recovering any panic
//
// All four steps always run regardless of earlier failures so the report
// captures the full health picture of the extension.
func RunLifecycle(ctx context.Context, ext Extension, shape ExtShape, sink io.Writer) LifecycleReport {
	name := ext.Info().Name
	if name == "" {
		name = "<unnamed>"
	}
	rep := LifecycleReport{ExtName: name, Shape: shape}

	emit := func(ev LifecycleEvent) {
		ev.Shape = shape.String()
		ev.ExtName = name
		ev.TS = time.Now()
		rep.Events = append(rep.Events, ev)
		if ev.Err != nil {
			rep.Failed = true
		}
		if sink != nil {
			b, _ := json.Marshal(ev)
			_, _ = sink.Write(append(b, '\n'))
		}
	}

	// Step 1: Load — validate Info() completeness.
	if lcErr := validateInfo(ext.Info(), shape); lcErr != nil {
		emit(LifecycleEvent{Step: StepLoad, OK: false, Err: lcErr})
	} else {
		emit(LifecycleEvent{Step: StepLoad, OK: true, Detail: "info valid"})
	}

	// Step 2: Verify registrations — shape-specific schema checks.
	if lcErr := verifyRegistrations(ext.Info(), shape); lcErr != nil {
		emit(LifecycleEvent{Step: StepVerify, OK: false, Err: lcErr})
	} else {
		emit(LifecycleEvent{Step: StepVerify, OK: true, Detail: "registrations verified"})
	}

	// Step 3: Invoke — probe the primary dispatch path.
	runInvoke(ctx, ext, shape, emit)

	// Step 4: Shutdown — clean teardown with panic recovery.
	runShutdown(ext, emit)

	return rep
}

// validateInfo checks that Info() fields are complete enough to be usable,
// independent of shape.
func validateInfo(info Info, shape ExtShape) *LifecycleError {
	if info.Name == "" {
		return &LifecycleError{ErrCatLoad, "describe() returned empty name"}
	}
	if info.Description == "" {
		return &LifecycleError{ErrCatLoad, "describe() returned empty description"}
	}
	if shape == ShapeTool && (info.Schema == nil || string(info.Schema) == "null") {
		return &LifecycleError{ErrCatRegistration, "tool shape requires a non-null schema"}
	}
	return nil
}

// verifyRegistrations checks that Info() is structurally consistent with shape.
func verifyRegistrations(info Info, shape ExtShape) *LifecycleError {
	switch shape {
	case ShapeTool:
		if info.Schema == nil {
			return &LifecycleError{ErrCatRegistration, "tool schema is nil"}
		}
		var s map[string]any
		if err := json.Unmarshal(info.Schema, &s); err != nil {
			return &LifecycleError{ErrCatRegistration, "tool schema is not a valid JSON object: " + err.Error()}
		}
		if s["type"] == nil && s["properties"] == nil {
			return &LifecycleError{ErrCatRegistration, "tool schema missing 'type' or 'properties'"}
		}

	case ShapeCommand:
		if err := checkSchemaProp(info.Schema, "command"); err != nil {
			return &LifecycleError{ErrCatRegistration, "command shape: " + err.Error()}
		}

	case ShapeEventHook:
		if err := checkSchemaProp(info.Schema, "event"); err != nil {
			return &LifecycleError{ErrCatRegistration, "event_hook shape: " + err.Error()}
		}

	case ShapeConfiguration:
		if err := checkSchemaProp(info.Schema, "key"); err != nil {
			return &LifecycleError{ErrCatRegistration, "configuration shape: " + err.Error()}
		}

	case ShapeUIComponent:
		if err := checkSchemaProp(info.Schema, "action"); err != nil {
			return &LifecycleError{ErrCatRegistration, "ui_component shape: " + err.Error()}
		}

	case ShapeProvider:
		if err := checkSchemaProp(info.Schema, "action"); err != nil {
			return &LifecycleError{ErrCatRegistration, "provider shape: " + err.Error()}
		}
	}
	return nil
}

// checkSchemaProp returns an error if schema is non-nil but does not declare
// the given property under "properties".
func checkSchemaProp(schema json.RawMessage, prop string) error {
	if schema == nil {
		return nil // no schema declared; skip check
	}
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		return fmt.Errorf("schema is not a valid JSON object: %w", err)
	}
	props, _ := s["properties"].(map[string]any)
	if len(props) > 0 {
		if _, ok := props[prop]; !ok {
			return fmt.Errorf("schema 'properties' should declare %q", prop)
		}
	}
	return nil
}

// probePayload returns a shape-appropriate Execute payload for probing.
func probePayload(shape ExtShape) json.RawMessage {
	switch shape {
	case ShapeCommand:
		return json.RawMessage(`{"command":"help","args":[]}`)
	case ShapeProvider:
		return json.RawMessage(`{"action":"list"}`)
	case ShapeEventHook:
		return json.RawMessage(`{"event":"probe","data":null}`)
	case ShapeUIComponent:
		return json.RawMessage(`{"action":"probe","props":{}}`)
	case ShapeConfiguration:
		return json.RawMessage(`{"key":"__probe__"}`)
	case ShapeMulti:
		return json.RawMessage(`{"__probe__":true}`)
	default: // ShapeTool, ShapeGeneral
		return json.RawMessage(`{}`)
	}
}

// categorizeExecError maps an error message to an actionable ErrorCategory.
func categorizeExecError(msg string) ErrorCategory {
	switch {
	case strings.Contains(msg, "capability") && strings.Contains(msg, "not granted"):
		return ErrCatCapability
	case strings.Contains(msg, "context deadline exceeded"),
		strings.Contains(msg, "context canceled"):
		return ErrCatTimeout
	default:
		return ErrCatInvoke
	}
}

func runInvoke(ctx context.Context, ext Extension, shape ExtShape, emit func(LifecycleEvent)) {
	payload := probePayload(shape)
	content, isError, err := ext.Execute(ctx, payload)
	if err != nil {
		emit(LifecycleEvent{
			Step: StepInvoke,
			OK:   false,
			Err:  &LifecycleError{categorizeExecError(err.Error()), err.Error()},
		})
		return
	}
	emit(LifecycleEvent{
		Step:   StepInvoke,
		OK:     true,
		Detail: fmt.Sprintf("content=%d bytes is_error=%v", len(content), isError),
	})
}

func runShutdown(ext Extension, emit func(LifecycleEvent)) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			emit(LifecycleEvent{
				Step: StepShutdown,
				OK:   false,
				Err: &LifecycleError{
					ErrCatPanic,
					fmt.Sprintf("panic: %v\n%s", r, stack),
				},
			})
		}
	}()
	if err := ext.Close(); err != nil {
		emit(LifecycleEvent{
			Step: StepShutdown,
			OK:   false,
			Err:  &LifecycleError{ErrCatShutdown, err.Error()},
		})
		return
	}
	emit(LifecycleEvent{Step: StepShutdown, OK: true, Detail: "clean shutdown"})
}
