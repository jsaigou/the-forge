package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/i18n"
)

// HumanBytes renders a byte count for humans.
func HumanBytes(b int64) string {
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

// humanBytes is the in-package alias.
var humanBytes = HumanBytes

// StatusVerb prints a one-screen ops summary (forge status).
func StatusVerb() error {
	c, err := New()
	if err != nil {
		return err
	}
	st, err := c.Status()
	if err != nil {
		return err
	}
	sc, _ := c.SchedulerStatus()
	fmt.Println(i18n.PadRight(i18n.T("cli.status.label_host"), 6) + " " + st.Hostname)
	fmt.Println(i18n.PadRight(i18n.T("cli.status.label_forge"), 6) + " " + st.Version)
	fmt.Println(i18n.PadRight(i18n.T("cli.status.label_mode"), 6) + " " + st.Mode)
	for _, slot := range []string{"a1", "a2", "a3", "a4"} {
		model := i18n.T("cli.slot.empty")
		if m, ok := st.Slots[slot]; ok && m != nil && *m != "" {
			model = *m
			if sc != nil {
				if b, ok := sc.SlotMemoryBytes[slot]; ok && b > 0 {
					model += fmt.Sprintf(" (%s)", humanBytes(b))
				}
				if v, ok := sc.IdleSeconds[slot]; ok && v != nil {
					model += " " + i18n.T("cli.status.idle", *v)
				}
			}
		}
		fmt.Println(i18n.PadRight(slot+":", 6) + " " + model)
	}
	if sc != nil {
		bd := sc.MemoryBudget
		fmt.Println(i18n.T("cli.status.budget", humanBytes(bd.UsedBytes), humanBytes(bd.TotalBytes)))
	}
	mt, _ := c.Metrics()
	if mt != nil && mt.GTTUsedBytes != nil && mt.GTTTotalBytes != nil {
		fmt.Println(i18n.PadRight("gtt", 6) + " " + fmt.Sprintf("%s / %s", humanBytes(*mt.GTTUsedBytes), humanBytes(*mt.GTTTotalBytes)))
	}
	return nil
}

// ModelsVerb lists catalog configs available to load.
func ModelsVerb() error {
	c, err := New()
	if err != nil {
		return err
	}
	cards, err := c.ConfigCards()
	if err != nil {
		return err
	}
	for _, cfg := range cards {
		def := ""
		if cfg.IsDefault {
			def = i18n.T("cli.models.default_suffix")
		}
		fmt.Printf("%s ctx %s %s%s\n", i18n.PadRight(cfg.Name, 28), i18n.PadRight(fmt.Sprintf("%d", cfg.NCtx), 8), cfg.Status, def)
	}
	return nil
}

// LoadVerb loads a config onto a slot (slot optional → server default).
func LoadVerb(mode, slot string) error {
	c, err := New()
	if err != nil {
		return err
	}
	r, err := c.Load(mode, slot)
	if err != nil {
		return err
	}
	if !r.Success {
		return fmt.Errorf("%s", i18n.T("cli.load.failed", r.Message))
	}
	fmt.Println(i18n.T("cli.load.result", mode, orSlot(slot), r.NCtx))
	return nil
}

func orSlot(s string) string {
	if s == "" {
		return i18n.T("cli.load.default_slot")
	}
	return s
}

// UnloadVerb empties a slot.
func UnloadVerb(slot string) error {
	c, err := New()
	if err != nil {
		return err
	}
	r, err := c.Unload(slot)
	if err != nil {
		return err
	}
	if !r.Success {
		return fmt.Errorf("%s", i18n.T("cli.unload.failed", r.Message))
	}
	fmt.Println(i18n.T("cli.unload.result", slot))
	return nil
}

// ServicesVerb lists infra services; with action+name toggles one.
func ServicesVerb(action, name string) error {
	c, err := New()
	if err != nil {
		return err
	}
	switch action {
	case "":
	case "start":
		return c.ServiceStart(name)
	case "stop":
		return c.ServiceStop(name)
	default:
		return errors.New(i18n.T("cli.services.usage"))
	}
	l, err := c.InfraServices()
	if err != nil {
		return err
	}
	for _, s := range l.Services {
		state := i18n.T("cli.services.state_down")
		if s.Active {
			state = i18n.T("cli.services.state_up")
		}
		port := ""
		if s.Port > 0 {
			port = fmt.Sprintf(" :%d", s.Port)
		}
		fmt.Println(i18n.PadRight(s.Label, 22) + " " + i18n.PadRight(state, 4) + port)
	}
	return nil
}

// cliKeyTTL is the default lifetime for a keys-export-minted CLI/TUI key
// (security sprint 3, #36) — a sane default for a key a human re-exports
// occasionally, not a hard requirement; router/MCP consumer keys minted via
// `forge mint-key` are unaffected and stay unbounded/non-expiring by default.
const cliKeyTTL = 90 * 24 * time.Hour

// KeyExportVerb mints an operator CLI key and writes it to the keyfile. By
// default the new key is bound to this request's own resolved client IP and
// expires in 90 days (#34/#36) — for the common case (run directly on the
// host) that's loopback; for the documented remote-admin-over-tailnet flow
// (FORGE_API_URL pointed at the tailnet HTTPS endpoint) it's the caller's
// own tailnet IP. unbound skips the IP binding entirely — pass it when the
// exported key needs to work from more than one machine/IP.
func KeyExportVerb(unbound bool) error {
	c, err := New()
	if err != nil {
		return err
	}
	resp, err := c.KeyCreate("forge", "cli-tui", "operator", !unbound, int64(cliKeyTTL.Seconds()))
	if err != nil {
		return fmt.Errorf("%s: %w", i18n.T("cli.keyexport.requires_admin"), err)
	}
	p := KeyPath()
	if err := os.MkdirAll(strings.TrimSuffix(p, "/cli.key"), 0o700); err != nil {
		fmt.Println(resp.Token)
		return err
	}
	if err := os.WriteFile(p, []byte(resp.Token+"\n"), 0o600); err != nil {
		fmt.Println(resp.Token)
		return err
	}
	fmt.Println(i18n.T("cli.keyexport.written", resp.Key.KeyID, p))
	return nil
}
