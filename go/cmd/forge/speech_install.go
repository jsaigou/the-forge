// SPDX-License-Identifier: Apache-2.0

package main

// `forge speech install <id>` and `forge speech key rotate` (docs/speech-setup-plan.md sections 5.1 and 5.2). Both run as the daemon's service user.
// Neither ever prints a key or a token.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/cli"
	"github.com/jsaigou/the-forge/internal/i18n"
	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/speechsetup"
	"github.com/jsaigou/the-forge/internal/store"
)

const (
	speechKeyName = "forge-tts-speech"
	// speechEnvName is the env file forge-tts.service reads, under the engine root.
	speechEnvName = "tts_voices/.internal_token_env"
)

func runSpeechInstall(args []string) error {
	usage := errors.New(i18n.T("cli.speech.usage"))
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return usage
	}
	id := args[0]
	fl := flag.NewFlagSet("forge speech install", flag.ContinueOnError)
	var (
		stage   = fl.String("stage", "", "run one stage only: "+strings.Join(speechsetup.Stages, ", "))
		dryRun  = fl.Bool("dry-run", false, "say what each stage would do")
		replace = fl.Bool("replace", false, "make the engine again, keeping the existing directory beside it")
		root    = fl.String("root", specialist.EngineRoot, "engine root; another root installs without registering")
		jobs    = fl.Int("jobs", 0, "compile jobs (0 = half the processors)")
		dbPath  = fl.String("db", "/var/lib/forge/forge.db", i18n.T("forge.cli.flag_db_path"))
	)
	if err := fl.Parse(args[1:]); err != nil {
		return err
	}
	if fl.NArg() != 0 || !path.IsAbs(*root) {
		return usage
	}
	r, ok := speechsetup.RecipeByID(id)
	if !ok {
		return errors.New(i18n.T("cli.speech.install.unknown", id))
	}
	if os.Geteuid() == 0 && !*dryRun {
		return errors.New(i18n.T("cli.speech.install.root_refused", id))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	in := &speechsetup.Installer{
		Root: path.Clean(*root), Jobs: *jobs, Replace: *replace, DryRun: *dryRun,
		Say: func(key string, values ...any) { fmt.Println(i18n.T(key, values...)) },
	}
	out, err := in.Install(ctx, r, *stage)
	if err != nil {
		return err
	}
	if *dryRun || (!out.Adopted && !out.Activated) {
		return nil
	}
	if in.Root != specialist.EngineRoot {
		fmt.Println(i18n.T("cli.speech.install.no_register", in.Root))
		return nil
	}
	client, revoke, err := scopedClient(*dbPath, authz.ScopeSpeechSetup, "speech-setup")
	if err == nil {
		defer revoke()
		err = cli.SpeechRegisterVerb(client, id)
	}
	if err != nil {
		return errors.New(i18n.T("cli.speech.install.register_failed", err.Error(), id))
	}
	return nil
}

func runSpeechKey(args []string) error {
	if len(args) < 1 || args[0] != "rotate" {
		return errors.New(i18n.T("cli.speech.usage"))
	}
	fl := flag.NewFlagSet("forge speech key rotate", flag.ContinueOnError)
	var (
		dbPath = fl.String("db", "/var/lib/forge/forge.db", i18n.T("forge.cli.flag_db_path"))
		env    = fl.String("env", path.Join(specialist.EngineRoot, speechEnvName), "the env file forge-tts reads")
		studio = fl.String("voice-studio-env", "", "also give a Voice Studio env file the speech service's token")
		keep   = fl.Bool("if-needed", false, "keep the current key while it works and has more than 30 days left (the installer uses this)")
	)
	if err := fl.Parse(args[1:]); err != nil {
		return err
	}
	if fl.NArg() != 0 {
		return errors.New(i18n.T("cli.speech.usage"))
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }() // the key is committed when MintScopedKey returns
	expires, rotated, err := rotateSpeechKey(context.Background(), authz.New(db), *env, *studio, *keep, time.Now())
	if err != nil {
		return err
	}
	if rotated {
		fmt.Println(i18n.T("cli.speech.key.written", expires.Format(time.DateOnly), *env))
	} else {
		fmt.Println(i18n.T("cli.speech.key.kept", expires.Format(time.DateOnly)))
	}
	if *studio != "" {
		fmt.Println(i18n.T("cli.speech.key.voice_studio", *studio))
	}
	if rotated {
		fmt.Println(i18n.T("cli.speech.key.restart"))
	}
	return nil
}

// speechKeyRenewal is how long before a key expires that "if needed" replaces it (the same notice the readiness report gives).
const speechKeyRenewal = 30 * 24 * time.Hour

// currentSpeechKey reports until when the env file's key can stay: it must verify as a speech key from this host and not be close to expiring.
func currentSpeechKey(ctx context.Context, auth *authz.Authorizer, current map[string]string, now time.Time) (time.Time, bool, error) {
	key := current["FORGE_TTS_DAEMON_KEY"]
	expires, perr := time.Parse(time.DateOnly, current["FORGE_TTS_DAEMON_KEY_EXPIRES"])
	if key == "" || perr != nil || expires.Sub(now) < speechKeyRenewal {
		return time.Time{}, false, nil
	}
	id, err := auth.VerifyBearerFrom(ctx, "127.0.0.1", key, authz.KindForge)
	if errors.Is(err, authz.ErrUnauthenticated) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("check the current speech key: %w", err)
	}
	return expires, id.Scope == authz.ScopeSpeech, nil
}

// rotateSpeechKey gives forge-tts a new daemon key (revoking the previous one) and keeps its internal token, making one the first time. With keep,
// a key that still works stays. Every file it writes is readable by its owner alone, and the files are checked for writing before the old key is
// revoked.
func rotateSpeechKey(ctx context.Context, auth *authz.Authorizer, envFile, studioEnv string, keep bool, now time.Time) (time.Time, bool, error) {
	none := time.Time{}
	current, err := readEnvFile(envFile)
	if err != nil {
		return none, false, err
	}
	token := current["FORGE_TTS_INTERNAL_TOKEN"]
	if token == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return none, false, err
		}
		token = hex.EncodeToString(raw)
	}
	targets := []string{envFile}
	if studioEnv != "" {
		targets = append(targets, studioEnv)
	}
	for _, f := range targets {
		probe, err := os.CreateTemp(path.Dir(f), ".write-check-*")
		if err != nil {
			return none, false, fmt.Errorf("cannot write %s, so the key was not changed: %w", f, err)
		}
		if err := errors.Join(probe.Close(), os.Remove(probe.Name())); err != nil {
			return none, false, err
		}
	}
	if keep && current["FORGE_TTS_INTERNAL_TOKEN"] != "" {
		expires, ok, err := currentSpeechKey(ctx, auth, current, now)
		if err != nil {
			return none, false, err
		}
		if ok {
			if studioEnv != "" {
				if err := updateEnvFile(studioEnv, map[string]string{"VOICE_STUDIO_NODE_TOKEN": token}); err != nil {
					return none, false, err
				}
			}
			return expires, false, nil
		}
	}
	expires := now.Add(authz.MaxSpeechKeyTTL - time.Hour)
	key, err := auth.MintScopedKey(ctx, speechKeyName, "", authz.RoleOperator, "127.0.0.1", expires, authz.ScopeSpeech)
	if err != nil {
		return none, false, err
	}
	if err := updateEnvFile(envFile, map[string]string{
		"FORGE_TTS_DAEMON_KEY": key, "FORGE_TTS_DAEMON_KEY_EXPIRES": expires.Format(time.DateOnly), "FORGE_TTS_INTERNAL_TOKEN": token,
	}); err != nil {
		return none, false, err
	}
	if studioEnv != "" {
		if err := updateEnvFile(studioEnv, map[string]string{"VOICE_STUDIO_NODE_TOKEN": token}); err != nil {
			return none, false, err
		}
	}
	return expires, true, nil
}

// readEnvFile reads KEY=VALUE lines; a file that does not exist is empty.
func readEnvFile(p string) (map[string]string, error) {
	out := map[string]string{}
	f, err := os.Open(p) //nolint:gosec // G304: the env file the administrator named
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read only
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok && !strings.HasPrefix(k, "#") {
			out[k] = v
		}
	}
	return out, sc.Err()
}

// updateEnvFile sets keys in an env file, keeping every other line, and leaves it mode 0600. The replacement is atomic.
func updateEnvFile(p string, set map[string]string) error {
	raw, err := os.ReadFile(p) //nolint:gosec // G304: the env file the administrator named
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var lines []string
	if s := strings.TrimRight(string(raw), "\n"); s != "" {
		lines = strings.Split(s, "\n")
	}
	done := map[string]bool{}
	out := make([]string, 0, len(lines)+len(set))
	for _, l := range lines {
		k, _, ok := strings.Cut(strings.TrimSpace(l), "=")
		if v, replace := set[k]; ok && replace {
			if !done[k] {
				out = append(out, k+"="+v)
				done[k] = true
			}
			continue
		}
		out = append(out, l)
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		if !done[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+set[k])
	}
	tmp, err := os.CreateTemp(path.Dir(p), ".env-*") // created mode 0600
	if err != nil {
		return err
	}
	_, werr := tmp.WriteString(strings.Join(out, "\n") + "\n")
	if err := errors.Join(werr, tmp.Close()); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return os.Rename(tmp.Name(), p)
}
