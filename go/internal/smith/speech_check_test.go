// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/speechsetup"
	"github.com/jsaigou/the-forge/internal/store"
)

func TestSpeechReadinessCheck(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	report := speechsetup.Report{Capabilities: []speechsetup.Capability{
		{ID: "preset", State: speechsetup.StateNotInstalled}, {ID: "clone", State: speechsetup.StateNotRegistered}, {ID: "design", State: speechsetup.StateDisabled, Engine: "d"},
	}}
	env := &CheckEnv{Store: db, SpeechReport: func(context.Context) (speechsetup.Report, error) { return report, nil }}

	// Nobody set speech up (an engine that is turned off does not count): nothing to say.
	if f := runSpeechReadiness(ctx, env); f.Severity != SeverityOK || f.SummaryKey != "checks.speech_readiness.unused" {
		t.Fatalf("unused: %+v", f)
	}

	// A working engine and no address for the speech service.
	report.Capabilities[0] = speechsetup.Capability{ID: "preset", State: speechsetup.StateReady, Usable: true, Engine: "kokoro"}
	if f := runSpeechReadiness(ctx, env); f.Severity != SeverityWarn || f.SummaryKey != "checks.speech_readiness.front_door_unset" || f.Params["more"] != "" {
		t.Fatalf("no address: %+v", f)
	}
	door := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(door.Close)
	if err := db.Settings().Set(ctx, "infra.router", []byte(`{"tts_url":"`+door.URL+`/v1"}`)); err != nil {
		t.Fatal(err)
	}
	// A kind nobody added is not a complaint.
	if f := runSpeechReadiness(ctx, env); f.Severity != SeverityOK || f.SummaryKey != "checks.speech_readiness.ok" {
		t.Fatalf("working: %+v", f)
	}

	// The key: quiet well before it expires, a warning in its last 30 days.
	a := authz.New(db)
	mint := func(name string, ttl time.Duration) {
		if _, err := a.MintScopedKey(ctx, name, "", authz.RoleOperator, "127.0.0.1", time.Now().Add(ttl), authz.ScopeSpeech); err != nil {
			t.Fatal(err)
		}
	}
	mint("speech", 10*24*time.Hour)
	f := runSpeechReadiness(ctx, env)
	if f.SummaryKey != "checks.speech_readiness.key_expiring" || f.Params["days"] != 9 && f.Params["days"] != 10 {
		t.Fatalf("expiring: %+v", f)
	}
	mint("speech-new", 300*24*time.Hour) // rotated under another name: the newest expiry counts
	if f := runSpeechReadiness(ctx, env); f.Severity != SeverityOK {
		t.Fatalf("rotated: %+v", f)
	}

	// What was set up and broke, with the others counted.
	report.Capabilities[0] = speechsetup.Capability{ID: "preset", State: speechsetup.StateFilesMissing, Engine: "kokoro"}
	report.Capabilities[1] = speechsetup.Capability{ID: "clone", State: speechsetup.StateFailed, Engine: "z"}
	door.Close()
	f = runSpeechReadiness(ctx, env)
	if f.SummaryKey != "checks.speech_readiness.files_missing" || f.Params["engine"] != "kokoro" || f.Params["more"] != " (+2)" || !strings.Contains(f.Summary, "(+2)") {
		t.Fatalf("broken: %+v", f)
	}

	// The speech service has a key but there is no engine at all.
	report.Capabilities = []speechsetup.Capability{{ID: "preset", State: speechsetup.StateNotInstalled}}
	if f := runSpeechReadiness(ctx, env); f.SummaryKey != "checks.speech_readiness.no_engine" {
		t.Fatalf("no engine: %+v", f)
	}
	if f := runSpeechReadiness(ctx, &CheckEnv{Store: db}); f.Severity == SeverityWarn || f.Severity == SeverityCrit {
		t.Fatalf("no pool: %+v", f)
	}
}
