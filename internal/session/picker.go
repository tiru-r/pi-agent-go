package session

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── List item ─────────────────────────────────────────────────────────────────

// sessionItem implements list.Item for a single session entry in the picker.
type sessionItem struct {
	meta SessionMeta
}

func (i sessionItem) Title() string {
	title := i.meta.Title
	if title == "" {
		title = i.meta.ID[:8]
	}
	if len(title) > 48 {
		title = title[:45] + "..."
	}
	return title
}

func (i sessionItem) Description() string {
	model := i.meta.Model
	if model == "" {
		model = "unknown"
	}
	age := formatAge(i.meta.UpdatedAt)
	return fmt.Sprintf("%s  %s  %d msgs", model, age, i.meta.MessageCount)
}

func (i sessionItem) FilterValue() string {
	return i.meta.Title + " " + i.meta.ID + " " + i.meta.Model
}

// formatAge returns a human-readable age string for the given time.
func formatAge(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 02")
	}
}

// ── Picker model ──────────────────────────────────────────────────────────────

// Picker is a Bubbletea list-based session picker overlay.
type Picker struct {
	list   list.Model
	index  []SessionMeta // kept for re-filtering
	chosen string        // set when user confirms a session
}

// NewPicker constructs a Picker from a slice of SessionMeta records.
func NewPicker(index []SessionMeta) *Picker {
	items := make([]list.Item, 0, len(index))
	for _, m := range index {
		items = append(items, sessionItem{meta: m})
	}

	delegate := list.NewDefaultDelegate()
	delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.
		Foreground(lipgloss.Color("#007acc")).
		Bold(true)
	delegate.Styles.SelectedDesc = delegate.Styles.SelectedDesc.
		Foreground(lipgloss.Color("#9cdcfe"))

	l := list.New(items, delegate, 72, 20)
	l.Title = "Select a session"
	l.Styles.Title = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#d4d4d4"))
	l.SetShowStatusBar(true)
	l.SetFilteringEnabled(true)
	l.SetShowHelp(true)

	return &Picker{
		list:  l,
		index: index,
	}
}

// Init satisfies tea.Model.
func (p *Picker) Init() tea.Cmd {
	return nil
}

// Update satisfies tea.Model.
func (p *Picker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.list.SetWidth(msg.Width - 4)
		p.list.SetHeight(msg.Height - 6)
		return p, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			item, ok := p.list.SelectedItem().(sessionItem)
			if ok {
				p.chosen = item.meta.ID
			}
			return p, tea.Quit

		case "esc", "q":
			p.chosen = ""
			return p, tea.Quit
		}
	}

	var cmd tea.Cmd
	p.list, cmd = p.list.Update(msg)
	return p, cmd
}

// View satisfies tea.Model.
func (p *Picker) View() string {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(p.list.View())
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().
		Foreground(lipgloss.Color("#6a6a6a")).
		Italic(true).
		Render("  Enter: open  Esc/q: cancel  /: filter"))
	sb.WriteString("\n")
	return sb.String()
}

// Run launches the session picker as a standalone Bubbletea program and
// returns the chosen session ID (empty string if cancelled).
func Run(index []SessionMeta) (string, error) {
	if len(index) == 0 {
		return "", nil
	}
	picker := NewPicker(index)
	p := tea.NewProgram(picker,
		tea.WithAltScreen(),
		tea.WithOutput(os.Stderr),
	)
	m, err := p.Run()
	if err != nil {
		return "", err
	}
	if pick, ok := m.(*Picker); ok {
		return pick.chosen, nil
	}
	return "", nil
}
