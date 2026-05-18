// Package acp implements the Agent Client Protocol (ACP) — a JSON-RPC 2.0
// protocol spoken over stdio that lets Zed use pi as a native agent via the
// agent_servers configuration.
//
// Wire format: one JSON object per line (no framing headers).
//
// Zed → pi  (requests)
//
//	initialize                 {protocolVersion, clientInfo?, clientCapabilities?}
//	session/new                {}
//	session/prompt             {sessionId, prompt:[{type:"text",text:"…"}]}
//	session/cancel             {sessionId}  (notification, no id)
//	session/set_config_option  {sessionId, configId, value}
//	session/set_model          {sessionId, modelId}
//	session/close              {sessionId}
//
// pi → Zed  (responses + notifications)
//
//	initialize result          {protocolVersion, agentInfo:{name,version}}
//	session/new result         {sessionId, configOptions:[model-select, thinking-select]}
//	session/update notification {sessionId, update:{sessionUpdate:"agent_message_chunk"|"agent_thought_chunk", content:{type:"text",text:"…"}}}
//	session/update notification {sessionId, update:{sessionUpdate:"tool_call",        toolCallId, kind, status:"in_progress", title, rawInput, locations:[{path,line?}]}}  (tool starts)
//	session/update notification {sessionId, update:{sessionUpdate:"tool_call_update", toolCallId, status:"completed"|"failed", rawOutput}}  (tool done + Follow pi)
//	session/prompt result      {stopReason, usage?}
//	session/set_config_option  {configOptions:[…]}
//	error response             {code, message}
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
	"strings"
	"sync"
	"time"

	"github.com/tiru-r/pi-agent-go/internal/agent"
	"github.com/tiru-r/pi-agent-go/internal/autocomplete"
	"github.com/tiru-r/pi-agent-go/internal/config"
	"github.com/tiru-r/pi-agent-go/internal/extensions"
	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/provider"
	"github.com/tiru-r/pi-agent-go/internal/provider/factory"
	"github.com/tiru-r/pi-agent-go/internal/provider/openrouter"
	"github.com/tiru-r/pi-agent-go/internal/runtime"
	"github.com/tiru-r/pi-agent-go/internal/session"
	"github.com/tiru-r/pi-agent-go/internal/tools"
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
	ProtocolVersion   int              `json:"protocolVersion"`
	AgentInfo         acpImpl          `json:"agentInfo"`
	AgentCapabilities *acpCapabilities `json:"agentCapabilities,omitempty"`
	AuthMethods       []any            `json:"authMethods"` // empty array = no auth required (ACP spec default)
}

type acpImpl struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// acpCapabilities declares agent feature support per the ACP spec.
type acpCapabilities struct {
	LoadSession        bool                   `json:"loadSession,omitempty"`
	ModelSelector      bool                   `json:"modelSelector,omitempty"`
	PromptCapabilities *acpPromptCapabilities `json:"promptCapabilities,omitempty"`
}

type acpPromptCapabilities struct {
	Image           bool `json:"image,omitempty"`
	Audio           bool `json:"audio,omitempty"`
	EmbeddedContext bool `json:"embeddedContext,omitempty"`
}

// acpModel describes a model returned in the model config-option.
type acpModel struct {
	ID               string
	DisplayName      string
	ContextWindow    int
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
	SessionID string        `json:"sessionId"`
	Prompt    promptContent `json:"prompt"` // new: [{type:"text"|"image"|…}]; old: "string"
	MessageID string        `json:"messageId,omitempty"`
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

// acpInitParams is the Zed → pi initialize request body.
type acpInitParams struct {
	ProtocolVersion    int             `json:"protocolVersion"`
	ClientInfo         json.RawMessage `json:"clientInfo,omitempty"`
	ClientCapabilities json.RawMessage `json:"clientCapabilities,omitempty"`
}

// acpSessionNewParams is the Zed → pi session/new request body.
type acpSessionNewParams struct {
	CWD string `json:"cwd,omitempty"`
}

// acpSessionLoadParams is the Zed → pi session/load request body.
type acpSessionLoadParams struct {
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd,omitempty"`
}

type acpSessionLoadResult struct {
	SessionID     string         `json:"sessionId"`
	ConfigOptions []acpConfigOpt `json:"configOptions,omitempty"`
}

// ── Tool update types ─────────────────────────────────────────────────────────

// acpToolCallUpdateParams is the session/update notification for tool_call_update,
// which replaces the old agent_tool_use / agent_tool_result / agent_location types
// that Zed 1.2+ no longer accepts. It also powers the "Follow Pi" feature via Locations.
type acpToolCallUpdateParams struct {
	SessionID string            `json:"sessionId"`
	Update    acpToolCallUpdate `json:"update"`
}

// acpToolCallUpdate covers both "tool_call" (initial creation) and
// "tool_call_update" (status/output changes). The SessionUpdate field
// distinguishes the two per the ACP spec:
//   - "tool_call"        → creates the entry in Zed (sent when execution starts)
//   - "tool_call_update" → patches an existing entry (sent when execution finishes)
//
// rawInput and rawOutput are JSON values (not strings) per the ACP spec.
type acpToolCallUpdate struct {
	SessionUpdate string           `json:"sessionUpdate"`    // "tool_call" | "tool_call_update"
	ToolCallID    string           `json:"toolCallId"`
	Kind          string           `json:"kind,omitempty"`   // read|edit|execute|search|other
	Status        string           `json:"status,omitempty"` // in_progress|completed|failed
	Title         string           `json:"title,omitempty"`
	RawInput      json.RawMessage  `json:"rawInput,omitempty"`  // JSON value
	RawOutput     json.RawMessage  `json:"rawOutput,omitempty"` // JSON value
	Locations     []acpToolCallLoc `json:"locations,omitempty"`
}

// acpToolCallLoc is a file location embedded in tool_call / tool_call_update for Follow Pi.
type acpToolCallLoc struct {
	Path string `json:"path"`
	Line *int   `json:"line,omitempty"`
}

// ── Prompt content block parsing ──────────────────────────────────────────────

// acpIncomingBlock is a single block in an incoming ACP prompt array.
// It covers text, image, and all embedded-context types Zed may send.
type acpIncomingBlock struct {
	Type string `json:"type"`
	// Text-bearing fields (text, file, selection, symbol, branch_diff, thread, rules)
	Text string `json:"text,omitempty"`
	Path string `json:"path,omitempty"`
	Name string `json:"name,omitempty"` // symbol name
	// Selection range
	Start *acpTextPosition `json:"start,omitempty"`
	End   *acpTextPosition `json:"end,omitempty"`
	// Image — Zed sends one of these two shapes
	Image  *acpImageRef    `json:"image,omitempty"`
	Source *acpImageSource `json:"source,omitempty"`
}

type acpTextPosition struct {
	Line   int `json:"line"`
	Column int `json:"column,omitempty"`
}

type acpImageRef struct {
	URL string `json:"url"`
	Alt string `json:"alt,omitempty"`
}

type acpImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// promptContent holds a parsed ACP prompt as model content blocks.
// It accepts both the legacy plain-string format and the new array format.
type promptContent struct {
	Blocks []model.ContentBlock
}

func (p *promptContent) IsEmpty() bool { return len(p.Blocks) == 0 }

func (p *promptContent) UnmarshalJSON(b []byte) error {
	// Legacy format: plain string
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if s != "" {
			p.Blocks = []model.ContentBlock{{Type: model.ContentTypeText, Text: s}}
		}
		return nil
	}
	// New format: typed content-block array
	var raw []acpIncomingBlock
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	p.Blocks = convertIncomingBlocks(raw)
	return nil
}

// convertIncomingBlocks maps ACP wire blocks to model content blocks.
func convertIncomingBlocks(raw []acpIncomingBlock) []model.ContentBlock {
	out := make([]model.ContentBlock, 0, len(raw))
	for _, blk := range raw {
		switch blk.Type {
		case "text":
			if blk.Text != "" {
				out = append(out, model.ContentBlock{Type: model.ContentTypeText, Text: blk.Text})
			}
		case "image":
			if cb, ok := convertImageBlock(blk); ok {
				out = append(out, cb)
			}
		case "file":
			if t := formatFileContext(blk); t != "" {
				out = append(out, model.ContentBlock{Type: model.ContentTypeText, Text: t})
			}
		case "selection":
			if t := formatSelectionContext(blk); t != "" {
				out = append(out, model.ContentBlock{Type: model.ContentTypeText, Text: t})
			}
		case "symbol":
			if t := formatSymbolContext(blk); t != "" {
				out = append(out, model.ContentBlock{Type: model.ContentTypeText, Text: t})
			}
		case "branch_diff":
			if blk.Text != "" {
				out = append(out, model.ContentBlock{
					Type: model.ContentTypeText,
					Text: "**Branch Diff:**\n```diff\n" + blk.Text + "\n```",
				})
			}
		case "thread":
			if blk.Text != "" {
				out = append(out, model.ContentBlock{
					Type: model.ContentTypeText,
					Text: "**Thread context:**\n" + blk.Text,
				})
			}
		case "rules":
			if blk.Text != "" {
				out = append(out, model.ContentBlock{
					Type: model.ContentTypeText,
					Text: "**Rules:**\n" + blk.Text,
				})
			}
		default:
			// Unknown types: preserve any text content.
			if blk.Text != "" {
				out = append(out, model.ContentBlock{Type: model.ContentTypeText, Text: blk.Text})
			}
		}
	}
	return out
}

// convertImageBlock converts an ACP image block to a model ContentBlock.
// Returns false if the block has no usable source.
func convertImageBlock(blk acpIncomingBlock) (model.ContentBlock, bool) {
	src := &model.ImageSource{}
	switch {
	case blk.Image != nil && blk.Image.URL != "":
		url := blk.Image.URL
		if rest, ok := strings.CutPrefix(url, "data:"); ok {
			// data:<mediaType>;base64,<data>
			mediaType, enc, ok2 := strings.Cut(rest, ";")
			if !ok2 || !strings.HasPrefix(enc, "base64,") {
				return model.ContentBlock{}, false
			}
			src.Type = "base64"
			src.MediaType = mediaType
			src.Data = enc[len("base64,"):]
		} else {
			src.Type = "url"
			src.URL = url
		}
	case blk.Source != nil && blk.Source.Type != "":
		src.Type = blk.Source.Type
		src.MediaType = blk.Source.MediaType
		src.Data = blk.Source.Data
		src.URL = blk.Source.URL
	default:
		return model.ContentBlock{}, false
	}
	return model.ContentBlock{Type: model.ContentTypeImage, Source: src}, true
}

func formatFileContext(blk acpIncomingBlock) string {
	if blk.Text == "" {
		return ""
	}
	if blk.Path != "" {
		return "**File: " + blk.Path + "**\n```" + langFromPath(blk.Path) + "\n" + blk.Text + "\n```"
	}
	return blk.Text
}

func formatSelectionContext(blk acpIncomingBlock) string {
	if blk.Text == "" {
		return ""
	}
	header := "**Selection"
	if blk.Path != "" {
		header += " from " + blk.Path
		if blk.Start != nil {
			header += fmt.Sprintf(" (line %d", blk.Start.Line+1)
			if blk.End != nil && blk.End.Line != blk.Start.Line {
				header += fmt.Sprintf("–%d", blk.End.Line+1)
			}
			header += ")"
		}
	}
	header += ":**"
	return header + "\n```" + langFromPath(blk.Path) + "\n" + blk.Text + "\n```"
}

func formatSymbolContext(blk acpIncomingBlock) string {
	if blk.Text == "" {
		return ""
	}
	header := "**Symbol"
	if blk.Name != "" {
		header += ": " + blk.Name
	}
	if blk.Path != "" {
		header += " in " + blk.Path
	}
	header += ":**"
	return header + "\n```" + langFromPath(blk.Path) + "\n" + blk.Text + "\n```"
}

func langFromPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".mts", ".cts":
		return "typescript"
	case ".tsx":
		return "tsx"
	case ".jsx":
		return "jsx"
	case ".rs":
		return "rust"
	case ".c":
		return "c"
	case ".cpp", ".cc", ".cxx", ".c++":
		return "cpp"
	case ".h", ".hpp", ".hxx":
		return "cpp"
	case ".java":
		return "java"
	case ".rb":
		return "ruby"
	case ".sh", ".bash":
		return "bash"
	case ".zsh":
		return "zsh"
	case ".fish":
		return "fish"
	case ".md", ".mdx":
		return "markdown"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".html", ".htm":
		return "html"
	case ".css":
		return "css"
	case ".scss":
		return "scss"
	case ".sql":
		return "sql"
	case ".proto":
		return "protobuf"
	case ".swift":
		return "swift"
	case ".kt", ".kts":
		return "kotlin"
	case ".lua":
		return "lua"
	case ".ex", ".exs":
		return "elixir"
	case ".zig":
		return "zig"
	case ".nix":
		return "nix"
	case ".tf", ".hcl":
		return "hcl"
	case ".xml":
		return "xml"
	case ".dart":
		return "dart"
	case ".php":
		return "php"
	case ".cs":
		return "csharp"
	case ".scala":
		return "scala"
	case ".hs":
		return "haskell"
	case ".ml", ".mli":
		return "ocaml"
	case ".elm":
		return "elm"
	case ".svelte":
		return "svelte"
	case ".vue":
		return "vue"
	case ".r":
		return "r"
	case ".diff", ".patch":
		return "diff"
	default:
		return ""
	}
}

// expandAtFilesInBlocks applies @file expansion to every text block in-place.
func expandAtFilesInBlocks(blocks []model.ContentBlock, cwd string) ([]model.ContentBlock, []string) {
	var allExpanded []string
	out := make([]model.ContentBlock, len(blocks))
	for i, blk := range blocks {
		if blk.Type == model.ContentTypeText && blk.Text != "" {
			expanded, files := autocomplete.ExpandAtFiles(blk.Text, cwd)
			out[i] = model.ContentBlock{Type: model.ContentTypeText, Text: expanded}
			allExpanded = append(allExpanded, files...)
		} else {
			out[i] = blk
		}
	}
	return out, allExpanded
}

// ── Session state ─────────────────────────────────────────────────────────────

type sessionState struct {
	// sess persists conversation history to JSONL; nil if file creation failed.
	sess         *session.Session
	msgs         []model.Message // in-memory cache, always the authoritative view
	modelID      string
	thinkLevel   model.ThinkingLevel
	mode         agent.AgentMode
	cwd          string // project root from session/new or session/load
	systemPrefix string // project snapshot injected at the top of every system prompt
	// monitor persists runtime intelligence (circuit breakers, safety, OPE) across
	// prompts within this session. A fresh monitor per-prompt loses all history.
	monitor *runtime.Monitor
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

	// models / modelIndex / acpIndex are populated in the background on startup.
	// models is the trimmed ACP-wire format; modelIndex retains full metadata
	// (including pricing) for O(1) profile lookups; acpIndex is an O(1) view
	// of models keyed by ID for compactor lookups.
	modelsReady chan struct{}
	modelsMu    sync.RWMutex
	models      []acpModel
	modelIndex  map[string]model.ModelInfo // ID → full info, for O(1) profile lookups
	acpIndex    map[string]acpModel        // ID → ACP model, for O(1) compactor lookups

	// monitor provides runtime intelligence across all sessions.
	monitor *runtime.Monitor

	// extMgr manages loaded extensions and provides hook broadcasting.
	extMgr *extensions.Manager

	// sqliteStore is the session index; nil when SQLite is disabled or unavailable.
	sqliteStore *session.SQLiteStore

	// completer handles input/complete requests.
	completer *autocomplete.Provider

	// pendingToolLocs maps toolID → file locations extracted at exec time so
	// sendToolDoneUpdate can include them in the tool_call_update notification,
	// which is what triggers Zed's "Follow Pi" navigation.
	pendingToolLocs sync.Map
}

// New builds a Server. ctx is the server's lifetime context: it is passed
// to initialisation calls (extension loading, model prefetch) so they are
// bounded by the server's lifetime rather than context.Background().
func New(ctx context.Context, cfg *config.Config) (*Server, error) {
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
	// Initialize extension manager. Failure is non-fatal.
	extMgr, err := extensions.New(ctx, cfg.ExtensionsDir)
	if err != nil {
		slog.Warn("acp: extensions init failed", "err", err)
		extMgr, err = extensions.New(ctx, "") // empty = no extensions
		if err != nil {
			slog.Warn("acp: extensions fallback also failed", "err", err)
		}
	}
	s.extMgr = extMgr
	s.completer = autocomplete.New(extMgr, nil)
	for _, t := range extensions.WrapAsTools(extMgr) {
		tools.Register(t)
	}

	// Open SQLite session index if enabled. Failure is non-fatal.
	if cfg.SQLite {
		if err := os.MkdirAll(cfg.SessionDir, 0o700); err != nil {
			slog.Warn("acp: cannot create session dir for sqlite", "err", err)
		} else {
			if store, err := session.NewSQLiteStore(filepath.Join(cfg.SessionDir, "index.db")); err == nil {
				s.sqliteStore = store
			} else {
				slog.Warn("acp: sqlite store unavailable", "err", err)
			}
		}
	}
	go s.prefetchModels(ctx)
	return s, nil
}

func (s *Server) prefetchModels(ctx context.Context) {
	defer close(s.modelsReady)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	infos, err := openrouter.FetchModels(ctx, s.cfg.OpenRouterAPIKey)
	if err != nil {
		slog.Warn("model prefetch failed", "err", err)
		return
	}
	acpModels := toACPModels(infos)
	idx := make(map[string]model.ModelInfo, len(infos))
	acpIdx := make(map[string]acpModel, len(infos))
	for i, m := range infos {
		idx[m.ID] = m
		acpIdx[m.ID] = acpModels[i]
	}

	s.modelsMu.Lock()
	s.models = acpModels
	s.modelIndex = idx
	s.acpIndex = acpIdx
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

// Close releases resources held by the server (extension runtimes, SQLite).
func (s *Server) Close() {
	if s.extMgr != nil {
		s.extMgr.Close()
	}
	if s.sqliteStore != nil {
		_ = s.sqliteStore.Close()
	}
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
		case "session/load":
			go s.handleSessionLoad(ctx, &req)
		case "session/new":
			go s.handleSessionNew(&req)
		default:
			s.dispatch(&req)
		}
	}
}

// dispatch handles synchronous, fast RPC methods that complete entirely in
// memory and need no cancellation or deadline. Async methods (session/prompt,
// session/load, session/new) are launched as goroutines directly in Serve.
func (s *Server) dispatch(req *request) {
	switch req.Method {
	case "initialize":
		s.handleInitialize(req)
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
	case "input/complete":
		s.handleInputComplete(req)
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
	if req.Params != nil {
		var p acpInitParams
		if err := json.Unmarshal(req.Params, &p); err == nil && p.ProtocolVersion != 0 {
			if p.ProtocolVersion != protocolVersion {
				s.sendError(rawID(req.ID), -32600,
					fmt.Sprintf("unsupported protocol version %d (server supports %d)", p.ProtocolVersion, protocolVersion))
				return
			}
		}
	}
	s.sendResult(rawID(req.ID), acpInitResult{
		ProtocolVersion: protocolVersion,
		AgentInfo:       acpImpl{Name: "pi", Version: "1.0.0"},
		AgentCapabilities: &acpCapabilities{
			LoadSession:   true,
			ModelSelector: true,
			PromptCapabilities: &acpPromptCapabilities{
				Image:           true,
				EmbeddedContext: true,
			},
		},
		AuthMethods: []any{},
	})
}

func (s *Server) handleSessionNew(req *request) {
	// Parse optional params — Zed sends {cwd: "/path/to/project"}.
	var p acpSessionNewParams
	if req.Params != nil {
		_ = json.Unmarshal(req.Params, &p)
	}

	// Wait up to 5 s for the background model fetch.
	t := time.NewTimer(5 * time.Second)
	select {
	case <-s.modelsReady:
		t.Stop()
	case <-t.C:
		slog.Warn("model fetch timed out during session/new")
	}

	// Strip provider prefix that may have been stored in old configs.
	modelID := strings.TrimPrefix(s.cfg.Model, "openrouter/")

	thinkLevel := model.ThinkingLevel(s.cfg.ThinkingLevel)
	if thinkLevel == "" {
		thinkLevel = model.ThinkingLevelOff
	}

	// Create a persistent JSONL session for this ACP session.
	var sess *session.Session
	if err := os.MkdirAll(s.cfg.SessionDir, 0o700); err != nil {
		slog.Warn("acp: cannot create session dir", "err", err)
	} else {
		if newSess, err := session.New(s.cfg.SessionDir); err == nil {
			sess = newSess
		} else {
			slog.Warn("acp: create session file", "err", err)
		}
	}

	// Use the file's UUID as the ACP session ID so session/load can round-trip.
	id := newSessionID()
	if sess != nil {
		id = sess.ID
	}

	newState := &sessionState{
		sess:         sess,
		modelID:      modelID,
		thinkLevel:   thinkLevel,
		mode:         agent.AgentModeAct,
		cwd:          p.CWD,
		systemPrefix: projectSnapshot(p.CWD),
		monitor:      runtime.NewMonitor(),
	}
	s.sessionsMu.Lock()
	s.sessions[id] = newState
	s.sessionsMu.Unlock()

	// Register in SQLite index so `pi session list` shows it immediately.
	if s.sqliteStore != nil && sess != nil {
		if err := s.sqliteStore.SaveSession(sess); err != nil {
			slog.Warn("acp: sqlite save session failed", "session", id, "err", err)
		} else if err := s.sqliteStore.UpdateSessionMeta(sess.ID, modelID, "openrouter"); err != nil {
			slog.Warn("acp: sqlite update session meta failed", "session", id, "err", err)
		}
	}

	s.modelsMu.RLock()
	models := s.models
	s.modelsMu.RUnlock()

	s.sendResult(rawID(req.ID), acpSessionNewResult{
		SessionID:     id,
		ConfigOptions: s.makeConfigOptions(newState.modelID, newState.thinkLevel, newState.mode, models),
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
		s.sendError(rawID(req.ID), -32001, "session not found: "+p.SessionID)
		return
	}

	// Decode string value.
	var valStr string
	if err := json.Unmarshal(p.Value, &valStr); err != nil {
		s.sendError(rawID(req.ID), -32602, "invalid config value: "+err.Error())
		return
	}

	s.sessionsMu.Lock()
	switch p.ConfigID {
	case "model":
		sess.modelID = valStr
	case "thinking_level":
		sess.thinkLevel = model.ThinkingLevel(valStr)
	case "mode":
		sess.mode = agent.AgentMode(valStr)
	}
	modelID := sess.modelID
	thinkLevel := sess.thinkLevel
	mode := sess.mode
	fileSess := sess.sess
	s.sessionsMu.Unlock()

	// Persist the config change so it survives a server restart.
	if fileSess != nil {
		var entry session.Entry
		switch p.ConfigID {
		case "model":
			entry = session.Entry{Type: session.EntryModelChange, Model: modelID}
		case "thinking_level":
			entry = session.Entry{Type: session.EntryThinkingLevel, Level: string(thinkLevel)}
		case "mode":
			entry = session.Entry{
				Type:     session.EntryMetadata,
				Metadata: map[string]any{"mode": string(mode)},
			}
		}
		if entry.Type != "" {
			if err := fileSess.Append(entry); err != nil {
				slog.Warn("acp: failed to persist config change", "configId", p.ConfigID, "session", p.SessionID, "err", err)
			}
		}
	}

	s.modelsMu.RLock()
	models := s.models
	s.modelsMu.RUnlock()

	s.sendResult(rawID(req.ID), acpSetConfigResult{
		ConfigOptions: s.makeConfigOptions(modelID, thinkLevel, mode, models),
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
		s.sendError(rawID(req.ID), -32001, "session not found: "+p.SessionID)
		return
	}

	s.sessionsMu.Lock()
	sess.modelID = p.ModelID
	modelID := sess.modelID
	thinkLevel := sess.thinkLevel
	mode := sess.mode
	fileSess := sess.sess
	s.sessionsMu.Unlock()

	if fileSess != nil {
		if err := fileSess.Append(session.Entry{Type: session.EntryModelChange, Model: modelID}); err != nil {
			slog.Warn("acp: failed to persist model change", "session", p.SessionID, "model", modelID, "err", err)
		}
	}

	s.modelsMu.RLock()
	models := s.models
	s.modelsMu.RUnlock()

	s.sendResult(rawID(req.ID), acpSetConfigResult{
		ConfigOptions: s.makeConfigOptions(modelID, thinkLevel, mode, models),
	})
}

func (s *Server) handleSessionCancel(req *request) {
	if req.Params == nil {
		if req.ID != nil {
			s.sendResult(rawID(req.ID), map[string]any{})
		}
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		if req.ID != nil {
			s.sendError(rawID(req.ID), -32602, "invalid params: "+err.Error())
		}
		return
	}
	s.cancelsMu.Lock()
	if fn, ok := s.cancels[p.SessionID]; ok {
		fn()
	}
	s.cancelsMu.Unlock()
	if req.ID != nil {
		s.sendResult(rawID(req.ID), map[string]any{})
	}
}

func (s *Server) handleSessionClose(req *request) {
	if req.Params == nil {
		if req.ID != nil {
			s.sendResult(rawID(req.ID), map[string]any{})
		}
		return
	}
	var p acpSessionCloseParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		slog.Warn("acp: invalid session/close params", "err", err)
		if req.ID != nil {
			s.sendError(rawID(req.ID), -32602, "invalid params: "+err.Error())
		}
		return
	}

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
	if p.Prompt.IsEmpty() {
		s.sendError(rawID(req.ID), -32602, "prompt is required")
		return
	}

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
		if err := os.MkdirAll(s.cfg.SessionDir, 0o700); err != nil {
			slog.Warn("acp: auto-create session dir failed", "err", err)
		} else if ns, err := session.New(s.cfg.SessionDir); err != nil {
			slog.Warn("acp: auto-create session failed", "err", err)
		} else {
			newSess = ns
		}
		ss = &sessionState{
			sess:       newSess,
			modelID:    strings.TrimPrefix(s.cfg.Model, "openrouter/"),
			thinkLevel: model.ThinkingLevel(s.cfg.ThinkingLevel),
			mode:       agent.AgentModeAct,
		}
		s.sessionsMu.Lock()
		s.sessions[p.SessionID] = ss
		s.sessionsMu.Unlock()
	}

	s.sessionsMu.RLock()
	modelID := ss.modelID
	thinkLevel := ss.thinkLevel
	agentMode := ss.mode
	cwd := ss.cwd
	systemPrefix := ss.systemPrefix
	fileSess := ss.sess
	mon := ss.monitor
	history := make([]model.Message, len(ss.msgs))
	copy(history, ss.msgs)
	s.sessionsMu.RUnlock()

	// Expand @file tokens in text blocks relative to the project working directory.
	promptBlocks, expandedFiles := expandAtFilesInBlocks(p.Prompt.Blocks, cwd)
	if len(expandedFiles) > 0 {
		slog.Debug("session/prompt: expanded @files", "session", p.SessionID, "files", expandedFiles)
	}

	// Inject cwd into context so tools (bash, etc.) run in the right directory.
	if cwd != "" {
		cctx = tools.WithCWD(cctx, cwd)
	}

	historyLen := len(history)

	system := s.cfg.SystemPrompt
	if systemPrefix != "" {
		if system != "" {
			system = systemPrefix + "\n" + system
		} else {
			system = systemPrefix
		}
	}
	maxTokens := s.cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = agent.DefaultMaxTokens
	}

	ag := agent.New(s.provider, modelID, system, maxTokens)
	ag.Hooks = s.extMgr
	ag.Profile = s.classifyModel(modelID)
	ag.Compactor = s.makeCompactor(modelID, ag.Profile)
	// Wire async compaction so mid-session summarisation never blocks the
	// current turn. The background goroutine's lifetime is bounded by cctx
	// (the session-prompt context) and the 3-minute timeout inside Trigger.
	// Note: the result does NOT carry across separate session/prompt calls
	// (each creates a fresh Agent); within a single multi-turn run it works.
	ag.BGCompactor = agent.NewBackgroundCompactor(cctx, ag.Compactor)
	ag.ConfigTemp = s.cfg.Temperature
	slog.Debug("profile resolved",
		"model", modelID,
		"tier", ag.Profile.Tier,
		"max_output", ag.Profile.MaxOutputTokens,
		"max_turns", ag.Profile.RecommendedMaxTurns,
		"parallel_tools", ag.Profile.ParallelToolBudget,
	)

	// Construct the capability context at the RPC boundary: lifecycle (cctx),
	// no token budget (0/0), and the session's persistent monitor so runtime
	// intelligence (circuit breakers, safety, OPE) accumulates across prompts.
	cx := agent.NewAgentCx(cctx, 0, 0, mon)

	var finalStop model.StopReason = model.StopReasonEndTurn
	var finalUsage model.Usage

	slog.Debug("session/prompt", "session", p.SessionID, "model", modelID, "thinking", thinkLevel, "mode", agentMode)

	updatedMsgs, err := ag.Run(cx, promptBlocks, history, agent.Options{
		ThinkingLevel: thinkLevel,
		Mode:          agentMode,
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
			slog.Debug("tool queued", "name", ev.ToolName)
		case agent.EventKindToolExec:
			s.sendToolExecUpdate(p.SessionID, ev.ToolID, ev.ToolName, ev.ToolInput)
		case agent.EventKindToolDone:
			s.sendToolDoneUpdate(p.SessionID, ev.ToolResult)
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
		s.sendError(rawID(req.ID), -32001, "agent error: "+err.Error())
		return
	}

	// Persist new messages to the JSONL session file.
	if fileSess != nil {
		for _, msg := range updatedMsgs[historyLen:] {
			if err := fileSess.AppendMessage(msg, nil); err != nil {
				slog.Warn("acp: failed to persist message", "session", p.SessionID, "err", err)
			}
		}
		// Keep SQLite index in sync.
		if s.sqliteStore != nil {
			if err := s.sqliteStore.SaveSession(fileSess); err != nil {
				slog.Warn("acp: sqlite save failed", "session", p.SessionID, "err", err)
			} else if err := s.sqliteStore.UpdateSessionMeta(fileSess.ID, modelID, "openrouter"); err != nil {
				slog.Warn("acp: sqlite meta update failed", "session", p.SessionID, "err", err)
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

// handleSessionLoad loads a prior session from disk, registers it, replays
// assistant history as session/update notifications, then returns the result.
func (s *Server) handleSessionLoad(ctx context.Context, req *request) {
	if req.Params == nil {
		s.sendError(rawID(req.ID), -32602, "params required")
		return
	}
	var p acpSessionLoadParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.sendError(rawID(req.ID), -32602, "invalid params: "+err.Error())
		return
	}
	if p.SessionID == "" {
		s.sendError(rawID(req.ID), -32602, "sessionId is required")
		return
	}

	// If already resident in memory, return immediately.
	if existing := s.getSession(p.SessionID); existing != nil {
		s.modelsMu.RLock()
		models := s.models
		s.modelsMu.RUnlock()
		s.sendResult(rawID(req.ID), acpSessionLoadResult{
			SessionID:     p.SessionID,
			ConfigOptions: s.makeConfigOptions(existing.modelID, existing.thinkLevel, existing.mode, models),
		})
		return
	}

	path := filepath.Join(s.cfg.SessionDir, p.SessionID+".jsonl")
	sess, err := session.Open(path)
	if err != nil {
		// File missing (e.g. Pi restarted); create a fresh session with the
		// same ID so Zed can continue without a hard error.
		slog.Warn("acp: session file not found, creating fresh", "session", p.SessionID, "err", err)
		sess, err = session.NewWithID(s.cfg.SessionDir, p.SessionID)
		if err != nil {
			s.sendError(rawID(req.ID), -32001, "session not found: "+p.SessionID)
			return
		}
	}

	// Start from config defaults, then let session entries override.
	modelID := strings.TrimPrefix(s.cfg.Model, "openrouter/")
	thinkLevel := model.ThinkingLevel(s.cfg.ThinkingLevel)
	if thinkLevel == "" {
		thinkLevel = model.ThinkingLevelOff
	}
	mode := agent.AgentModeAct

	// Restore model/thinking/mode from persisted session entries (last value wins).
	for _, e := range sess.Snapshot() {
		switch e.Type {
		case session.EntryModelChange:
			if e.Model != "" {
				modelID = strings.TrimPrefix(e.Model, "openrouter/")
			}
		case session.EntryThinkingLevel:
			if e.Level != "" {
				thinkLevel = model.ThinkingLevel(e.Level)
			}
		case session.EntryMetadata:
			if m, ok := e.Metadata["mode"].(string); ok && m != "" {
				mode = agent.AgentMode(m)
			}
		}
	}

	msgs := sess.Messages()
	s.sessionsMu.Lock()
	s.sessions[p.SessionID] = &sessionState{
		sess:         sess,
		msgs:         msgs,
		modelID:      modelID,
		thinkLevel:   thinkLevel,
		mode:         mode,
		cwd:          p.CWD,
		systemPrefix: projectSnapshot(p.CWD),
		monitor:      runtime.NewMonitor(),
	}
	s.sessionsMu.Unlock()

	// Replay conversation history so Zed reconstructs the full thread including
	// tool calls and results, not just text/thinking blocks.
	for _, msg := range msgs {
		if ctx.Err() != nil {
			return
		}
		switch msg.Role {
		case model.RoleAssistant:
			for _, block := range msg.Content {
				switch block.Type {
				case model.ContentTypeText:
					if block.Text != "" {
						s.sendSessionUpdate(p.SessionID, "agent_message_chunk", block.Text)
					}
				case model.ContentTypeThinking:
					if block.Thinking != "" {
						s.sendSessionUpdate(p.SessionID, "agent_thought_chunk", block.Thinking)
					}
				case model.ContentTypeToolUse:
					s.sendToolExecUpdate(p.SessionID, block.ID, block.Name, block.Input)
				}
			}
		case model.RoleUser:
			for _, block := range msg.Content {
				if block.Type == model.ContentTypeToolResult {
					s.sendToolDoneUpdate(p.SessionID, block)
				}
			}
		}
	}

	s.modelsMu.RLock()
	models := s.models
	s.modelsMu.RUnlock()

	s.sendResult(rawID(req.ID), acpSessionLoadResult{
		SessionID:     p.SessionID,
		ConfigOptions: s.makeConfigOptions(modelID, thinkLevel, mode, models),
	})
}

// toolCallKind maps a Pi tool name to the ACP ToolKind value.
// Values match the ACP spec snake_case enum: read, edit, execute, search, other.
func toolCallKind(toolName string) string {
	switch toolName {
	case "read", "ls":
		return "read"
	case "write", "edit", "hashline_edit":
		return "edit"
	case "bash":
		return "execute"
	case "grep", "find":
		return "search"
	default:
		return "other"
	}
}

// sendToolExecUpdate sends a "tool_call" notification when a tool begins executing.
// Per the ACP spec, "tool_call" creates the entry in Zed; only "tool_call_update"
// patches an existing entry. Using "tool_call" here prevents "Tool call not found"
// errors that arise when Zed receives a "tool_call_update" for an unknown ID.
// rawInput is passed as a JSON value (not a string) per the ACP spec.
func (s *Server) sendToolExecUpdate(sessionID, toolID, name string, input json.RawMessage) {
	upd := acpToolCallUpdate{
		SessionUpdate: "tool_call",
		ToolCallID:    toolID,
		Kind:          toolCallKind(name),
		Status:        "in_progress",
		Title:         name,
		RawInput:      input, // JSON value, not string(input)
	}
	if path, line := extractFileLocation(name, input); path != "" {
		locs := []acpToolCallLoc{{Path: path, Line: line}}
		upd.Locations = locs
		s.pendingToolLocs.Store(toolID, locs)
	}
	s.sendNotification("session/update", acpToolCallUpdateParams{
		SessionID: sessionID,
		Update:    upd,
	})
}

// sendToolDoneUpdate sends a "tool_call_update" notification when a tool finishes.
// rawOutput is encoded as a JSON string value per the ACP spec (Option<Value>).
func (s *Server) sendToolDoneUpdate(sessionID string, result model.ContentBlock) {
	var sb strings.Builder
	for _, c := range result.Content {
		if c.Type == model.ContentTypeText {
			sb.WriteString(c.Text)
		}
	}
	status := "completed"
	if result.IsError {
		status = "failed"
	}
	var rawOutput json.RawMessage
	if sb.Len() > 0 {
		if b, err := json.Marshal(sb.String()); err == nil {
			rawOutput = b
		}
	}
	upd := acpToolCallUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallID:    result.ToolUseID,
		Status:        status,
		RawOutput:     rawOutput,
	}
	if v, ok := s.pendingToolLocs.LoadAndDelete(result.ToolUseID); ok {
		upd.Locations = v.([]acpToolCallLoc)
	}
	s.sendNotification("session/update", acpToolCallUpdateParams{
		SessionID: sessionID,
		Update:    upd,
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

func (s *Server) makeConfigOptions(modelID string, thinkLevel model.ThinkingLevel, mode agent.AgentMode, models []acpModel) []acpConfigOpt {
	thinkStr := string(normalizeThinkingLevel(thinkLevel))
	if mode == "" {
		mode = agent.AgentModeAct
	}

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
		{
			Type:         "select",
			ID:           "mode",
			Name:         "Mode",
			Category:     "agent",
			CurrentValue: string(mode),
			Options: []acpSelectOpt{
				{Value: "act",         Name: "Execute"},
				{Value: "plan",        Name: "Plan"},
				{Value: "plan_act",    Name: "Plan & Act"},
				{Value: "interactive", Name: "Interactive"},
				{Value: "pipe",        Name: "Pipe"},
				{Value: "handoff",     Name: "Handoff"},
			},
		},
	}
}

func (s *Server) getSession(id string) *sessionState {
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()
	return s.sessions[id]
}

// classifyModel looks up the full ModelInfo for modelID from the index and
// returns its model.Profile. Falls back to a zero-value Profile when the model
// is not yet in the cache (e.g. before prefetch completes).
func (s *Server) classifyModel(modelID string) model.Profile {
	s.modelsMu.RLock()
	m, ok := s.modelIndex[modelID]
	s.modelsMu.RUnlock()

	if !ok {
		return model.Profile{} // zero = existing one-size-fits-all behavior
	}
	return model.ClassifyModel(m, s.cfg.ModelProfileOverrides)
}

// makeCompactor returns a Compactor configured for the given model and profile.
// The profile's CompactionReserveRatio is used to compute ReserveTokens when
// the model's ContextWindow is known.
func (s *Server) makeCompactor(modelID string, profile model.Profile) *agent.Compactor {
	s.modelsMu.RLock()
	m, ok := s.acpIndex[modelID]
	s.modelsMu.RUnlock()

	var ctxWindow, reserveTokens int
	if ok {
		ctxWindow = m.ContextWindow
		if ctxWindow > 0 && profile.CompactionReserveRatio > 0 {
			reserveTokens = int(float64(ctxWindow) * profile.CompactionReserveRatio)
		}
	}

	return &agent.Compactor{
		Provider:      s.provider,
		Model:         modelID,
		ContextWindow: ctxWindow,
		ReserveTokens: reserveTokens,
	}
}

func newSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		slog.Warn("acp: rand.Read failed, using time-based session ID", "err", err)
		now := time.Now().UnixNano()
		return fmt.Sprintf("%016x%016x", now, ^now)
	}
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
	defer s.outMu.Unlock()
	if _, err := s.out.Write(b); err != nil {
		slog.Error("acp: stdout write failed", "err", err)
		return
	}
	if err := s.out.WriteByte('\n'); err != nil {
		slog.Error("acp: stdout write newline failed", "err", err)
		return
	}
	if err := s.out.Flush(); err != nil {
		slog.Error("acp: stdout flush failed", "err", err)
	}
}

// rawID unmarshals a json.RawMessage ID to a concrete any (number or string).
func rawID(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		slog.Warn("acp: cannot decode request ID", "raw", string(raw), "err", err)
		return nil
	}
	return v
}


// ── input/complete ────────────────────────────────────────────────────────────

type acpCompleteParams struct {
	SessionID string `json:"sessionId"`
	Text      string `json:"text"`
	Cursor    int    `json:"cursor"`
}

type acpCompleteResult struct {
	Suggestions []acpSuggestion `json:"suggestions"`
	Replace     acpRange        `json:"replace"`
}

type acpSuggestion struct {
	Label      string `json:"label"`
	Detail     string `json:"detail,omitempty"`
	Kind       string `json:"kind"`
	InsertText string `json:"insertText"`
}

type acpRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

func (s *Server) handleInputComplete(req *request) {
	if req.Params == nil {
		s.sendError(rawID(req.ID), -32602, "params required")
		return
	}
	var p acpCompleteParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.sendError(rawID(req.ID), -32602, "invalid params: "+err.Error())
		return
	}

	var cwd string
	if ss := s.getSession(p.SessionID); ss != nil {
		s.sessionsMu.RLock()
		cwd = ss.cwd
		s.sessionsMu.RUnlock()
	}

	suggestions, replace, err := s.completer.Complete(p.Text, p.Cursor, cwd)
	if err != nil {
		s.sendError(rawID(req.ID), -32001, "complete: "+err.Error())
		return
	}

	result := acpCompleteResult{
		Suggestions: make([]acpSuggestion, 0, len(suggestions)),
		Replace:     acpRange{Start: replace.Start, End: replace.End},
	}
	for _, sg := range suggestions {
		result.Suggestions = append(result.Suggestions, acpSuggestion{
			Label:      sg.Label,
			Detail:     sg.Detail,
			Kind:       sg.Kind.String(),
			InsertText: sg.InsertText,
		})
	}
	s.sendResult(rawID(req.ID), result)
}

// extractFileLocation parses the tool input JSON and returns the path and
// optional line number for file-accessing tools (read, write, edit,
// hashline_edit). Returns ("", nil) for all other tools.
// For the read tool the offset parameter is used as the line hint so Follow Pi
// scrolls to the section actually being read.
func extractFileLocation(toolName string, input json.RawMessage) (path string, line *int) {
	switch toolName {
	case "read", "write", "edit", "hashline_edit":
	default:
		return "", nil
	}
	var p struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"` // read tool: 1-based start line
	}
	if err := json.Unmarshal(input, &p); err != nil || p.Path == "" {
		return "", nil
	}
	if toolName == "read" && p.Offset > 0 {
		return p.Path, &p.Offset
	}
	return p.Path, nil
}


func toACPModels(infos []model.ModelInfo) []acpModel {
	out := make([]acpModel, 0, len(infos))
	for _, m := range infos {
		out = append(out, acpModel{
			ID:               m.ID,
			DisplayName:      m.DisplayName,
			ContextWindow:    m.ContextWindow,
			MaxTokens:        m.MaxTokens,
			SupportsThinking: m.SupportsThinking,
		})
	}
	return out
}
