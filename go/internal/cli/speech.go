// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/i18n"
	"github.com/jsaigou/the-forge/internal/speechsetup"
)

// SpeechReadiness is the answer of GET /api/v1/speech/readiness.
type SpeechReadiness struct {
	speechsetup.Report
	FrontDoor *struct {
		State string `json:"state"`
		Code  string `json:"code,omitempty"`
	} `json:"front_door,omitempty"`
}

// SpeechReadiness reports what the node can do with speech.
func (c *Client) SpeechReadiness() (*SpeechReadiness, error) {
	var r SpeechReadiness
	if err := c.GetJSON("/api/v1/speech/readiness", &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// SpeechStatusVerb prints the speech readiness report (forge speech status [--json]).
func SpeechStatusVerb(c *Client, asJSON bool) error {
	r, err := c.SpeechReadiness()
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	for _, line := range speechStatusLines(r) {
		fmt.Println(line)
	}
	var tools struct {
		Tools []config.ExternalTool `json:"tools"`
	}
	if err := c.GetJSON("/api/v1/external-tools", &tools); err != nil {
		fmt.Fprintln(os.Stderr, i18n.T("cli.speech.studio.unread", err.Error()))
		return nil
	}
	for _, t := range tools.Tools {
		if config.IsVoiceStudio(t) {
			fmt.Println(i18n.PadRight(i18n.T("cli.speech.studio.line"), 18) + " " + t.URL)
		}
	}
	return nil
}

func speechStatusLines(r *SpeechReadiness) []string {
	summary := "cli.speech.summary_none"
	switch {
	case r.Ready:
		summary = "cli.speech.summary_ready"
	case r.Usable:
		summary = "cli.speech.summary_partial"
	}
	lines := []string{i18n.T(summary)}
	for _, c := range r.Capabilities {
		line := i18n.PadRight(i18n.T("cli.speech.cap_"+c.ID), 18) + " " + i18n.PadRight(i18n.T("cli.speech.state_"+c.State), 18)
		if c.Engine != "" {
			line += " " + c.Engine
		}
		lines = append(lines, line)
		if c.Action != "" {
			lines = append(lines, "    "+i18n.T("cli.speech.action_"+c.Action))
		}
		if c.Detail != "" {
			lines = append(lines, "    "+c.Detail)
		}
	}
	if r.FrontDoor != nil {
		lines = append(lines, i18n.PadRight(i18n.T("cli.speech.front_door"), 18)+" "+i18n.T("cli.speech.front_door_"+r.FrontDoor.State))
	}
	return lines
}

// SpeechRecipes is the answer of GET /api/v1/speech/recipes.
type SpeechRecipes struct {
	Root    string                     `json:"root"`
	Recipes []speechsetup.RecipeStatus `json:"recipes"`
}

// SpeechRecipesVerb lists the engines the server can set up and what it has of each (forge speech recipes [--json]).
func SpeechRecipesVerb(c *Client, asJSON bool) error {
	var r SpeechRecipes
	if err := c.GetJSON("/api/v1/speech/recipes", &r); err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	for _, line := range speechRecipeLines(r) {
		fmt.Println(line)
	}
	return nil
}

// maxListedProblems keeps the listing short: a missing install lacks every file.
const maxListedProblems = 3

func speechRecipeLines(r SpeechRecipes) []string {
	lines := []string{i18n.T("cli.speech.engines_under", r.Root)}
	for _, s := range r.Recipes {
		line := i18n.PadRight(s.ID, 18) + " " + i18n.PadRight(i18n.T("cli.speech.recipe_"+s.State), 20)
		if s.Engine != "" {
			line += " " + s.Engine
		} else {
			line += " " + i18n.T("cli.speech.recipe_size", fmt.Sprintf("%.1f", float64(s.DownloadBytes)/1e9))
		}
		lines = append(lines, line)
		for i, p := range s.Problems {
			if i == maxListedProblems {
				lines = append(lines, "    "+i18n.T("cli.speech.recipe_more", len(s.Problems)-i))
				break
			}
			lines = append(lines, "    "+p.Path+": "+i18n.T("cli.speech.problem_"+p.Problem))
		}
		if s.InstallCommand != "" {
			lines = append(lines, "    "+i18n.T("cli.speech.recipe_install", s.InstallCommand))
		}
	}
	return lines
}

// SpeechRegisterVerb registers an engine whose files are on the server (forge speech register <id>). The client needs the speech-setup scope or an admin key.
func SpeechRegisterVerb(c *Client, id string) error {
	var r struct {
		speechsetup.Result
		RestartRequired bool `json:"restart_required"`
	}
	if err := c.PostJSON("/api/v1/speech/recipes/"+url.PathEscape(id)+"/register", nil, &r); err != nil {
		return err
	}
	head := "cli.speech.registered_already"
	if r.Created {
		head = "cli.speech.registered"
	}
	fmt.Println(i18n.T(head, r.Engine, i18n.T("cli.speech.policy_"+r.Policy)))
	if r.SlotAdded != "" {
		fmt.Println("  " + i18n.T("cli.speech.slot_added", r.SlotAdded))
	}
	if r.DefaultSet {
		fmt.Println("  " + i18n.T("cli.speech.default_set"))
	}
	if !r.Enabled {
		fmt.Println("  " + i18n.T("cli.speech.engine_off"))
	}
	if r.RestartRequired {
		fmt.Println("  " + i18n.T("cli.speech.restart_required"))
	}
	return nil
}
