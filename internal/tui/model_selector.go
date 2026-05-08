package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/pi-agent/pi/internal/model"
)

// ModelSelector is a searchable model-picker overlay.
type ModelSelector struct {
	items    []model.ModelInfo
	filtered []model.ModelInfo
	cursor   int
	query    string
	input    textinput.Model
	chosen   string // set when the user picks a model
	styles   Styles
}

// NewModelSelector returns a new ModelSelector pre-populated with all
// models from model.Registry.
func NewModelSelector(styles Styles) *ModelSelector {
	inp := textinput.New()
	inp.Placeholder = "Type to filter models…"
	inp.Focus()

	s := &ModelSelector{
		items:  model.Registry,
		styles: styles,
		input:  inp,
	}
	s.filter()
	return s
}

// Init satisfies tea.Model.
func (s *ModelSelector) Init() tea.Cmd {
	return textinput.Blink
}

// Update satisfies tea.Model.
func (s *ModelSelector) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "ctrl+c":
			// Cancel — caller checks chosen == "".
			return s, nil

		case "enter":
			if len(s.filtered) > 0 {
				s.chosen = s.filtered[s.cursor].ID
			}
			return s, nil

		case "up", "ctrl+p":
			if s.cursor > 0 {
				s.cursor--
			}
			return s, nil

		case "down", "ctrl+n":
			if s.cursor < len(s.filtered)-1 {
				s.cursor++
			}
			return s, nil

		default:
			var cmd tea.Cmd
			s.input, cmd = s.input.Update(msg)
			newQuery := s.input.Value()
			if newQuery != s.query {
				s.query = newQuery
				s.cursor = 0
				s.filter()
			}
			return s, cmd
		}
	}

	var cmd tea.Cmd
	s.input, cmd = s.input.Update(msg)
	return s, cmd
}

// View satisfies tea.Model.
func (s *ModelSelector) View() string {
	var sb strings.Builder

	sb.WriteString("\n")
	sb.WriteString(s.styles.Title.Render("  Select Model"))
	sb.WriteString("\n\n")
	sb.WriteString("  ")
	sb.WriteString(s.input.View())
	sb.WriteString("\n")
	sb.WriteString(s.styles.Muted.Render("  " + strings.Repeat("─", 60)))
	sb.WriteString("\n\n")

	const maxVisible = 12
	if len(s.filtered) == 0 {
		sb.WriteString(s.styles.MutedItalic.Render("  No models match."))
		sb.WriteString("\n")
	} else {
		start := 0
		if s.cursor >= maxVisible {
			start = s.cursor - maxVisible + 1
		}
		end := start + maxVisible
		if end > len(s.filtered) {
			end = len(s.filtered)
		}

		for i := start; i < end; i++ {
			item := s.filtered[i]
			prefix := "  "
			line := fmt.Sprintf("%-30s  %-10s  %s",
				item.DisplayName, providerBadge(item.Provider), item.ID)

			if i == s.cursor {
				sb.WriteString(s.styles.Selection.Render(prefix + line))
			} else {
				sb.WriteString(prefix)
				sb.WriteString(s.styles.Muted.Render(item.DisplayName))
				sb.WriteString("  ")
				sb.WriteString(s.styles.AccentBold.Render(providerBadge(item.Provider)))
				sb.WriteString("  ")
				sb.WriteString(s.styles.MutedItalic.Render(item.ID))
			}
			sb.WriteString("\n")
		}

		if len(s.filtered) > maxVisible {
			sb.WriteString(s.styles.MutedItalic.Render(
				fmt.Sprintf("  … %d more", len(s.filtered)-maxVisible)))
			sb.WriteString("\n")
		}
	}

	sb.WriteString("\n")
	sb.WriteString(s.styles.MutedItalic.Render(
		"  ↑/↓: navigate  Enter: select  Esc: cancel"))
	sb.WriteString("\n")
	return sb.String()
}

// filter updates s.filtered based on the current query string.
func (s *ModelSelector) filter() {
	if s.query == "" {
		s.filtered = s.items
		return
	}
	q := strings.ToLower(s.query)
	filtered := make([]model.ModelInfo, 0, len(s.items))
	for _, item := range s.items {
		if strings.Contains(strings.ToLower(item.ID), q) ||
			strings.Contains(strings.ToLower(item.DisplayName), q) ||
			strings.Contains(strings.ToLower(item.Provider), q) {
			filtered = append(filtered, item)
		}
	}
	s.filtered = filtered
}

// providerBadge returns a short badge label for a provider name.
func providerBadge(provider string) string {
	switch provider {
	case "anthropic":
		return "[anthropic]"
	case "openai":
		return "[openai]"
	case "gemini":
		return "[gemini]"
	case "azure":
		return "[azure]"
	case "bedrock":
		return "[bedrock]"
	case "vertex":
		return "[vertex]"
	case "cohere":
		return "[cohere]"
	case "copilot":
		return "[copilot]"
	case "gitlab":
		return "[gitlab]"
	default:
		return "[" + provider + "]"
	}
}
