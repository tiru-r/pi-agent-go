package extensions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tiru-r/pi-agent-go/internal/extensions"
)

// ── test double ───────────────────────────────────────────────────────────────

type mockExt struct {
	info    extensions.Info
	execFn  func(ctx context.Context, params json.RawMessage) (string, bool, error)
	closeFn func() error
}

func (m *mockExt) Info() extensions.Info { return m.info }

func (m *mockExt) Execute(ctx context.Context, params json.RawMessage) (string, bool, error) {
	if m.execFn != nil {
		return m.execFn(ctx, params)
	}
	return "ok", false, nil
}

func (m *mockExt) Close() error {
	if m.closeFn != nil {
		return m.closeFn()
	}
	return nil
}

// validToolInfo returns an Info that passes all tool-shape checks.
func validToolInfo(name string) extensions.Info {
	return extensions.Info{
		Name:        name,
		Description: "a test tool",
		Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

// ── happy path ────────────────────────────────────────────────────────────────

func TestRunLifecycle_HappyPath(t *testing.T) {
	ext := &mockExt{info: validToolInfo("happy-tool")}

	var buf bytes.Buffer
	rep := extensions.RunLifecycle(context.Background(), ext, extensions.ShapeTool, &buf)

	if rep.Failed {
		t.Fatalf("expected success, got failures:\n%s", buf.String())
	}
	if len(rep.Events) != 4 {
		t.Fatalf("expected 4 lifecycle events, got %d", len(rep.Events))
	}
	steps := []extensions.LifecycleStep{
		extensions.StepLoad, extensions.StepVerify,
		extensions.StepInvoke, extensions.StepShutdown,
	}
	for i, step := range steps {
		ev := rep.Events[i]
		if ev.Step != step {
			t.Errorf("event[%d]: expected step %q, got %q", i, step, ev.Step)
		}
		if !ev.OK {
			t.Errorf("event[%d] step %q should be OK: %v", i, step, ev.Err)
		}
	}
}

// ── load failure ──────────────────────────────────────────────────────────────

func TestRunLifecycle_LoadFailure_EmptyName(t *testing.T) {
	ext := &mockExt{info: extensions.Info{Description: "no name"}}

	var buf bytes.Buffer
	rep := extensions.RunLifecycle(context.Background(), ext, extensions.ShapeTool, &buf)

	if !rep.Failed {
		t.Fatal("expected failure for empty name")
	}
	ev := rep.Events[0]
	if ev.Step != extensions.StepLoad || ev.OK {
		t.Errorf("expected load step to fail, got step=%q ok=%v", ev.Step, ev.OK)
	}
	if ev.Err == nil || ev.Err.Category != extensions.ErrCatLoad {
		t.Errorf("expected %s category, got %v", extensions.ErrCatLoad, ev.Err)
	}
	// Remaining steps still run.
	if len(rep.Events) != 4 {
		t.Errorf("expected 4 events even after load failure, got %d", len(rep.Events))
	}
}

func TestRunLifecycle_LoadFailure_EmptyDescription(t *testing.T) {
	ext := &mockExt{info: extensions.Info{
		Name:   "no-desc",
		Schema: json.RawMessage(`{"type":"object"}`),
	}}
	var buf bytes.Buffer
	rep := extensions.RunLifecycle(context.Background(), ext, extensions.ShapeTool, &buf)
	if !rep.Failed {
		t.Fatal("expected failure for empty description")
	}
}

// ── registration mismatch ─────────────────────────────────────────────────────

func TestRunLifecycle_VerifyFail_NilSchema(t *testing.T) {
	ext := &mockExt{info: extensions.Info{
		Name:        "no-schema",
		Description: "missing schema",
	}}
	var buf bytes.Buffer
	rep := extensions.RunLifecycle(context.Background(), ext, extensions.ShapeTool, &buf)

	if !rep.Failed {
		t.Fatal("expected failure for nil schema on tool shape")
	}
	var verifyEv extensions.LifecycleEvent
	for _, ev := range rep.Events {
		if ev.Step == extensions.StepVerify {
			verifyEv = ev
		}
	}
	if verifyEv.OK {
		t.Error("verify step should have failed")
	}
	if verifyEv.Err == nil || verifyEv.Err.Category != extensions.ErrCatRegistration {
		t.Errorf("expected %s, got %v", extensions.ErrCatRegistration, verifyEv.Err)
	}
}

func TestRunLifecycle_VerifyFail_CommandMissingProp(t *testing.T) {
	// A command-shape extension whose schema has properties but no "command" key.
	ext := &mockExt{info: extensions.Info{
		Name:        "bad-cmd",
		Description: "command without property",
		Schema:      json.RawMessage(`{"type":"object","properties":{"other":{}}}`),
	}}
	var buf bytes.Buffer
	rep := extensions.RunLifecycle(context.Background(), ext, extensions.ShapeCommand, &buf)
	if !rep.Failed {
		t.Fatal("expected failure for command shape with missing 'command' property")
	}
}

// ── invoke failure ────────────────────────────────────────────────────────────

func TestRunLifecycle_InvokeFailure(t *testing.T) {
	ext := &mockExt{
		info: validToolInfo("failing-tool"),
		execFn: func(_ context.Context, _ json.RawMessage) (string, bool, error) {
			return "", true, errors.New("internal execute error")
		},
	}
	var buf bytes.Buffer
	rep := extensions.RunLifecycle(context.Background(), ext, extensions.ShapeTool, &buf)

	if !rep.Failed {
		t.Fatal("expected failure")
	}
	var invokeEv extensions.LifecycleEvent
	for _, ev := range rep.Events {
		if ev.Step == extensions.StepInvoke {
			invokeEv = ev
		}
	}
	if invokeEv.OK {
		t.Error("invoke step should have failed")
	}
	if invokeEv.Err == nil || invokeEv.Err.Category != extensions.ErrCatInvoke {
		t.Errorf("expected %s, got %v", extensions.ErrCatInvoke, invokeEv.Err)
	}
}

func TestRunLifecycle_InvokeCapabilityDenied(t *testing.T) {
	ext := &mockExt{
		info: validToolInfo("capped-tool"),
		execFn: func(_ context.Context, _ json.RawMessage) (string, bool, error) {
			return "", true, errors.New(`capability "exec" not granted to extension "capped-tool"`)
		},
	}
	var buf bytes.Buffer
	rep := extensions.RunLifecycle(context.Background(), ext, extensions.ShapeTool, &buf)

	var invokeEv extensions.LifecycleEvent
	for _, ev := range rep.Events {
		if ev.Step == extensions.StepInvoke {
			invokeEv = ev
		}
	}
	if invokeEv.Err == nil || invokeEv.Err.Category != extensions.ErrCatCapability {
		t.Errorf("expected %s, got %v", extensions.ErrCatCapability, invokeEv.Err)
	}
}

// ── shutdown ──────────────────────────────────────────────────────────────────

func TestRunLifecycle_ShutdownError(t *testing.T) {
	ext := &mockExt{
		info:    validToolInfo("close-err"),
		closeFn: func() error { return errors.New("close failed") },
	}
	var buf bytes.Buffer
	rep := extensions.RunLifecycle(context.Background(), ext, extensions.ShapeTool, &buf)

	if !rep.Failed {
		t.Fatal("expected failure")
	}
	var shutEv extensions.LifecycleEvent
	for _, ev := range rep.Events {
		if ev.Step == extensions.StepShutdown {
			shutEv = ev
		}
	}
	if shutEv.OK {
		t.Error("shutdown step should have failed")
	}
	if shutEv.Err == nil || shutEv.Err.Category != extensions.ErrCatShutdown {
		t.Errorf("expected %s, got %v", extensions.ErrCatShutdown, shutEv.Err)
	}
}

func TestRunLifecycle_ShutdownPanic(t *testing.T) {
	ext := &mockExt{
		info:    validToolInfo("panicky"),
		closeFn: func() error { panic("intentional test panic") },
	}
	var buf bytes.Buffer
	// Must not propagate the panic.
	rep := extensions.RunLifecycle(context.Background(), ext, extensions.ShapeTool, &buf)

	if !rep.Failed {
		t.Fatal("expected failure due to panic")
	}
	var shutEv extensions.LifecycleEvent
	for _, ev := range rep.Events {
		if ev.Step == extensions.StepShutdown {
			shutEv = ev
		}
	}
	if shutEv.OK {
		t.Error("shutdown step should have failed")
	}
	if shutEv.Err == nil || shutEv.Err.Category != extensions.ErrCatPanic {
		t.Errorf("expected %s, got %v", extensions.ErrCatPanic, shutEv.Err)
	}
}

// ── JSONL output ──────────────────────────────────────────────────────────────

func TestRunLifecycle_JSONLOutput(t *testing.T) {
	ext := &mockExt{info: validToolInfo("jsonl-test")}
	var buf bytes.Buffer
	extensions.RunLifecycle(context.Background(), ext, extensions.ShapeTool, &buf)

	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))
	if len(lines) != 4 {
		t.Fatalf("expected 4 JSONL lines, got %d:\n%s", len(lines), buf.String())
	}
	for i, line := range lines {
		var ev extensions.LifecycleEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Errorf("line %d is not valid JSON: %v — %s", i, err, line)
		}
		if ev.ExtName == "" {
			t.Errorf("line %d: ext_name is empty", i)
		}
		if ev.Shape == "" {
			t.Errorf("line %d: shape is empty", i)
		}
		if ev.TS.IsZero() {
			t.Errorf("line %d: timestamp is zero", i)
		}
	}
}

func TestRunLifecycle_NilSink(t *testing.T) {
	ext := &mockExt{info: validToolInfo("nil-sink")}
	// Must not panic when sink is nil.
	rep := extensions.RunLifecycle(context.Background(), ext, extensions.ShapeTool, nil)
	if rep.Failed {
		t.Fatal("expected success")
	}
}

// ── ExtShape.String() ─────────────────────────────────────────────────────────

func TestExtShapeString(t *testing.T) {
	cases := map[extensions.ExtShape]string{
		extensions.ShapeTool:          "tool",
		extensions.ShapeCommand:       "command",
		extensions.ShapeProvider:      "provider",
		extensions.ShapeEventHook:     "event_hook",
		extensions.ShapeUIComponent:   "ui_component",
		extensions.ShapeConfiguration: "configuration",
		extensions.ShapeMulti:         "multi",
		extensions.ShapeGeneral:       "general",
	}
	for shape, want := range cases {
		if got := shape.String(); got != want {
			t.Errorf("Shape(%d).String() = %q, want %q", shape, got, want)
		}
	}
}

// ── probe payload shapes ──────────────────────────────────────────────────────

func TestRunLifecycle_ProbePayloadReachesExecute(t *testing.T) {
	// Verify that the shape-specific probe payload is forwarded to Execute.
	shapes := []struct {
		shape   extensions.ExtShape
		wantKey string
	}{
		{extensions.ShapeCommand, "command"},
		{extensions.ShapeProvider, "action"},
		{extensions.ShapeEventHook, "event"},
		{extensions.ShapeUIComponent, "action"},
		{extensions.ShapeConfiguration, "key"},
	}
	for _, tc := range shapes {
		t.Run(tc.shape.String(), func(t *testing.T) {
			var gotParams json.RawMessage
			ext := &mockExt{
				info: extensions.Info{
					Name:        "probe-" + tc.shape.String(),
					Description: "probe test",
					Schema:      json.RawMessage(`{"type":"object","properties":{"` + tc.wantKey + `":{}}}`),
				},
				execFn: func(_ context.Context, params json.RawMessage) (string, bool, error) {
					gotParams = params
					return "ok", false, nil
				},
			}
			extensions.RunLifecycle(context.Background(), ext, tc.shape, nil)
			var m map[string]any
			if err := json.Unmarshal(gotParams, &m); err != nil {
				t.Fatalf("params not valid JSON: %v", err)
			}
			if _, ok := m[tc.wantKey]; !ok {
				t.Errorf("expected key %q in probe payload, got %v", tc.wantKey, m)
			}
		})
	}
}
