// Package rpc implements a JSON-RPC 2.0 server over stdin/stdout for SDK embedding.
package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/pi-agent/pi/internal/agent"
	"github.com/pi-agent/pi/internal/config"
	"github.com/pi-agent/pi/internal/model"
	"github.com/pi-agent/pi/internal/session"
)

// ── JSON-RPC types ─────────────────────────────────────────────────────────────

// Request is an inbound JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// Response is an outbound JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

// RPCError is the error object inside a JSON-RPC response.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Notification is an outbound JSON-RPC notification (no ID).
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// Standard JSON-RPC 2.0 error codes.
const (
	ErrParseError     = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternal       = -32603
)

// ── Server ────────────────────────────────────────────────────────────────────

// Server is the JSON-RPC server.
type Server struct {
	ag      *agent.Agent
	cfg     *config.Config
	session *session.Session

	in  io.Reader
	out io.Writer
	enc *json.Encoder
}

// NewServer creates a new Server that reads from in and writes to out.
// in/out default to os.Stdin/os.Stdout when nil.
func NewServer(ag *agent.Agent, cfg *config.Config, in io.Reader, out io.Writer) *Server {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	enc := json.NewEncoder(out)
	return &Server{ag: ag, cfg: cfg, in: in, out: out, enc: enc}
}

// Serve reads JSON-RPC requests from stdin and dispatches them until ctx is
// cancelled or EOF.
func (s *Server) Serve(ctx context.Context) error {
	scanner := bufio.NewScanner(s.in)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("rpc: read: %w", err)
			}
			return nil // EOF
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = s.sendError(nil, ErrParseError, "parse error: "+err.Error())
			continue
		}
		if req.JSONRPC != "2.0" {
			_ = s.sendError(req.ID, ErrInvalidRequest, "jsonrpc must be \"2.0\"")
			continue
		}

		if err := s.dispatch(ctx, &req); err != nil {
			_ = s.sendError(req.ID, ErrInternal, err.Error())
		}
	}
}

// dispatch routes a request to its handler.
func (s *Server) dispatch(ctx context.Context, req *Request) error {
	switch req.Method {
	case "chat":
		return s.handleChat(ctx, req)
	case "session/new":
		return s.handleSessionNew(req)
	case "session/list":
		return s.handleSessionList(req)
	case "session/open":
		return s.handleSessionOpen(req)
	case "model/list":
		return s.handleModelList(req)
	default:
		return s.sendError(req.ID, ErrMethodNotFound,
			fmt.Sprintf("method not found: %s", req.Method))
	}
}

// ── Method handlers ───────────────────────────────────────────────────────────

// chatParams are the parameters for the "chat" method.
type chatParams struct {
	Message string `json:"message"`
	Model   string `json:"model,omitempty"`
	System  string `json:"system,omitempty"`
}

func (s *Server) handleChat(ctx context.Context, req *Request) error {
	var p chatParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return s.sendError(req.ID, ErrInvalidParams, "invalid params: "+err.Error())
	}
	if p.Message == "" {
		return s.sendError(req.ID, ErrInvalidParams, "message is required")
	}

	// Override model if specified.
	if p.Model != "" {
		s.cfg.Model = p.Model
	}

	opts := agent.Options{
		System: p.System,
	}
	if opts.System == "" {
		opts.System = s.cfg.SystemPrompt
	}

	var history []model.Message
	if s.session != nil {
		history = s.session.Messages()
	}

	// Stream events as JSON-RPC notifications.
	onEvent := func(ev agent.AgentEvent) {
		switch ev.Kind {
		case agent.EventKindText:
			_ = s.sendNotification("chat/event", map[string]any{
				"event": "text",
				"text":  ev.Delta,
			})
		case agent.EventKindThinking:
			_ = s.sendNotification("chat/event", map[string]any{
				"event": "thinking",
				"text":  ev.Delta,
			})
		case agent.EventKindToolStart:
			_ = s.sendNotification("chat/event", map[string]any{
				"event":     "tool_start",
				"tool_id":   ev.ToolID,
				"tool_name": ev.ToolName,
			})
		case agent.EventKindToolDone:
			_ = s.sendNotification("chat/event", map[string]any{
				"event":    "tool_done",
				"is_error": ev.ToolResult.IsError,
			})
		case agent.EventKindDone:
			_ = s.sendNotification("chat/event", map[string]any{
				"event": "done",
				"usage": ev.Usage,
			})
		case agent.EventKindError:
			_ = s.sendNotification("chat/event", map[string]any{
				"event": "error",
				"error": ev.Err.Error(),
			})
		}
	}

	finalMsgs, err := s.ag.Run(ctx, p.Message, history, opts, onEvent)
	if err != nil {
		return s.sendError(req.ID, ErrInternal, "agent error: "+err.Error())
	}

	// Persist to session if active (messages after the original history).
	if s.session != nil {
		start := len(history)
		if start < len(finalMsgs) {
			for _, msg := range finalMsgs[start:] {
				_ = s.session.AppendMessage(msg, nil)
			}
		}
	}

	// Extract final assistant response text.
	var responseText string
	for i := len(finalMsgs) - 1; i >= 0; i-- {
		if finalMsgs[i].Role == model.RoleAssistant {
			responseText = finalMsgs[i].Text()
			break
		}
	}

	return s.sendResult(req.ID, map[string]any{
		"response": responseText,
		"messages": len(finalMsgs),
	})
}

func (s *Server) handleSessionNew(req *Request) error {
	dir := s.cfg.SessionDir
	sess, err := session.New(dir)
	if err != nil {
		return s.sendError(req.ID, ErrInternal, "create session: "+err.Error())
	}
	s.session = sess
	return s.sendResult(req.ID, map[string]any{
		"session_id": sess.ID,
	})
}

func (s *Server) handleSessionList(req *Request) error {
	// Collect sessions from the JSONL dir.
	dir := s.cfg.SessionDir
	store, err := session.NewSQLiteStore(dir + "/index.db")
	if err != nil {
		return s.sendError(req.ID, ErrInternal, "open index: "+err.Error())
	}
	defer store.Close()

	metas, err := store.ListSessions()
	if err != nil {
		return s.sendError(req.ID, ErrInternal, "list sessions: "+err.Error())
	}

	type sessionSummary struct {
		ID           string `json:"id"`
		Title        string `json:"title"`
		Model        string `json:"model"`
		MessageCount int    `json:"message_count"`
		UpdatedAt    string `json:"updated_at"`
	}
	summaries := make([]sessionSummary, 0, len(metas))
	for _, m := range metas {
		summaries = append(summaries, sessionSummary{
			ID:           m.ID,
			Title:        m.Title,
			Model:        m.Model,
			MessageCount: m.MessageCount,
			UpdatedAt:    m.UpdatedAt.Format("2006-01-02T15:04:05Z"),
		})
	}
	return s.sendResult(req.ID, map[string]any{"sessions": summaries})
}

type sessionOpenParams struct {
	SessionID string `json:"session_id"`
}

func (s *Server) handleSessionOpen(req *Request) error {
	var p sessionOpenParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return s.sendError(req.ID, ErrInvalidParams, "invalid params: "+err.Error())
	}
	if p.SessionID == "" {
		return s.sendError(req.ID, ErrInvalidParams, "session_id is required")
	}

	path := s.cfg.SessionDir + "/" + p.SessionID + ".jsonl"
	sess, err := session.Open(path)
	if err != nil {
		return s.sendError(req.ID, ErrInternal, "open session: "+err.Error())
	}
	s.session = sess

	msgs := sess.Messages()
	return s.sendResult(req.ID, map[string]any{
		"session_id":    sess.ID,
		"title":         sess.Title(),
		"message_count": len(msgs),
	})
}

func (s *Server) handleModelList(req *Request) error {
	type modelEntry struct {
		ID               string  `json:"id"`
		Provider         string  `json:"provider"`
		DisplayName      string  `json:"display_name"`
		MaxTokens        int     `json:"max_tokens"`
		SupportsTools    bool    `json:"supports_tools"`
		SupportsVision   bool    `json:"supports_vision"`
		SupportsThinking bool    `json:"supports_thinking"`
		InputCostPer1M   float64 `json:"input_cost_per_1m,omitempty"`
		OutputCostPer1M  float64 `json:"output_cost_per_1m,omitempty"`
	}

	entries := make([]modelEntry, 0, len(model.Registry))
	for _, m := range model.Registry {
		entries = append(entries, modelEntry{
			ID:               m.ID,
			Provider:         m.Provider,
			DisplayName:      m.DisplayName,
			MaxTokens:        m.MaxTokens,
			SupportsTools:    m.SupportsTools,
			SupportsVision:   m.SupportsVision,
			SupportsThinking: m.SupportsThinking,
			InputCostPer1M:   m.InputCostPer1M,
			OutputCostPer1M:  m.OutputCostPer1M,
		})
	}
	return s.sendResult(req.ID, map[string]any{"models": entries})
}

// ── Wire helpers ──────────────────────────────────────────────────────────────

func (s *Server) sendResult(id any, result any) error {
	return s.enc.Encode(Response{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

func (s *Server) sendError(id any, code int, message string) error {
	return s.enc.Encode(Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &RPCError{Code: code, Message: message},
	})
}

func (s *Server) sendNotification(method string, params any) error {
	return s.enc.Encode(Notification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	})
}
