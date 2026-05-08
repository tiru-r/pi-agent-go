// Package acp implements the Zed Agent Client Protocol (ACP) — a JSON-RPC 2.0
// protocol spoken over stdio that lets Zed use pi as a native language model
// provider.
//
// Wire format: one JSON object per line (no framing headers).
//
// Zed → pi  (requests)
//   initialize  {}
//   complete    {id, model, messages, system?, max_tokens?, temperature?, tools?}
//   cancel      {id}
//
// pi → Zed  (responses + notifications)
//   initialize response  {protocolVersion, name, models:[{id, displayName, maxTokens}]}
//   chunk notification   {method:"chunk", params:{id, text}}
//   complete response    {stopReason, usage?}
//   error response       {code, message}
package acp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/tiru-r/pi-agent-go/internal/config"
	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/provider"
	"github.com/tiru-r/pi-agent-go/internal/provider/factory"
)

const protocolVersion = 1

// ── JSON-RPC 2.0 wire types ───────────────────────────────────────────────────

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // null | number | string
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string     `json:"jsonrpc"`
	ID      any        `json:"id"`
	Result  any        `json:"result,omitempty"`
	Error   *rpcError  `json:"error,omitempty"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ── ACP-specific parameter / result shapes ────────────────────────────────────

type initializeResult struct {
	ProtocolVersion int        `json:"protocolVersion"`
	Name            string     `json:"name"`
	Models          []acpModel `json:"models"`
}

type acpModel struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	MaxTokens        int    `json:"maxTokens"`
	Provider         string `json:"provider"`
	SupportsTools    bool   `json:"supportsTools,omitempty"`
	SupportsVision   bool   `json:"supportsVision,omitempty"`
	SupportsThinking bool   `json:"supportsThinking,omitempty"`
}

type completeParams struct {
	// ID is the caller-assigned request identifier used to match chunk
	// notifications and cancel requests back to this call.
	ID            json.RawMessage        `json:"id"`
	Model         string                 `json:"model"`
	Messages      []acpMessage           `json:"messages"`
	System        string                 `json:"system,omitempty"`
	MaxTokens     int                    `json:"maxTokens,omitempty"`
	Temperature   *float64               `json:"temperature,omitempty"`
	StopSequences []string               `json:"stopSequences,omitempty"`
	Tools         []model.ToolDefinition `json:"tools,omitempty"`
	ThinkingLevel string                 `json:"thinkingLevel,omitempty"`
}

type acpMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type completeResult struct {
	StopReason model.StopReason `json:"stopReason"`
	Usage      *model.Usage     `json:"usage,omitempty"`
}

type chunkParams struct {
	ID   any    `json:"id"`
	Text string `json:"text"`
}

type cancelParams struct {
	ID json.RawMessage `json:"id"`
}

type sessionPromptParams struct {
	// ID is the caller-assigned request identifier for matching chunk notifications.
	ID          json.RawMessage  `json:"id"`
	SessionID   string           `json:"sessionId"`
	Model       string           `json:"model"`
	Prompt      string           `json:"prompt"`
	System      string           `json:"system,omitempty"`
	MaxTokens   int              `json:"maxTokens,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
	Tools       []model.ToolDefinition `json:"tools,omitempty"`
}

type sessionNewResult struct {
	SessionID string `json:"sessionId"`
}

// ── Server ────────────────────────────────────────────────────────────────────

// Server speaks the Zed ACP protocol over stdin/stdout.
type Server struct {
	cfg      *config.Config
	provider provider.Provider

	// outMu serialises writes to stdout so concurrent goroutines don't interleave.
	outMu sync.Mutex
	out   *bufio.Writer

	// cancels maps a normalised request-ID string to the cancel function for the
	// in-flight complete call.
	cancelsMu sync.Mutex
	cancels   map[string]context.CancelFunc
}

// New builds a Server.  It resolves the provider from cfg immediately so that
// startup errors surface before Zed sends the first request.
func New(cfg *config.Config) (*Server, error) {
	p, err := factory.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("acp: init provider: %w", err)
	}
	return &Server{
		cfg:     cfg,
		provider: p,
		out:     bufio.NewWriter(os.Stdout),
		cancels: make(map[string]context.CancelFunc),
	}, nil
}

// Serve reads line-delimited JSON from stdin and dispatches each message.
// It returns when ctx is cancelled or stdin reaches EOF.
func (s *Server) Serve(ctx context.Context) error {
	in := bufio.NewReader(os.Stdin)
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("acp: read: %w", err)
		}
		if len(line) == 0 || (len(line) == 1 && line[0] == '\n') {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.sendError(nil, -32700, "parse error: "+err.Error())
			continue
		}

		// Notifications (no ID) and requests both flow through dispatch.
		// complete is handled in a goroutine so it doesn't block the read loop.
		switch req.Method {
		case "complete":
			go s.handleComplete(ctx, &req)
		case "session/prompt":
			go s.handleSessionPrompt(ctx, &req)
		default:
			s.dispatch(ctx, &req)
		}
	}
}

// dispatch handles synchronous methods on the read goroutine.
func (s *Server) dispatch(ctx context.Context, req *request) {
	switch req.Method {
	case "initialize":
		s.handleInitialize(req)
	case "session/new":
		s.handleSessionNew(req)
	case "cancel":
		s.handleCancel(req)
	default:
		if req.ID != nil {
			s.sendError(rawID(req.ID), -32601, "method not found: "+req.Method)
		}
	}
}

// ── Method handlers ───────────────────────────────────────────────────────────

func (s *Server) handleInitialize(req *request) {
	models := buildModelList()
	s.sendResult(rawID(req.ID), initializeResult{
		ProtocolVersion: protocolVersion,
		Name:            "pi",
		Models:          models,
	})
}

func (s *Server) handleSessionNew(req *request) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		s.sendError(rawID(req.ID), -32000, "session/new: "+err.Error())
		return
	}
	s.sendResult(rawID(req.ID), sessionNewResult{SessionID: hex.EncodeToString(b)})
}

func (s *Server) handleComplete(ctx context.Context, req *request) {
	var p completeParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.sendError(rawID(req.ID), -32602, "invalid params: "+err.Error())
		return
	}

	// Build a child context that can be cancelled by a cancel request.
	callID := string(p.ID)
	cctx, cancel := context.WithCancel(ctx)
	s.registerCancel(callID, cancel)
	defer s.unregisterCancel(callID)
	defer cancel()

	// Map ACP messages → provider messages.
	msgs := make([]model.Message, 0, len(p.Messages))
	for _, m := range p.Messages {
		role := model.Role(m.Role)
		msgs = append(msgs, model.NewTextMessage(role, m.Content))
	}

	maxTokens := p.MaxTokens
	if maxTokens == 0 {
		maxTokens = s.cfg.MaxTokens
	}
	if maxTokens == 0 {
		maxTokens = 8096
	}

	thinkLevel := model.ThinkingLevel(p.ThinkingLevel)
	if thinkLevel == "" {
		thinkLevel = model.ThinkingLevelOff
	}

	modelID := p.Model
	if modelID == "" {
		modelID = s.cfg.Model
	}

	req2 := &provider.Request{
		Model:         modelID,
		Messages:      msgs,
		System:        p.System,
		Tools:         p.Tools,
		MaxTokens:     maxTokens,
		Temperature:   p.Temperature,
		ThinkingLevel: thinkLevel,
		StopSequences: p.StopSequences,
	}

	events, err := s.provider.Stream(cctx, req2)
	if err != nil {
		s.sendError(rawID(req.ID), -32000, "provider error: "+err.Error())
		return
	}

	var (
		usage      model.Usage
		stopReason = model.StopReasonEndTurn
		callIDany  any
	)
	// Unmarshal p.ID back to a concrete Go value for the chunk params.
	_ = json.Unmarshal(p.ID, &callIDany)

	for ev := range events {
		switch ev.Type {
		case provider.EventTextDelta:
			if ev.Text != "" {
				s.sendNotification("chunk", chunkParams{ID: callIDany, Text: ev.Text})
			}
		case provider.EventMessageStop:
			stopReason = ev.StopReason
			usage = ev.Usage
		case provider.EventError:
			if ev.Err != nil && cctx.Err() == nil {
				slog.Warn("acp stream error", "err", ev.Err)
				s.sendError(rawID(req.ID), -32000, "stream error: "+ev.Err.Error())
				return
			}
		}
	}

	// If the context was cancelled (by a cancel request), send a cancelled error.
	if cctx.Err() != nil {
		s.sendError(rawID(req.ID), -32800, "request cancelled")
		return
	}

	s.sendResult(rawID(req.ID), completeResult{
		StopReason: stopReason,
		Usage:      &usage,
	})
}

func (s *Server) handleCancel(req *request) {
	var p cancelParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return // cancel is best-effort, swallow parse errors
	}
	s.cancelRequest(string(p.ID))
}

func (s *Server) handleSessionPrompt(ctx context.Context, req *request) {
	var p sessionPromptParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.sendError(rawID(req.ID), -32602, "invalid params: "+err.Error())
		return
	}
	if p.Prompt == "" {
		s.sendError(rawID(req.ID), -32602, "prompt is required")
		return
	}

	callID := string(p.ID)
	cctx, cancel := context.WithCancel(ctx)
	s.registerCancel(callID, cancel)
	defer s.unregisterCancel(callID)
	defer cancel()

	msgs := []model.Message{model.NewTextMessage(model.RoleUser, p.Prompt)}

	maxTokens := p.MaxTokens
	if maxTokens == 0 {
		maxTokens = s.cfg.MaxTokens
	}
	if maxTokens == 0 {
		maxTokens = 8096
	}

	modelID := p.Model
	if modelID == "" {
		modelID = s.cfg.Model
	}

	req2 := &provider.Request{
		Model:     modelID,
		Messages:  msgs,
		System:    p.System,
		Tools:     p.Tools,
		MaxTokens: maxTokens,
		Temperature: p.Temperature,
	}

	events, err := s.provider.Stream(cctx, req2)
	if err != nil {
		s.sendError(rawID(req.ID), -32000, "provider error: "+err.Error())
		return
	}

	var (
		usage      model.Usage
		stopReason = model.StopReasonEndTurn
		callIDany  any
	)
	_ = json.Unmarshal(p.ID, &callIDany)

	for ev := range events {
		switch ev.Type {
		case provider.EventTextDelta:
			if ev.Text != "" {
				s.sendNotification("chunk", chunkParams{ID: callIDany, Text: ev.Text})
			}
		case provider.EventMessageStop:
			stopReason = ev.StopReason
			usage = ev.Usage
		case provider.EventError:
			if ev.Err != nil && cctx.Err() == nil {
				slog.Warn("acp session/prompt stream error", "err", ev.Err)
				s.sendError(rawID(req.ID), -32000, "stream error: "+ev.Err.Error())
				return
			}
		}
	}

	if cctx.Err() != nil {
		s.sendError(rawID(req.ID), -32800, "request cancelled")
		return
	}

	s.sendResult(rawID(req.ID), completeResult{
		StopReason: stopReason,
		Usage:      &usage,
	})
}

// ── Cancel registry ───────────────────────────────────────────────────────────

func (s *Server) registerCancel(id string, fn context.CancelFunc) {
	s.cancelsMu.Lock()
	s.cancels[id] = fn
	s.cancelsMu.Unlock()
}

func (s *Server) unregisterCancel(id string) {
	s.cancelsMu.Lock()
	delete(s.cancels, id)
	s.cancelsMu.Unlock()
}

func (s *Server) cancelRequest(id string) {
	s.cancelsMu.Lock()
	fn, ok := s.cancels[id]
	s.cancelsMu.Unlock()
	if ok {
		fn()
	}
}

// ── Write helpers ─────────────────────────────────────────────────────────────

func (s *Server) sendResult(id any, result any) {
	s.write(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) sendError(id any, code int, msg string) {
	s.write(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *Server) sendNotification(method string, params any) {
	s.write(notification{JSONRPC: "2.0", Method: method, Params: params})
}

func (s *Server) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("acp marshal", "err", err)
		return
	}
	s.outMu.Lock()
	_, _ = s.out.Write(b)
	_ = s.out.WriteByte('\n')
	_ = s.out.Flush()
	s.outMu.Unlock()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// rawID unmarshals a json.RawMessage ID to a concrete any (number or string).
// Returns nil for JSON null or zero-length input.
func rawID(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}

// buildModelList returns all registered models as ACP model descriptors.
func buildModelList() []acpModel {
	all := model.Registry
	out := make([]acpModel, 0, len(all))
	for _, m := range all {
		out = append(out, acpModel{
			ID:               m.ID,
			DisplayName:      m.DisplayName,
			MaxTokens:        m.MaxTokens,
			Provider:         m.Provider,
			SupportsTools:    m.SupportsTools,
			SupportsVision:   m.SupportsVision,
			SupportsThinking: m.SupportsThinking,
		})
	}
	return out
}
