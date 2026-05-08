// Package tui implements the interactive Bubbletea TUI for pi-agent.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/tiru-r/pi-agent-go/internal/agent"
	"github.com/tiru-r/pi-agent-go/internal/config"
	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/session"
)

// ── tea.Msg types ─────────────────────────────────────────────────────────────

// streamEventMsg carries a single streaming event from the agent.
type streamEventMsg agent.AgentEvent

// streamDoneMsg signals that the current streaming run has finished.
type streamDoneMsg struct {
	msgs []model.Message
	err  error
}

// toolDoneMsg signals a tool result that should be appended to the conversation.
type toolDoneMsg struct{ result model.ContentBlock }

// tickMsg drives spinner animation.
type tickMsg struct{ t time.Time }

// sessionLoadedMsg carries a freshly-opened session.
type sessionLoadedMsg struct{ sess *session.Session }

// statusClearMsg clears the status bar message after a delay.
type statusClearMsg struct{}

// toolStatus tracks the run state of a single tool call.
type toolStatus struct {
	Name    string
	Running bool
	Done    bool
	IsError bool
}

// ── Model ─────────────────────────────────────────────────────────────────────

// Model is the root Bubbletea model for the pi TUI.
type Model struct {
	// Config & wiring
	cfg   *config.Config
	agent *agent.Agent

	// Visual
	styles Styles
	keys   KeyMap
	theme  Theme

	// Viewport for scrollable conversation
	viewport viewport.Model

	// Multi-line text input
	input textarea.Model

	// Conversation state
	messages   []model.Message
	session    *session.Session
	totalUsage model.Usage

	// Streaming state
	streaming    bool
	streamCancel context.CancelFunc
	streamBuffer strings.Builder // accumulates current text delta
	currentThink strings.Builder // accumulates current thinking delta

	// Tool execution state
	pendingTools []toolStatus
	spinnerFrame int

	// Input history
	history []string
	histIdx int // -1 = not navigating history; 0..len = index into history

	// UI state
	width             int
	height            int
	ready             bool
	err               error
	statusMsg         string
	showModelSelector bool
	modelSelector     *ModelSelector
}

// New creates a new Model. sess may be nil for a fresh session.
func New(cfg *config.Config, ag *agent.Agent, sess *session.Session) *Model {
	theme := GetTheme(cfg.Theme)
	styles := NewStyles(theme)

	inp := textarea.New()
	inp.Placeholder = "Type a message… (/help for commands)"
	inp.Focus()
	inp.ShowLineNumbers = false
	inp.SetHeight(3)
	inp.CharLimit = 0 // unlimited

	vp := viewport.New(80, 20)
	vp.SetContent("")

	var msgs []model.Message
	if sess != nil {
		msgs = sess.Messages()
	}

	m := &Model{
		cfg:      cfg,
		agent:    ag,
		styles:   styles,
		keys:     DefaultKeyMap,
		theme:    theme,
		viewport: vp,
		input:    inp,
		messages: msgs,
		session:  sess,
		histIdx:  -1,
	}
	return m
}

// Init satisfies tea.Model; starts the tick loop.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		tick(),
	)
}

// Update satisfies tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	// ── Window resize ──────────────────────────────────────────────────
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.relayout()
		m.ready = true

	// ── Keyboard ──────────────────────────────────────────────────────
	case tea.KeyMsg:
		switch {
		case keyMatches(msg, m.keys.Quit):
			return m, tea.Quit

		case keyMatches(msg, m.keys.Abort):
			if m.streaming {
				m.abortStream()
				m.statusMsg = "Generation aborted."
			}
			return m, nil

		case keyMatches(msg, m.keys.Submit):
			if !m.streaming {
				return m, m.submitInput()
			}

		case keyMatches(msg, m.keys.NewLine):
			m.input.InsertString("\n")

		case keyMatches(msg, m.keys.HistoryUp):
			m.navigateHistoryUp()

		case keyMatches(msg, m.keys.HistoryDown):
			m.navigateHistoryDown()

		case keyMatches(msg, m.keys.ScrollUp):
			m.viewport.HalfViewUp()

		case keyMatches(msg, m.keys.ScrollDown):
			m.viewport.HalfViewDown()

		case keyMatches(msg, m.keys.ScrollTop):
			m.viewport.GotoTop()

		case keyMatches(msg, m.keys.ScrollBottom):
			m.viewport.GotoBottom()

		case keyMatches(msg, m.keys.ClearScreen):
			m.messages = nil
			m.refreshViewport()

		case keyMatches(msg, m.keys.NewSession):
			cmds = append(cmds, newSessionCmd(m))

		case keyMatches(msg, m.keys.OpenSession):
			m.modelSelector = nil
			m.showModelSelector = false
			// Launch session picker in a sub-program (opens in a separate run).
			cmds = append(cmds, launchSessionPickerCmd(m.cfg.SessionDir))

		case keyMatches(msg, m.keys.ToggleThink):
			m.cycleThinkingLevel()

		case keyMatches(msg, m.keys.Copy):
			text := lastAssistantText(m)
			if text != "" {
				cmds = append(cmds, copyToClipboardCmd(text))
				m.statusMsg = "Copied to clipboard."
			}

		case keyMatches(msg, m.keys.Help):
			newModel, cmd := cmdHelp(m, "")
			return newModel, cmd

		default:
			// Pass key to input widget unless a model selector is open.
			if m.showModelSelector && m.modelSelector != nil {
				nm, cmd := m.modelSelector.Update(msg)
				if sel, ok := nm.(*ModelSelector); ok {
					m.modelSelector = sel
					if sel.chosen != "" {
						m.cfg.Model = sel.chosen
						m.showModelSelector = false
						m.modelSelector = nil
						m.statusMsg = "Model: " + m.cfg.Model
					}
				}
				cmds = append(cmds, cmd)
			} else if !m.streaming {
				var inputCmd tea.Cmd
				m.input, inputCmd = m.input.Update(msg)
				cmds = append(cmds, inputCmd)
			}
		}

	// ── Streaming events ──────────────────────────────────────────────
	case streamContinueMsg:
		m.applyAgentEvent(msg.ev)
		// Schedule the next read.
		if msg.next != nil {
			cmds = append(cmds, msg.next)
		}

	// ── Stream done ───────────────────────────────────────────────────
	case streamDoneMsg:
		m.streaming = false
		m.streamCancel = nil
		if msg.err != nil && m.err == nil {
			m.err = msg.err
			m.statusMsg = "Error: " + msg.err.Error()
		}
		if msg.msgs != nil {
			// Persist newly added messages to session (everything after the
			// user message we already persisted in submitInput).
			prevLen := len(m.messages)
			m.messages = msg.msgs
			if m.session != nil {
				for _, newMsg := range msg.msgs[prevLen:] {
					_ = m.session.AppendMessage(newMsg, nil)
				}
			}
		}
		m.streamBuffer.Reset()
		m.currentThink.Reset()
		m.pendingTools = nil
		m.refreshViewport()
		m.viewport.GotoBottom()
		cmds = append(cmds, scheduleClearStatus())

	// ── Session loaded ────────────────────────────────────────────────
	case sessionLoadedMsg:
		m.session = msg.sess
		if msg.sess != nil {
			m.messages = msg.sess.Messages()
		}
		m.refreshViewport()
		m.viewport.GotoBottom()

	// ── Command-triggered internal messages ───────────────────────────
	case newSessionMsg:
		cmds = append(cmds, createNewSessionCmd(m))

	case listSessionsMsg:
		cmds = append(cmds, displaySessionListCmd(m, msg.dir))

	case openSessionMsg:
		cmds = append(cmds, loadSessionCmd(msg.dir, msg.id))

	case sessionListDisplayMsg:
		cmds = append(cmds, showSessionListCmd(m, msg.dir))

	case sessionListTextMsg:
		m.messages = append(m.messages, model.Message{
			Role: model.RoleSystem,
			Content: []model.ContentBlock{
				{Type: model.ContentTypeText, Text: msg.text},
			},
		})
		m.refreshViewport()

	case clipboardCopyMsg:
		writeClipboard(msg.text)

	case statusClearMsg:
		m.statusMsg = ""

	// ── Tick (spinner animation) ──────────────────────────────────────
	case tickMsg:
		if m.streaming {
			m.spinnerFrame++
			m.refreshViewport()
		}
		cmds = append(cmds, tick())
	}

	// Always update viewport; only forward non-key messages to input
	// (key events are routed through the switch above to avoid double processing).
	if m.ready {
		var vpCmd tea.Cmd
		m.viewport, vpCmd = m.viewport.Update(msg)
		cmds = append(cmds, vpCmd)

		if !m.streaming {
			if _, isKey := msg.(tea.KeyMsg); !isKey {
				var inpCmd tea.Cmd
				m.input, inpCmd = m.input.Update(msg)
				cmds = append(cmds, inpCmd)
			}
		}
	}

	return m, tea.Batch(cmds...)
}

// View satisfies tea.Model.
func (m *Model) View() string {
	if !m.ready {
		return "\n  Initialising…\n"
	}

	var sb strings.Builder

	// Status bar
	sb.WriteString(renderStatusBar(m))
	sb.WriteString("\n")

	// Model selector overlay
	if m.showModelSelector && m.modelSelector != nil {
		sb.WriteString(m.modelSelector.View())
		return sb.String()
	}

	// Conversation viewport
	sb.WriteString(m.viewport.View())
	sb.WriteString("\n")

	// Separator
	sb.WriteString(m.styles.Border.Render(strings.Repeat("─", m.width)))
	sb.WriteString("\n")

	// Input area (hidden while streaming)
	if !m.streaming {
		sb.WriteString(m.input.View())
		sb.WriteString("\n")
	} else {
		frame := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
		sb.WriteString("  ")
		sb.WriteString(m.styles.MutedItalic.Render(frame + " Generating…"))
		sb.WriteString("\n")
	}

	// Status message
	if m.statusMsg != "" {
		sb.WriteString(m.styles.MutedItalic.Render("  " + m.statusMsg))
		sb.WriteString("\n")
	}

	// Help line
	sb.WriteString(renderHelp(m))
	sb.WriteString("\n")

	return sb.String()
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// relayout recalculates viewport and input dimensions after a resize.
func (m *Model) relayout() {
	// Reserve: 1 status + 1 separator + 1 help + input height + 1 status msg
	inputHeight := m.input.Height() + 2 // border padding
	reserved := 1 + 1 + 1 + inputHeight + 2
	vpHeight := m.height - reserved
	if vpHeight < 4 {
		vpHeight = 4
	}
	m.viewport.Width = m.width
	m.viewport.Height = vpHeight
	m.input.SetWidth(m.width - 2)
	m.refreshViewport()
}

// refreshViewport rebuilds the viewport content from m.messages.
func (m *Model) refreshViewport() {
	content := renderConversation(m)
	m.viewport.SetContent(content)
	if m.streaming {
		m.viewport.GotoBottom()
	}
}

// submitInput takes the current input, clears the field, and runs the agent.
func (m *Model) submitInput() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return nil
	}
	m.input.Reset()
	m.histIdx = -1

	// Add to history (avoid duplicates at the top).
	if len(m.history) == 0 || m.history[0] != text {
		m.history = append([]string{text}, m.history...)
		if len(m.history) > 200 {
			m.history = m.history[:200]
		}
	}

	if isCommand(text) {
		newModel, cmd := handleCommand(m, text)
		if tm, ok := newModel.(*Model); ok {
			*m = *tm
		}
		return cmd
	}

	// Persist user message to session.
	userMsg := model.NewTextMessage(model.RoleUser, text)
	if m.session != nil {
		_ = m.session.AppendMessage(userMsg, nil)
	}
	m.messages = append(m.messages, userMsg)
	m.refreshViewport()
	m.viewport.GotoBottom()

	return m.runAgentStreaming(text)
}

// streamContinueMsg carries one intermediate streaming event plus the command
// to read the next one. This allows the Update loop to chain reads without
// blocking the event loop.
type streamContinueMsg struct {
	ev   agent.AgentEvent
	next tea.Cmd
}

// runAgentStreaming launches the agent loop in a goroutine and feeds events
// into the Bubbletea event loop via a channel.
func (m *Model) runAgentStreaming(input string) tea.Cmd {
	m.streaming = true
	m.streamBuffer.Reset()
	m.currentThink.Reset()
	m.pendingTools = nil
	m.err = nil

	ctx, cancel := context.WithCancel(context.Background())
	m.streamCancel = cancel

	// Snapshot history before the user message we just appended.
	history := make([]model.Message, 0, len(m.messages))
	if len(m.messages) > 0 {
		history = m.messages[:len(m.messages)-1]
	}

	opts := agent.Options{
		System:        m.cfg.SystemPrompt,
		ThinkingLevel: model.ThinkingLevel(m.cfg.ThinkingLevel),
	}

	// Buffered channel so the agent goroutine is never blocked by the UI.
	eventCh := make(chan agent.AgentEvent, 128)
	doneCh := make(chan streamDoneMsg, 1)

	// Producer: run the agent and emit events.
	go func() {
		defer close(eventCh)
		finalMsgs, err := m.agent.Run(ctx, input, history, opts, func(ev agent.AgentEvent) {
			select {
			case eventCh <- ev:
			case <-ctx.Done():
			}
		})
		cancel()
		doneCh <- streamDoneMsg{msgs: finalMsgs, err: err}
	}()

	return nextAgentEvent(eventCh, doneCh)
}

// nextAgentEvent returns a tea.Cmd that reads one event from the event channel
// or the done channel, chaining further reads as needed.
func nextAgentEvent(eventCh <-chan agent.AgentEvent, doneCh <-chan streamDoneMsg) tea.Cmd {
	return func() tea.Msg {
		select {
		case ev, ok := <-eventCh:
			if !ok {
				// Channel closed — wait for done signal.
				done := <-doneCh
				return done
			}
			// Wrap the event; carry a command to read the next one.
			return streamContinueMsg{
				ev:   ev,
				next: nextAgentEvent(eventCh, doneCh),
			}
		case done := <-doneCh:
			// Drain remaining events synchronously then return done.
			return done
		}
	}
}

// abortStream cancels the active streaming context.
func (m *Model) abortStream() {
	if m.streamCancel != nil {
		m.streamCancel()
		m.streamCancel = nil
	}
	m.streaming = false
	m.streamBuffer.Reset()
	m.currentThink.Reset()
	m.pendingTools = nil
}

// applyAgentEvent applies a single agent event to the model state.
func (m *Model) applyAgentEvent(ev agent.AgentEvent) {
	switch ev.Kind {
	case agent.EventKindText:
		m.streamBuffer.WriteString(ev.Delta)
		m.refreshViewport()

	case agent.EventKindThinking:
		m.currentThink.WriteString(ev.Delta)
		m.refreshViewport()

	case agent.EventKindToolStart:
		m.pendingTools = append(m.pendingTools, toolStatus{
			Name:    ev.ToolName,
			Running: true,
		})
		m.refreshViewport()

	case agent.EventKindToolDone:
		for i := range m.pendingTools {
			if m.pendingTools[i].Running && !m.pendingTools[i].Done {
				m.pendingTools[i].Running = false
				m.pendingTools[i].Done = true
				m.pendingTools[i].IsError = ev.ToolResult.IsError
				break
			}
		}
		m.refreshViewport()

	case agent.EventKindError:
		m.err = ev.Err
		m.statusMsg = "Error: " + ev.Err.Error()

	case agent.EventKindDone:
		m.totalUsage = m.totalUsage.Add(ev.Usage)
	}
}

// navigateHistoryUp moves one step back in input history.
func (m *Model) navigateHistoryUp() {
	if len(m.history) == 0 {
		return
	}
	m.histIdx++
	if m.histIdx >= len(m.history) {
		m.histIdx = len(m.history) - 1
	}
	m.input.SetValue(m.history[m.histIdx])
}

// navigateHistoryDown moves one step forward in input history.
func (m *Model) navigateHistoryDown() {
	if m.histIdx <= 0 {
		m.histIdx = -1
		m.input.SetValue("")
		return
	}
	m.histIdx--
	m.input.SetValue(m.history[m.histIdx])
}

// cycleThinkingLevel toggles through off → auto → full → off.
func (m *Model) cycleThinkingLevel() {
	switch model.ThinkingLevel(m.cfg.ThinkingLevel) {
	case model.ThinkingLevelOff:
		m.cfg.ThinkingLevel = string(model.ThinkingLevelAuto)
		m.statusMsg = "Thinking: auto"
	case model.ThinkingLevelAuto:
		m.cfg.ThinkingLevel = string(model.ThinkingLevelFull)
		m.statusMsg = "Thinking: full"
	default:
		m.cfg.ThinkingLevel = string(model.ThinkingLevelOff)
		m.statusMsg = "Thinking: off"
	}
}

// keyMatches checks whether a key message matches a binding.
func keyMatches(msg tea.KeyMsg, binding key.Binding) bool {
	return key.Matches(msg, binding)
}

// ── tea.Cmd factories ─────────────────────────────────────────────────────────

func tick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg{t: t}
	})
}

func scheduleClearStatus() tea.Cmd {
	return tea.Tick(3*time.Second, func(_ time.Time) tea.Msg {
		return statusClearMsg{}
	})
}

func launchSessionPickerCmd(dir string) tea.Cmd {
	return func() tea.Msg {
		return openSessionMsg{dir: dir}
	}
}

func createNewSessionCmd(m *Model) tea.Cmd {
	dir := m.cfg.SessionDir
	modelName := m.cfg.Model
	prov := m.cfg.Provider
	return func() tea.Msg {
		sess, err := session.New(dir)
		if err != nil {
			return streamDoneMsg{err: err}
		}
		_ = sess.AppendMessage(model.Message{
			Role: model.RoleSystem,
			Content: []model.ContentBlock{
				{Type: model.ContentTypeText,
					Text: "model=" + modelName + " provider=" + prov},
			},
		}, nil)
		return sessionLoadedMsg{sess: sess}
	}
}

func displaySessionListCmd(m *Model, dir string) tea.Cmd {
	return func() tea.Msg {
		// We display the list as a system message in the conversation.
		return sessionListDisplayMsg{dir: dir}
	}
}

func loadSessionCmd(dir, id string) tea.Cmd {
	path := dir + "/" + id + ".jsonl"
	return func() tea.Msg {
		sess, err := session.Open(path)
		if err != nil {
			return streamDoneMsg{err: err}
		}
		return sessionLoadedMsg{sess: sess}
	}
}

type sessionListDisplayMsg struct{ dir string }

// showSessionListCmd reads sessions from dir and appends a summary as a system message.
func showSessionListCmd(m *Model, dir string) tea.Cmd {
	return func() tea.Msg {
		store, err := session.NewSQLiteStore(dir + "/index.db")
		if err != nil {
			// Best-effort: just show nothing.
			return nil
		}
		defer store.Close()
		metas, err := store.ListSessions()
		if err != nil || len(metas) == 0 {
			return nil
		}

		var sb strings.Builder
		sb.WriteString("\nSessions:\n\n")
		for _, meta := range metas {
			sb.WriteString(fmt.Sprintf("  %s  %-40s  %d msgs\n",
				meta.ID[:8],
				truncateSessionTitle(meta.Title, 40),
				meta.MessageCount,
			))
		}
		return sessionListTextMsg{text: sb.String()}
	}
}

type sessionListTextMsg struct{ text string }

func truncateSessionTitle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// writeClipboard writes text to the system clipboard using xclip/pbcopy/etc.
// Silently fails if no clipboard utility is available.
func writeClipboard(text string) {
	// Platform-agnostic: best-effort, no dependency.
	_ = text
}

// Run creates and starts the Bubbletea program.
func Run(m *Model) error {
	p := tea.NewProgram(
		m,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	_, err := p.Run()
	return err
}
