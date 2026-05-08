// Package acp implements the Zed Agent Client Protocol (ACP) — a JSON-RPC 2.0
// protocol spoken over stdio that lets Zed use pi as a native language model
// provider.
//
// Wire format: one JSON object per line (no framing headers).
//
// Zed → pi  (requests)
//   initialize     {}
//   complete       {id, model, messages, system?, max_tokens?, temperature?, tools?}
//   session/new    {}
//   session/prompt {id, sessionId, model, prompt, system?, max_tokens?, tools?}
//   cancel         {id}
//
// pi → Zed  (responses + notifications)
//   initialize response  {protocolVersion, name, models:[{id, displayName, maxTokens}]}
//   chunk notification   {method:"chunk", params:{id, text}}
//   complete response    {stopReason, usage?}
//   error response       {code, message}
//
// Session history is maintained in-memory per sessionId.  Set PI_DEBUG=1 to
// write verbose logs to ~/.pi/agent/acp.log.
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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tiru-r/pi-agent-go/internal/agent"
	"github.com/tiru-r/pi-agent-go/internal/config"
	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/provider"
	"github.com/tiru-r/pi-agent-go/internal/provider/factory"
	"github.com/tiru-r/pi-agent-go/internal/provider/openrouter"
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
	Role    string     `json:"role"`
	Content flexString `json:"content"`
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

// flexString unmarshals JSON that may arrive as a plain string or as an array
// of content blocks (e.g. [{type:"text",text:"…"}]).  Used for both message
// content fields and the session/prompt prompt field.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &blocks); err != nil {
		return err
	}
	var sb strings.Builder
	for _, blk := range blocks {
		sb.WriteString(blk.Text)
	}
	*f = flexString(sb.String())
	return nil
}

type sessionPromptParams struct {
	// ID is the caller-assigned request identifier for matching chunk notifications.
	ID          json.RawMessage        `json:"id"`
	SessionID   string                 `json:"sessionId"`
	Model       string                 `json:"model"`
	Prompt      flexString             `json:"prompt"`
	System      string                 `json:"system,omitempty"`
	MaxTokens   int                    `json:"maxTokens,omitempty"`
	Temperature *float64               `json:"temperature,omitempty"`
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
	// in-flight complete or session/prompt call.
	cancelsMu sync.Mutex
	cancels   map[string]context.CancelFunc

	// sessions holds per-sessionId message history (in-memory, lives until the
	// server exits / Zed closes the subprocess).
	sessionsMu sync.RWMutex
	sessions   map[string][]model.Message

	// models is populated in the background on startup by fetching OpenRouter's
	// /api/v1/models endpoint.  modelsReady is closed when the fetch completes.
	modelsReady chan struct{}
	modelsMu    sync.RWMutex
	models      []acpModel
}

// New builds a Server.  It resolves the provider from cfg immediately so that
// startup errors surface before Zed sends the first request.
// Set PI_DEBUG=1 to write verbose logs to ~/.pi/agent/acp.log.
func New(cfg *config.Config) (*Server, error) {
	p, err := factory.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("acp: init provider: %w", err)
	}
	if os.Getenv("PI_DEBUG") != "" {
		setupDebugLog(cfg)
	}
	s := &Server{
		cfg:         cfg,
		provider:    p,
		out:         bufio.NewWriter(os.Stdout),
		cancels:     make(map[string]context.CancelFunc),
		sessions:    make(map[string][]model.Message),
		modelsReady: make(chan struct{}),
	}
	go s.prefetchModels()
	return s, nil
}

// prefetchModels fetches the OpenRouter model list in the background.
// Closes s.modelsReady when done (whether successful or not).
func (s *Server) prefetchModels() {
	defer close(s.modelsReady)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	infos, err := openrouter.FetchModels(ctx, s.cfg.OpenRouterAPIKey)
	if err != nil {
		slog.Warn("model prefetch failed", "err", err)
		return
	}
	s.modelsMu.Lock()
	s.models = toACPModels(infos)
	s.modelsMu.Unlock()
	slog.Debug("models loaded", "count", len(infos))
}

// setupDebugLog redirects slog output to ~/.pi/agent/acp.log.
func setupDebugLog(cfg *config.Config) {
	logDir := filepath.Dir(cfg.SessionDir)
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(logDir, "acp.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})))
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
	// Wait up to 5 s for the background model fetch; proceed with whatever is ready.
	select {
	case <-s.modelsReady:
	case <-time.After(5 * time.Second):
		slog.Warn("model fetch timed out during initialize")
	}
	s.modelsMu.RLock()
	models := s.models
	s.modelsMu.RUnlock()
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
		msgs = append(msgs, model.NewTextMessage(role, string(m.Content)))
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
		case provider.EventThinkingDelta:
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

	var callIDany any
	_ = json.Unmarshal(p.ID, &callIDany)

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
	system := p.System
	if system == "" {
		system = s.cfg.SystemPrompt
	}

	history := s.loadSession(p.SessionID)
	ag := agent.New(s.provider, modelID, system, maxTokens)

	var finalStop model.StopReason = model.StopReasonEndTurn
	var finalUsage model.Usage

	slog.Debug("session/prompt start", "session", p.SessionID, "model", modelID, "history_len", len(history))

	updatedMsgs, err := ag.Run(cctx, string(p.Prompt), history, agent.Options{}, func(ev agent.AgentEvent) {
		switch ev.Kind {
		case agent.EventKindText:
			if ev.Delta != "" {
				s.sendNotification("chunk", chunkParams{ID: callIDany, Text: ev.Delta})
			}
		case agent.EventKindThinking:
			if ev.Delta != "" {
				s.sendNotification("chunk", chunkParams{ID: callIDany, Text: ev.Delta})
			}
		case agent.EventKindToolStart:
			s.sendNotification("chunk", chunkParams{ID: callIDany, Text: fmt.Sprintf("\n[tool: %s]\n", ev.ToolName)})
			slog.Debug("tool start", "name", ev.ToolName)
		case agent.EventKindToolDone:
			slog.Debug("tool done", "name", ev.ToolResult.Name)
		case agent.EventKindDone:
			finalStop = ev.StopReason
			finalUsage = ev.Usage
		}
	})

	if cctx.Err() != nil {
		s.sendError(rawID(req.ID), -32800, "request cancelled")
		return
	}
	if err != nil {
		slog.Warn("session/prompt agent error", "err", err)
		s.sendError(rawID(req.ID), -32000, "agent error: "+err.Error())
		return
	}

	s.saveSession(p.SessionID, updatedMsgs)
	slog.Debug("session/prompt done", "session", p.SessionID, "msgs", len(updatedMsgs))

	s.sendResult(rawID(req.ID), completeResult{
		StopReason: finalStop,
		Usage:      &finalUsage,
	})
}

// ── Session history store ─────────────────────────────────────────────────────

func (s *Server) loadSession(id string) []model.Message {
	if id == "" {
		return nil
	}
	s.sessionsMu.RLock()
	msgs := s.sessions[id]
	s.sessionsMu.RUnlock()
	out := make([]model.Message, len(msgs))
	copy(out, msgs)
	return out
}

func (s *Server) saveSession(id string, msgs []model.Message) {
	if id == "" {
		return
	}
	s.sessionsMu.Lock()
	s.sessions[id] = msgs
	s.sessionsMu.Unlock()
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

// toACPModels converts ModelInfo slice to ACP model descriptors.
func toACPModels(infos []model.ModelInfo) []acpModel {
	out := make([]acpModel, 0, len(infos))
	for _, m := range infos {
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
