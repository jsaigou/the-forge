package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/jsaigou/the-forge/internal/cli"
)

const refreshInterval = 3 * time.Second

func humanBytes(b int64) string {
	const kb, mb, gb = 1 << 10, 1 << 20, 1 << 30
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/gb)
	case b >= mb:
		return fmt.Sprintf("%.0f MB", float64(b)/mb)
	case b >= kb:
		return fmt.Sprintf("%.0f KB", float64(b)/kb)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// ── Overview page ────────────────────────────────────────────────────────────

type overviewPage struct {
	client  *cli.Client
	width   int
	status  *cli.Status
	sched   *cli.SchedulerStatus
	metrics *cli.Metrics
	notifs  []cli.Notification
}

func newOverviewPage(c *cli.Client) *overviewPage { return &overviewPage{client: c} }

func (p *overviewPage) Name() string { return "Overview" }

func (p *overviewPage) SetSize(w, h int) { p.width = w }

func (p *overviewPage) Update(msg tea.Msg) tea.Cmd {
	switch msg.(type) {
	case tickMsg:
		return tea.Batch(
			p.fetch(),
		)
	case overviewDataMsg:
		// applied in fetch's tea.Cmd closure
	}
	return nil
}

type overviewDataMsg struct{}

func (p *overviewPage) fetch() tea.Cmd {
	c := p.client
	return func() tea.Msg {
		st, err := c.Status()
		if err != nil {
			return errMsg{err}
		}
		sc, _ := c.SchedulerStatus()
		mt, _ := c.Metrics()
		nf, _ := c.Notifications()
		p.status, p.sched, p.metrics = st, sc, mt
		if nf != nil {
			p.notifs = nf.Notifications
		}
		return overviewDataMsg{}
	}
}

func (p *overviewPage) View() string {
	if p.status == nil {
		return dimStyle.Render(" loading…")
	}
	var b strings.Builder

	b.WriteString(fmt.Sprintf("host %s · forge %s · mode %s\n\n",
		p.status.Hostname, p.status.Version, okStyle.Render(p.status.Mode)))

	// slots one-liner
	var slots []string
	for _, name := range []string{"a1", "a2", "a3", "a4"} {
		model := "(empty)"
		style := dimStyle
		if m, ok := p.status.Slots[name]; ok && m != nil && *m != "" {
			model = *m
			style = lipgloss.NewStyle()
		}
		slots = append(slots, fmt.Sprintf("%s %s", warnStyle.Render(name), style.Render(model)))
	}
	b.WriteString(strings.Join(slots, "   ") + "\n\n")

	// GTT meter
	if p.metrics != nil && p.metrics.GTTUsedBytes != nil && p.metrics.GTTTotalBytes != nil {
		used, total := *p.metrics.GTTUsedBytes, *p.metrics.GTTTotalBytes
		pct := float64(used) / float64(total)
		barW := min64(int64(p.width-40), 40)
		filled := int(pct * float64(barW))
		bar := strings.Repeat("█", filled) + strings.Repeat("░", int(barW)-filled)
		color := okStyle
		if pct > 0.9 {
			color = critStyle
		} else if pct > 0.7 {
			color = warnStyle
		}
		b.WriteString(fmt.Sprintf("GTT [%s] %s / %s (%.0f%%)\n",
			color.Render(bar), humanBytes(used), humanBytes(total), pct*100))
	}
	if p.metrics != nil && p.metrics.TempCelsius != nil {
		b.WriteString(fmt.Sprintf("temp %.0f°C", *p.metrics.TempCelsius))
		if p.metrics.GPUUsePct != nil {
			b.WriteString(fmt.Sprintf(" · gpu %.0f%%", *p.metrics.GPUUsePct))
		}
		b.WriteString("\n")
	}

	// per-slot memory from scheduler
	if p.sched != nil && len(p.sched.SlotMemoryBytes) > 0 {
		var parts []string
		for slot, mem := range p.sched.SlotMemoryBytes {
			idle := "?"
			if v, ok := p.sched.IdleSeconds[slot]; ok && v != nil {
				idle = fmt.Sprintf("%.0fs", *v)
			}
			parts = append(parts, fmt.Sprintf("%s %s idle %s", slot, humanBytes(mem), idle))
		}
		b.WriteString(dimStyle.Render(strings.Join(parts, " · ")) + "\n")
	}

	// alerts
	if len(p.notifs) > 0 {
		b.WriteString("\n" + warnStyle.Render("alerts:") + "\n")
		n := len(p.notifs)
		if n > 6 {
			n = 6
		}
		for _, a := range p.notifs[:n] {
			style := warnStyle
			if a.Severity == "critical" || a.Severity == "crit" {
				style = critStyle
			}
			b.WriteString(fmt.Sprintf("  %s %s ×%d — %s\n",
				style.Render(a.Severity), a.Code, a.Occurrences, truncate(a.Message, max(20, p.width-30))))
		}
	}
	return b.String()
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// ── Slots page ───────────────────────────────────────────────────────────────

type slotsPage struct {
	client     *cli.Client
	table      table.Model
	configs    []cli.ConfigCard
	sched      *cli.SchedulerStatus
	cursorSlot string
	width      int
}

type slotsDataMsg struct{}

func newSlotsPage(c *cli.Client) *slotsPage {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "SLOT", Width: 6},
			{Title: "MODEL", Width: 34},
			{Title: "MEM", Width: 10},
			{Title: "IDLE", Width: 8},
		}),
		table.WithFocused(true),
		table.WithHeight(6),
	)
	st := table.DefaultStyles()
	st.Header = st.Header.Bold(true)
	st.Selected = st.Selected.Foreground(lipgloss.Color("0")).Background(lipgloss.Color("205"))
	t.SetStyles(st)
	return &slotsPage{client: c, table: t}
}

func (p *slotsPage) Name() string { return "Slots" }

func (p *slotsPage) SetSize(w, h int) { p.width = w; p.table.SetHeight(max(3, h-8)) }

func (p *slotsPage) Update(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case tickMsg:
		return p.fetch()
	case slotsDataMsg:
		p.refreshRows()
		return nil
	case tea.KeyMsg:
		switch m.String() {
		case "enter":
			slot := p.selectedSlot()
			if slot == "" {
				return func() tea.Msg { return errMsg{fmt.Errorf("no slot row selected")} }
			}
			cur := p.client
			s := slot
			return func() tea.Msg {
				r, err := cur.Unload(s)
				if err != nil {
					return errMsg{err}
				}
				return infoMsg{"unload " + s + ": " + r.Message}
			}
		case "l":
			return p.loadSelected()
		}
		if p.table.Focused() {
			var cmd tea.Cmd
			p.table, cmd = p.table.Update(msg)
			return cmd
		}
	}
	return nil
}

func (p *slotsPage) selectedSlot() string {
	rows := p.table.Rows()
	if p.table.Cursor() < len(rows) {
		return rows[p.table.Cursor()][0]
	}
	return ""
}

// loadSelected loads the first not-loaded config onto the selected empty slot.
// (Full model picking lands with the models tab; this is the fast path.)
func (p *slotsPage) loadSelected() tea.Cmd {
	slot := p.selectedSlot()
	if slot == "" {
		return func() tea.Msg { return errMsg{fmt.Errorf("no slot row selected")} }
	}
	loaded := map[string]bool{}
	for _, m := range p.sched.Slots {
		if m != nil && *m != "" {
			loaded[*m] = true
		}
	}
	var pick string
	for _, cfg := range p.configs {
		if !loaded[cfg.Name] {
			pick = cfg.Name
			break
		}
	}
	if pick == "" {
		return func() tea.Msg { return infoMsg{"every config is already loaded"} }
	}
	c, mode := p.client, pick
	return func() tea.Msg {
		r, err := c.Load(mode, slot)
		if err != nil {
			return errMsg{err}
		}
		return infoMsg{fmt.Sprintf("load %s → %s: %s", mode, slot, r.Message)}
	}
}

func (p *slotsPage) fetch() tea.Cmd {
	c := p.client
	return func() tea.Msg {
		sc, err := c.SchedulerStatus()
		if err != nil {
			return errMsg{err}
		}
		cards, _ := c.ConfigCards()
		p.sched = sc
		if cards != nil {
			p.configs = cards
		}
		return slotsDataMsg{}
	}
}

func (p *slotsPage) refreshRows() {
	rows := []table.Row{}
	for _, slot := range []string{"a1", "a2", "a3", "a4"} {
		model := ""
		if m, ok := p.sched.Slots[slot]; ok && m != nil {
			model = *m
		}
		mem := "-"
		if b, ok := p.sched.SlotMemoryBytes[slot]; ok && b > 0 {
			mem = humanBytes(b)
		}
		idle := "-"
		if v, ok := p.sched.IdleSeconds[slot]; ok && v != nil {
			idle = fmt.Sprintf("%.0fs", *v)
		}
		rows = append(rows, table.Row{slot, orDash(model), mem, idle})
	}
	p.table.SetRows(rows)
}

func orDash(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}

func (p *slotsPage) View() string {
	var b strings.Builder
	b.WriteString(dimStyle.Render(" enter unload · l load first-free config · up/down select") + "\n")
	b.WriteString(p.table.View() + "\n")
	if p.sched != nil {
		bd := p.sched.MemoryBudget
		b.WriteString(dimStyle.Render(fmt.Sprintf("budget used %s / %s (free %s)",
			humanBytes(bd.UsedBytes), humanBytes(bd.TotalBytes), humanBytes(bd.FreeBytes))) + "\n")
		if q := p.sched.Queue; len(q) > 0 {
			b.WriteString(fmt.Sprintf("queue: %d pending\n", len(q)))
		}
	}
	return b.String()
}

// ── Services page ────────────────────────────────────────────────────────────

type servicesPage struct {
	client *cli.Client
	width  int
	list   *cli.InfraServices
}

type servicesDataMsg struct{}

func newServicesPage(c *cli.Client) *servicesPage { return &servicesPage{client: c} }

func (p *servicesPage) Name() string { return "Services" }

func (p *servicesPage) SetSize(w, h int) { p.width = w }

func (p *servicesPage) Update(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case tickMsg:
		return p.fetch()
	case servicesDataMsg:
		return nil
	case tea.KeyMsg:
		idx := m.String()
		if len(idx) == 1 && idx[0] >= '1' && idx[0] <= '9' && p.list != nil {
			i := int(idx[0] - '1')
			if i < len(p.list.Services) {
				svc := p.list.Services[i]
				c := p.client
				start := !svc.Active
				return func() tea.Msg {
					var err error
					if start {
						err = c.ServiceStart(svc.Key)
					} else {
						err = c.ServiceStop(svc.Key)
					}
					if err != nil {
						return errMsg{err}
					}
					if start {
						return infoMsg{"started " + svc.Key}
					}
					return infoMsg{"stopped " + svc.Key}
				}
			}
		}
	}
	return nil
}

func (p *servicesPage) fetch() tea.Cmd {
	c := p.client
	return func() tea.Msg {
		l, err := c.InfraServices()
		if err != nil {
			return errMsg{err}
		}
		p.list = l
		return servicesDataMsg{}
	}
}

func (p *servicesPage) View() string {
	if p.list == nil {
		return dimStyle.Render(" loading…")
	}
	var b strings.Builder
	b.WriteString(dimStyle.Render(" 1-9 toggle service") + "\n\n")
	for i, s := range p.list.Services {
		state := critStyle.Render("● down")
		if s.Active {
			state = okStyle.Render("● up")
		}
		port := ""
		if s.Port > 0 {
			port = fmt.Sprintf(" :%d", s.Port)
		}
		fmt.Fprintf(&b, "  %d. %-22s %s%s  %s\n", i+1, s.Label, state, dimStyle.Render(port), dimStyle.Render(s.Unit))
	}
	return b.String()
}
