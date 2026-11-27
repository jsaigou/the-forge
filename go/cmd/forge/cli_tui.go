package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/cli"
	"github.com/jsaigou/the-forge/internal/i18n"
	"github.com/jsaigou/the-forge/internal/tui"
)

// isCLIVerb reports whether args[1] names a client verb (vs a daemon flag).
func isCLIVerb(s string) bool {
	switch s {
	case "tui", "status", "models", "load", "unload", "services", "keys-export", "speech":
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
		rest, wait := splitNoWait(args[2:])
		if len(rest) < 1 {
			return fmt.Errorf("usage: forge load <mode> [slot] [--no-wait]")
		}
		slot := ""
		if len(rest) > 1 {
			slot = rest[1]
		}
		return cli.LoadVerb(rest[0], slot, wait)
	case "unload":
		rest, wait := splitNoWait(args[2:])
		if len(rest) < 1 {
			return fmt.Errorf("usage: forge unload <slot> [--no-wait]")
		}
		return cli.UnloadVerb(rest[0], wait)
	case "services":
		action, name := "", ""
		if len(args) > 2 {
			action = args[2]
		}
		if len(args) > 3 {
			name = args[3]
		}
		return cli.ServicesVerb(action, name)
	case "speech":
		return runSpeechCLI(args[2:])
	case "keys-export":
		unbound := len(args) > 2 && args[2] == "-unbound"
		return cli.KeyExportVerb(unbound)
	default:
		return fmt.Errorf("unknown client verb: %s", verb)
	}
}

// runSpeechCLI is `forge speech status | recipes | register | install | key`.
func runSpeechCLI(args []string) error {
	usage := errors.New(i18n.T("cli.speech.usage"))
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "status", "recipes":
		asJSON := len(args) == 2 && args[1] == "--json"
		if len(args) > 1 && !asJSON {
			return usage
		}
		// The operator's CLI key if there is one; on the server itself (the installer) a short-lived key minted from the database.
		client, err := cli.New()
		revoke := func() {}
		if err != nil {
			var mintErr error
			if client, revoke, mintErr = scopedClient("/var/lib/forge/forge.db", authz.ScopeSpeechSetup, "speech-status"); mintErr != nil {
				return errors.Join(err, mintErr)
			}
		}
		defer revoke()
		if args[0] == "recipes" {
			return cli.SpeechRecipesVerb(client, asJSON)
		}
		return cli.SpeechStatusVerb(client, asJSON)
	case "register":
		fs := flag.NewFlagSet("forge speech register", flag.ContinueOnError)
		dbPath := fs.String("db", "/var/lib/forge/forge.db", i18n.T("forge.cli.flag_db_path"))
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return usage
		}
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return usage
		}
		client, revoke, err := scopedClient(*dbPath, authz.ScopeSpeechSetup, "speech-setup")
		if err != nil {
			return err
		}
		defer revoke()
		return cli.SpeechRegisterVerb(client, args[1])
	case "install":
		return runSpeechInstall(args[1:])
	case "key":
		return runSpeechKey(args[1:])
	case "studio-link":
		return runStudioLink(args[1:])
	}
	return usage
}

// splitNoWait removes --no-wait from a verb's arguments. load and unload follow their operation to the end by
// default, so a failed load exits non-zero with the reason; --no-wait restores "start it and return".
func splitNoWait(args []string) (rest []string, wait bool) {
	wait = true
	for _, a := range args {
		if a == "--no-wait" {
			wait = false
			continue
		}
		rest = append(rest, a)
	}
	return rest, wait
}
