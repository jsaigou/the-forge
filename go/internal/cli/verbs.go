package cli

import (
	"fmt"
	"os"
	"strings"
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
	fmt.Printf("host   %s\n", st.Hostname)
	fmt.Printf("forge  %s\n", st.Version)
	fmt.Printf("mode   %s\n", st.Mode)
	for _, slot := range []string{"a1", "a2", "a3", "a4"} {
		model := "(empty)"
		if m, ok := st.Slots[slot]; ok && m != nil && *m != "" {
			model = *m
			if sc != nil {
				if b, ok := sc.SlotMemoryBytes[slot]; ok && b > 0 {
					model += fmt.Sprintf(" (%s)", humanBytes(b))
				}
				if v, ok := sc.IdleSeconds[slot]; ok && v != nil {
					model += fmt.Sprintf(" idle %.0fs", *v)
				}
			}
		}
		fmt.Printf("%-6s %s\n", slot+":", model)
	}
	if sc != nil {
		bd := sc.MemoryBudget
		fmt.Printf("budget used %s / %s\n", humanBytes(bd.UsedBytes), humanBytes(bd.TotalBytes))
	}
	mt, _ := c.Metrics()
	if mt != nil && mt.GTTUsedBytes != nil && mt.GTTTotalBytes != nil {
		fmt.Printf("gtt    %s / %s\n", humanBytes(*mt.GTTUsedBytes), humanBytes(*mt.GTTTotalBytes))
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
			def = "  (default)"
		}
		fmt.Printf("%-28s ctx %-8d %s%s\n", cfg.Name, cfg.NCtx, cfg.Status, def)
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
		return fmt.Errorf("load failed: %s", r.Message)
	}
	fmt.Printf("loaded %s on %s (n_ctx=%d)\n", mode, orSlot(slot), r.NCtx)
	return nil
}

func orSlot(s string) string {
	if s == "" {
		return "(default slot)"
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
		return fmt.Errorf("unload failed: %s", r.Message)
	}
	fmt.Printf("unloaded %s\n", slot)
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
		return fmt.Errorf("usage: forge services [start|stop <name>]")
	}
	l, err := c.InfraServices()
	if err != nil {
		return err
	}
	for _, s := range l.Services {
		state := "down"
		if s.Active {
			state = "up"
		}
		port := ""
		if s.Port > 0 {
			port = fmt.Sprintf(" :%d", s.Port)
		}
		fmt.Printf("%-22s %-4s%s\n", s.Label, state, port)
	}
	return nil
}

// KeyExportVerb mints an operator CLI key and writes it to the keyfile.
func KeyExportVerb() error {
	c, err := New()
	if err != nil {
		return err
	}
	resp, err := c.KeyCreate("forge", "cli-tui", "operator")
	if err != nil {
		return fmt.Errorf("minting requires admin — run `forge mint-key -kind forge -name cli -role operator` on the host: %w", err)
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
	fmt.Printf("key %s written to %s\n", resp.KeyID, p)
	return nil
}
