// SPDX-License-Identifier: Apache-2.0

package httpapi

// compressor_handlers.go — Compressor proxy config, passthrough, and the
// (still-501) proxy lifecycle (Contract 1 §2 #18–21). Split out of
// handlers.go by Sprint 0 (docs/v5-sprint0-contract-freeze.md §0.1); pure
// move, no behavior change. Owner track after split: BE-5.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/compressorctl"
	"github.com/jsaigou/the-forge/internal/store"
)

// handleCompressorConfig returns the proxy + provider configuration (Contract
// 1 §2 #18). Provider API keys are NOT returned here (§0.9 relocated
// provider-key management to the Settings routes); the api_key field is
// always "" — use GET /api/v1/providers for the masked key form.
func (s *Server) handleCompressorConfig(w http.ResponseWriter, r *http.Request) {
	resp := compressorConfigResponse{
		Proxies:   []compressorProxyJSON{},
		Providers: []routerProviderJSON{},
	}
	if s.deps.Routing == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// passthrough_all from settings (default false).
	passthroughAll := false
	if s.deps.Settings != nil {
		if raw, err := s.deps.Settings.Get(ctx, "compressor.passthrough_all"); err == nil {
			_ = json.Unmarshal(raw, &passthroughAll)
		}
	}
	resp.PassthroughAll = passthroughAll

	// external_enabled from settings (default false) — same read shape as
	// passthroughAll, mirroring router.externalCompressorEnabled
	// (internal/router/routing.go), which this package cannot import
	// (Contract 2 ownership table); see compressorConfigResponse's doc
	// comment for why the frontend needs it (Sprint 10).
	if s.deps.Settings != nil {
		if raw, err := s.deps.Settings.Get(ctx, "compressor.external_enabled"); err == nil {
			_ = json.Unmarshal(raw, &resp.ExternalEnabled)
		}
	}

	proxies, err := s.deps.Routing.Proxies(ctx)
	if err == nil {
		for _, p := range proxies {
			active := unitActive(s.snapshot(), p.Unit)
			resp.Proxies = append(resp.Proxies, compressorProxyJSON{
				ID:        p.ID,
				Service:   p.Service,
				Label:     p.Label,
				Port:      p.Port,
				TargetURL: p.TargetURL,
				Unit:      p.Unit,
				Active:    active,
				// p.Passthrough is store.ProxyRow's own column — the same
				// field router.compressorBypassed() (routing.go) actually
				// enforces. Found live 2026-07-29: this used to be
				// `passthroughAll || passthroughServices[p.Service]` reading
				// a compressor.passthrough_services settings-KV list that
				// handleCompressorPassthrough wrote but nothing on the real
				// routing path ever read — every per-proxy bypass toggle a
				// human clicked displayed as "on" here while silently doing
				// nothing to actual traffic. See handleCompressorPassthrough.
				Passthrough: passthroughAll || p.Passthrough,
			})
		}
	}
	// Providers are still listed here for proxy↔provider linkage display, but
	// API key material is no longer returned from the Compressor config endpoint
	// at all (§0.9): provider keys are managed via the Settings routes
	// (POST /api/v1/providers, PUT /api/v1/providers/{name}/key). The masked
	// key is available via GET /api/v1/providers (BE-3). routerProviderJSON
	// keeps the api_key field for shape compat but it is always "" here.
	providers, err := s.deps.Routing.Providers(ctx)
	if err == nil {
		for _, p := range providers {
			resp.Providers = append(resp.Providers, routerProviderJSON{
				ID:            p.ID,
				Name:          p.Name,
				APIKey:        "",
				TargetURL:     p.TargetURL,
				CompressorProxy: p.CompressorProxyName,
				Model:         p.Model,
				Model2:        p.Model2,
			})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCompressorPassthrough updates the global or per-proxy compressor bypass
// (Contract 1 §2 #21). Global (scope "all") persists to store.Settings JSON
// KV (compressor.passthrough_all) — the same key router.passthroughAll()
// reads. Per-proxy (scope "proxy") persists on the proxy's own
// store.ProxyRow.Passthrough column via SaveProxy — the same field
// router.compressorBypassed() (routing.go) actually reads to decide whether to
// route around one proxy.
//
// Per-proxy used to write to a compressor.passthrough_services settings-KV
// list instead (found live 2026-07-29, while building a Console status
// widget for this exact subsystem): handleCompressorConfig's read side
// happened to also consult that same list, so the Settings-page toggle
// looked like it worked — self-consistently wrong — but router.go's actual
// enforcement never read it at all. Every per-proxy bypass a human ever
// clicked was a silent no-op; only the global toggle (which both sides
// agreed used the settings key) ever really worked. Fixed by making both
// sides agree on the store row instead.
func (s *Server) handleCompressorPassthrough(w http.ResponseWriter, r *http.Request) {
	var b compressorPassthroughBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	b2, fields := b.validate()
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	b = b2

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	passthroughAll := false
	if s.deps.Settings != nil {
		if raw, err := s.deps.Settings.Get(ctx, "compressor.passthrough_all"); err == nil {
			_ = json.Unmarshal(raw, &passthroughAll)
		}
	}

	if b.Scope == "all" {
		if s.deps.Settings == nil {
			writeError(w, http.StatusServiceUnavailable, "settings store not wired")
			return
		}
		passthroughAll = *b.Enabled
		allJSON, _ := json.Marshal(passthroughAll)
		if err := s.deps.Settings.Set(ctx, "compressor.passthrough_all", allJSON); err != nil {
			writeInternalError(w, err)
			return
		}
	} else {
		if s.deps.Routing == nil {
			writeError(w, http.StatusServiceUnavailable, "compressor store not wired")
			return
		}
		proxies, err := s.deps.Routing.Proxies(ctx)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		found := false
		for _, p := range proxies {
			if p.Service != b.Service {
				continue
			}
			found = true
			p.Passthrough = *b.Enabled
			if err := s.deps.Routing.SaveProxy(ctx, p); err != nil {
				writeInternalError(w, err)
				return
			}
			break
		}
		if !found {
			writeError(w, http.StatusNotFound, "proxy not found")
			return
		}
	}

	s.audit(r, "human", "compressor_passthrough", b.Service, fmt.Sprintf("enabled=%v", *b.Enabled))
	if s.deps.Publish != nil {
		s.deps.Publish.Publish("config_updated", map[string]any{
			"action": "compressor_passthrough_updated",
		})
	}

	// passthrough_services reflects the real per-proxy store state (not the
	// old, disconnected settings list) for any caller still reading it.
	var passthroughServices []string
	if s.deps.Routing != nil {
		if proxies, err := s.deps.Routing.Proxies(ctx); err == nil {
			for _, p := range proxies {
				if p.Passthrough {
					passthroughServices = append(passthroughServices, p.Service)
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                   true,
		"passthrough_all":      passthroughAll,
		"passthrough_services": passthroughServices,
	})
}

// handleCompressorLifecycle serves both POST /compressor/restart (operator) and
// POST /compressor/proxy/teardown (admin) — Contract 1 §2 #19-20, body
// `{"service": string}` -> `{"ok": true}`. Distinguished by request path
// since both routes share this handler (router.go registration).
//
// Phase 2 (docs/v5-headroom-topology.md §5, decided option (a)): this no
// longer authors systemd unit files (the Phase 9b attempt at that was
// deliberately backed out — see git history — because the write path needs
// root). Instead it only writes an env file to a testuser-writable directory and
// starts/stops/restarts the pre-installed `headroom@<service>` template-unit
// instance (systemd/headroom@.service + polkit/51-headroom.rules, a one-time manual root
// install, not authored by this code) via the D-Bus adapter, over a polkit
// grant extended to compressor-*/headroom@* the same way. Requires both
// Deps.Compressor and Deps.CompressorProvisioner wired; either nil keeps this
// at 501 with an honest reason (Phase 4 stub environment, most tests).
func (s *Server) handleCompressorLifecycle(w http.ResponseWriter, r *http.Request) {
	if s.deps.Routing == nil || s.deps.CompressorProvisioner == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "Compressor proxy lifecycle needs both the proxy store and the provisioner wired",
			"path":  r.URL.Path,
		})
		return
	}
	var b compressorLifecycleBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	b2, fields := b.validate()
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	b = b2

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	proxies, err := s.deps.Routing.Proxies(ctx)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	var row store.ProxyRow
	found := false
	for _, p := range proxies {
		if p.Service == b.Service {
			row, found = p, true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "proxy not found")
		return
	}

	if strings.HasSuffix(r.URL.Path, "/teardown") {
		s.handleCompressorTeardown(ctx, w, r, row)
		return
	}
	s.handleCompressorRestart(ctx, w, r, row)
}

// provisionerFor returns the single wired Provisioner (Sprint 7,
// docs/v5-headroom-replacement.md, dropped the dual-Provisioner dispatch
// this used to do — Sprint 6 confirmed zero live compressor_proxies rows
// still pointed at the old headroom-ai-shaped template, so there is only
// ever one template to own a unit now).
func (s *Server) provisionerFor(unit string) *compressorctl.Provisioner {
	return s.deps.CompressorProvisioner
}

func (s *Server) handleCompressorRestart(ctx context.Context, w http.ResponseWriter, r *http.Request, row store.ProxyRow) {
	if !row.OrphanedAt.IsZero() {
		writeError(w, http.StatusConflict, "proxy is torn down — link/provision it again before restarting")
		return
	}
	if err := s.provisionerFor(row.Unit).Restart(ctx, row.Unit); err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "compressor_proxy_restart", row.Service, "")
	if s.deps.Publish != nil {
		s.deps.Publish.Publish("config_updated", map[string]any{
			"action":  "compressor_proxy_restarted",
			"service": row.Service,
		})
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCompressorTeardown(ctx context.Context, w http.ResponseWriter, r *http.Request, row store.ProxyRow) {
	if err := s.provisionerFor(row.Unit).Teardown(ctx, row.Unit); err != nil {
		writeInternalError(w, err)
		return
	}
	// Soft-orphan rather than hard-delete: keeps the row (and its savings
	// history) visible for audit; router.routing.go's lookup helpers skip
	// any row with a non-zero OrphanedAt, so it's never routed to again.
	row.OrphanedAt = time.Now()
	if err := s.deps.Routing.SaveProxy(ctx, row); err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "compressor_proxy_teardown", row.Service, "")
	if s.deps.Publish != nil {
		s.deps.Publish.Publish("config_updated", map[string]any{
			"action":  "compressor_proxy_torn_down",
			"service": row.Service,
		})
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleCompressorProxyCreate — POST /api/v1/compressor/proxy/create (admin).
// Creates a standalone proxy not linked to a provider (e.g. a local
// upstream, or Sprint 3's shared "external" instance —
// docs/v5-headroom-replacement.md). Provisions the env file + systemd unit
// directly via Provisioner.Provision, then persists the row. For
// provider-linked proxies, use PUT /api/v1/providers/{ref} with
// compressor_proxy instead — that path also sets up the provider↔proxy link.
//
// b.Template is accepted for wire-shape compatibility but unused (Sprint 7,
// docs/v5-headroom-replacement.md, dropped the dual-Provisioner dispatch
// this used to pick between).
func (s *Server) handleCompressorProxyCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Routing == nil || s.deps.CompressorProvisioner == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "Compressor proxy lifecycle needs both the proxy store and the provisioner wired",
			"path":  r.URL.Path,
		})
		return
	}
	var b compressorProxyCreateBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	b2, fields := b.validate()
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	b = b2

	// b.Template is accepted but unused (Sprint 7,
	// docs/v5-headroom-replacement.md, dropped the dual-Provisioner
	// dispatch this used to pick between — there is only ever one
	// Provisioner now).
	provisioner := s.deps.CompressorProvisioner

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	proxies, err := s.deps.Routing.Proxies(ctx)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	for _, p := range proxies {
		if p.Service == b.Service && p.OrphanedAt.IsZero() {
			writeError(w, http.StatusConflict, "proxy service already exists")
			return
		}
	}

	token, err := compressorctl.GenerateToken()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	port := compressorctl.AllocatePort(proxies, compressorctl.DefaultBasePort)
	row := store.ProxyRow{
		Service:   b.Service,
		Label:     b.Label,
		Port:      port,
		TargetURL: b.TargetURL,
		Unit:      provisioner.UnitName(b.Service),
		Token:     token,
		CreatedAt: time.Now(),
	}
	if err := provisioner.Provision(ctx, row); err != nil {
		writeInternalError(w, err)
		return
	}
	if err := s.deps.Routing.SaveProxy(ctx, row); err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "compressor_proxy_create", b.Service, "")
	if s.deps.Publish != nil {
		s.deps.Publish.Publish("config_updated", map[string]any{
			"action":  "compressor_proxy_created",
			"service": b.Service,
		})
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"ok":      true,
		"service": row.Service,
		"port":    row.Port,
		"unit":    row.Unit,
	})
}

// handleCompressorMigrate — POST /api/v1/compressor/proxy/migrate (admin).
// Moves an existing proxy row from its current unit (typically a legacy
// headroom@<service> instance) onto Sprint 3's forge-compress@<service>
// template (docs/v5-headroom-replacement.md's "Store / provisioning /
// routing changes" section): tears down the old unit, provisions a fresh
// instance under the new template with a freshly generated token (never
// reuses a secret across a unit boundary), then updates the row's Unit +
// Token in place — same Service/Label/TargetURL/ProviderID/Port, so a0's
// existing routing (which reads the row fresh every request, never caches
// it) picks up the new unit with no restart of a0 itself required.
//
// Deliberately does NOT touch Passthrough or compressor.passthrough_all —
// migrating which process a service's traffic WOULD go through and
// actually routing traffic through it are separate concerns; flip
// Passthrough via the existing POST /api/v1/compressor/passthrough action
// once the migrated instance is confirmed healthy.
//
// Ordering note: Teardown runs before Provision, so a failure partway
// through (Provision failing after a successful Teardown) leaves the row
// still pointing at the OLD unit name in the store — which is now stopped
// and (if it was itself a template instance) has had its own env file
// removed, so it won't cleanly restart either. This is only safe to run
// against a service currently NOT carrying live traffic (verify
// passthrough_all / this row's own Passthrough first) — this handler
// doesn't check that itself, since "should I migrate this now" is an
// operator judgment call, not something to silently gate on.
func (s *Server) handleCompressorMigrate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Routing == nil || s.deps.CompressorProvisioner == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "Compressor proxy migration needs the proxy store and the provisioner wired",
			"path":  r.URL.Path,
		})
		return
	}
	var b compressorLifecycleBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	b2, fields := b.validate()
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	b = b2

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	proxies, err := s.deps.Routing.Proxies(ctx)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	var row store.ProxyRow
	found := false
	for _, p := range proxies {
		if p.Service == b.Service {
			row, found = p, true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "proxy not found")
		return
	}
	if !row.OrphanedAt.IsZero() {
		writeError(w, http.StatusConflict, "proxy is torn down — link/provision it again before migrating")
		return
	}
	if strings.HasPrefix(row.Unit, s.deps.CompressorProvisioner.TemplatePrefix) {
		writeError(w, http.StatusConflict, "proxy is already on the forge-compress@ template")
		return
	}

	if err := s.provisionerFor(row.Unit).Teardown(ctx, row.Unit); err != nil {
		writeInternalError(w, err)
		return
	}

	newToken, err := compressorctl.GenerateToken()
	if err != nil {
		writeInternalError(w, err)
		return
	}
	newRow := row
	newRow.Unit = s.deps.CompressorProvisioner.UnitName(row.Service)
	newRow.Token = newToken

	if err := s.deps.CompressorProvisioner.Provision(ctx, newRow); err != nil {
		writeInternalError(w, err)
		return
	}
	if err := s.deps.Routing.SaveProxy(ctx, newRow); err != nil {
		writeInternalError(w, err)
		return
	}

	s.audit(r, identity(r).Name, "compressor_proxy_migrated", row.Service, "unit "+row.Unit+" -> "+newRow.Unit)
	if s.deps.Publish != nil {
		s.deps.Publish.Publish("config_updated", map[string]any{
			"action":  "compressor_proxy_migrated",
			"service": row.Service,
			"unit":    newRow.Unit,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": row.Service,
		"unit":    newRow.Unit,
		"port":    newRow.Port,
	})
}
