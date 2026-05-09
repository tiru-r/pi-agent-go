// Package acp implements the Agent Client Protocol (ACP) — a JSON-RPC 2.0
// protocol spoken over stdio that lets Zed use pi as a native agent via the
// agent_servers configuration.
//
// Wire format: one JSON object per line (no framing headers).
//
// Zed → pi  (requests)
//   initialize                 {protocolVersion, clientInfo?, clientCapabilities?}
//   session/new                {}
//   session/prompt             {sessionId, prompt:[{type:"text",text:"…"}]}
//   session/cancel             {sessionId}  (notification, no id)
//   session/set_config_option  {sessionId, configId, value}
//   session/set_model          {sessionId, modelId}
//   session/close              {sessionId}
//
// pi → Zed  (responses + notifications)
//   initialize result          {protocolVersion, agentInfo:{name,version}}
//   session/new result         {sessionId, configOptions:[model-select, thinking-select]}
//   session/update notification {sessionId, update:{sessionUpdate:"agent_message_chunk"|"agent_thought_chunk", content:{type:"text",text:"…"}}}
//   session/prompt result      {stopReason, usage?}
//   session/set_config_option  {configOptions:[…]}
//   error response             {code, message}
//
// Set PI_DEBUG=1 to write verbose logs to ~/.pi/agent/acp.log.
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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tiru-r/pi-agent-go/internal/agent"
	"github.com/tiru-r/pi-agent-go/internal/config"
	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/provider"
	"github.com/tiru-r/pi-agent-go/internal/provider/factory"
	"github.com/tiru-r/pi-agent-go/internal/provider/openrouter"
	"github.com/tiru-r/pi-agent-go/internal/runtime"
	"github.com/tiru-r/pi-agent-go/internal/session"
)

const protocolVersion = 1

// ── JSON-RPC 2.0 wire types ───────────────────────────────────────────────────

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // null | number | string; absent = notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
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

// ── ACP wire shapes ───────────────────────────────────────────────────────────

type acpInitResult struct {
	ProtocolVersion int      `json:"protocolVersion"`
	AgentInfo       acpImpl  `json:"agentInfo"`
}

type acpImpl struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// acpModel describes a model returned in the model config-option.
type acpModel struct {
	ID               string
	DisplayName      string
	MaxTokens        int
	SupportsThinking bool
}

// acpSelectOpt is one value in a select config-option.
type acpSelectOpt struct {
	Value string `json:"value"`
	Name  string `json:"name"`
}

// acpConfigOpt is a session config option (type="select" or type="boolean").
// The fields deliberately mirror the ACP SessionConfigOption shape.
type acpConfigOpt struct {
	Type         string         `json:"type"`
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Category     string         `json:"category,omitempty"`
	Description  string         `json:"description,omitempty"`
	CurrentValue string         `json:"currentValue"`
	Options      []acpSelectOpt `json:"options"`
}

type acpSessionNewResult struct {
	SessionID     string         `json:"sessionId"`
	ConfigOptions []acpConfigOpt `json:"configOptions,omitempty"`
}

// acpUpdateParams is the session/update notification payload.
type acpUpdateParams struct {
	SessionID string    `json:"sessionId"`
	Update    acpUpdate `json:"update"`
}

type acpUpdate struct {
	SessionUpdate string     `json:"sessionUpdate"` // "agent_message_chunk" | "agent_thought_chunk"
	Content       acpContent `json:"content"`
}

type acpContent struct {
	Type string `json:"type"` // always "text"
	Text string `json:"text"`
}

// acpPromptParams covers both the new ACP (prompt array) and the old format.
type acpPromptParams struct {
	SessionID string     `json:"sessionId"`
	Prompt    flexString `json:"prompt"`    // new: [{type:"text",text:…}]; old: "string"
	MessageID string     `json:"messageId,omitempty"`
}

type acpPromptResult struct {
	StopReason string    `json:"stopReason"`
	Usage      *acpUsage `json:"usage,omitempty"`
}

type acpUsage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	TotalTokens  int `json:"totalTokens"`
}

type acpSetConfigParams struct {
	SessionID string          `json:"sessionId"`
	ConfigID  string          `json:"configId"`
	Value     json.RawMessage `json:"value"` // string value for select options
}

type acpSetConfigResult struct {
	ConfigOptions []acpConfigOpt `json:"configOptions"`
}

type acpSetModelParams struct {
	SessionID string `json:"sessionId"`
	ModelID   string `json:"modelId"`
}

type acpSessionCloseParams struct {
	SessionID string `json:"sessionId"`
}

// flexString unmarshals JSON that may arrive as a plain string or as an array
// of content blocks (e.g. [{type:"text",text:"…"}]).
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

// ── Session state ─────────────────────────────────────────────────────────────

type sessionState struct {
	// sess persists conversation history to JSONL; nil if file creation failed.
	sess       *session.Session
	msgs       []model.Message // in-memory cache, always the authoritative view
	modelID    string
	thinkLevel model.ThinkingLevel
}

// ── Server ────────────────────────────────────────────────────────────────────

// Server speaks the Zed ACP protocol over stdin/stdout.
type Server struct {
	cfg      *config.Config
	provider provider.Provider

	// outMu serialises writes to stdout so concurrent goroutines don't interleave.
	outMu sync.Mutex
	out   *bufio.Writer

	// cancels maps sessionId to the cancel function for an in-flight prompt.
	cancelsMu sync.Mutex
	cancels   map[string]context.CancelFunc

	// sessions holds per-sessionId state (messages, model, thinking level).
	sessionsMu sync.RWMutex
	sessions   map[string]*sessionState

	// models is populated in the background on startup by fetching OpenRouter's
	// /api/v1/models endpoint. modelsReady is closed when the fetch completes.
	modelsReady chan struct{}
	modelsMu    sync.RWMutex
	models      []acpModel

	// monitor provides runtime intelligence across all sessions.
	monitor *runtime.Monitor

	// sqliteStore is the session index; nil when SQLite is disabled or unavailable.
	sqliteStore *session.SQLiteStore
}

// New builds a Server.
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
		sessions:    make(map[string]*sessionState),
		modelsReady: make(chan struct{}),
		monitor:     runtime.NewMonitor(),
	}
	// Open SQLite session index if enabled. Failure is non-fatal.
	if cfg.SQLite {
		if err := os.MkdirAll(cfg.SessionDir, 0o700); err == nil {
			if store, err := session.NewSQLiteStore(filepath.Join(cfg.SessionDir, "index.db")); err == nil {
				s.sqliteStore = store
			} else {
				slog.Warn("acp: sqlite store unavailable", "err", err)
			}
		}
	}
	go s.prefetchModels()
	return s, nil
}

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

		switch req.Method {
		case "session/prompt":
			go s.handleSessionPrompt(ctx, &req)
		default:
			s.dispatch(ctx, &req)
		}
	}
}

func (s *Server) dispatch(_ context.Context, req *request) {
	switch req.Method {
	case "initialize":
		s.handleInitialize(req)
	case "session/new":
		s.handleSessionNew(req)
	case "session/cancel":
		s.handleSessionCancel(req)
	case "session/set_config_option":
		s.handleSessionSetConfigOption(req)
	case "session/set_model":
		s.handleSessionSetModel(req)
	case "session/close":
		s.handleSessionClose(req)
	case "runtime/report":
		s.handleRuntimeReport(req)
	default:
		if req.ID != nil {
			s.sendError(rawID(req.ID), -32601, "method not found: "+req.Method)
		}
	}
}

// ── Method handlers ───────────────────────────────────────────────────────────

func (s *Server) handleRuntimeReport(req *request) {
	report := s.monitor.Report()
	s.sendResult(rawID(req.ID), report)
}

func (s *Server) handleInitialize(req *request) {
	s.sendResult(rawID(req.ID), acpInitResult{
		ProtocolVersion: protocolVersion,
		AgentInfo:       acpImpl{Name: "pi", Version: "1.0.0"},
	})
}

func (s *Server) handleSessionNew(req *request) {
	// Wait up to 5 s for the background model fetch.
	select {
	case <-s.modelsReady:
	case <-time.After(5 * time.Second):
		slog.Warn("model fetch timed out during session/new")
	}

	id := newSessionID()

	// Strip provider prefix that may have been stored in old configs.
	modelID := strings.TrimPrefix(s.cfg.Model, "openrouter/")

	thinkLevel := model.ThinkingLevel(s.cfg.ThinkingLevel)
	if thinkLevel == "" {
		thinkLevel = model.ThinkingLevelOff
	}

	// Create a persistent JSONL session for this ACP session.
	var sess *session.Session
	if err := os.MkdirAll(s.cfg.SessionDir, 0o700); err == nil {
		if newSess, err := session.New(s.cfg.SessionDir); err == nil {
			sess = newSess
		} else {
			slog.Warn("acp: create session file", "err", err)
		}
	}

	s.sessionsMu.Lock()
	s.sessions[id] = &sessionState{
		sess:       sess,
		modelID:    modelID,
		thinkLevel: thinkLevel,
	}
	s.sessionsMu.Unlock()

	// Register in SQLite index so `pi session list` shows it immediately.
	if s.sqliteStore != nil && sess != nil {
		if err := s.sqliteStore.SaveSession(sess); err == nil {
			_ = s.sqliteStore.UpdateSessionMeta(sess.ID, modelID, "openrouter")
		}
	}

	s.modelsMu.RLock()
	models := s.models
	s.modelsMu.RUnlock()

	s.sendResult(rawID(req.ID), acpSessionNewResult{
		SessionID:     id,
		ConfigOptions: s.makeConfigOptions(modelID, thinkLevel, models),
	})
}

func (s *Server) handleSessionSetConfigOption(req *request) {
	var p acpSetConfigParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.sendError(rawID(req.ID), -32602, "invalid params: "+err.Error())
		return
	}

	sess := s.getSession(p.SessionID)
	if sess == nil {
		s.sendError(rawID(req.ID), -32000, "session not found: "+p.SessionID)
		return
	}

	// Decode string value.
	var valStr string
	_ = json.Unmarshal(p.Value, &valStr)

	s.sessionsMu.Lock()
	switch p.ConfigID {
	case "model":
		sess.modelID = valStr
	case "thinking_level":
		sess.thinkLevel = model.ThinkingLevel(valStr)
	}
	modelID := sess.modelID
	thinkLevel := sess.thinkLevel
	s.sessionsMu.Unlock()

	s.modelsMu.RLock()
	models := s.models
	s.modelsMu.RUnlock()

	s.sendResult(rawID(req.ID), acpSetConfigResult{
		ConfigOptions: s.makeConfigOptions(modelID, thinkLevel, models),
	})
}

func (s *Server) handleSessionSetModel(req *request) {
	var p acpSetModelParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.sendError(rawID(req.ID), -32602, "invalid params: "+err.Error())
		return
	}

	sess := s.getSession(p.SessionID)
	if sess == nil {
		s.sendError(rawID(req.ID), -32000, "session not found: "+p.SessionID)
		return
	}

	s.sessionsMu.Lock()
	sess.modelID = p.ModelID
	s.sessionsMu.Unlock()

	s.sendResult(rawID(req.ID), map[string]any{})
}

func (s *Server) handleSessionCancel(req *request) {
	if req.Params == nil {
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return
	}
	s.cancelsMu.Lock()
	if fn, ok := s.cancels[p.SessionID]; ok {
		fn()
	}
	s.cancelsMu.Unlock()
}

func (s *Server) handleSessionClose(req *request) {
	if req.Params == nil {
		if req.ID != nil {
			s.sendResult(rawID(req.ID), map[string]any{})
		}
		return
	}
	var p acpSessionCloseParams
	_ = json.Unmarshal(req.Params, &p)

	// Cancel any in-flight prompt and remove session.
	s.cancelsMu.Lock()
	if fn, ok := s.cancels[p.SessionID]; ok {
		fn()
		delete(s.cancels, p.SessionID)
	}
	s.cancelsMu.Unlock()

	s.sessionsMu.Lock()
	ss := s.sessions[p.SessionID]
	delete(s.sessions, p.SessionID)
	s.sessionsMu.Unlock()

	if ss != nil && ss.sess != nil {
		_ = ss.sess.Close()
	}

	if req.ID != nil {
		s.sendResult(rawID(req.ID), map[string]any{})
	}
}

func (s *Server) handleSessionPrompt(ctx context.Context, req *request) {
	var p acpPromptParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.sendError(rawID(req.ID), -32602, "invalid params: "+err.Error())
		return
	}
	if p.Prompt == "" {
		s.sendError(rawID(req.ID), -32602, "prompt is required")
		return
	}

	// Expand @file tokens so Zed users can reference local files.
	prompt := expandAtFilesACP(string(p.Prompt))

	cctx, cancel := context.WithCancel(ctx)
	s.cancelsMu.Lock()
	s.cancels[p.SessionID] = cancel
	s.cancelsMu.Unlock()
	defer func() {
		s.cancelsMu.Lock()
		delete(s.cancels, p.SessionID)
		s.cancelsMu.Unlock()
		cancel()
	}()

	ss := s.getSession(p.SessionID)
	if ss == nil {
		// Auto-create session with defaults if not found.
		var newSess *session.Session
		if err := os.MkdirAll(s.cfg.SessionDir, 0o700); err == nil {
			newSess, _ = session.New(s.cfg.SessionDir)
		}
		ss = &sessionState{
			sess:       newSess,
			modelID:    strings.TrimPrefix(s.cfg.Model, "openrouter/"),
			thinkLevel: model.ThinkingLevel(s.cfg.ThinkingLevel),
		}
		s.sessionsMu.Lock()
		s.sessions[p.SessionID] = ss
		s.sessionsMu.Unlock()
	}

	s.sessionsMu.RLock()
	modelID := ss.modelID
	thinkLevel := ss.thinkLevel
	fileSess := ss.sess
	history := make([]model.Message, len(ss.msgs))
	copy(history, ss.msgs)
	s.sessionsMu.RUnlock()

	historyLen := len(history)

	system := s.cfg.SystemPrompt
	maxTokens := s.cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 8096
	}

	ag := agent.New(s.provider, modelID, system, maxTokens)
	ag.Monitor = s.monitor

	var finalStop model.StopReason = model.StopReasonEndTurn
	var finalUsage model.Usage

	slog.Debug("session/prompt", "session", p.SessionID, "model", modelID, "thinking", thinkLevel)

	updatedMsgs, err := ag.Run(cctx, prompt, history, agent.Options{
		ThinkingLevel: thinkLevel,
	}, func(ev agent.AgentEvent) {
		switch ev.Kind {
		case agent.EventKindText:
			if ev.Delta != "" {
				s.sendSessionUpdate(p.SessionID, "agent_message_chunk", ev.Delta)
			}
		case agent.EventKindThinking:
			if ev.Delta != "" {
				s.sendSessionUpdate(p.SessionID, "agent_thought_chunk", ev.Delta)
			}
		case agent.EventKindToolStart:
			s.sendSessionUpdate(p.SessionID, "agent_message_chunk",
				fmt.Sprintf("\n[tool: %s]\n", ev.ToolName))
			slog.Debug("tool start", "name", ev.ToolName)
		case agent.EventKindToolDone:
			slog.Debug("tool done", "name", ev.ToolResult.Name)
		case agent.EventKindDone:
			finalStop = ev.StopReason
			finalUsage = ev.Usage
		}
	})

	if cctx.Err() != nil {
		s.sendResult(rawID(req.ID), acpPromptResult{StopReason: "cancelled"})
		return
	}
	if err != nil {
		slog.Warn("session/prompt agent error", "err", err)
		s.sendError(rawID(req.ID), -32000, "agent error: "+err.Error())
		return
	}

	// Persist new messages to the JSONL session file.
	if fileSess != nil {
		for _, msg := range updatedMsgs[historyLen:] {
			_ = fileSess.AppendMessage(msg, nil)
		}
		// Keep SQLite index in sync.
		if s.sqliteStore != nil {
			if err := s.sqliteStore.SaveSession(fileSess); err == nil {
				_ = s.sqliteStore.UpdateSessionMeta(fileSess.ID, modelID, "openrouter")
			}
		}
	}

	// Update in-memory cache.
	s.sessionsMu.Lock()
	if ss2 := s.sessions[p.SessionID]; ss2 != nil {
		ss2.msgs = updatedMsgs
	}
	s.sessionsMu.Unlock()

	total := finalUsage.InputTokens + finalUsage.OutputTokens
	s.sendResult(rawID(req.ID), acpPromptResult{
		StopReason: string(finalStop),
		Usage: &acpUsage{
			InputTokens:  finalUsage.InputTokens,
			OutputTokens: finalUsage.OutputTokens,
			TotalTokens:  total,
		},
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (s *Server) sendSessionUpdate(sessionID, updateType, text string) {
	s.sendNotification("session/update", acpUpdateParams{
		SessionID: sessionID,
		Update: acpUpdate{
			SessionUpdate: updateType,
			Content:       acpContent{Type: "text", Text: text},
		},
	})
}

// normalizeThinkingLevel maps legacy aliases to the canonical 6-level set so
// that the ACP config option always shows a valid current selection.
func normalizeThinkingLevel(level model.ThinkingLevel) model.ThinkingLevel {
	switch level {
	case model.ThinkingLevelOff, model.ThinkingLevelMinimal,
		model.ThinkingLevelLow, model.ThinkingLevelMedium,
		model.ThinkingLevelHigh, model.ThinkingLevelXHigh:
		return level
	default:
		return model.ThinkingLevelOff
	}
}

func (s *Server) makeConfigOptions(modelID string, thinkLevel model.ThinkingLevel, models []acpModel) []acpConfigOpt {
	thinkStr := string(normalizeThinkingLevel(thinkLevel))

	// Build model options list.
	var modelOpts []acpSelectOpt
	currentFound := false
	for _, m := range models {
		if m.ID == modelID {
			currentFound = true
		}
		modelOpts = append(modelOpts, acpSelectOpt{Value: m.ID, Name: m.DisplayName})
	}
	// Ensure the current model is always present.
	if !currentFound && modelID != "" {
		modelOpts = append([]acpSelectOpt{{Value: modelID, Name: modelID}}, modelOpts...)
	}
	if len(modelOpts) == 0 {
		modelOpts = []acpSelectOpt{{Value: modelID, Name: modelID}}
	}

	return []acpConfigOpt{
		{
			Type:         "select",
			ID:           "model",
			Name:         "Model",
			Category:     "model",
			CurrentValue: modelID,
			Options:      modelOpts,
		},
		{
			Type:         "select",
			ID:           "thinking_level",
			Name:         "Thinking",
			Category:     "thought_level",
			CurrentValue: thinkStr,
			Options: []acpSelectOpt{
				{Value: "off", Name: "Off"},
				{Value: "minimal", Name: "Minimal"},
				{Value: "low", Name: "Low"},
				{Value: "medium", Name: "Medium"},
				{Value: "high", Name: "High"},
				{Value: "xhigh", Name: "Max"},
			},
		},
	}
}

func (s *Server) getSession(id string) *sessionState {
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()
	return s.sessions[id]
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
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

// rawID unmarshals a json.RawMessage ID to a concrete any (number or string).
func rawID(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}

// atFileRe matches @<non-whitespace> tokens used for file expansion.
var atFileRe = regexp.MustCompile(`@(\S+)`)

// expandAtFilesACP replaces @filepath tokens in s with the file's contents.
// Unreadable paths are left unchanged.
func expandAtFilesACP(s string) string {
	return atFileRe.ReplaceAllStringFunc(s, func(match string) string {
		data, err := os.ReadFile(match[1:]) // strip leading @
		if err != nil {
			return match
		}
		return string(data)
	})
}

func toACPModels(infos []model.ModelInfo) []acpModel {
	out := make([]acpModel, 0, len(infos))
	for _, m := range infos {
		out = append(out, acpModel{
			ID:               m.ID,
			DisplayName:      m.DisplayName,
			MaxTokens:        m.MaxTokens,
			SupportsThinking: m.SupportsThinking,
		})
	}
	return out
}
