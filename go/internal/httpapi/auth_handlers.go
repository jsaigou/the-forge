// SPDX-License-Identifier: Apache-2.0

package httpapi

// auth_handlers.go — Access Policy & Auth v2 endpoints
// (docs/v5-sprint0-auth-design.md §6). Sprint 0-AUTH Phase A implements:
// step-up, TOTP, identity linking, API-key management, policy + config.
// Phase B implements: WebAuthn registration + assertion + credential CRUD.
// Phase C implements: recovery codes + forward_auth_header provider support.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/store"
)

// ── Session / step-up ───────────────────────────────────────────────────────

// handleAuthStepUp — POST /api/v1/auth/step-up.
// Frozen shape: stepUpRequest{ factor, password?, code? } →
// stepUpResponse{ assurance, assurance_at }.
//
// Verifies the factor (password or TOTP), then elevates the current session:
// rotates the session ID + CSRF token and sets the assurance level + timestamp
// (§3.5). Bearer-key identities cannot step up (they have no session).
func (s *Server) handleAuthStepUp(w http.ResponseWriter, r *http.Request) {
	ident := identity(r)
	if ident.KeyID != "" {
		writeError(w, http.StatusBadRequest, "bearer tokens cannot step up")
		return
	}
	sess, ok := r.Context().Value(sessionKey).(store.Session)
	if !ok || sess.ID == "" {
		writeError(w, http.StatusForbidden, "step-up requires an active session")
		return
	}

	var b stepUpRequest
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.Factor == "" {
		writeValidationError(w, map[string]string{"factor": "is required"})
		return
	}

	var newAssurance authz.Assurance
	switch authz.Assurance(b.Factor) {
	case authz.AssurancePassword:
		if s.deps.StepUpVerifier == nil {
			writeError(w, http.StatusServiceUnavailable, "step-up not available")
			return
		}
		if b.Password == "" {
			writeValidationError(w, map[string]string{"password": "is required for password factor"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := s.deps.StepUpVerifier.VerifyPassword(ctx, sess.UserID, b.Password); err != nil {
			writeError(w, http.StatusUnauthorized, "invalid password")
			return
		}
		newAssurance = authz.AssurancePassword
	case authz.AssuranceTOTP:
		if s.deps.TOTPStore == nil {
			writeError(w, http.StatusServiceUnavailable, "TOTP not available")
			return
		}
		if b.Code == "" {
			writeValidationError(w, map[string]string{"code": "is required for totp factor"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		totp, err := s.deps.TOTPStore.Get(ctx, sess.UserID)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "TOTP not enrolled")
			return
		}
		if !totp.Confirmed {
			writeError(w, http.StatusUnauthorized, "TOTP not confirmed")
			return
		}
		if !authz.ValidateTOTP(totp.Secret, b.Code, time.Now()) {
			writeError(w, http.StatusUnauthorized, "invalid TOTP code")
			return
		}
		newAssurance = authz.AssuranceTOTP
	case "recovery_code":
		// Recovery codes (Phase C, §8): a recovery code proves the user
		// was previously trusted (they generated codes while authed).
		// It elevates to L1 (password level) so the user can access
		// Settings to re-enroll TOTP/passkeys after losing a device.
		if s.deps.RecoveryService == nil {
			writeError(w, http.StatusServiceUnavailable, "recovery codes not available")
			return
		}
		if b.Code == "" {
			writeValidationError(w, map[string]string{"code": "is required for recovery_code factor"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := s.deps.RecoveryService.VerifyCode(ctx, sess.UserID, b.Code); err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or used recovery code")
			return
		}
		newAssurance = authz.AssurancePassword
	default:
		writeValidationError(w, map[string]string{"factor": "must be one of: password, totp, recovery_code"})
		return
	}

	// Rotate session ID + CSRF token (§8: session fixation prevention).
	newSID, err := authz.RandomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session rotation failed")
		return
	}
	newCSRF, err := authz.RandomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session rotation failed")
		return
	}
	now := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.deps.Sessions.ElevateSession(ctx, sess.ID, newSID, newCSRF, string(newAssurance), now); err != nil {
		writeError(w, http.StatusInternalServerError, "session elevation failed")
		return
	}
	// Set the new cookie so the browser uses the rotated session ID.
	elevatedSess := sess
	elevatedSess.ID = newSID
	elevatedSess.CSRFToken = newCSRF
	elevatedSess.Assurance = string(newAssurance)
	elevatedSess.AssuranceAt = now
	s.setSessionCookie(w, elevatedSess)

	s.audit(r, ident.Name, "step_up", string(newAssurance), "")
	at := unixSeconds(now)
	writeJSON(w, http.StatusOK, stepUpResponse{
		Assurance:   string(newAssurance),
		AssuranceAt: &at,
	})
}

// ── WebAuthn registration + assertion (Phase B) ─────────────────────────────

// challengeCookieName is the cookie that carries the challenge token between
// begin and finish WebAuthn ceremonies.
const challengeCookieName = "forge_wa_challenge"

// handleWebAuthnRegisterBegin — POST /api/v1/auth/webauthn/register/begin.
// Frozen shape: webauthnBeginRegisterResponse{ options }.
// Begins a registration ceremony: generates creation options + stores the
// challenge session in-memory, returns options to the client + sets a
// challenge cookie.
func (s *Server) handleWebAuthnRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if s.deps.WebAuthnService == nil {
		writeError(w, http.StatusServiceUnavailable, "WebAuthn not available")
		return
	}
	sess, ok := r.Context().Value(sessionKey).(store.Session)
	if !ok || sess.ID == "" {
		writeError(w, http.StatusForbidden, "WebAuthn enrollment requires an active session")
		return
	}
	ident := identity(r)
	wa, err := s.webAuthnInstance(r)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := s.deps.WebAuthnService.BeginRegistration(ctx, wa, ident.Name)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// Set the challenge token in a short-lived cookie.
	http.SetCookie(w, &http.Cookie{
		Name:     challengeCookieName,
		Value:    result.ChallengeToken,
		Path:     "/",
		MaxAge:   300, // 5 min
		HttpOnly: true,
		Secure:   s.cookieSecure(),
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, webauthnBeginRegisterResponse{
		Options: creationOptionsFromProtocol(result.Creation),
	})
}

// handleWebAuthnRegisterFinish — POST /api/v1/auth/webauthn/register/finish.
// Frozen shape: webauthnFinishRegisterRequest{ response } →
// webauthnFinishRegisterResponse{ credential }.
func (s *Server) handleWebAuthnRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if s.deps.WebAuthnService == nil {
		writeError(w, http.StatusServiceUnavailable, "WebAuthn not available")
		return
	}
	sess, ok := r.Context().Value(sessionKey).(store.Session)
	if !ok || sess.ID == "" {
		writeError(w, http.StatusForbidden, "WebAuthn enrollment requires an active session")
		return
	}
	ident := identity(r)
	challengeToken := challengeCookie(r)
	if challengeToken == "" {
		writeError(w, http.StatusBadRequest, "missing challenge cookie")
		return
	}
	var b webauthnFinishRegisterRequest
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	// Re-encode the response body for the WebAuthn library to parse.
	body, err := json.Marshal(b.Response)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal failed")
		return
	}
	wa, err := s.webAuthnInstance(r)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := s.deps.WebAuthnService.FinishRegistration(ctx, wa, ident.Name, challengeToken, b.Label, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Clear the challenge cookie.
	http.SetCookie(w, &http.Cookie{
		Name: challengeCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cookieSecure(), SameSite: http.SameSiteLaxMode,
	})
	s.audit(r, ident.Name, "webauthn_register", result.CredentialID, "")
	writeJSON(w, http.StatusOK, webauthnFinishRegisterResponse{
		Credential: webauthnCredentialJSON{
			ID:         result.CredentialID,
			Label:      b.Label,
			Transports: result.Transports,
			CreatedAt:  unixSeconds(time.Now()),
		},
	})
}

// handleWebAuthnAssertBegin — POST /api/v1/auth/webauthn/assert/begin.
// Frozen shape: webauthnBeginAssertResponse{ options }.
// Begins an assertion ceremony for step-up to passkey (L2).
func (s *Server) handleWebAuthnAssertBegin(w http.ResponseWriter, r *http.Request) {
	if s.deps.WebAuthnService == nil {
		writeError(w, http.StatusServiceUnavailable, "WebAuthn not available")
		return
	}
	sess, ok := r.Context().Value(sessionKey).(store.Session)
	if !ok || sess.ID == "" {
		writeError(w, http.StatusForbidden, "WebAuthn assertion requires an active session")
		return
	}
	ident := identity(r)
	wa, err := s.webAuthnInstance(r)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := s.deps.WebAuthnService.BeginAssertion(ctx, wa, ident.Name)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     challengeCookieName,
		Value:    result.ChallengeToken,
		Path:     "/",
		MaxAge:   300,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, webauthnBeginAssertResponse{
		Options: requestOptionsFromProtocol(result.Assertion),
	})
}

// handleWebAuthnAssertFinish — POST /api/v1/auth/webauthn/assert/finish.
// Frozen shape: webauthnFinishAssertRequest{ response } →
// webauthnFinishAssertResponse{ verified, assurance? }.
// Completes the assertion: verifies the passkey response and elevates the
// session to passkey (L2) assurance.
func (s *Server) handleWebAuthnAssertFinish(w http.ResponseWriter, r *http.Request) {
	if s.deps.WebAuthnService == nil {
		writeError(w, http.StatusServiceUnavailable, "WebAuthn not available")
		return
	}
	sess, ok := r.Context().Value(sessionKey).(store.Session)
	if !ok || sess.ID == "" {
		writeError(w, http.StatusForbidden, "WebAuthn assertion requires an active session")
		return
	}
	ident := identity(r)
	challengeToken := challengeCookie(r)
	if challengeToken == "" {
		writeError(w, http.StatusBadRequest, "missing challenge cookie")
		return
	}
	var b webauthnFinishAssertRequest
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	body, err := json.Marshal(b.Response)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal failed")
		return
	}
	wa, err := s.webAuthnInstance(r)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := s.deps.WebAuthnService.FinishAssertion(ctx, wa, ident.Name, challengeToken, body)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	// Clear the challenge cookie.
	http.SetCookie(w, &http.Cookie{
		Name: challengeCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cookieSecure(), SameSite: http.SameSiteLaxMode,
	})

	// Elevate the session to passkey (L2).
	newSID, err := authz.RandomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session rotation failed")
		return
	}
	newCSRF, err := authz.RandomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session rotation failed")
		return
	}
	now := time.Now()
	elevCtx, elevCancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer elevCancel()
	if err := s.deps.Sessions.ElevateSession(elevCtx, sess.ID, newSID, newCSRF, string(authz.AssurancePasskey), now); err != nil {
		writeError(w, http.StatusInternalServerError, "session elevation failed")
		return
	}
	elevatedSess := sess
	elevatedSess.ID = newSID
	elevatedSess.CSRFToken = newCSRF
	elevatedSess.Assurance = string(authz.AssurancePasskey)
	elevatedSess.AssuranceAt = now
	s.setSessionCookie(w, elevatedSess)

	s.audit(r, ident.Name, "webauthn_assert", result.CredentialID, "")
	writeJSON(w, http.StatusOK, webauthnFinishAssertResponse{
		Verified:  true,
		Assurance: string(authz.AssurancePasskey),
	})
}

// handleWebAuthnCredentialsList — GET /api/v1/auth/webauthn/credentials.
// Frozen shape: webauthnCredentialsResponse{ credentials: [...] }.
func (s *Server) handleWebAuthnCredentialsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.WebAuthnService == nil {
		writeError(w, http.StatusServiceUnavailable, "WebAuthn not available")
		return
	}
	sess, ok := r.Context().Value(sessionKey).(store.Session)
	if !ok || sess.ID == "" {
		writeError(w, http.StatusForbidden, "WebAuthn credential list requires an active session")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	creds, err := s.deps.WebAuthnService.ListCredentials(ctx, sess.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "credential list failed")
		return
	}
	resp := webauthnCredentialsResponse{Credentials: []webauthnCredentialJSON{}}
	for _, c := range creds {
		resp.Credentials = append(resp.Credentials, toWebAuthnCredentialJSON(c))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleWebAuthnCredentialDelete — DELETE /api/v1/auth/webauthn/credentials/{id}.
func (s *Server) handleWebAuthnCredentialDelete(w http.ResponseWriter, r *http.Request) {
	if s.deps.WebAuthnService == nil {
		writeError(w, http.StatusServiceUnavailable, "WebAuthn not available")
		return
	}
	sess, ok := r.Context().Value(sessionKey).(store.Session)
	if !ok || sess.ID == "" {
		writeError(w, http.StatusForbidden, "WebAuthn credential delete requires an active session")
		return
	}
	credID := r.PathValue("id")
	if credID == "" {
		writeError(w, http.StatusBadRequest, "credential id is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	cred, err := s.deps.WebAuthnService.GetCredential(ctx, credID)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "credential not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "credential lookup failed")
		return
	}
	if cred.UserID != sess.UserID && identity(r).Role != authz.RoleAdmin {
		writeError(w, http.StatusNotFound, "credential not found")
		return
	}
	if err := s.deps.WebAuthnService.DeleteCredential(ctx, credID); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "credential not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "credential delete failed")
		return
	}
	s.audit(r, identity(r).Name, "webauthn_credential_delete", credID, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── TOTP ────────────────────────────────────────────────────────────────────

// handleTOTPEnroll — POST /api/v1/auth/totp/enroll.
// Frozen shape: totpEnrollResponse{ secret, otpauth_uri }.
// Generates a new TOTP secret (unconfirmed) for the current user. The
// secret is NOT active until confirmed via /totp/confirm with a valid code.
func (s *Server) handleTOTPEnroll(w http.ResponseWriter, r *http.Request) {
	if s.deps.TOTPStore == nil {
		writeError(w, http.StatusServiceUnavailable, "TOTP not available")
		return
	}
	sess, ok := r.Context().Value(sessionKey).(store.Session)
	if !ok || sess.ID == "" {
		writeError(w, http.StatusForbidden, "TOTP enrollment requires an active session")
		return
	}
	secret, err := authz.GenerateTOTPSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "TOTP secret generation failed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.deps.TOTPStore.Save(ctx, store.TOTPSecret{
		UserID:    sess.UserID,
		Secret:    secret,
		Confirmed: false,
		CreatedAt: time.Now(),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "TOTP save failed")
		return
	}
	issuer := "Forge"
	accountName := identity(r).Name
	writeJSON(w, http.StatusOK, totpEnrollResponse{
		Secret:     secret,
		OTPAuthURI: authz.TOTPOtpAuthURI(issuer, accountName, secret),
	})
}

// handleTOTPConfirm — POST /api/v1/auth/totp/confirm.
// Frozen shape: totpConfirmRequest{ code } → totpConfirmResponse{ active }.
// Activates the TOTP secret after the user enters a valid code from their
// authenticator app.
func (s *Server) handleTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	if s.deps.TOTPStore == nil {
		writeError(w, http.StatusServiceUnavailable, "TOTP not available")
		return
	}
	sess, ok := r.Context().Value(sessionKey).(store.Session)
	if !ok || sess.ID == "" {
		writeError(w, http.StatusForbidden, "TOTP confirmation requires an active session")
		return
	}
	var b totpConfirmRequest
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.Code == "" {
		writeValidationError(w, map[string]string{"code": "is required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	totp, err := s.deps.TOTPStore.Get(ctx, sess.UserID)
	if err != nil {
		writeError(w, http.StatusNotFound, "no TOTP enrollment in progress")
		return
	}
	if !authz.ValidateTOTP(totp.Secret, b.Code, time.Now()) {
		writeError(w, http.StatusUnauthorized, "invalid TOTP code")
		return
	}
	if err := s.deps.TOTPStore.Confirm(ctx, sess.UserID); err != nil {
		writeError(w, http.StatusInternalServerError, "TOTP confirmation failed")
		return
	}
	s.audit(r, identity(r).Name, "totp_confirm", "", "")
	writeJSON(w, http.StatusOK, totpConfirmResponse{Active: true})
}

// handleTOTPDelete — DELETE /api/v1/auth/totp.
// Removes the user's TOTP secret (un-enrolls 2FA).
func (s *Server) handleTOTPDelete(w http.ResponseWriter, r *http.Request) {
	if s.deps.TOTPStore == nil {
		writeError(w, http.StatusServiceUnavailable, "TOTP not available")
		return
	}
	sess, ok := r.Context().Value(sessionKey).(store.Session)
	if !ok || sess.ID == "" {
		writeError(w, http.StatusForbidden, "TOTP removal requires an active session")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.deps.TOTPStore.Delete(ctx, sess.UserID); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "no TOTP enrolled")
			return
		}
		writeError(w, http.StatusInternalServerError, "TOTP removal failed")
		return
	}
	s.audit(r, identity(r).Name, "totp_delete", "", "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── Identity linking ─────────────────────────────────────────────────────────

// handleIdentityLinksList — GET /api/v1/auth/identity-links.
// Frozen shape: identityLinksResponse{ links: identityLinkResponse[] }.
// Admins see all links; non-admins see only their own.
func (s *Server) handleIdentityLinksList(w http.ResponseWriter, r *http.Request) {
	if s.deps.IdentityLinks == nil {
		writeError(w, http.StatusServiceUnavailable, "identity links not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	ident := identity(r)
	var links []store.IdentityLink
	var err error
	if ident.Role == authz.RoleAdmin {
		links, err = s.deps.IdentityLinks.List(ctx)
	} else {
		sess, ok := r.Context().Value(sessionKey).(store.Session)
		if !ok || sess.ID == "" {
			links = []store.IdentityLink{}
		} else {
			links, err = s.deps.IdentityLinks.ListByUser(ctx, sess.UserID)
		}
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "identity links query failed")
		return
	}
	resp := identityLinksResponse{Links: []identityLinkResponse{}}
	for _, link := range links {
		resp.Links = append(resp.Links, identityLinkResponse{
			Provider:  link.Provider,
			Principal: link.Principal,
			UserID:    link.UserID,
			CreatedAt: unixSeconds(link.CreatedAt),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleIdentityLinkCreate — POST /api/v1/auth/identity-links.
// Frozen shape: identityLinkCreateRequest{ provider, principal, user_id } →
// identityLinkResponse.
func (s *Server) handleIdentityLinkCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.IdentityLinks == nil {
		writeError(w, http.StatusServiceUnavailable, "identity links not available")
		return
	}
	var b identityLinkCreateRequest
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.Provider == "" || b.Principal == "" || b.UserID == 0 {
		fields := map[string]string{}
		if b.Provider == "" {
			fields["provider"] = "is required"
		}
		if b.Principal == "" {
			fields["principal"] = "is required"
		}
		if b.UserID == 0 {
			fields["user_id"] = "is required"
		}
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	link := store.IdentityLink{
		Provider:  b.Provider,
		Principal: b.Principal,
		UserID:    b.UserID,
		CreatedAt: time.Now(),
	}
	if err := s.deps.IdentityLinks.Create(ctx, link); err != nil {
		writeError(w, http.StatusInternalServerError, "identity link create failed")
		return
	}
	s.audit(r, identity(r).Name, "identity_link_create", b.Provider+":"+b.Principal, "")
	writeJSON(w, http.StatusCreated, identityLinkResponse{
		Provider:  link.Provider,
		Principal: link.Principal,
		UserID:    link.UserID,
		CreatedAt: unixSeconds(link.CreatedAt),
	})
}

// handleIdentityLinkDelete — DELETE /api/v1/auth/identity-links/{provider}/{principal}.
func (s *Server) handleIdentityLinkDelete(w http.ResponseWriter, r *http.Request) {
	if s.deps.IdentityLinks == nil {
		writeError(w, http.StatusServiceUnavailable, "identity links not available")
		return
	}
	provider := r.PathValue("provider")
	principal := r.PathValue("principal")
	if provider == "" || principal == "" {
		writeError(w, http.StatusBadRequest, "provider and principal are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.deps.IdentityLinks.Delete(ctx, provider, principal); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "identity link not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "identity link delete failed")
		return
	}
	s.audit(r, identity(r).Name, "identity_link_delete", provider+":"+principal, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── API-key management ──────────────────────────────────────────────────────

// handleKeysList — GET /api/v1/keys?kind=forge|router|mcp.
// Frozen shape: apiKeysResponse{ keys: apiKeyResponse[] } (masked; no secret).
func (s *Server) handleKeysList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Keys == nil {
		writeError(w, http.StatusServiceUnavailable, "keys store not available")
		return
	}
	kind := r.URL.Query().Get("kind")
	// Rebrand tolerance (2026-08): accept legacy kind=foundry as forge.
	if kind == "foundry" {
		kind = "forge"
	}
	if kind != "" && kind != "forge" && kind != "router" && kind != "mcp" {
		writeValidationError(w, map[string]string{"kind": "must be one of: forge, router, mcp"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	keys, err := s.deps.Keys.List(ctx, kind)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "keys query failed")
		return
	}
	resp := apiKeysResponse{Keys: []apiKeyResponse{}}
	for _, k := range keys {
		resp.Keys = append(resp.Keys, toAPIKeyResponse(k))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleKeyCreate — POST /api/v1/keys.
// Frozen shape: apiKeyCreateRequest{ kind, name, role? } →
// apiKeyCreateResponse{ token, key }.
func (s *Server) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.KeyManager == nil {
		writeError(w, http.StatusServiceUnavailable, "key manager not available")
		return
	}
	var b apiKeyCreateRequest
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	// Rebrand tolerance (2026-08): accept legacy kind=foundry as forge.
	if b.Kind == "foundry" {
		b.Kind = "forge"
	}
	if b.Kind == "" || b.Name == "" {
		fields := map[string]string{}
		if b.Kind == "" {
			fields["kind"] = "is required"
		} else if b.Kind != "forge" && b.Kind != "router" && b.Kind != "mcp" {
			fields["kind"] = "must be one of: forge, router, mcp"
		}
		if b.Name == "" {
			fields["name"] = "is required"
		}
		writeValidationError(w, fields)
		return
	}
	role := authz.Role(b.Role)
	if b.Kind == "forge" && role == "" {
		writeValidationError(w, map[string]string{"role": "is required for forge keys"})
		return
	}

	// #37: gating lives here (not requireAssurance) because the
	// self-rotation carve-out needs the decoded body. A session identity
	// always falls through to the normal policy evaluation, unchanged from
	// before #37. A bearer identity is allowed through WITHOUT a stepped-up
	// session only when this request is safe self-rotation: re-minting the
	// SAME kind+name as the calling key itself (exactly what MintKey's
	// existing "revoke any existing active key of this name first"
	// semantics already do — this is how `forge keys-export` refreshes its
	// own CLI key), at no more than the calling key's own role. Minting any
	// other key, or a higher role, via bearer still requires a session
	// step-up — which a bearer identity can never satisfy, so it's a hard
	// block in practice, same as DELETE (requireStrictAssurance).
	ident := identity(r)
	isSelfRotation := ident.KeyID != "" &&
		ident.Kind == authz.KindForge &&
		authz.KeyKind(b.Kind) == authz.KindForge &&
		b.Name == ident.Name &&
		ident.Role.Allows(role)
	if !isSelfRotation {
		allowed, decision, err := s.evaluateAssurance(r.Context(), ident, authz.ResourceAreaSettingsSecurity)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "policy load failed")
			return
		}
		if !allowed {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":    "step_up_required",
				"required": string(decision.Required),
				"resource": decision.Resource,
			})
			return
		}
	}

	if b.TTLSeconds < 0 {
		writeValidationError(w, map[string]string{"ttl_seconds": "must be >= 0"})
		return
	}
	var boundIP string
	if b.BindToRequester {
		boundIP = effectiveClientIP(r)
	}
	var expiresAt time.Time
	if b.TTLSeconds > 0 {
		expiresAt = time.Now().Add(time.Duration(b.TTLSeconds) * time.Second)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	token, err := s.deps.KeyManager.MintKey(ctx, authz.KeyKind(b.Kind), b.Name, b.DisplayName, role, boundIP, expiresAt)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// Fetch the created key row for the response (without the secret).
	keys, err := s.deps.Keys.List(ctx, b.Kind)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "key list failed")
		return
	}
	var keyRow store.APIKey
	for _, k := range keys {
		if k.Name == b.Name && k.RevokedAt.IsZero() {
			keyRow = k
			break
		}
	}
	s.audit(r, identity(r).Name, "key_mint", b.Kind+":"+b.Name, "")
	writeJSON(w, http.StatusCreated, apiKeyCreateResponse{
		Token: token,
		Key:   toAPIKeyResponse(keyRow),
	})
}

// handleKeyRevoke — DELETE /api/v1/keys/{keyid}.
func (s *Server) handleKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if s.deps.KeyManager == nil {
		writeError(w, http.StatusServiceUnavailable, "key manager not available")
		return
	}
	keyid := r.PathValue("keyid")
	if keyid == "" {
		writeError(w, http.StatusBadRequest, "keyid is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.deps.KeyManager.RevokeKey(ctx, keyid); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "key not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "key revoke failed")
		return
	}
	s.audit(r, identity(r).Name, "key_revoke", keyid, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── Policy + config ─────────────────────────────────────────────────────────

// handleAuthPolicyGet — GET /api/v1/auth/policy.
// Frozen shape: authPolicyResponse{ policy: { resourceKey: factor } }.
func (s *Server) handleAuthPolicyGet(w http.ResponseWriter, r *http.Request) {
	if s.deps.PolicyStore == nil {
		writeError(w, http.StatusServiceUnavailable, "policy store not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	policy, err := s.deps.PolicyStore.Load(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "policy load failed")
		return
	}
	writeJSON(w, http.StatusOK, authPolicyResponse{Policy: policy})
}

// handleAuthPolicyPut — PUT /api/v1/auth/policy.
// Frozen shape: authPolicyPutRequest{ policy } → authPolicyResponse{ policy }.
func (s *Server) handleAuthPolicyPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.PolicyStore == nil {
		writeError(w, http.StatusServiceUnavailable, "policy store not available")
		return
	}
	var b authPolicyPutRequest
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.Policy == nil {
		writeValidationError(w, map[string]string{"policy": "is required"})
		return
	}
	if invalid := authz.ValidatePolicy(b.Policy); len(invalid) > 0 {
		writeValidationError(w, invalid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.deps.PolicyStore.Save(ctx, b.Policy); err != nil {
		writeError(w, http.StatusInternalServerError, "policy save failed")
		return
	}
	s.audit(r, identity(r).Name, "policy_update", "", "")
	writeJSON(w, http.StatusOK, authPolicyResponse{Policy: b.Policy})
}

// handleAuthConfigGet — GET /api/v1/auth/config.
// Frozen shape: authConfigResponse.
func (s *Server) handleAuthConfigGet(w http.ResponseWriter, r *http.Request) {
	resp := authConfigResponse{
		NetworkProvider:    "none",
		StepUpTTLMin:       int(authz.DefaultStepUpTTL / time.Minute),
		NetworkDefaultRole: string(authz.RoleViewer),
		A0TailnetBypass:    true,
	}
	if s.deps.NetworkIdentity != nil {
		resp.NetworkProvider = s.deps.NetworkIdentity.Name()
	}
	if s.deps.StepUpTTL > 0 {
		resp.StepUpTTLMin = int(s.deps.StepUpTTL / time.Minute)
	}
	if s.deps.NetworkDefaultRole != "" {
		resp.NetworkDefaultRole = string(s.deps.NetworkDefaultRole)
	}
	// Read RP ID + RP name from settings if available.
	if s.deps.Settings != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if raw, err := s.deps.Settings.Get(ctx, "auth.webauthn.rp_id"); err == nil {
			_ = json.Unmarshal(raw, &resp.WebAuthnRPID)
		}
		if raw, err := s.deps.Settings.Get(ctx, "auth.webauthn.rp_name"); err == nil {
			_ = json.Unmarshal(raw, &resp.WebAuthnRPName)
		}
		if raw, err := s.deps.Settings.Get(ctx, "auth.a0_tailnet_bypass"); err == nil {
			_ = json.Unmarshal(raw, &resp.A0TailnetBypass)
		}
		if raw, err := s.deps.Settings.Get(ctx, "auth.step_up_ttl_min"); err == nil {
			var v int
			if json.Unmarshal(raw, &v) == nil && v > 0 {
				resp.StepUpTTLMin = v
			}
		}
		if raw, err := s.deps.Settings.Get(ctx, "auth.network_default_role"); err == nil {
			var v string
			if json.Unmarshal(raw, &v) == nil && v != "" {
				resp.NetworkDefaultRole = v
			}
		}
		// Sprint 12 (was H) Phase 2: forward_auth_header's two config keys
		// were CLI-only (main.go:421-429) despite "forward_auth_header"
		// already being a selectable network_provider value in this same
		// response — an operator could pick it but never configure it from
		// the UI. Activates the frozen, previously-unpopulated
		// ProviderConfig field (Sprint 0 §3.2's stub shape) rather than
		// adding parallel dedicated fields.
		// Defaults mirror main.go's own fallback values exactly (main.go:421-422)
		// so this always shows the value actually in effect, not blank.
		headerName, trustedCIDRs := "X-Auth-Request-User", "127.0.0.0/8"
		if raw, err := s.deps.Settings.Get(ctx, "auth.provider.forward_auth_header.header_name"); err == nil {
			var v string
			if json.Unmarshal(raw, &v) == nil && v != "" {
				headerName = v
			}
		}
		if raw, err := s.deps.Settings.Get(ctx, "auth.provider.forward_auth_header.trusted_cidrs"); err == nil {
			var v string
			if json.Unmarshal(raw, &v) == nil && v != "" {
				trustedCIDRs = v
			}
		}
		resp.ProviderConfig = map[string]any{
			"header_name":   headerName,
			"trusted_cidrs": trustedCIDRs,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAuthConfigPut — PUT /api/v1/auth/config.
// Frozen shape: authConfigPutRequest → authConfigResponse.
func (s *Server) handleAuthConfigPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store not available")
		return
	}
	var b authConfigPutRequest
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.NetworkProvider != "" && b.NetworkProvider != "tailscale" && b.NetworkProvider != "forward_auth_header" && b.NetworkProvider != "none" {
		writeValidationError(w, map[string]string{"network_provider": "must be one of: tailscale, forward_auth_header, none"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	set := func(key string, v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", key, err)
		}
		return s.deps.Settings.Set(ctx, key, raw)
	}
	if b.NetworkProvider != "" {
		if err := set("auth.network_provider", b.NetworkProvider); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if b.WebAuthnRPID != "" {
		if err := set("auth.webauthn.rp_id", b.WebAuthnRPID); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if b.WebAuthnRPName != "" {
		if err := set("auth.webauthn.rp_name", b.WebAuthnRPName); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if b.StepUpTTLMin > 0 {
		if err := set("auth.step_up_ttl_min", b.StepUpTTLMin); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if b.NetworkDefaultRole != "" {
		if err := set("auth.network_default_role", b.NetworkDefaultRole); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if err := set("auth.a0_tailnet_bypass", b.A0TailnetBypass); err != nil {
		writeInternalError(w, err)
		return
	}
	// Sprint 12 (was H) Phase 2: forward_auth_header's two config keys,
	// carried in the (previously always-empty) ProviderConfig bag — see
	// handleAuthConfigGet's matching comment.
	if hn, ok := b.ProviderConfig["header_name"].(string); ok && hn != "" {
		if err := set("auth.provider.forward_auth_header.header_name", hn); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if cidrs, ok := b.ProviderConfig["trusted_cidrs"].(string); ok && cidrs != "" {
		// Structural check — every comma-separated entry must parse as a
		// CIDR, since authz.ParseCIDRs silently drops ones that don't
		// (identity.go), which would otherwise let a typo silently narrow
		// the trust list.
		parts := strings.Split(cidrs, ",")
		valid := 0
		for _, p := range parts {
			if p = strings.TrimSpace(p); p == "" {
				continue
			}
			if len(authz.ParseCIDRs(p)) == 1 {
				valid++
			}
		}
		wantValid := 0
		for _, p := range parts {
			if strings.TrimSpace(p) != "" {
				wantValid++
			}
		}
		if valid != wantValid {
			writeValidationError(w, map[string]string{"trusted_cidrs": "every comma-separated entry must be a valid CIDR (e.g. 10.0.0.0/8)"})
			return
		}
		// Sprint 12 (was H) Phase 3: the deeper check — does the SAVING
		// operator's own address remain covered, i.e. would this save lock
		// them out — only matters once forward_auth_header is (or is
		// becoming) the active network provider. Resolve the effective
		// provider (this request's value if it's setting one, else the
		// currently-stored one) before deciding whether to run it.
		effectiveProvider := b.NetworkProvider
		if effectiveProvider == "" {
			if raw, err := s.deps.Settings.Get(ctx, "auth.network_provider"); err == nil {
				_ = json.Unmarshal(raw, &effectiveProvider)
			}
		}
		if effectiveProvider == "forward_auth_header" {
			if check := checkTrustedCIDRLockout(r.RemoteAddr, cidrs); check != nil && check.Level == "error" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"error":  "preflight_failed",
					"fields": map[string]string{"trusted_cidrs": check.Message},
					"checks": []preflightCheck{*check},
				})
				return
			}
		}
		if err := set("auth.provider.forward_auth_header.trusted_cidrs", cidrs); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	s.audit(r, identity(r).Name, "auth_config_update", "", "")
	// Return the current config (re-read for consistency).
	s.handleAuthConfigGet(w, r)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// ── Recovery codes (Phase C, §8) ──────────────────────────────────────────────

// handleRecoveryCodesStatus — GET /api/v1/auth/recovery-codes.
// Returns whether the user has recovery codes and how many are unused.
func (s *Server) handleRecoveryCodesStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecoveryService == nil {
		writeError(w, http.StatusServiceUnavailable, "recovery codes not available")
		return
	}
	sess, ok := r.Context().Value(sessionKey).(store.Session)
	if !ok || sess.ID == "" {
		writeError(w, http.StatusForbidden, "recovery codes require an active session")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	codes, err := s.deps.RecoveryService.CountUnused(ctx, sess.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "recovery codes query failed")
		return
	}
	has, err := s.deps.RecoveryService.HasRecoveryCodes(ctx, sess.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "recovery codes query failed")
		return
	}
	writeJSON(w, http.StatusOK, recoveryCodesStatusResponse{
		HasCodes: has,
		Unused:   codes,
		Total:    10, // the fixed generation count
	})
}

// handleRecoveryCodesGenerate — POST /api/v1/auth/recovery-codes/generate.
// Generates a fresh set of recovery codes and returns the plaintext codes
// exactly once. Existing codes are replaced.
func (s *Server) handleRecoveryCodesGenerate(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecoveryService == nil {
		writeError(w, http.StatusServiceUnavailable, "recovery codes not available")
		return
	}
	sess, ok := r.Context().Value(sessionKey).(store.Session)
	if !ok || sess.ID == "" {
		writeError(w, http.StatusForbidden, "recovery codes require an active session")
		return
	}
	// 5s is a generous safety net: generation now hashes each code with
	// HMAC-SHA256 (microseconds), not a memory-hard KDF, so it never
	// approaches this bound even under load. See internal/authz/recovery.go.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	codes, err := s.deps.RecoveryService.GenerateCodes(ctx, sess.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "recovery code generation failed")
		return
	}
	s.audit(r, identity(r).Name, "recovery_codes_generate", "", "")
	writeJSON(w, http.StatusOK, recoveryCodesGenerateResponse{
		Codes: codes,
		Total: len(codes),
	})
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// toAPIKeyResponse converts a store.APIKey to the frozen wire shape (masked;
// never the secret).
func toAPIKeyResponse(k store.APIKey) apiKeyResponse {
	resp := apiKeyResponse{
		KeyID:       k.KeyID,
		Kind:        k.Kind,
		Name:        k.Name,
		Role:        k.Role,
		DisplayName: k.DisplayName,
		BoundIP:     k.BoundIP,
		CreatedAt:   unixSeconds(k.CreatedAt),
	}
	if !k.LastUsedAt.IsZero() {
		t := unixSeconds(k.LastUsedAt)
		resp.LastUsedAt = &t
	}
	if !k.RevokedAt.IsZero() {
		t := unixSeconds(k.RevokedAt)
		resp.RevokedAt = &t
	}
	if !k.ExpiresAt.IsZero() {
		t := unixSeconds(k.ExpiresAt)
		resp.ExpiresAt = &t
	}
	return resp
}

// ── WebAuthn helpers ──────────────────────────────────────────────────────────

// webAuthnInstance creates a go-webauthn instance from the current RP config
// in settings + the request's Origin header (§7: origin derived from the
// browser's Origin header so registration/auth work regardless of which
// hostname the dashboard is served at — ops. vs forgehost.).
func (s *Server) webAuthnInstance(r *http.Request) (*webauthn.WebAuthn, error) {
	rpID := "example-tailnet.ts.net"
	rpName := "Forge"
	if s.deps.Settings != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if raw, err := s.deps.Settings.Get(ctx, "auth.webauthn.rp_id"); err == nil {
			var v string
			if json.Unmarshal(raw, &v) == nil && v != "" {
				rpID = v
			}
		}
		if raw, err := s.deps.Settings.Get(ctx, "auth.webauthn.rp_name"); err == nil {
			var v string
			if json.Unmarshal(raw, &v) == nil && v != "" {
				rpName = v
			}
		}
	}
	// Origin from the request — the browser sends the full origin
	// (https://ops.example.ts.net).
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Fall back to constructing from Host + scheme.
		scheme := "https"
		if r.TLS == nil && strings.HasPrefix(r.Host, "localhost") {
			scheme = "http"
		}
		origin = scheme + "://" + r.Host
	}
	return authz.NewWebAuthnInstance(authz.RPConfig{
		RPID:          rpID,
		RPDisplayName: rpName,
		RPOrigins:     []string{origin},
	})
}

// challengeCookie extracts the WebAuthn challenge token from the cookie.
func challengeCookie(r *http.Request) string {
	c, err := r.Cookie(challengeCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// creationOptionsFromProtocol converts the go-webauthn protocol.CredentialCreation
// to the frozen JSON shape the PWA expects.
func creationOptionsFromProtocol(c *protocol.CredentialCreation) webauthnCreationOptionsJSON {
	if c == nil {
		return webauthnCreationOptionsJSON{}
	}
	resp := c.Response
	rp := webauthnRelyingPartyJSON{
		ID:   resp.RelyingParty.ID,
		Name: resp.RelyingParty.Name,
	}
	user := webauthnUserJSON{
		ID:          fmt.Sprintf("%s", resp.User.ID),
		Name:        resp.User.Name,
		DisplayName: resp.User.DisplayName,
	}
	challenge := string(resp.Challenge)
	var params []webauthnPubKeyCredParamJSON
	for _, p := range resp.Parameters {
		params = append(params, webauthnPubKeyCredParamJSON{
			Type: string(p.Type),
			Alg:  int(p.Algorithm),
		})
	}
	var exclude []webauthnPublicKeyCredentialDescriptorJSON
	for _, e := range resp.CredentialExcludeList {
		exclude = append(exclude, credentialDescriptorFromProtocol(e))
	}
	authSel := &webauthnAuthenticatorSelectionJSON{
		AuthenticatorAttachment: string(resp.AuthenticatorSelection.AuthenticatorAttachment),
		ResidentKey:             string(resp.AuthenticatorSelection.ResidentKey),
		UserVerification:        string(resp.AuthenticatorSelection.UserVerification),
	}
	return webauthnCreationOptionsJSON{
		RP:                     rp,
		User:                   user,
		Challenge:              challenge,
		PubKeyCredParams:       params,
		Timeout:                resp.Timeout,
		ExcludeCredentials:     exclude,
		AuthenticatorSelection: authSel,
		Attestation:            string(resp.Attestation),
	}
}

// requestOptionsFromProtocol converts the go-webauthn protocol.CredentialAssertion
// to the frozen JSON shape the PWA expects.
func requestOptionsFromProtocol(a *protocol.CredentialAssertion) webauthnRequestOptionsJSON {
	if a == nil {
		return webauthnRequestOptionsJSON{}
	}
	resp := a.Response
	challenge := string(resp.Challenge)
	var allow []webauthnPublicKeyCredentialDescriptorJSON
	for _, c := range resp.AllowedCredentials {
		allow = append(allow, credentialDescriptorFromProtocol(c))
	}
	return webauthnRequestOptionsJSON{
		Challenge:        challenge,
		Timeout:          resp.Timeout,
		RPID:             resp.RelyingPartyID,
		AllowCredentials: allow,
		UserVerification: string(resp.UserVerification),
	}
}

func credentialDescriptorFromProtocol(d protocol.CredentialDescriptor) webauthnPublicKeyCredentialDescriptorJSON {
	var transports []string
	for _, t := range d.Transport {
		transports = append(transports, string(t))
	}
	return webauthnPublicKeyCredentialDescriptorJSON{
		ID:         string(d.CredentialID),
		Type:       string(d.Type),
		Transports: transports,
	}
}

// toWebAuthnCredentialJSON converts a store credential record to the frozen wire shape.
func toWebAuthnCredentialJSON(c authz.WebAuthnCredentialRecord) webauthnCredentialJSON {
	var transports []string
	if c.Transports != "" {
		_ = json.Unmarshal([]byte(c.Transports), &transports)
	}
	resp := webauthnCredentialJSON{
		ID:         c.ID,
		Label:      c.Label,
		Transports: transports,
		CreatedAt:  unixSeconds(c.CreatedAt),
	}
	if !c.LastUsedAt.IsZero() {
		t := unixSeconds(c.LastUsedAt)
		resp.LastUsedAt = &t
	}
	return resp
}
