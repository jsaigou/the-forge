package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/jsaigou/the-forge/internal/cli"
)

// smithInputMsg focuses the smith prompt when its tab opens.
type smithInputMsg struct{}

// ── Compressor page ──────────────────────────────────────────────────────────

type compressorPage struct {
	client  *cli.Client
	width   int
	summary *cli.CompressorSummary
}

type compressorDataMsg struct{}

func newCompressorPage(c *cli.Client) *compressorPage { return &compressorPage{client: c} }

func (p *compressorPage) Name() string { return "Compressor" }

func (p *compressorPage) SetSize(w, h int) { p.width = w }

func (p *compressorPage) Update(msg tea.Msg) tea.Cmd {
	if _, ok := msg.(tickMsg); ok {
		return p.fetch()
	}
	return nil
}

func (p *compressorPage) fetch() tea.Cmd {
	c := p.client
	return func() tea.Msg {
		s, err := c.CompressorSummary("24h")
		if err != nil {
			return errMsg{err}
		}
		p.summary = s
		return compressorDataMsg{}
	}
}

func (p *compressorPage) View() string {
	if p.summary == nil {
		return dimStyle.Render(" loading…")
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("window %s\n\n", p.summary.Window))
	b.WriteString(fmt.Sprintf("%-12s %10s %10s %10s %8s %8s %8s\n",
		"proxy", "tok in", "tok out", "saved", "reqs", "cached", "hit%"))
	for _, px := range p.summary.Proxies {
		hit := "-"
		if px.CacheHitRatePct != nil {
			hit = fmt.Sprintf("%.0f%%", *px.CacheHitRatePct)
		}
		fmt.Fprintf(&b, "%-12s %10s %10s %10s %8d %8d %8s\n",
			px.Proxy, humanBytes(px.TokensIn), humanBytes(px.TokensOut),
			humanBytes(px.TokensSaved), px.Requests, px.RequestsCached, hit)
	}
	return b.String()
}

// ── Keys page ────────────────────────────────────────────────────────────────

type keysPage struct {
	client    *cli.Client
	width     int
	keys      []cli.APIKey
	providers []cli.Provider
	cursor    int
}

type keysDataMsg struct{}

func newKeysPage(c *cli.Client) *keysPage { return &keysPage{client: c} }

func (p *keysPage) Name() string { return "Keys" }

func (p *keysPage) SetSize(w, h int) { p.width = w }

func (p *keysPage) Update(msg tea.Msg) tea.Cmd {
	switch msg.(type) {
	case tickMsg:
		return p.fetch()
	case keysDataMsg:
		return nil
	case tea.KeyMsg:
		m := msg.(tea.KeyMsg)
		switch m.String() {
		case "up", "k":
			p.cursor--
		case "down", "j":
			p.cursor++
		case "r":
			if len(p.keys) > 0 && p.cursor >= 0 && p.cursor < len(p.keys) {
				kid := p.keys[p.cursor].KeyID
				c := p.client
				return func() tea.Msg {
					if err := c.KeyRevoke(kid); err != nil {
						return errMsg{err}
					}
					return infoMsg{"revoked " + kid}
				}
			}
		}
		p.cursor = clamp(p.cursor, 0, max(0, len(p.keys)-1))
	}
	return nil
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (p *keysPage) fetch() tea.Cmd {
	c := p.client
	return func() tea.Msg {
		k, err := c.Keys()
		if err != nil {
			return errMsg{err}
		}
		p.keys = k.Keys
		p.cursor = clamp(p.cursor, 0, max(0, len(p.keys)-1))
		if pr, err := c.Providers(); err == nil {
			p.providers = pr.Providers
		}
		return keysDataMsg{}
	}
}

func (p *keysPage) View() string {
	var b strings.Builder
	if len(p.keys) == 0 {
		b.WriteString(dimStyle.Render(" no keys visible (admin role required)") + "\n")
	} else {
		b.WriteString(dimStyle.Render(" up/down select · r revoke") + "\n")
		for i, k := range p.keys {
			cur := " "
			if i == p.cursor {
				cur = ">"
			}
			fmt.Fprintf(&b, "%s %-8s %-24s %-9s %s\n",
				cur, k.Kind, truncate(k.Name, 24), orDashStr(k.Role), keyTime(k.LastUsed))
		}
	}
	if len(p.providers) > 0 {
		b.WriteString("\nproviders:\n")
		for _, pr := range p.providers {
			fmt.Fprintf(&b, "  %-20s %s\n", pr.Name, dimStyle.Render(pr.APIKeyMasked))
		}
	}
	return b.String()
}

func orDashStr(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func keyTime(t *float64) string {
	if t == nil || *t == 0 {
		return "never used"
	}
	return time.Unix(int64(*t), 0).Format("01-02 15:04")
}

// ── Smith page ───────────────────────────────────────────────────────────────

type smithTokenMsg struct{ delta string }
type smithDoneMsg struct{}

type smithLine struct {
	who  string // "you" | "smith"
	text string
}

type smithPage struct {
	client     *cli.Client
	width      int
	height     int
	input      textinput.Model
	transcript []smithLine
	convID     int64
	streaming  bool
	sender     func(tea.Msg) // set by Run(); pushes msgs into the program
}

func newSmithPage(c *cli.Client) *smithPage {
	in := textinput.New()
	in.Placeholder = "ask smith… (enter to send)"
	in.CharLimit = 4000
	in.Width = 100
	return &smithPage{client: c, input: in}
}

func (p *smithPage) Name() string { return "Smith" }

func (p *smithPage) SetSize(w, h int) { p.width, p.height = w, h }

// SetSender wires the program's Send for streaming updates.
func (p *smithPage) SetSender(send func(tea.Msg)) { p.sender = send }

func (p *smithPage) Update(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case tickMsg:
		return nil
	case smithInputMsg:
		return textinput.Blink
	case smithTokenMsg:
		if n := len(p.transcript); n > 0 && p.transcript[n-1].who == "smith" {
			p.transcript[n-1].text += m.delta
		}
		return nil
	case smithDoneMsg:
		p.streaming = false
		return nil
	case errMsg:
		p.streaming = false
		if n := len(p.transcript); n > 0 && p.transcript[n-1].who == "smith" && p.transcript[n-1].text == "" {
			p.transcript = p.transcript[:n-1]
		}
		return nil
	case tea.KeyMsg:
		if m.String() == "enter" {
			text := strings.TrimSpace(p.input.Value())
			if text == "" || p.streaming {
				return nil
			}
			p.transcript = append(p.transcript, smithLine{"you", text}, smithLine{"smith", ""})
			p.input.SetValue("")
			p.streaming = true
			return p.send(text)
		}
		var cmd tea.Cmd
		p.input, cmd = p.input.Update(m)
		return cmd
	}
	return nil
}

func (p *smithPage) send(text string) tea.Cmd {
	c, conv, send := p.client, p.convID, p.sender
	return func() tea.Msg {
		reply, err := c.SmithChat(conv, text, true, false)
		if err != nil {
			return errMsg{err}
		}
		go streamSmith(reply.ConversationID, c, send)
		return nil
	}
}

// streamSmith consumes SSE until this conversation's message completes,
// pushing token deltas into the running program.
func streamSmith(conversationID int64, c *cli.Client, send func(tea.Msg)) {
	defer func() { send(smithDoneMsg{}) }()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	_ = c.StreamEvents(ctx, map[string]func(json.RawMessage){
		"smith:token": func(raw json.RawMessage) {
			var v struct {
				ConversationID int64  `json:"conversation_id"`
				Delta          string `json:"delta"`
			}
			if json.Unmarshal(raw, &v) != nil || v.ConversationID != conversationID {
				return
			}
			send(smithTokenMsg{delta: v.Delta})
		},
	})
}

func (p *smithPage) View() string {
	var b strings.Builder
	for _, l := range p.transcript {
		if l.who == "you" {
			b.WriteString(okStyle.Render("you › ") + l.text + "\n")
		} else {
			b.WriteString(titleStyle.Render("smith › ") + l.text + "\n")
		}
	}
	if p.streaming {
		b.WriteString(dimStyle.Render("  …thinking") + "\n")
	}
	b.WriteString("\n" + p.input.View())
	return b.String()
}
