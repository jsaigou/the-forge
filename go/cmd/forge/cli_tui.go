package main

import (
	"fmt"

	"github.com/jsaigou/the-forge/internal/cli"
	"github.com/jsaigou/the-forge/internal/tui"
)

// isCLIVerb reports whether args[1] names a client verb (vs a daemon flag).
func isCLIVerb(s string) bool {
	switch s {
	case "tui", "status", "models", "load", "unload", "services", "keys-export":
		return true
	}
	return false
}

// runCLI dispatches the client verbs. All are explicit — nothing here
// runs on a bare `forge`, because that invocation is the daemon.
func runCLI(args []string) error {
	verb := args[1]
	switch verb {
	case "tui":
		c, err := cli.New()
		if err != nil {
			return err
		}
		return tui.Run(c)
	case "status":
		return cli.StatusVerb()
	case "models":
		return cli.ModelsVerb()
	case "load":
		if len(args) < 3 {
			return fmt.Errorf("usage: forge load <mode> [slot]")
		}
		slot := ""
		if len(args) > 3 {
			slot = args[3]
		}
		return cli.LoadVerb(args[2], slot)
	case "unload":
		if len(args) < 3 {
			return fmt.Errorf("usage: forge unload <slot>")
		}
		return cli.UnloadVerb(args[2])
	case "services":
		action, name := "", ""
		if len(args) > 2 {
			action = args[2]
		}
		if len(args) > 3 {
			name = args[3]
		}
		return cli.ServicesVerb(action, name)
	case "keys-export":
		unbound := len(args) > 2 && args[2] == "-unbound"
		return cli.KeyExportVerb(unbound)
	default:
		return fmt.Errorf("unknown client verb: %s", verb)
	}
}
