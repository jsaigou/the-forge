// SPDX-License-Identifier: Apache-2.0

package smith

// speech_readiness (docs/speech-setup-plan.md, step 2): when someone has set speech up, is it still able to speak? The check stays quiet on a host
// that never set it up, and about a kind of voice nobody added or somebody turned off: it reports what was meant to work and does not.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/speechsetup"
	"github.com/jsaigou/the-forge/internal/store"
)

// speechKeyWarning is how long before the speech key expires the check starts saying so.
const speechKeyWarning = 30 * 24 * time.Hour

const speechProbeTimeout = 2 * time.Second

type speechIssue struct {
	key     string // the summary is checks.speech_readiness.<key>
	summary string
	params  map[string]any
}

// speechKey reports the speech front door's key: whether one was ever issued and not revoked, and the latest expiry among those (the one in use after
// a rotation under a new name).
func speechKey(ctx context.Context, db *store.DB) (exists bool, expires time.Time, err error) {
	keys, err := db.Keys().List(ctx, string(authz.KindForge))
	if err != nil {
		return false, time.Time{}, err
	}
	for _, k := range keys {
		if k.Scope != authz.ScopeSpeech || !k.RevokedAt.IsZero() {
			continue
		}
		exists = true
		if k.ExpiresAt.After(expires) {
			expires = k.ExpiresAt
		}
	}
	return exists, expires, nil
}

// speechFrontDoorURL is router.tts_url, "" when it is not set.
func speechFrontDoorURL(ctx context.Context, db *store.DB) (string, error) {
	raw, err := db.Settings().Get(ctx, "infra.router")
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var v struct {
		TTSURL string `json:"tts_url"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("infra.router: %w", err)
	}
	return strings.TrimRight(v.TTSURL, "/"), nil
}

func speechFrontDoorAnswers(ctx context.Context, client *http.Client, base string) bool {
	ctx, cancel := context.WithTimeout(ctx, speechProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/v1")+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }() // only the status matters: a close error changes nothing
	return resp.StatusCode/100 == 2
}

func runSpeechReadiness(ctx context.Context, env *CheckEnv) Finding {
	const id = "speech_readiness"
	if env.SpeechReport == nil || env.Store == nil {
		return skipFinding(id, "speech engine pool not running")
	}
	report, err := env.SpeechReport(ctx)
	if err != nil {
		return skipFinding(id, "speech readiness unreadable: "+err.Error())
	}
	hasKey, expires, err := speechKey(ctx, env.Store)
	if err != nil {
		return skipFinding(id, "keys unreadable: "+err.Error())
	}
	// An engine that is registered and not turned off is one somebody wants. A stopped one counts: stopping it was a choice, removing the speech
	// service or its key would be too.
	engines := 0
	for _, c := range report.Capabilities {
		if c.Engine != "" && c.State != speechsetup.StateDisabled {
			engines++
		}
	}
	if !hasKey && engines == 0 {
		return Finding{CheckID: id, Severity: SeverityOK, Summary: "speech is not set up on this host", SummaryKey: "checks.speech_readiness.unused",
			Evidence: map[string]any{"wanted": false}}
	}

	var issues []speechIssue
	add := func(key, summary string, params map[string]any) {
		issues = append(issues, speechIssue{key: key, summary: summary, params: params})
	}
	for _, c := range report.Capabilities {
		switch c.State {
		case speechsetup.StateFilesMissing:
			add("files_missing", fmt.Sprintf("speech engine %q has lost files it needs", c.Engine), map[string]any{"engine": c.Engine})
		case speechsetup.StateFailed:
			add("failed", fmt.Sprintf("speech engine %q failed to start", c.Engine), map[string]any{"engine": c.Engine})
		case speechsetup.StateNoSlot:
			add("no_slot", fmt.Sprintf("speech engine %q has no dedicated slot to run in", c.Engine), map[string]any{"engine": c.Engine})
		}
	}
	if engines == 0 {
		add("no_engine", "the speech service is set up but no speech engine is", map[string]any{})
	}
	base, err := speechFrontDoorURL(ctx, env.Store)
	if err != nil {
		return skipFinding(id, "router settings unreadable: "+err.Error())
	}
	client := env.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	switch {
	case base == "":
		add("front_door_unset", "the speech service's address is not set (router tts_url)", map[string]any{})
	case !speechFrontDoorAnswers(ctx, client, base):
		add("front_door_down", "the speech service (forge-tts) is not answering", map[string]any{})
	}
	now := time.Now()
	if env.Now != nil {
		now = env.Now()
	}
	if hasKey && !expires.IsZero() {
		switch left := expires.Sub(now); {
		case left <= 0:
			add("key_expired", "the speech service's key has expired", map[string]any{})
		case left < speechKeyWarning:
			days := int(left.Hours() / 24)
			add("key_expiring", fmt.Sprintf("the speech service's key expires in %d day(s)", days), map[string]any{"days": days})
		}
	}

	evidence := map[string]any{"wanted": true, "capabilities": report.Capabilities, "front_door": base}
	if len(issues) == 0 {
		return Finding{CheckID: id, Severity: SeverityOK, Summary: "speech works as set up", SummaryKey: "checks.speech_readiness.ok", Evidence: evidence}
	}
	first := issues[0]
	summary, more := first.summary, ""
	if len(issues) > 1 {
		more = fmt.Sprintf(" (+%d)", len(issues)-1) // the same in every language
		summary += more
		all := make([]string, 0, len(issues))
		for _, i := range issues {
			all = append(all, i.summary)
		}
		evidence["issues"] = all
	}
	first.params["more"] = more
	return Finding{CheckID: id, Severity: SeverityWarn, Summary: summary, SummaryKey: "checks.speech_readiness." + first.key, Params: first.params, Evidence: evidence}
}
