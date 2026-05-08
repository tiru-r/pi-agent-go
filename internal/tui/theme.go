package tui

import "github.com/charmbracelet/lipgloss"

// Theme holds the color palette for the TUI.
type Theme struct {
	// Text colors
	Primary   lipgloss.Color
	Secondary lipgloss.Color
	Muted     lipgloss.Color
	Error     lipgloss.Color
	Success   lipgloss.Color
	Warning   lipgloss.Color

	// UI element colors
	UserBubble      lipgloss.Color
	AssistantBubble lipgloss.Color
	ToolName        lipgloss.Color
	ToolResult      lipgloss.Color
	Thinking        lipgloss.Color
	Border          lipgloss.Color
	StatusBar       lipgloss.Color
	StatusBarText   lipgloss.Color
	InputBorder     lipgloss.Color
	Cursor          lipgloss.Color
}

// DarkTheme is the default dark-background colour scheme.
var DarkTheme = Theme{
	Primary:   lipgloss.Color("#d4d4d4"),
	Secondary: lipgloss.Color("#9cdcfe"),
	Muted:     lipgloss.Color("#6a6a6a"),
	Error:     lipgloss.Color("#f44747"),
	Success:   lipgloss.Color("#4ec9b0"),
	Warning:   lipgloss.Color("#ce9178"),

	UserBubble:      lipgloss.Color("#264f78"),
	AssistantBubble: lipgloss.Color("#1e3a2f"),
	ToolName:        lipgloss.Color("#dcdcaa"),
	ToolResult:      lipgloss.Color("#9cdcfe"),
	Thinking:        lipgloss.Color("#586e75"),
	Border:          lipgloss.Color("#3c3c3c"),
	StatusBar:       lipgloss.Color("#007acc"),
	StatusBarText:   lipgloss.Color("#ffffff"),
	InputBorder:     lipgloss.Color("#007acc"),
	Cursor:          lipgloss.Color("#aeafad"),
}

// LightTheme is the light-background colour scheme.
var LightTheme = Theme{
	Primary:   lipgloss.Color("#2d2d2d"),
	Secondary: lipgloss.Color("#0066bf"),
	Muted:     lipgloss.Color("#7a7a7a"),
	Error:     lipgloss.Color("#c62828"),
	Success:   lipgloss.Color("#2e8b57"),
	Warning:   lipgloss.Color("#b36200"),

	UserBubble:      lipgloss.Color("#cce7ff"),
	AssistantBubble: lipgloss.Color("#e8f5e9"),
	ToolName:        lipgloss.Color("#795e26"),
	ToolResult:      lipgloss.Color("#0066bf"),
	Thinking:        lipgloss.Color("#9e9e9e"),
	Border:          lipgloss.Color("#c8c8c8"),
	StatusBar:       lipgloss.Color("#0066bf"),
	StatusBarText:   lipgloss.Color("#ffffff"),
	InputBorder:     lipgloss.Color("#0066bf"),
	Cursor:          lipgloss.Color("#000000"),
}

// GetTheme returns a theme by name ("dark" or "light").
// Any unrecognised name falls back to DarkTheme.
func GetTheme(name string) Theme {
	switch name {
	case "light":
		return LightTheme
	default:
		return DarkTheme
	}
}

// Styles holds pre-built lipgloss styles for every TUI element.
type Styles struct {
	UserMessage      lipgloss.Style
	AssistantMessage lipgloss.Style
	ToolUse          lipgloss.Style
	ToolResult       lipgloss.Style
	ThinkingBlock    lipgloss.Style
	ErrorBlock       lipgloss.Style
	StatusBar        lipgloss.Style
	Input            lipgloss.Style
	Border           lipgloss.Style
	Timestamp        lipgloss.Style
	ModelBadge       lipgloss.Style
	TokenCount       lipgloss.Style

	// Internal helpers (used by view helpers)
	Accent      lipgloss.Style
	AccentBold  lipgloss.Style
	SuccessBold lipgloss.Style
	WarningBold lipgloss.Style
	ErrorBold   lipgloss.Style
	Muted       lipgloss.Style
	MutedBold   lipgloss.Style
	MutedItalic lipgloss.Style
	Selection   lipgloss.Style
	Title       lipgloss.Style
}

// NewStyles builds a Styles set from the given theme.
func NewStyles(theme Theme) Styles {
	return Styles{
		UserMessage: lipgloss.NewStyle().
			Foreground(theme.Primary).
			Bold(true),
		AssistantMessage: lipgloss.NewStyle().
			Foreground(theme.Success).
			Bold(true),
		ToolUse: lipgloss.NewStyle().
			Foreground(theme.ToolName).
			Bold(true),
		ToolResult: lipgloss.NewStyle().
			Foreground(theme.ToolResult).
			Italic(true),
		ThinkingBlock: lipgloss.NewStyle().
			Foreground(theme.Thinking).
			Italic(true),
		ErrorBlock: lipgloss.NewStyle().
			Foreground(theme.Error).
			Bold(true),
		StatusBar: lipgloss.NewStyle().
			Background(theme.StatusBar).
			Foreground(theme.StatusBarText).
			Bold(true),
		Input: lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(theme.InputBorder).
			Padding(0, 1),
		Border: lipgloss.NewStyle().
			Foreground(theme.Border),
		Timestamp: lipgloss.NewStyle().Foreground(theme.Muted).Italic(true),
		ModelBadge: lipgloss.NewStyle().
			Foreground(theme.StatusBarText).
			Background(theme.StatusBar).
			Padding(0, 1),
		TokenCount: lipgloss.NewStyle().Foreground(theme.Muted),

		Accent:      lipgloss.NewStyle().Foreground(theme.Secondary),
		AccentBold:  lipgloss.NewStyle().Foreground(theme.Secondary).Bold(true),
		SuccessBold: lipgloss.NewStyle().Foreground(theme.Success).Bold(true),
		WarningBold: lipgloss.NewStyle().Foreground(theme.Warning).Bold(true),
		ErrorBold:   lipgloss.NewStyle().Foreground(theme.Error).Bold(true),
		Muted:       lipgloss.NewStyle().Foreground(theme.Muted),
		MutedBold:   lipgloss.NewStyle().Foreground(theme.Muted).Bold(true),
		MutedItalic: lipgloss.NewStyle().Foreground(theme.Muted).Italic(true),
		Selection: lipgloss.NewStyle().
			Background(theme.UserBubble).
			Foreground(theme.Primary).
			Bold(true),
		Title: lipgloss.NewStyle().
			Foreground(theme.Secondary).
			Bold(true),
	}
}
