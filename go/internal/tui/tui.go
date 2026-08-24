// Package tui — The Forge ops console (Bubble Tea). Talks to the local
// daemon's dashboard API via internal/cli.
package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/jsaigou/the-forge/internal/cli"
)

// tickMsg drives periodic refresh of the active page.
type tickMsg struct{}

// errMsg carries a failed background fetch/action to the UI.
type errMsg struct{ err error }

// infoMsg carries one-line action feedback.
type infoMsg struct{ text string }

type page interface {
	Name() string
	Update(tea.Msg) tea.Cmd
	View() string
	SetSize(w, h int)
}

type keyMap struct {
	quit key.Binding
	tabs key.Binding
	help key.Binding
}

var keys = keyMap{
	quit: key.NewBinding(key.WithKeys("ctrl+c", "q"), key.WithHelp("q", "quit")),
	tabs: key.NewBinding(key.WithKeys("tab", "1", "2", "3", "4", "5", "6"), key.WithHelp("tab/1-6", "switch tab")),
	help: key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
}

var (
	titleStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Padding(0, 1)
	tabStyle       = lipgloss.NewStyle().Padding(0, 2)
	activeTabStyle = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("205")).Foreground(lipgloss.Color("0")).Padding(0, 2)
	errStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196")).Padding(0, 1)
	infoStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("46")).Padding(0, 1)
	dimStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	okStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	warnStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	critStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	borderStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
)

// Session is the running TUI state.
type Session struct {
	client   *cli.Client
	pages    []page
	active   int
	width    int
	height   int
	errText  string
	infoText string
	help     help.Model
	showHelp bool
}

// New builds the TUI session against an API client.
func New(c *cli.Client) *Session {
	s := &Session{
		client: c,
		pages: []page{
			newOverviewPage(c),
			newSlotsPage(c),
			newServicesPage(c),
			newCompressorPage(c),
			newKeysPage(c),
			newSmithPage(c),
		},
		help: help.New(),
	}
	return s
}

// Run starts the Bubble Tea program.
func Run(c *cli.Client) error {
	s := New(c)
	p := tea.NewProgram(s, tea.WithAltScreen())
	for _, pg := range s.pages {
		if sp, ok := pg.(*smithPage); ok {
			sp.SetSender(p.Send)
		}
	}
	_, err := p.Run()
	return err
}

func (s *Session) Init() tea.Cmd {
	return tea.Batch(s.pages[s.active].Update(tickMsg{}), tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func (s *Session) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		s.width, s.height = m.Width, m.Height
		for _, p := range s.pages {
			p.SetSize(m.Width, m.Height-4) // header + tabs + footer
		}
		return s, nil

	case tea.KeyMsg:
		switch {
		case key.Matches(m, keys.quit):
			return s, tea.Quit
		case m.String() == "ctrl+c":
			return s, tea.Quit
		case m.String() == "?":
			s.showHelp = !s.showHelp
			return s, nil
		case m.String() == "tab" || (len(m.String()) == 1 && m.String() >= "1" && m.String() <= "6"):
			idx := s.active
			if m.String() == "tab" {
				idx = (s.active + 1) % len(s.pages)
			} else if n := int(m.String()[0] - '1'); n < len(s.pages) {
				idx = n
			}
			if idx != s.active {
				s.active = idx
				s.errText, s.infoText = "", ""
			}
			cmds := []tea.Cmd{s.pages[s.active].Update(tickMsg{})}
			if _, ok := s.pages[s.active].(*smithPage); ok {
				cmds = append(cmds, func() tea.Msg { return smithInputMsg{} })
			}
			return s, tea.Batch(cmds...)
		}
		// fall through to page handling below

	case tickMsg:
		cmd := s.pages[s.active].Update(m)
		return s, tea.Batch(cmd, tickCmd())

	case errMsg:
		s.errText = m.err.Error()
		return s, nil

	case infoMsg:
		s.infoText = m.text
		return s, nil
	}

	cmd := s.pages[s.active].Update(msg)
	return s, cmd
}

func (s *Session) View() string {
	if s.width == 0 {
		return "loading…"
	}
	var b strings.Builder

	b.WriteString(titleStyle.Render("The Forge"))
	b.WriteString(dimStyle.Render(fmt.Sprintf("  ops console  %dx%d\n\n", s.width, s.height)))

	for i, p := range s.pages {
		name := fmt.Sprintf("%d %s", i+1, p.Name())
		if i == s.active {
			b.WriteString(activeTabStyle.Render(name))
		} else {
			b.WriteString(tabStyle.Render(name))
		}
	}
	b.WriteString("\n\n")

	body := strings.TrimRight(s.pages[s.active].View(), "\n")
	b.WriteString(body)

	// footer
	b.WriteString("\n")
	if s.errText != "" {
		b.WriteString(errStyle.Render(" ✗ "+truncate(s.errText, s.width-4)) + "\n")
	} else if s.infoText != "" {
		b.WriteString(infoStyle.Render(" ✓ "+truncate(s.infoText, s.width-4)) + "\n")
	}
	if s.showHelp {
		b.WriteString(dimStyle.Render(" q quit · tab/1-6 tabs · ? toggle help · per-tab keys shown in panels") + "\n")
	}
	return b.String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:max(0, n-1)]) + "…"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
