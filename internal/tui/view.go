package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wrap"
	"github.com/pi-agent/pi/internal/model"
)

const (
	toolResultMaxLines = 10
	thinkingMaxRunes   = 120
)

// renderMessage renders a single model.Message into a styled string.
// isLast indicates it is the last message; if so and streaming is active the
// streamBuf is appended to the assistant text inline.
func renderMessage(m *Model, msg model.Message, isLast bool, streamBuf string) string {
	var sb strings.Builder
	switch msg.Role {
	case model.RoleUser:
		sb.WriteString("\n")
		sb.WriteString(m.styles.AccentBold.Render("You:"))
		sb.WriteString("\n")
		for _, block := range msg.Content {
			sb.WriteString(renderContentBlock(m, block, false))
		}
	case model.RoleAssistant:
		sb.WriteString("\n")
		sb.WriteString(m.styles.SuccessBold.Render("Assistant:"))
		sb.WriteString("\n")
		streaming := isLast && m.streaming
		for _, block := range msg.Content {
			sb.WriteString(renderContentBlock(m, block, streaming))
		}
		if streaming && streamBuf != "" {
			text := wrapText(streamBuf, m.width-4)
			sb.WriteString(text)
			sb.WriteString(" ▋\n") // streaming cursor
		}
	case model.RoleTool:
		// Tool result messages are rendered inline with the assistant turn.
		// They can appear as top-level messages when the session stores them.
		for _, block := range msg.Content {
			sb.WriteString(renderContentBlock(m, block, false))
		}
	case model.RoleSystem:
		sb.WriteString("\n")
		sb.WriteString(m.styles.MutedItalic.Render("System: "+msg.Text()))
		sb.WriteString("\n")
	}
	return sb.String()
}

// renderContentBlock renders a single content block.
func renderContentBlock(m *Model, block model.ContentBlock, streaming bool) string {
	switch block.Type {
	case model.ContentTypeText:
		text := block.Text
		if streaming {
			return "" // text rendered separately with streamBuf cursor
		}
		return wrapText(text, m.width-4) + "\n"

	case model.ContentTypeThinking:
		return renderThinking(m, block)

	case model.ContentTypeToolUse:
		// Find the matching status entry if present.
		var status *toolStatus
		for i := range m.pendingTools {
			if m.pendingTools[i].Name == block.Name {
				status = &m.pendingTools[i]
				break
			}
		}
		return renderToolUse(m, block, status)

	case model.ContentTypeToolResult:
		return renderToolResult(m, block)

	default:
		return ""
	}
}

// renderToolUse renders a tool call line with a status indicator.
func renderToolUse(m *Model, block model.ContentBlock, status *toolStatus) string {
	var sb strings.Builder
	sb.WriteString("  ")

	indicator := "○"
	if status != nil {
		switch {
		case status.IsError:
			indicator = m.styles.ErrorBold.Render("✗")
		case status.Done:
			indicator = m.styles.SuccessBold.Render("✓")
		case status.Running:
			indicator = spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
		}
	}

	toolLine := fmt.Sprintf("%s %s(%s)",
		indicator,
		m.styles.ToolUse.Render(block.Name),
		truncateJSON(block.Input, 60),
	)
	sb.WriteString(toolLine)
	sb.WriteString("\n")
	return sb.String()
}

// renderToolResult renders a tool result block, truncating long output.
func renderToolResult(m *Model, block model.ContentBlock) string {
	if len(block.Content) == 0 {
		return ""
	}

	var sb strings.Builder
	for _, inner := range block.Content {
		if inner.Type != model.ContentTypeText {
			continue
		}
		lines := strings.Split(inner.Text, "\n")
		if len(lines) > toolResultMaxLines {
			for _, l := range lines[:toolResultMaxLines] {
				sb.WriteString("  ")
				sb.WriteString(m.styles.ToolResult.Render(l))
				sb.WriteString("\n")
			}
			more := len(lines) - toolResultMaxLines
			sb.WriteString("  ")
			sb.WriteString(m.styles.MutedItalic.Render(
				fmt.Sprintf("... (%d more lines)", more)))
			sb.WriteString("\n")
		} else {
			for _, l := range lines {
				sb.WriteString("  ")
				sb.WriteString(m.styles.ToolResult.Render(l))
				sb.WriteString("\n")
			}
		}
	}
	if block.IsError {
		return m.styles.ErrorBold.Render("  Error: ") + sb.String()
	}
	return sb.String()
}

// renderThinking renders an extended thinking block, dimmed and collapsed.
func renderThinking(m *Model, block model.ContentBlock) string {
	if block.Thinking == "" {
		return ""
	}
	text := block.Thinking
	if utf8.RuneCountInString(text) > thinkingMaxRunes {
		runes := []rune(text)
		text = string(runes[:thinkingMaxRunes]) + "..."
	}
	// Show only first line for brevity.
	text = strings.SplitN(text, "\n", 2)[0]
	line := m.styles.MutedItalic.Render(fmt.Sprintf("  Thinking: %s", text))
	return line + "\n"
}

// renderStatusBar renders the top status bar.
func renderStatusBar(m *Model) string {
	model := m.cfg.Model
	provider := m.cfg.Provider

	sessionID := ""
	if m.session != nil {
		sessionID = m.session.ID[:8]
	}

	left := fmt.Sprintf(" Pi  %s  [%s/%s]", lipgloss.NewStyle().Bold(true).Render("pi"),
		provider, model)
	if sessionID != "" {
		left += fmt.Sprintf("  session:%s", sessionID)
	}

	right := ""
	if m.totalUsage.InputTokens > 0 || m.totalUsage.OutputTokens > 0 {
		right = fmt.Sprintf("in:%d out:%d ", m.totalUsage.InputTokens, m.totalUsage.OutputTokens)
	}

	// Pad to full width.
	bar := left
	rightLen := len(right)
	pad := m.width - len(left) - rightLen
	if pad > 0 {
		bar += strings.Repeat(" ", pad)
	}
	bar += right

	return m.styles.StatusBar.Render(bar)
}

// renderHelp renders the bottom key-binding hint line.
func renderHelp(m *Model) string {
	if m.streaming {
		return m.styles.MutedItalic.Render(
			" Esc: abort  Ctrl+C: quit")
	}
	return m.styles.MutedItalic.Render(
		" Enter: submit  Shift+Enter: newline  Ctrl+O: sessions  Ctrl+T: thinking  ?: help  Ctrl+C: quit")
}

// renderConversation builds the full conversation string for the viewport.
func renderConversation(m *Model) string {
	if len(m.messages) == 0 && !m.streaming {
		welcome := m.styles.MutedItalic.Render(
			"  Welcome to Pi. Type a message and press Enter to start.\n" +
				"  Type /help for available commands.")
		return welcome
	}

	var sb strings.Builder
	for i, msg := range m.messages {
		isLast := i == len(m.messages)-1
		sb.WriteString(renderMessage(m, msg, isLast, m.streamBuffer.String()))
	}

	// If we're streaming but there are no messages yet (first token before
	// the response message is committed), show the stream buffer here.
	if m.streaming && (len(m.messages) == 0 ||
		m.messages[len(m.messages)-1].Role != model.RoleAssistant) {
		sb.WriteString("\n")
		sb.WriteString(m.styles.SuccessBold.Render("Assistant:"))
		sb.WriteString("\n")
		if m.currentThink.Len() > 0 {
			text := m.currentThink.String()
			if utf8.RuneCountInString(text) > thinkingMaxRunes {
				runes := []rune(text)
				text = string(runes[:thinkingMaxRunes]) + "..."
			}
			text = strings.SplitN(text, "\n", 2)[0]
			sb.WriteString(m.styles.MutedItalic.Render("  Thinking: " + text))
			sb.WriteString("\n")
		}
		if m.streamBuffer.Len() > 0 {
			text := wrapText(m.streamBuffer.String(), m.width-4)
			sb.WriteString(text)
			sb.WriteString(" ▋\n")
		} else {
			frame := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
			sb.WriteString("  ")
			sb.WriteString(frame)
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// formatTimestamp returns a short human-readable timestamp string.
func formatTimestamp(t time.Time) string {
	now := time.Now()
	if now.Sub(t) < 24*time.Hour {
		return t.Format("15:04")
	}
	return t.Format("Jan 02 15:04")
}

// wrapText wraps text to maxWidth using muesli/reflow.
func wrapText(text string, maxWidth int) string {
	if maxWidth <= 0 {
		return text
	}
	return wrap.String(text, maxWidth)
}

// truncateJSON returns a short preview of a JSON value.
func truncateJSON(raw []byte, max int) string {
	if len(raw) == 0 {
		return ""
	}
	s := string(raw)
	if len(s) > max {
		return s[:max-3] + "..."
	}
	return s
}

// spinnerFrames are the animation frames for the streaming indicator.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
