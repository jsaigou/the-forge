// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/i18n"
	"github.com/jsaigou/the-forge/internal/speechsetup"
	"github.com/jsaigou/the-forge/internal/store"
)

// Every state, action and front door state the report can carry has wording: a missing translation prints its key.
func TestSpeechStatusLines_NeverPrintAKey(t *testing.T) {
	states := []string{speechsetup.StateReady, speechsetup.StateStarting, speechsetup.StateNotRegistered, speechsetup.StateNoSlot,
		speechsetup.StateDisabled, speechsetup.StateStopped, speechsetup.StateFailed, speechsetup.StateUnknown}
	actions := []string{speechsetup.ActionRegister, speechsetup.ActionAddSlot, speechsetup.ActionEnable, speechsetup.ActionStart,
		speechsetup.ActionCheckLog, speechsetup.ActionWait}
	r := &SpeechReadiness{Report: speechsetup.Evaluate(speechsetup.Input{Engines: []store.Occupant{}})}
	for _, s := range states {
		r.Capabilities = append(r.Capabilities, speechsetup.Capability{ID: speechsetup.CapClone, State: s})
	}
	for _, a := range actions {
		r.Capabilities = append(r.Capabilities, speechsetup.Capability{ID: speechsetup.CapDesign, State: speechsetup.StateFailed, Action: a})
	}
	for _, fd := range []string{"ok", "unreachable", "not_configured"} {
		r.FrontDoor = &struct {
			State string `json:"state"`
			Code  string `json:"code,omitempty"`
		}{State: fd}
		for _, flags := range [][2]bool{{false, false}, {false, true}, {true, true}} {
			r.Ready, r.Usable = flags[0], flags[1]
			if out := strings.Join(speechStatusLines(r), "\n"); strings.Contains(out, "cli.speech.") {
				t.Fatalf("untranslated key in:\n%s", out)
			}
		}
	}
}

// Every state, action and file problem the server can report has words in both languages: a missing one would print its key.
func TestSpeechMessagesExistInBothLanguages(t *testing.T) {
	keys := []string{"cli.speech.usage", "cli.speech.engines_under", "cli.speech.recipe_size", "cli.speech.recipe_more", "cli.speech.registered",
		"cli.speech.registered_already", "cli.speech.slot_added", "cli.speech.default_set", "cli.speech.engine_off", "cli.speech.restart_required",
		"cli.speech.policy_" + store.PolicyAlwaysOn, "cli.speech.policy_" + store.PolicyOnDemand}
	for _, s := range []string{speechsetup.StateReady, speechsetup.StateStarting, speechsetup.StateNotInstalled, speechsetup.StateFilesMissing,
		speechsetup.StateNotRegistered, speechsetup.StateNoSlot, speechsetup.StateDisabled, speechsetup.StateStopped, speechsetup.StateFailed, speechsetup.StateUnknown} {
		keys = append(keys, "cli.speech.state_"+s)
	}
	for _, a := range []string{speechsetup.ActionInstall, speechsetup.ActionRestore, speechsetup.ActionRegister, speechsetup.ActionAddSlot,
		speechsetup.ActionEnable, speechsetup.ActionStart, speechsetup.ActionCheckLog, speechsetup.ActionWait} {
		keys = append(keys, "cli.speech.action_"+a)
	}
	for _, s := range []string{speechsetup.RecipeRegistered, speechsetup.RecipePresent, speechsetup.RecipeAbsent} {
		keys = append(keys, "cli.speech.recipe_"+s)
	}
	for _, p := range []string{speechsetup.ProblemMissing, speechsetup.ProblemNotAFile, speechsetup.ProblemNotExecutable, speechsetup.ProblemWrongSize, speechsetup.ProblemUnreadable} {
		keys = append(keys, "cli.speech.problem_"+p)
	}
	for _, k := range keys {
		for _, lang := range []string{"en", "ja"} {
			if got := i18n.TLang(lang, k); got == k || (lang == "ja" && got == i18n.TLang("en", k)) {
				t.Errorf("%s has no %s text", k, lang)
			}
		}
	}
}

func TestSpeechRecipeLinesKeepALongListShort(t *testing.T) {
	lines := speechRecipeLines(SpeechRecipes{Root: "/opt/x", Recipes: []speechsetup.RecipeStatus{{ID: "zonos", State: speechsetup.RecipeAbsent,
		DownloadBytes: 6_700_000_000, Problems: []speechsetup.FileProblem{{Path: "/a", Problem: "missing"}, {Path: "/b", Problem: "missing"},
			{Path: "/c", Problem: "missing"}, {Path: "/d", Problem: "missing"}, {Path: "/e", Problem: "missing"}}}}})
	text := strings.Join(lines, "\n")
	if len(lines) != 6 || !strings.Contains(text, "6.7") || strings.Contains(text, "/d") || strings.Contains(text, "%!") {
		t.Fatalf("lines:\n%s", text)
	}
}
