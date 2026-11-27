// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
)

// Challenge, user handle and credential ids are raw bytes; on the wire they are base64url that decodes back to exactly those bytes.
func TestWebAuthnOptionsCarryBase64URLNotRawBytes(t *testing.T) {
	raw := []byte{0xff, 0xfe, 0x80, 0x01, 0xc3, 0x28, 0x00, 0x7f}
	dec := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("%q is not unpadded base64url: %v", s, err)
		}
		return b
	}

	a := &protocol.CredentialAssertion{Response: protocol.PublicKeyCredentialRequestOptions{
		Challenge:          protocol.URLEncodedBase64(raw),
		AllowedCredentials: []protocol.CredentialDescriptor{{Type: protocol.PublicKeyCredentialType, CredentialID: raw}},
	}}
	req := requestOptionsFromProtocol(a)
	if !bytes.Equal(dec(req.Challenge), raw) {
		t.Errorf("assertion challenge does not round-trip: %q", req.Challenge)
	}
	if len(req.AllowCredentials) != 1 || !bytes.Equal(dec(req.AllowCredentials[0].ID), raw) {
		t.Errorf("allowCredentials id does not round-trip: %+v", req.AllowCredentials)
	}

	c := &protocol.CredentialCreation{Response: protocol.PublicKeyCredentialCreationOptions{
		Challenge:             protocol.URLEncodedBase64(raw),
		User:                  protocol.UserEntity{ID: raw, CredentialEntity: protocol.CredentialEntity{Name: "testuser"}},
		CredentialExcludeList: []protocol.CredentialDescriptor{{Type: protocol.PublicKeyCredentialType, CredentialID: raw}},
	}}
	cr := creationOptionsFromProtocol(c)
	if !bytes.Equal(dec(cr.Challenge), raw) || !bytes.Equal(dec(cr.User.ID), raw) {
		t.Errorf("creation challenge/user id do not round-trip: %q %q", cr.Challenge, cr.User.ID)
	}
	if len(cr.ExcludeCredentials) != 1 || !bytes.Equal(dec(cr.ExcludeCredentials[0].ID), raw) {
		t.Errorf("excludeCredentials id does not round-trip: %+v", cr.ExcludeCredentials)
	}
	b, _ := json.Marshal(req)
	if bytes.Contains(b, []byte("\ufffd")) || bytes.Contains(b, []byte("\u0000")) {
		t.Errorf("wire JSON contains replacement or NUL characters: %s", b)
	}
}

// An option field of a type the helper does not know must come out empty (loud failure in the browser), never as a guessed encoding.
func TestB64urlBytesDoesNotGuessUnknownTypes(t *testing.T) {
	for _, v := range []any{"already-encoded", 42, struct{ X int }{1}, []int{1, 2}} {
		if got := b64urlBytes(v); got != "" {
			t.Errorf("b64urlBytes(%T) = %q, want empty", v, got)
		}
	}
	if b64urlBytes(nil) != "" || b64urlBytes([]byte{}) != "" {
		t.Error("nil and empty input encode to empty")
	}
	if got := b64urlBytes([]byte("hi")); got != "aGk" {
		t.Errorf("b64urlBytes(hi) = %q, want aGk", got)
	}
}
