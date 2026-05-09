package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/tools"
)

// WrapAsTools returns a tools.Tool for each loaded extension plus the "ext"
// meta-dispatcher tool (only added when at least one extension is loaded).
// Call tools.Register on each returned tool to add it to the registry.
func WrapAsTools(m *Manager) []tools.Tool {
	if len(m.extensions) == 0 {
		return nil
	}
	ts := make([]tools.Tool, 0, len(m.extensions)+1)
	for _, ext := range m.extensions {
		ts = append(ts, &extWrapper{ext: ext})
	}
	ts = append(ts, &extDispatchTool{mgr: m})
	return ts
}

// ── per-extension wrapper ─────────────────────────────────────────────────────

type extWrapper struct{ ext Extension }

func (w *extWrapper) Name() string             { return w.ext.Info().Name }
func (w *extWrapper) Description() string      { return w.ext.Info().Description }
func (w *extWrapper) Schema() json.RawMessage  { return w.ext.Info().Schema }

func (w *extWrapper) Execute(ctx context.Context, params json.RawMessage) (*tools.Result, error) {
	content, isError, err := w.ext.Execute(ctx, params)
	if err != nil {
		return errResult("extension error: " + err.Error()), nil
	}
	return &tools.Result{
		Content: []model.ContentBlock{{Type: model.ContentTypeText, Text: content}},
		IsError: isError,
	}, nil
}

// ── ext meta-dispatcher ───────────────────────────────────────────────────────

type extDispatchTool struct{ mgr *Manager }

func (t *extDispatchTool) Name() string { return "ext" }

func (t *extDispatchTool) Description() string {
	names := make([]string, 0, len(t.mgr.extensions))
	for _, e := range t.mgr.extensions {
		names = append(names, e.Info().Name)
	}
	if len(names) == 0 {
		return "Invoke a named extension. No extensions are currently loaded."
	}
	return "Invoke a named extension. Available: " + strings.Join(names, ", ")
}

func (t *extDispatchTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "extension": {"type": "string",  "description": "Name of the extension to invoke."},
    "params":    {"type": "object",  "description": "Parameters to pass to the extension."}
  },
  "required": ["extension"]
}`)
}

func (t *extDispatchTool) Execute(ctx context.Context, rawParams json.RawMessage) (*tools.Result, error) {
	var p struct {
		Extension string          `json:"extension"`
		Params    json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(rawParams, &p); err != nil {
		return errResult("invalid params: " + err.Error()), nil
	}
	ext, ok := t.mgr.Get(p.Extension)
	if !ok {
		return errResult(fmt.Sprintf("extension %q not found", p.Extension)), nil
	}
	params := p.Params
	if params == nil {
		params = json.RawMessage("{}")
	}
	content, isError, err := ext.Execute(ctx, params)
	if err != nil {
		return errResult("extension error: " + err.Error()), nil
	}
	return &tools.Result{
		Content: []model.ContentBlock{{Type: model.ContentTypeText, Text: content}},
		IsError: isError,
	}, nil
}

func errResult(msg string) *tools.Result {
	return &tools.Result{
		Content: []model.ContentBlock{{Type: model.ContentTypeText, Text: msg}},
		IsError: true,
	}
}
