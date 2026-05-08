package tui

import "github.com/charmbracelet/bubbles/key"

// KeyMap defines all keyboard shortcuts used in the TUI.
type KeyMap struct {
	Submit       key.Binding // Enter / Ctrl+D — submit the current input
	NewLine      key.Binding // Shift+Enter / Alt+Enter — insert newline
	Quit         key.Binding // Ctrl+C / Ctrl+Q — exit the application
	Abort        key.Binding // Escape — cancel current generation
	HistoryUp    key.Binding // Up — navigate input history
	HistoryDown  key.Binding // Down — navigate input history
	ScrollUp     key.Binding // PageUp / Ctrl+U — scroll viewport up
	ScrollDown   key.Binding // PageDown / Ctrl+D — scroll viewport down
	ScrollTop    key.Binding // Ctrl+Home — jump to top
	ScrollBottom key.Binding // Ctrl+End — jump to bottom
	Copy         key.Binding // Ctrl+Y — copy last response
	Help         key.Binding // ? — toggle help overlay
	ClearScreen  key.Binding // Ctrl+L — clear screen
	NewSession   key.Binding // Ctrl+N — open a new session
	OpenSession  key.Binding // Ctrl+O — open the session picker
	ToggleThink  key.Binding // Ctrl+T — cycle thinking level
}

// DefaultKeyMap is the factory-default key binding set.
var DefaultKeyMap = KeyMap{
	Submit: key.NewBinding(
		key.WithKeys("enter", "ctrl+d"),
		key.WithHelp("Enter/Ctrl+D", "submit"),
	),
	NewLine: key.NewBinding(
		key.WithKeys("shift+enter", "alt+enter"),
		key.WithHelp("Shift+Enter", "new line"),
	),
	Quit: key.NewBinding(
		key.WithKeys("ctrl+c", "ctrl+q"),
		key.WithHelp("Ctrl+C/Q", "quit"),
	),
	Abort: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("Esc", "abort"),
	),
	HistoryUp: key.NewBinding(
		key.WithKeys("up"),
		key.WithHelp("↑", "history up"),
	),
	HistoryDown: key.NewBinding(
		key.WithKeys("down"),
		key.WithHelp("↓", "history down"),
	),
	ScrollUp: key.NewBinding(
		key.WithKeys("pgup", "ctrl+u"),
		key.WithHelp("PgUp/Ctrl+U", "scroll up"),
	),
	ScrollDown: key.NewBinding(
		key.WithKeys("pgdown", "ctrl+f"),
		key.WithHelp("PgDn/Ctrl+F", "scroll down"),
	),
	ScrollTop: key.NewBinding(
		key.WithKeys("ctrl+home"),
		key.WithHelp("Ctrl+Home", "scroll top"),
	),
	ScrollBottom: key.NewBinding(
		key.WithKeys("ctrl+end"),
		key.WithHelp("Ctrl+End", "scroll bottom"),
	),
	Copy: key.NewBinding(
		key.WithKeys("ctrl+y"),
		key.WithHelp("Ctrl+Y", "copy last response"),
	),
	Help: key.NewBinding(
		key.WithKeys("?"),
		key.WithHelp("?", "help"),
	),
	ClearScreen: key.NewBinding(
		key.WithKeys("ctrl+l"),
		key.WithHelp("Ctrl+L", "clear screen"),
	),
	NewSession: key.NewBinding(
		key.WithKeys("ctrl+n"),
		key.WithHelp("Ctrl+N", "new session"),
	),
	OpenSession: key.NewBinding(
		key.WithKeys("ctrl+o"),
		key.WithHelp("Ctrl+O", "open session"),
	),
	ToggleThink: key.NewBinding(
		key.WithKeys("ctrl+t"),
		key.WithHelp("Ctrl+T", "toggle thinking"),
	),
}
