package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/tiru-r/pi-agent-go/internal/model"
)

// Command is a slash-command handler.
type Command struct {
	Name        string
	Aliases     []string
	Description string
	Handler     func(m *Model, args string) (tea.Model, tea.Cmd)
}

// commands is the authoritative list of REPL slash-commands.
// Populated in init() to avoid an initialization cycle (cmdHelp references commands).
var commands []Command

func init() {
	commands = []Command{
		{
			Name:        "help",
			Aliases:     []string{"h"},
			Description: "Show available commands",
			Handler:     cmdHelp,
		},
		{
			Name:        "clear",
			Aliases:     []string{"cls"},
			Description: "Clear conversation history",
			Handler:     cmdClear,
		},
		{
			Name:        "model",
			Aliases:     []string{"m"},
			Description: "Switch model: /model claude-opus-4-7",
			Handler:     cmdModel,
		},
		{
			Name:        "session",
			Aliases:     []string{"s"},
			Description: "Session commands: /session new|list|open <id>",
			Handler:     cmdSession,
		},
		{
			Name:        "think",
			Description: "Toggle extended thinking: /think on|off|auto",
			Handler:     cmdThink,
		},
		{
			Name:        "system",
			Description: "Set system prompt: /system You are a helpful assistant",
			Handler:     cmdSystem,
		},
		{
			Name:        "copy",
			Aliases:     []string{"cp"},
			Description: "Copy last response to clipboard",
			Handler:     cmdCopy,
		},
		{
			Name:        "exit",
			Aliases:     []string{"quit", "q"},
			Description: "Exit pi",
			Handler:     cmdExit,
		},
	}
}

// isCommand reports whether input starts with '/'.
func isCommand(input string) bool {
	return strings.HasPrefix(strings.TrimSpace(input), "/")
}

// handleCommand dispatches the input to the matching command handler.
// Returns the updated model and any command to run.
func handleCommand(m *Model, input string) (tea.Model, tea.Cmd) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return m, nil
	}
	// Strip leading slash.
	body := input[1:]
	parts := strings.SplitN(body, " ", 2)
	name := strings.ToLower(parts[0])
	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}

	for _, cmd := range commands {
		if cmd.Name == name {
			return cmd.Handler(m, args)
		}
		for _, alias := range cmd.Aliases {
			if alias == name {
				return cmd.Handler(m, args)
			}
		}
	}

	m.statusMsg = fmt.Sprintf("Unknown command /%s — type /help for a list.", name)
	return m, nil
}

// ── Command handlers ──────────────────────────────────────────────────────────

func cmdHelp(m *Model, _ string) (tea.Model, tea.Cmd) {
	var sb strings.Builder
	sb.WriteString("\nAvailable commands:\n\n")
	for _, cmd := range commands {
		aliases := ""
		if len(cmd.Aliases) > 0 {
			aliases = " (/" + strings.Join(cmd.Aliases, ", /") + ")"
		}
		sb.WriteString(fmt.Sprintf("  /%-12s%s — %s\n", cmd.Name, aliases, cmd.Description))
	}
	sb.WriteString("\nKeyboard shortcuts:\n\n")
	sb.WriteString("  Enter         Submit message\n")
	sb.WriteString("  Shift+Enter   Insert newline\n")
	sb.WriteString("  Esc           Abort current generation\n")
	sb.WriteString("  Ctrl+C/Q      Quit\n")
	sb.WriteString("  PgUp/PgDn     Scroll conversation\n")
	sb.WriteString("  Ctrl+U/F      Scroll up/down\n")
	sb.WriteString("  Ctrl+Home/End Jump to top/bottom\n")
	sb.WriteString("  Up/Down       Navigate input history\n")
	sb.WriteString("  Ctrl+Y        Copy last response\n")
	sb.WriteString("  Ctrl+N        New session\n")
	sb.WriteString("  Ctrl+O        Open session picker\n")
	sb.WriteString("  Ctrl+T        Toggle thinking\n")
	sb.WriteString("  Ctrl+L        Clear screen\n")

	// Append to conversation as a system-style message.
	m.messages = append(m.messages, model.Message{
		Role: model.RoleSystem,
		Content: []model.ContentBlock{
			{Type: model.ContentTypeText, Text: sb.String()},
		},
	})
	m.refreshViewport()
	return m, nil
}

func cmdClear(m *Model, _ string) (tea.Model, tea.Cmd) {
	m.messages = nil
	m.session = nil
	m.totalUsage = model.Usage{}
	m.streamBuffer.Reset()
	m.currentThink.Reset()
	m.pendingTools = nil
	m.statusMsg = "Conversation cleared."
	m.refreshViewport()
	return m, nil
}

func cmdModel(m *Model, args string) (tea.Model, tea.Cmd) {
	args = strings.TrimSpace(args)
	if args == "" {
		// Launch the model selector overlay.
		m.showModelSelector = true
		m.modelSelector = NewModelSelector(m.styles)
		return m, nil
	}
	m.cfg.Model = args
	m.statusMsg = fmt.Sprintf("Model set to %s.", args)
	return m, nil
}

func cmdSession(m *Model, args string) (tea.Model, tea.Cmd) {
	parts := strings.Fields(args)
	if len(parts) == 0 {
		m.statusMsg = "Usage: /session new|list|open <id>"
		return m, nil
	}
	switch parts[0] {
	case "new":
		return m, newSessionCmd(m)
	case "list":
		return m, listSessionsCmd(m)
	case "open":
		if len(parts) < 2 {
			m.statusMsg = "Usage: /session open <id>"
			return m, nil
		}
		return m, openSessionCmd(m, parts[1])
	default:
		m.statusMsg = fmt.Sprintf("Unknown session subcommand: %s", parts[0])
	}
	return m, nil
}

func cmdThink(m *Model, args string) (tea.Model, tea.Cmd) {
	switch strings.ToLower(strings.TrimSpace(args)) {
	case "on", "full":
		m.cfg.ThinkingLevel = string(model.ThinkingLevelFull)
		m.statusMsg = "Thinking: full"
	case "auto":
		m.cfg.ThinkingLevel = string(model.ThinkingLevelAuto)
		m.statusMsg = "Thinking: auto"
	case "off", "":
		m.cfg.ThinkingLevel = string(model.ThinkingLevelOff)
		m.statusMsg = "Thinking: off"
	default:
		m.statusMsg = "Usage: /think on|off|auto"
	}
	return m, nil
}

func cmdSystem(m *Model, args string) (tea.Model, tea.Cmd) {
	args = strings.TrimSpace(args)
	if args == "" {
		current := m.cfg.SystemPrompt
		if current == "" {
			current = "(none)"
		}
		m.statusMsg = "Current system prompt: " + current
		return m, nil
	}
	m.cfg.SystemPrompt = args
	m.statusMsg = "System prompt updated."
	return m, nil
}

func cmdCopy(m *Model, _ string) (tea.Model, tea.Cmd) {
	last := lastAssistantText(m)
	if last == "" {
		m.statusMsg = "Nothing to copy."
		return m, nil
	}
	return m, copyToClipboardCmd(last)
}

func cmdExit(_ *Model, _ string) (tea.Model, tea.Cmd) {
	return nil, tea.Quit
}

// ── Helper tea.Cmds ───────────────────────────────────────────────────────────

func newSessionCmd(m *Model) tea.Cmd {
	return func() tea.Msg {
		return newSessionMsg{}
	}
}

func listSessionsCmd(m *Model) tea.Cmd {
	dir := m.cfg.SessionDir
	return func() tea.Msg {
		return listSessionsMsg{dir: dir}
	}
}

func openSessionCmd(m *Model, id string) tea.Cmd {
	dir := m.cfg.SessionDir
	return func() tea.Msg {
		return openSessionMsg{dir: dir, id: id}
	}
}

func copyToClipboardCmd(text string) tea.Cmd {
	return func() tea.Msg {
		return clipboardCopyMsg{text: text}
	}
}

// lastAssistantText returns the text of the last assistant message.
func lastAssistantText(m *Model) string {
	for i := len(m.messages) - 1; i >= 0; i-- {
		if m.messages[i].Role == model.RoleAssistant {
			return m.messages[i].Text()
		}
	}
	return ""
}

// ── Internal message types used by command handlers ───────────────────────────

type newSessionMsg struct{}
type listSessionsMsg struct{ dir string }
type openSessionMsg struct {
	dir string
	id  string
}
type clipboardCopyMsg struct{ text string }
