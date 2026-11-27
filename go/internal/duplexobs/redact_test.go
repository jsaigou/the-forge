// SPDX-License-Identifier: Apache-2.0

package duplexobs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
)

func TestRedactorScrubsTokenForms(t *testing.T) {
	r := NewRedactor()
	tok := "sk-" + "router-" + strings.Repeat("a", 12)
	cases := []string{
		"Authorization: Bearer " + tok,
		"client sent bearer." + tok,
		"token=" + tok,
		"api_key: " + tok,
		tok,
	}
	for _, in := range cases {
		out, n := r.Redact(in)
		if n == 0 {
			t.Errorf("%q was not redacted", in)
		}
		if strings.Contains(out, tok) {
			t.Errorf("%q leaked the secret: %q", in, out)
		}
	}

	out, n := r.Redact("client_done")
	if out != "client_done" || n != 0 {
		t.Errorf("benign reason changed: %q n=%d", out, n)
	}
}

func TestSummaryAndLogsNeverCarryTokensOrReferenceAudio(t *testing.T) {
	const audioMarker = "REFERENCE-SECRET-AUDIO-MARKER"
	token := "sk-" + "router-" + strings.Repeat("b", 12)
	now := time.Unix(1_700_000_000, 0)
	o := New(duplex.SessionInfo{SessionID: "dup_01JTEST", Tenant: "loopback"}, Config{
		Now: func() time.Time { return now },
	})
	ref := duplex.Reference{ID: "sana", Audio: []byte(audioMarker)}
	o.SetReference(ref)
	o.Drop("input token=" + token)
	o.Observe(duplex.Event{Kind: duplex.EventDone, Reason: "client sent bearer." + token})

	s := o.Close("cancel: Authorization: Bearer " + token)
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	logs := s.LogLine()
	referenceFields := LogLine(ReferenceLogFields(ref))

	for _, got := range []string{string(raw), logs, referenceFields} {
		if strings.Contains(got, token) {
			t.Errorf("token leaked: %s", got)
		}
		if strings.Contains(got, audioMarker) {
			t.Errorf("reference audio leaked: %s", got)
		}
	}

	if s.ReferenceBytes != len(audioMarker) || !s.HasReference {
		t.Errorf("reference metadata = bytes %d present %v", s.ReferenceBytes, s.HasReference)
	}
	if !strings.Contains(referenceFields, "reference_bytes=") ||
		!strings.Contains(referenceFields, "reference_audio=withheld") {
		t.Errorf("reference fields = %q", referenceFields)
	}
	if !s.Redaction.Enabled || !s.Redaction.ReferenceAudioWithheld {
		t.Errorf("redaction status = %+v", s.Redaction)
	}
	if s.Redaction.PatternsRedacted < 3 {
		t.Errorf("redacted patterns = %d, want the drop, done and close reasons counted",
			s.Redaction.PatternsRedacted)
	}
	if strings.Contains(s.CloseReason, token) {
		t.Errorf("close reason not redacted: %+v", s)
	}
}

func TestReferenceLogFieldsKinds(t *testing.T) {
	cases := []struct {
		ref  duplex.Reference
		kind string
	}{
		{duplex.Reference{}, "none"},
		{duplex.Reference{ID: "x"}, "named"},
		{duplex.Reference{Audio: []byte{1, 2}}, "audio"},
		{duplex.Reference{ID: "x", Audio: []byte{1}}, "named+audio"},
	}
	for _, c := range cases {
		fields := ReferenceLogFields(c.ref)
		if fields[0].Value != c.kind {
			t.Errorf("%+v kind = %q, want %q", c.ref, fields[0].Value, c.kind)
		}
	}
}
