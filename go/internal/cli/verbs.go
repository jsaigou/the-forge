package cli

import (
	"errors"
	"fmt"
	"io"
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
		if p := bd.Partition; p != nil {
			fmt.Println(i18n.T("cli.status.partition", humanBytes(p.ModelUsedBytes), humanBytes(p.ModelBudgetBytes), humanBytes(p.FreeBytes), p.Binding))
		} else {
			fmt.Println(i18n.T("cli.status.budget", humanBytes(bd.UsedBytes), humanBytes(bd.TotalBytes)))
		}
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

// opPollSeconds is how long each long-poll for an operation waits server-side. It stays well under the client's
// 30s HTTP timeout.
const opPollSeconds = 20

// opWaitLimit is the CLI's overall patience for one load or unload. The engine has its own, longer, startup
// timeout per service; this only bounds how long the command keeps waiting before telling the user to look.
var opWaitLimit = 20 * time.Minute

var errOpTimeout = errors.New("cli: operation still running")

// waitOperation follows an operation to its end with long-polls, returning errOpTimeout (and the last state) if
// it is still running after limit.
func (c *Client) waitOperation(id string, limit time.Duration) (*Operation, error) {
	deadline := time.Now().Add(limit)
	for {
		t0 := time.Now()
		op, err := c.Operation(id, opPollSeconds)
		if err != nil {
			return nil, err
		}
		if op.State != "running" {
			return op, nil
		}
		if time.Now().After(deadline) {
			return op, errOpTimeout
		}
		if time.Since(t0) < time.Second {
			time.Sleep(time.Second) // a daemon that ignores wait= must not be hammered
		}
	}
}

// LoadVerb loads a config onto a slot (slot optional → server default). With wait it follows the load to its
// end: it prints the actual context on success, and on failure the reason and the unit's last log lines, and
// returns an error (so the process exits non-zero). Without wait it starts the load and returns.
func LoadVerb(mode, slot string, wait bool) error {
	c, err := New()
	if err != nil {
		return err
	}
	return loadWith(c, mode, slot, wait, os.Stdout, os.Stderr)
}

func loadWith(c *Client, mode, slot string, wait bool, out, errOut io.Writer) error {
	if slot == "" {
		st, err := c.Status()
		if err != nil {
			return err
		}
		for _, name := range []string{"a1", "a2", "a3", "a4"} {
			if m, ok := st.Slots[name]; !ok || m == nil || *m == "" {
				slot = name
				break
			}
		}
		if slot == "" {
			return fmt.Errorf("no empty slot for %s", mode)
		}
	}
	r, err := c.Load(mode, slot)
	if err != nil {
		return err
	}
	if !r.Success {
		return fmt.Errorf("%s", i18n.T("cli.load.failed", r.Message))
	}
	if r.Operation == nil || r.Operation.ID == "" {
		// A daemon that does not track operations: all we know is that it started.
		fmt.Fprintln(out, i18n.T("cli.load.started", mode, orSlot(slot), "-"))
		return nil
	}
	if !wait {
		fmt.Fprintln(out, i18n.T("cli.load.started", mode, orSlot(slot), r.Operation.ID))
		return nil
	}
	op, err := c.waitOperation(r.Operation.ID, opWaitLimit)
	if errors.Is(err, errOpTimeout) {
		return fmt.Errorf("%s", i18n.T("cli.load.timeout", opWaitLimit.String()))
	}
	if err != nil {
		return err
	}
	if op.State == "failed" {
		if op.Detail != "" {
			fmt.Fprintln(errOut, i18n.T("cli.load.failed_detail"))
			for _, l := range strings.Split(op.Detail, "\n") {
				fmt.Fprintln(errOut, "  "+l)
			}
		}
		return fmt.Errorf("%s", i18n.T("cli.load.failed", op.Message))
	}
	if op.NCtx > 0 {
		fmt.Fprintln(out, i18n.T("cli.load.result", mode, orSlot(slot), op.NCtx))
	} else {
		fmt.Fprintln(out, i18n.T("cli.load.result_unknown_ctx", mode, orSlot(slot)))
	}
	return nil
}

func orSlot(s string) string {
	if s == "" {
		return i18n.T("cli.load.default_slot")
	}
	return s
}

// UnloadVerb empties a slot. Like LoadVerb it follows a single-slot unload to its end unless wait is false;
// "all" is not tracked as an operation by the daemon, so it reports only that it started.
func UnloadVerb(slot string, wait bool) error {
	c, err := New()
	if err != nil {
		return err
	}
	return unloadWith(c, slot, wait, os.Stdout)
}

func unloadWith(c *Client, slot string, wait bool, out io.Writer) error {
	r, err := c.Unload(slot)
	if err != nil {
		return err
	}
	if !r.Success {
		return fmt.Errorf("%s", i18n.T("cli.unload.failed", r.Message))
	}
	if r.Operation == nil || r.Operation.ID == "" {
		fmt.Fprintln(out, i18n.T("cli.unload.started", slot, "-"))
		return nil
	}
	if !wait {
		fmt.Fprintln(out, i18n.T("cli.unload.started", slot, r.Operation.ID))
		return nil
	}
	op, err := c.waitOperation(r.Operation.ID, opWaitLimit)
	if errors.Is(err, errOpTimeout) {
		return fmt.Errorf("%s", i18n.T("cli.load.timeout", opWaitLimit.String()))
	}
	if err != nil {
		return err
	}
	if op.State == "failed" {
		return fmt.Errorf("%s", i18n.T("cli.unload.failed", op.Message))
	}
	fmt.Fprintln(out, i18n.T("cli.unload.result", slot))
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
