// SPDX-License-Identifier: Apache-2.0

package httpapi

// settings_handlers.go — billing/currency settings (§0.9) + provider CRUD
// (§0.9, relocated off the Compressor page). Owner track BE-5.
//
// Provider-key management moved here from Compressor: GET masks keys (via the
// §0.3 providers read endpoint, BE-3); PUT .../key is write-only and never
// echoes the secret. The Compressor config endpoint no longer returns provider
// key material at all (compressor_handlers.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/jsaigou/the-forge/internal/compressorctl"
	"github.com/jsaigou/the-forge/internal/providers"
	"github.com/jsaigou/the-forge/internal/store"
)

// Registered settings keys (Sprint 0 §0.12). These are the canonical dotted
// names for the app-mutated JSON KV (store.Settings), added by the polish
// sprint on top of 0001's set (ui.theme, ui.bookmarks, ui.help_button,
// nfs.shares, notes.sections, scheduler.config, router.busy_mode,
// compressor.passthrough_all/…_services). Declaring them here "registers" the
// keys — a single source of truth the BE tracks reference instead of stringly
// typing them, and the freeze-doc checklist item they satisfy.
const (
	// §0.2 billing/currency (BE-2).
	SettingBillingDisplayCurrency = "billing.display_currency"
	SettingBillingFxSourceURL     = "billing.fx_source_url"
	SettingBillingFxRefreshMin    = "billing.fx_refresh_min"

	// §0.4 metrics time-series (BE-1).
	SettingMetricsRetentionDays  = "metrics.retention_days"
	SettingMetricsSampleInterval = "metrics.sample_interval_s"

	// SettingLoggingForwardURL ("logging.forward_url") was removed from the
	// registered vocabulary in Sprint 12 (was H) Phase 1: the forwarder it
	// was meant for was never built, and a repo-wide grep found zero
	// production readers — it existed only as a name that looked like a
	// real, configurable feature. Migration 0029 deletes any stored row.
)

// registeredSettingKeys is the enumerable form of the Sprint 0 §0.12 keys, so
// tests and any future validation can assert the registered set without
// re-listing the constants.
var registeredSettingKeys = []string{
	SettingBillingDisplayCurrency,
	SettingBillingFxSourceURL,
	SettingBillingFxRefreshMin,
	SettingMetricsRetentionDays,
	SettingMetricsSampleInterval,
}

// currencyRE matches a 3-letter ISO 4217 currency code (uppercase).
var currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

// ── Billing settings (GET/PUT /api/v1/billing/settings) ──────────────────────

// handleBillingSettingsGet — GET /api/v1/billing/settings (§0.9, operator).
// Frozen shape: billingSettingsResponse{ display_currency, fx_source_url?,
// fx_refresh_min? }. Reads from store.Settings; display_currency defaults
// to "USD" when unset (the §0.2 default).
func (s *Server) handleBillingSettingsGet(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store not wired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	resp := billingSettingsResponse{DisplayCurrency: "USD"}
	if raw, err := s.deps.Settings.Get(ctx, SettingBillingDisplayCurrency); err == nil {
		_ = json.Unmarshal(raw, &resp.DisplayCurrency)
		if resp.DisplayCurrency == "" {
			resp.DisplayCurrency = "USD"
		}
	}
	if raw, err := s.deps.Settings.Get(ctx, SettingBillingFxSourceURL); err == nil {
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			resp.FxSourceURL = &v
		}
	}
	if raw, err := s.deps.Settings.Get(ctx, SettingBillingFxRefreshMin); err == nil {
		var v int
		if json.Unmarshal(raw, &v) == nil && v > 0 {
			resp.FxRefreshMin = &v
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleBillingSettingsPut — PUT /api/v1/billing/settings (§0.9, admin).
// Body: { display_currency, fx_source_url?, fx_refresh_min? }. Persists to
// store.Settings JSON KV. CSRF-checked + requireRole(admin) by the router;
// audit-logged here.
func (s *Server) handleBillingSettingsPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store not wired")
		return
	}
	var b billingSettingsBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	fields := map[string]string{}
	if b.DisplayCurrency == "" {
		fields["display_currency"] = "is required"
	} else if !currencyRE.MatchString(b.DisplayCurrency) {
		fields["display_currency"] = "must be a 3-letter ISO 4217 code (e.g. USD)"
	}
	if b.FxRefreshMin != nil && *b.FxRefreshMin < 1 {
		fields["fx_refresh_min"] = "must be ≥ 1"
	}
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	set := func(key string, v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", key, err)
		}
		return s.deps.Settings.Set(ctx, key, raw)
	}
	if err := set(SettingBillingDisplayCurrency, b.DisplayCurrency); err != nil {
		writeInternalError(w, err)
		return
	}
	// Optional fields: nil = leave existing; pointer = persist (including
	// empty string / zero, which clears the setting).
	if b.FxSourceURL != nil {
		if err := set(SettingBillingFxSourceURL, *b.FxSourceURL); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if b.FxRefreshMin != nil {
		if err := set(SettingBillingFxRefreshMin, *b.FxRefreshMin); err != nil {
			writeInternalError(w, err)
			return
		}
	}

	s.audit(r, identity(r).Name, "billing_settings", "display_currency", b.DisplayCurrency)

	resp := billingSettingsResponse{DisplayCurrency: b.DisplayCurrency}
	if b.FxSourceURL != nil && *b.FxSourceURL != "" {
		v := *b.FxSourceURL
		resp.FxSourceURL = &v
	} else if raw, err := s.deps.Settings.Get(ctx, SettingBillingFxSourceURL); err == nil {
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			resp.FxSourceURL = &v
		}
	}
	if b.FxRefreshMin != nil && *b.FxRefreshMin > 0 {
		v := *b.FxRefreshMin
		resp.FxRefreshMin = &v
	} else if raw, err := s.deps.Settings.Get(ctx, SettingBillingFxRefreshMin); err == nil {
		var v int
		if json.Unmarshal(raw, &v) == nil && v > 0 {
			resp.FxRefreshMin = &v
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── Provider CRUD (POST/PUT/PUT-key/DELETE /api/v1/providers) ───────────────
//
// Relocated from the Compressor page (§0.9). All mutating routes are
// requireRole(admin) + CSRF-checked by the router; audit-logged here. The
// api_key is a write-only secret: PUT .../key accepts it but never echoes
// the full value — only the masked form is returned for confirmation.

// handleProviderCreate — POST /api/v1/providers (§0.9, admin). Body:
// {name, bill_currency, target_url, status_url, credits_url}. The API key
// is NOT set on create — use PUT .../key for that.
func (s *Server) handleProviderCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Routing == nil {
		writeError(w, http.StatusServiceUnavailable, "compressor store not wired")
		return
	}
	var b providerCreateBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := b.validate(); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	existing, err := s.deps.Routing.Providers(ctx)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	for _, p := range existing {
		if p.Name == b.Name {
			writeError(w, http.StatusConflict, "provider already exists")
			return
		}
	}

	// BillingEnabled defaults to true unless the create body explicitly
	// disables it — a bare Go bool zero-value here would silently create
	// every new provider with billing OFF (the column's DEFAULT 1 only
	// applies when a column is omitted from the INSERT, and SaveProvider
	// always binds it explicitly). Same normalization pattern as
	// BillCurrency's "" → "USD" a few lines below in SaveProvider.
	billingEnabled := true
	if b.BillingEnabled != nil {
		billingEnabled = *b.BillingEnabled
	}
	row := store.ProviderRow{
		Name:              b.Name,
		BillCurrency:      b.BillCurrency,
		TargetURL:         b.TargetURL,
		StatusURL:         b.StatusURL,
		CreditsURL:        b.CreditsURL,
		OrgID:             b.OrgID,
		BillingEnabled:    billingEnabled,
		BillingConsoleURL: b.BillingConsoleURL,
		// New providers start ENABLED — a bare Go bool zero-value here would
		// silently create every provider disabled (SaveProvider binds the
		// column explicitly, so the DB DEFAULT 1 never applies). Same
		// normalization pattern as BillingEnabled just above.
		Enabled:            true,
		Country:            b.Country,
		DataResidencyGroup: b.DataResidencyGroup,
		CreatedAt:          time.Now(),
	}
	if err := s.deps.Routing.SaveProvider(ctx, row); err != nil {
		writeInternalError(w, err)
		return
	}
	// SaveProvider doesn't return the assigned id (INSERT path) — re-fetch
	// so the response carries the real surrogate PK (0042) instead of 0.
	created, _, err := s.deps.Routing.ProviderByName(ctx, b.Name)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// Opt-in auto-provision of a Compressor proxy (operator feedback 2026-08-14:
	// "adding a provider should add a proxy by default" — the FE create form
	// sends create_proxy=true by default, but the flag is opt-in server-side
	// so existing callers/tests that don't send it see no behavior change).
	// Best-effort — a provisioning failure is logged but never fails provider
	// creation (the provider is already saved; link one from Settings →
	// Routing later). Skipped when the provisioner isn't wired or the
	// provider has no target_url (a non-remote/standalone row).
	if b.CreateProxy != nil && *b.CreateProxy && s.deps.CompressorProvisioner != nil && b.TargetURL != "" {
		if proxies, perr := s.deps.Routing.Proxies(ctx); perr == nil {
			taken := make(map[string]bool, len(proxies))
			for _, p := range proxies {
				taken[p.Service] = true
			}
			if svc := compressorctl.DeriveServiceName(b.Name, func(n string) bool { return taken[n] }); svc != "" {
				if perr := s.reconcileCompressorProxyLink(ctx, "", svc, created); perr != nil {
					log.Printf("provider_create: auto-proxy %q for %q failed (non-fatal): %v", svc, b.Name, perr)
				}
			}
		}
	}
	s.audit(r, identity(r).Name, "provider_create", b.Name, "")
	writeJSON(w, http.StatusCreated, toProviderJSON(created))
}

// handleProviderUpdate — PUT /api/v1/providers/{ref} (§0.9, admin; ref is an
// id or a name, see resolveProviderRef). Updates non-secret fields
// (name — a rename since 0042 — bill_currency, target_url, status_url,
// credits_url, compressor_proxy, model, model2, enabled). Fields not present
// in the body are preserved; fields set to "" are cleared. Model
// catalog/pricing lives in offerings (Catalog → Offerings /
// Settings → Routing), not here.
func (s *Server) handleProviderUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Routing == nil {
		writeError(w, http.StatusServiceUnavailable, "compressor store not wired")
		return
	}
	ref := r.PathValue("ref")
	var b providerUpdateBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := b.validate(); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	row, ok, err := s.resolveProviderRef(ctx, ref)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	oldCompressorProxy := row.CompressorProxyName
	newCompressorProxy := oldCompressorProxy
	if b.Name != nil {
		row.Name = *b.Name
	}
	if b.BillCurrency != nil {
		row.BillCurrency = *b.BillCurrency
	}
	if b.TargetURL != nil {
		row.TargetURL = *b.TargetURL
	}
	if b.StatusURL != nil {
		row.StatusURL = *b.StatusURL
	}
	if b.CreditsURL != nil {
		row.CreditsURL = *b.CreditsURL
	}
	if b.OrgID != nil {
		row.OrgID = *b.OrgID
	}
	if b.CompressorProxy != nil {
		newCompressorProxy = *b.CompressorProxy
	}
	if b.Model != nil {
		row.Model = *b.Model
	}
	if b.Model2 != nil {
		row.Model2 = *b.Model2
	}
	if b.BillingEnabled != nil {
		row.BillingEnabled = *b.BillingEnabled
	}
	if b.BillingConsoleURL != nil {
		row.BillingConsoleURL = *b.BillingConsoleURL
	}
	if b.Enabled != nil {
		row.Enabled = *b.Enabled
	}
	if b.Country != nil {
		row.Country = *b.Country
	}
	if b.DataResidencyGroup != nil {
		row.DataResidencyGroup = *b.DataResidencyGroup
	}
	// Phase 2 (docs/v5-headroom-topology.md §5): provision/reconcile/tear
	// down the linked Compressor proxy before persisting the provider row, so
	// a provisioning failure never leaves a provider pointing at a proxy
	// that doesn't actually exist. No-ops entirely when
	// Deps.CompressorProvisioner is nil (subsystem not wired). Since 0042 the
	// link itself lives on the proxy row (provider_id), not a
	// router_providers column — reconcileCompressorProxyLink takes the
	// before/after service names explicitly instead of reading/writing a
	// field on row.
	if err := s.reconcileCompressorProxyLink(ctx, oldCompressorProxy, newCompressorProxy, row); err != nil {
		writeInternalError(w, fmt.Errorf("compressor proxy provisioning: %w", err))
		return
	}
	if err := s.deps.Routing.SaveProvider(ctx, row); err != nil {
		writeInternalError(w, err)
		return
	}
	// Keep the linked proxy's display label in sync with the provider name
	// (operator feedback 2026-08-14: the proxy's displayName should match the
	// provider's). reconcileCompressorProxyLink only sets the label when it
	// provisions fresh; a bare rename with no proxy change would otherwise
	// leave the proxy labeled with the old name. Best-effort, no-op when the
	// label already matches.
	if b.Name != nil && newCompressorProxy != "" {
		if perr := s.syncProxyLabel(ctx, row.ID, row.Name); perr != nil {
			log.Printf("provider_update: proxy label sync for %q failed (non-fatal): %v", row.Name, perr)
		}
	}
	// reconcileCompressorProxyLink just succeeded, so newCompressorProxy is now
	// the real state — row.CompressorProxyName is a read-only join-derived
	// field populated by the read path (Providers()/resolveProviderRef),
	// not refreshed by SaveProvider, so it must be set explicitly here or
	// this response would echo the pre-update link.
	row.CompressorProxyName = newCompressorProxy
	s.audit(r, identity(r).Name, "provider_update", row.Name, "")
	writeJSON(w, http.StatusOK, toProviderJSON(row))
}

// handleProviderKey — PUT /api/v1/providers/{ref}/key (§0.9, admin; ref is
// an id or a name, see resolveProviderRef). Sets the API key (secret;
// write-only). Never echoes the full secret — the response carries only the
// masked form so the UI can confirm the write.
func (s *Server) handleProviderKey(w http.ResponseWriter, r *http.Request) {
	if s.deps.Routing == nil {
		writeError(w, http.StatusServiceUnavailable, "compressor store not wired")
		return
	}
	ref := r.PathValue("ref")
	var b providerKeyBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if b.APIKey == "" {
		writeValidationError(w, map[string]string{"api_key": "is required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	row, ok, err := s.resolveProviderRef(ctx, ref)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	row.APIKey = b.APIKey
	if err := s.deps.Routing.SaveProvider(ctx, row); err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "provider_key", row.Name, "key_set")
	writeJSON(w, http.StatusOK, map[string]string{
		"name":           row.Name,
		"api_key_masked": maskSecret(b.APIKey),
	})
}

// handleProviderDelete — DELETE /api/v1/providers/{ref} (§0.9, admin; ref is
// an id or a name, see resolveProviderRef). Soft-deletes (0042) — see
// store.Routing.DeleteProvider's doc comment for why.
func (s *Server) handleProviderDelete(w http.ResponseWriter, r *http.Request) {
	if s.deps.Routing == nil {
		writeError(w, http.StatusServiceUnavailable, "compressor store not wired")
		return
	}
	ref := r.PathValue("ref")

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	row, ok, err := s.resolveProviderRef(ctx, ref)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}

	// Tear down a linked Compressor proxy before deleting the provider row
	// itself (Phase 2) — otherwise the proxy would be orphaned silently,
	// with no provider left to ever unlink it via a future update.
	if row.CompressorProxyName != "" {
		if err := s.teardownProxyIfUnreferenced(ctx, row.CompressorProxyName, row.ID); err != nil {
			writeInternalError(w, fmt.Errorf("compressor proxy teardown: %w", err))
			return
		}
	}

	if err := s.deps.Routing.DeleteProvider(ctx, row.ID); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "provider not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "provider_delete", row.Name, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleProviderDiscoverBilling — POST /api/v1/providers/{ref}/discover-billing
// (product/QA sprint, 2026-07-29; admin — same tier as the rest of
// provider-key management, since this makes outbound calls carrying the
// provider's stored API key). ref is an id or a name, see resolveProviderRef.
// Probes candidate balance-API URLs (providers.Discover — see its doc
// comment on why this is necessarily a curated-candidate approach, not spec
// parsing) and, only when a candidate is found AND the provider has no
// credits_url set yet, persists it — never overwrites an operator-set URL.
// Always reports what was tried, found or not, so the operator can see why
// nothing was saved.
func (s *Server) handleProviderDiscoverBilling(w http.ResponseWriter, r *http.Request) {
	if s.deps.Routing == nil {
		writeError(w, http.StatusServiceUnavailable, "compressor store not wired")
		return
	}
	ref := r.PathValue("ref")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	row, ok, err := s.resolveProviderRef(ctx, ref)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}

	result := providers.Discover(ctx, providers.Deps{}, row)

	saved := false
	if result.Found && row.CreditsURL == "" {
		row.CreditsURL = result.URL
		if err := s.deps.Routing.SaveProvider(ctx, row); err == nil {
			saved = true
		}
	}

	tried := make([]map[string]any, len(result.Tried))
	for i, t := range result.Tried {
		tried[i] = map[string]any{"url": t.URL, "parsed": t.Parsed}
	}
	s.audit(r, identity(r).Name, "provider_discover_billing", row.Name,
		fmt.Sprintf("found=%v saved=%v", result.Found, saved))
	writeJSON(w, http.StatusOK, map[string]any{
		"found": result.Found,
		"url":   result.URL,
		"saved": saved,
		"tried": tried,
	})
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// resolveProviderRef resolves a provider path segment that dual-accepts an
// id or a name (Phase 6 surrogate-key migration, 0042 — router_providers.name
// is no longer the primary key, so a stable id-based handle is needed for
// rename to work at all; name stays accepted for back-compat). Unambiguous
// by construction: validProviderName requires starting with a letter or
// number, so a name can never parse as an all-digit id. A soft-deleted
// provider (DeletedAt set) resolves the same as "not found" here — same as
// Providers()/List().
func (s *Server) resolveProviderRef(ctx context.Context, ref string) (store.ProviderRow, bool, error) {
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		p, ok, err := s.deps.Routing.ProviderByID(ctx, id)
		if err != nil || !ok || !p.DeletedAt.IsZero() {
			return store.ProviderRow{}, false, err
		}
		return p, true, nil
	}
	return s.deps.Routing.ProviderByName(ctx, ref)
}

// reconcileCompressorProxyLink provisions, reconciles, or tears down a
// provider's Compressor proxy to match newLink (Phase 2,
// docs/v5-headroom-topology.md §5). oldLink is the value before this update
// was applied. No-op entirely when Deps.CompressorProvisioner is nil.
//
// Since 0042 the link itself lives on the PROXY row (compressor_proxies.
// provider_id) — router_providers.headroom_proxy no longer exists as a
// column — so this sets provider.ID directly on the fetched/constructed
// ProxyRow and lets SaveProxy persist it like any other field, rather than
// writing a second column on the provider row the way the old bidirectional
// pair required.
func (s *Server) reconcileCompressorProxyLink(ctx context.Context, oldLink, newLink string, provider store.ProviderRow) error {
	if s.deps.CompressorProvisioner == nil {
		return nil
	}

	if oldLink != "" && oldLink != newLink {
		if err := s.teardownProxyIfUnreferenced(ctx, oldLink, provider.ID); err != nil {
			return err
		}
	}
	if newLink == "" {
		return nil
	}

	proxies, err := s.deps.Routing.Proxies(ctx)
	if err != nil {
		return err
	}
	var existing store.ProxyRow
	found := false
	for _, p := range proxies {
		if p.Service == newLink {
			existing, found = p, true
			break
		}
	}

	if found && existing.OrphanedAt.IsZero() {
		if existing.TargetURL == provider.TargetURL &&
			existing.ProviderID != nil && *existing.ProviderID == provider.ID {
			return nil // already provisioned, linked, and in sync — nothing to do
		}
		existing.TargetURL = provider.TargetURL
		existing.ProviderID = &provider.ID
		if err := s.deps.CompressorProvisioner.Reconcile(ctx, existing); err != nil {
			return err
		}
		return s.deps.Routing.SaveProxy(ctx, existing)
	}

	// Either a brand-new service name, or re-linking one that was
	// previously torn down (found && orphaned) — provision fresh rather
	// than trust a torn-down instance's old token/env state. Reuse the same
	// port on re-link so any operator-visible history (dashboard, logs
	// naming the port) stays stable across a relink.
	token, err := compressorctl.GenerateToken()
	if err != nil {
		return err
	}
	port := compressorctl.AllocatePort(proxies, compressorctl.DefaultBasePort)
	if found {
		port = existing.Port
	}
	row := store.ProxyRow{
		Service:    newLink,
		Label:      provider.Name,
		Port:       port,
		TargetURL:  provider.TargetURL,
		Unit:       s.deps.CompressorProvisioner.UnitName(newLink),
		ProviderID: &provider.ID,
		Token:      token,
		CreatedAt:  time.Now(),
	}
	if err := s.deps.CompressorProvisioner.Provision(ctx, row); err != nil {
		return err
	}
	return s.deps.Routing.SaveProxy(ctx, row)
}

// syncProxyLabel updates the linked proxy's Label to match the provider's
// display name. The proxy row is linked via provider_id (0042). Best-effort,
// called after a rename so the Compressor UI's proxy label tracks the provider
// (operator feedback 2026-08-14). No-ops when the provisioner isn't wired, no
// linked proxy exists, or the label already matches.
func (s *Server) syncProxyLabel(ctx context.Context, providerID int64, label string) error {
	if s.deps.CompressorProvisioner == nil {
		return nil
	}
	proxies, err := s.deps.Routing.Proxies(ctx)
	if err != nil {
		return err
	}
	for _, p := range proxies {
		if p.ProviderID != nil && *p.ProviderID == providerID && p.Label != label {
			p.Label = label
			return s.deps.Routing.SaveProxy(ctx, p)
		}
	}
	return nil
}

// teardownProxyIfUnreferenced tears down service's proxy — unless it's no
// longer linked to excludeProviderID at all (a race, or it was never really
// this provider's; the partial unique index on compressor_proxies.provider_id
// means at most one provider can be actively linked to a given proxy at a
// time, so "some other provider still references it" is no longer a state
// that can occur the way it could under the old, unconstrained
// bidirectional string pair — see the 0042 migration comment). A no-op if
// Deps.CompressorProvisioner is nil (row stays as-is) or no active row exists.
func (s *Server) teardownProxyIfUnreferenced(ctx context.Context, service string, excludeProviderID int64) error {
	proxies, err := s.deps.Routing.Proxies(ctx)
	if err != nil {
		return err
	}
	for _, p := range proxies {
		if p.Service != service || !p.OrphanedAt.IsZero() {
			continue
		}
		if p.ProviderID == nil || *p.ProviderID != excludeProviderID {
			return nil
		}
		if s.deps.CompressorProvisioner != nil {
			if err := s.deps.CompressorProvisioner.Teardown(ctx, p.Unit); err != nil {
				return err
			}
		}
		p.OrphanedAt = time.Now()
		return s.deps.Routing.SaveProxy(ctx, p)
	}
	return nil
}

// toProviderJSON converts a store.ProviderRow to the frozen providerJSON wire
// shape, with health/credits/models set to neutral defaults (BE-3's GET
// endpoint populates those from the provider_state cache).
func toProviderJSON(p store.ProviderRow) providerJSON {
	return providerJSON{
		ID:           p.ID,
		Name:         p.Name,
		APIKeyMasked: maskSecret(p.APIKey),
		BillCurrency: firstNonEmpty(p.BillCurrency, "USD"),
		Health: providerHealthJSON{
			State:  "unknown",
			AsOf:   nil,
			Source: "none",
			Detail: nil,
		},
		Credits: providerCreditsJSON{
			BalanceNative: nil,
			Currency:      nil,
			AsOf:          nil,
			Supported:     false,
		},
		Models:             []providerModelJSON{},
		BillingEnabled:     p.BillingEnabled,
		BillingConsoleURL:  p.BillingConsoleURL,
		CreditsURL:         p.CreditsURL,
		Enabled:            p.Enabled,
		Country:            p.Country,
		DataResidencyGroup: p.DataResidencyGroup,
	}
}

// validProviderName checks the provider display name matches the relaxed
// pattern (Unicode letters/numbers, spaces, .-_) — intentionally looser than
// serviceRE since provider names are display-only (surrogate IDs since 0042).
func validProviderName(name string) bool {
	return providerNameRE.MatchString(name) && len(name) <= 64
}

// ── Request bodies ───────────────────────────────────────────────────────────

// billingSettingsBody is the PUT /api/v1/billing/settings body.
// FxSourceURL/FxRefreshMin use pointers so "absent" (preserve existing) is
// distinct from "explicitly empty/zero" (clear).
type billingSettingsBody struct {
	DisplayCurrency string  `json:"display_currency"`
	FxSourceURL     *string `json:"fx_source_url"`
	FxRefreshMin    *int    `json:"fx_refresh_min"`
}

// providerCreateBody is the POST /api/v1/providers body (§0.9).
type providerCreateBody struct {
	Name         string `json:"name"`
	BillCurrency string `json:"bill_currency"`
	TargetURL    string `json:"target_url"`
	StatusURL    string `json:"status_url"`
	CreditsURL   string `json:"credits_url"`
	// OrgID (BE-3 F4 fix): X-Org-ID header some providers require
	// alongside the API key for their credits/analytics API (confirmed
	// for AI& — see store.ProviderRow.OrgID).
	OrgID string `json:"org_id"`
	// BillingEnabled/BillingConsoleURL (product/QA sprint, 2026-07-29).
	// BillingEnabled is a pointer so an absent field defaults to true
	// (see handleProviderCreate) rather than a bare Go bool's false
	// zero-value.
	BillingEnabled    *bool  `json:"billing_enabled"`
	BillingConsoleURL string `json:"billing_console_url"`
	// Country/DataResidencyGroup (multi-provider routing sprint,
	// 2026-08-06): the provider-level data residency fact, prefilled by the
	// PWA from the curated preset. "" = unknown.
	Country            string `json:"country"`
	DataResidencyGroup string `json:"data_residency_group"`
	// CreateProxy (operator feedback 2026-08-14): when true, the server
	// auto-provisions a Compressor proxy linked to this provider with a derived
	// safe name + label = provider display name. Opt-in (pointer) so existing
	// callers that omit it see no behavior change; the PWA create form sends
	// true by default.
	CreateProxy *bool `json:"create_proxy"`
}

func (b providerCreateBody) validate() map[string]string {
	fields := map[string]string{}
	if !validProviderName(b.Name) {
		fields["name"] = "must start with a letter or number and contain only letters, numbers, spaces, &, and .-_ (max 64)"
	}
	if b.BillCurrency != "" && !currencyRE.MatchString(b.BillCurrency) {
		fields["bill_currency"] = "must be a 3-letter ISO 4217 code (e.g. USD)"
	}
	return fields
}

// providerUpdateBody is the PUT /api/v1/providers/{name} body. Pointer
// fields so absent = preserve, present = set (including empty).
type providerUpdateBody struct {
	// Name, when present, renames the provider (0042 — name is no longer
	// the primary key, so this is now a plain field update like any other).
	Name          *string `json:"name"`
	BillCurrency  *string `json:"bill_currency"`
	TargetURL     *string `json:"target_url"`
	StatusURL     *string `json:"status_url"`
	CreditsURL    *string `json:"credits_url"`
	OrgID         *string `json:"org_id"`
	CompressorProxy *string `json:"compressor_proxy"`
	Model         *string `json:"model"`
	Model2        *string `json:"model2"`
	// BillingEnabled/BillingConsoleURL (product/QA sprint, 2026-07-29).
	BillingEnabled    *bool   `json:"billing_enabled"`
	BillingConsoleURL *string `json:"billing_console_url"`
	// Enabled (multi-provider routing sprint, 2026-08-06): disable a
	// provider without deleting it — the router stops routing its offerings
	// while credentials/offerings/linked proxies stay intact.
	Enabled *bool `json:"enabled"`
	// Country/DataResidencyGroup (same sprint): pointer fields so absent =
	// preserve, present-including-"" = set/clear.
	Country            *string `json:"country"`
	DataResidencyGroup *string `json:"data_residency_group"`
}

func (b providerUpdateBody) validate() map[string]string {
	fields := map[string]string{}
	if b.Name != nil && !validProviderName(*b.Name) {
		fields["name"] = "must start with a letter or number and contain only letters, numbers, spaces, &, and .-_ (max 64)"
	}
	if b.BillCurrency != nil && *b.BillCurrency != "" && !currencyRE.MatchString(*b.BillCurrency) {
		fields["bill_currency"] = "must be a 3-letter ISO 4217 code (e.g. USD)"
	}
	if b.CompressorProxy != nil && *b.CompressorProxy != "" &&
		(!serviceRE.MatchString(*b.CompressorProxy) || len(*b.CompressorProxy) > 64) {
		fields["compressor_proxy"] = "must match ^[a-z][a-z0-9_-]+$ (max 64)"
	}
	return fields
}

// providerKeyBody is the PUT /api/v1/providers/{name}/key body.
type providerKeyBody struct {
	APIKey string `json:"api_key"`
}
