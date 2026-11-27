// SPDX-License-Identifier: Apache-2.0

package tts

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/speechsetup"
)

func readinessThroughFrontDoor(t *testing.T, daemon http.HandlerFunc, expires time.Time) (Readiness, string) {
	t.Helper()
	d := httptest.NewServer(daemon)
	t.Cleanup(d.Close)
	res, err := NewDaemonResolver(DaemonResolverConfig{BaseURL: d.URL, Key: "sk-forge-test", KeyExpires: expires})
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(nil, "internal-token", t.TempDir())
	srv.SetReadinessReporter(res)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/readiness", nil)) // no token: it is open, like the engine list
	if rec.Code != 200 {
		t.Fatalf("HTTP %d %s", rec.Code, rec.Body)
	}
	var out Readiness
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out, rec.Body.String()
}

func TestReadiness_PassesTheDaemonReportOnWithoutItsDetail(t *testing.T) {
	got, raw := readinessThroughFrontDoor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/speech/readiness" || r.Header.Get("Authorization") != "Bearer sk-forge-test" {
			t.Errorf("asked %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		writeJSON(w, 200, speechsetup.Report{Usable: true, Capabilities: []speechsetup.Capability{
			{ID: speechsetup.CapClone, Mode: "base", State: speechsetup.StateFailed, Engine: "z", Code: speechsetup.CodeFailed, Detail: "/opt/secret/path: no such file"}}})
	}, time.Now().Add(10*24*time.Hour))
	if got.Daemon != DaemonOK || !got.Usable || got.Ready || len(got.Capabilities) != 1 || got.Capabilities[0].Code != speechsetup.CodeFailed {
		t.Errorf("report: %+v", got)
	}
	if strings.Contains(raw, "/opt/secret") {
		t.Errorf("the daemon's detail must not leave the host: %s", raw)
	}
	if !got.KeyWarning || got.KeyExpiresInDays == nil || *got.KeyExpiresInDays > 10 {
		t.Errorf("a key about to expire is said: %+v", got)
	}
}

func TestReadiness_SaysWhyItCannotAsk(t *testing.T) {
	for status, want := range map[int]string{401: DaemonKeyRefused, 403: DaemonUnsupported, 404: DaemonUnsupported, 500: DaemonUnreachable} {
		got, _ := readinessThroughFrontDoor(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }, time.Time{})
		if got.Daemon != want || got.Usable || got.Ready || got.Code == "" || len(got.Capabilities) != 3 || got.Capabilities[0].State != speechsetup.StateUnknown {
			t.Errorf("HTTP %d: %+v", status, got)
		}
		if got.KeyExpiresInDays != nil {
			t.Errorf("an unknown expiry is left out: %+v", got)
		}
	}
}

// The front door names the capabilities itself for the case where it cannot ask; they must be the daemon's.
func TestReadinessCapabilitiesMatchTheDaemons(t *testing.T) {
	want := speechsetup.Evaluate(speechsetup.Input{}).Capabilities
	if len(want) != len(readinessCapabilities) {
		t.Fatalf("daemon %d, front door %d", len(want), len(readinessCapabilities))
	}
	for i, c := range want {
		if readinessCapabilities[i].ID != c.ID || readinessCapabilities[i].Mode != c.Mode {
			t.Errorf("#%d: daemon %s/%s, front door %s/%s", i, c.ID, c.Mode, readinessCapabilities[i].ID, readinessCapabilities[i].Mode)
		}
	}
}

func TestSpeech_NoVoiceAndNoDefaultIsAClearRefusal(t *testing.T) {
	srv := NewServer(NewDualEngine(NewQwenTTS(&fakeBackend{}, NewRegistry(t.TempDir())), nil, "", ""), "", t.TempDir())
	for _, format := range []string{"wav", "pcm"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"input":"hi","response_format":"`+format+`"}`)))
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"code":"speech_no_voice"`) {
			t.Errorf("%s: %d %s", format, rec.Code, rec.Body)
		}
	}
}
