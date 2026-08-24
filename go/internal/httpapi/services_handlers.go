// SPDX-License-Identifier: Apache-2.0

package httpapi

// services_handlers.go — always-on + service-mode infra services list
// (Contract 1 §2 #6). Split out of handlers.go by Sprint 0
// (docs/v5-sprint0-contract-freeze.md §0.1); pure move, no behavior change.
// Owner track after split: BE-4 — §0.5 fixes the A0 wiring (the dead
// forge-router unit), renames "A0 Proxy" → "LLM Proxy (A0)", and fills the
// frozen detail + compressor_passthrough fields.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/smith"
)

// handleInfraServices returns the always-on + service-mode infrastructure
// services list (Contract 1 §2 #6).
func (s *Server) handleInfraServices(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	cfg := s.deps.Config()

	// Compressor passthrough state (§0.5): read from the same settings source
	// as the compressor config handler so the A0 row reflects the live bypass.
	passthroughAll := s.compressorPassthroughAll()
	localEnabled := s.compressorLocalEnabled()
	compressorRows, deadOnPath := s.compressorServiceRows(r.Context(), snap, passthroughAll, localEnabled)

	services := []infraService{}

	// A0 router (§0.5 root-cause fix): the forge-router unit no longer
	// exists in V5 — the router is a subsystem of forge-daemon. Active
	// state derives from the daemon being up (real unit state when the
	// collector probes forge-daemon; otherwise the handler responding
	// means the daemon is serving). Renamed "A0 Proxy" → "LLM Proxy (A0)"
	// (2026-07-29) → "LLM Router (A0)" (operator feedback 2026-08-14).
	//
	// Found live 2026-07-29: this row used to report Active purely from the
	// daemon process, with zero awareness of whether the Compressor proxies
	// A0 actually routes every request through were reachable. After a host
	// reboot took down every headroom@ unit, this row kept showing
	// healthy/"routing" on Console while every single a0 chat-completion
	// request was failing with a transport error — a literal black hole
	// reported as green. Active now also requires that no proxy genuinely on
	// the live routing path (not orphaned, not bypassed, and for "local"
	// only when local fronting is enabled) is down.
	routerPort := 8085
	active := a0Active(snap) && len(deadOnPath) == 0
	detail := ""
	if active {
		if passthroughAll {
			detail = "passthrough on"
		} else {
			detail = "routing"
		}
	} else if len(deadOnPath) > 0 {
		detail = "compressor down: " + strings.Join(deadOnPath, ", ")
	}
	services = append(services, infraService{
		Name:                "LLM Router (A0)",
		Unit:                ptrString("forge-daemon"),
		Port:                &routerPort,
		Active:              active,
		Kind:                "systemd",
		ModeKey:             nil,
		Detail:              ptrString(detail),
		CompressorPassthrough: &passthroughAll,
	})

	// One row per non-orphaned Compressor proxy (new 2026-07-29, same incident
	// as above): separate, per-proxy visibility so an operator can tell
	// *which* proxy is down rather than only "A0 is degraded somehow".
	services = append(services, compressorRows...)

	// STT + Embedding (always-on systemd services, declared via [ports]).
	if cfg != nil {
		if p, ok := cfg.Ports["stt"]; ok {
			services = append(services, infraService{
				Name: "STT", Unit: ptrString("forge-stt"),
				Port: &p, Active: unitActive(snap, "forge-stt"),
				Kind: "systemd", ModeKey: nil,
				Logo: serviceVendorLogo("STT"),
			})
		}
		if p, ok := cfg.Ports["embedding"]; ok {
			services = append(services, infraService{
				Name: "Embedding", Unit: ptrString("forge-embedding"),
				Port: &p, Active: unitActive(snap, "forge-embedding"),
				Kind: "systemd", ModeKey: nil,
				Logo: serviceVendorLogo("Embedding"),
			})
		}
		// Aligner (Qwen3 aligner, pre-existing on ForgeHost, unrelated to slots —
		// port 8086). Previously a
		// frontend-hardcoded stub (ServicesBar.tsx's ALIGNER_STUB) because no
		// backend row ever existed for it.
		if p, ok := cfg.Ports["aligner"]; ok {
			services = append(services, infraService{
				Name: "Aligner", Unit: ptrString("forge-aligner"),
				Port: &p, Active: unitActive(snap, "forge-aligner"),
				Kind: "systemd", ModeKey: nil,
				Logo: serviceVendorLogo("Aligner"),
			})
		}
	}

	// TTS (always-on systemd service).
	ttsPort := 8082
	ttsUnit := "forge-tts"
	if cfg != nil && cfg.Server.TTSUnit != "" {
		ttsUnit = cfg.Server.TTSUnit
	}
	services = append(services, infraService{
		Name: "TTS", Unit: ptrString(ttsUnit),
		Port: &ttsPort, Active: unitActive(snap, ttsUnit),
		Kind: "systemd", ModeKey: nil,
		Logo: serviceVendorLogo("TTS"),
	})

	// Service modes from config (e.g. ComfyUI). Icon comes from the mode's
	// own catalog-backed services.icon (config.Mode.Icon, plumbed through by
	// merged_config.go) — these are catalog rows, unlike the fixed services
	// above, so no literal map is needed here.
	if cfg != nil {
		for name, m := range cfg.Modes {
			if m.Type != "service" {
				continue
			}
			var unit *string
			if m.Unit != "" {
				u := m.Unit
				unit = &u
			}
			mk := name
			var logo *string
			if m.Icon != "" {
				logo = ptrString(m.Icon)
			}
			services = append(services, infraService{
				Name:    firstNonEmpty(m.Label, name),
				Unit:    unit,
				Port:    nil,
				Active:  unitActive(snap, deref(unit, "")),
				Kind:    "service_mode",
				ModeKey: &mk,
				Logo:    logo,
			})
		}
	}

	writeJSON(w, http.StatusOK, infraServicesResponse{Services: services})
}

// serviceVendorLogoBySlug maps a fixed infra service's display Name to an
// Icon manifest slug (web/src/assets/icons/manifest.ts) for the model it
// actually runs. Ground-truthed live against ForgeHost's real systemd unit files
// 2026-07-31 (Console polish pass), not guessed from doc comments:
//   - STT (forge-stt): parakeet-server --model .../nemotron-3.5-asr-streaming-0.6b-f16.gguf
//   - Embedding (forge-embedding): llama-server -m .../Qwen3-Embedding-0.6B-Q8_0.gguf
//   - Aligner (forge-aligner): aligner_server.py MODEL_NAME = "Qwen/Qwen3-ForcedAligner-0.6B"
//   - TTS (forge-tts): tts_server.py MODEL_IDS = "Qwen/Qwen3-TTS-12Hz-1.7B-*"
//
// These are bare [ports] entries with no model metadata anywhere in the
// store (unlike catalog-backed service modes, e.g. ComfyUI below), so a
// literal map is the honest fix rather than a workaround for a missing
// dynamic mechanism.
// Operator feedback 2026-08-14: the icon names the MODEL, not the company —
// Embedding/Aligner/TTS all run Qwen models, so they get the qwen mark
// (STT runs Nvidia Parakeet, hence nvidia).
var serviceVendorLogoBySlug = map[string]string{
	"STT":       "nvidia",
	"Embedding": "qwen",
	"Aligner":   "qwen",
	"TTS":       "qwen",
}

func serviceVendorLogo(name string) *string {
	if slug, ok := serviceVendorLogoBySlug[name]; ok {
		return ptrString(slug)
	}
	return nil
}

// a0Active reports whether the A0/LLM-Proxy router subsystem is serving
// (§0.5). In V5 the router runs inside forge-daemon (not a separate
// forge-router unit), so it is active iff the daemon is up — which is
// implied whenever this handler can respond. When the collector probes the
// forge-daemon unit (ExtraUnits), its real systemd state is used; when the
// unit is absent from the snapshot (not probed), the handler being callable
// means the daemon is up, so active is true. This is the root-cause fix for
// the "A0 shows stopped but is running" bug: forge-router is a dead unit
// name in V5.
func a0Active(snap *collector.Snapshot) bool {
	if snap != nil {
		if u, ok := snap.Units["forge-daemon"]; ok {
			return u.Active()
		}
	}
	return true
}

// compressorPassthroughAll reads the global compressor passthrough setting
// (compressor.passthrough_all) from store.Settings. Returns false when the
// settings store is not wired or the key is absent — the same default the
// compressor config handler uses (§0.5 surfaces this on the A0 row).
func (s *Server) compressorPassthroughAll() bool {
	if s.deps.Settings == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var v bool
	if raw, err := s.deps.Settings.Get(ctx, "compressor.passthrough_all"); err == nil {
		_ = json.Unmarshal(raw, &v)
	}
	return v
}

// compressorLocalEnabled reads compressor.local_enabled from store.Settings —
// mirrors router.localCompressorEnabled (internal/router/routing.go), which
// this package cannot import (Contract 2 ownership table), so the read is
// duplicated rather than shared. Default false, matching the router side.
func (s *Server) compressorLocalEnabled() bool {
	if s.deps.Settings == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var v bool
	if raw, err := s.deps.Settings.Get(ctx, "compressor.local_enabled"); err == nil {
		_ = json.Unmarshal(raw, &v)
	}
	return v
}

// compressorServiceRows returns one infraService row per non-orphaned Compressor
// proxy, for Console visibility (found live 2026-07-29 — see
// handleInfraServices's A0-row comment). Also returns the service names of
// any proxy that is genuinely on the live routing path right now — not
// orphaned, not bypassed (globally or individually), and for "local" only
// when local fronting is actually turned on — but whose systemd unit isn't
// active. That second list is what a "healthy A0" claim would be lying
// about, so handleInfraServices folds it into the A0 row's own Active flag.
func (s *Server) compressorServiceRows(ctx context.Context, snap *collector.Snapshot, passthroughAll, localEnabled bool) (rows []infraService, deadOnPath []string) {
	hp := s.deps.Routing
	if hp == nil {
		return nil, nil
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	proxies, err := hp.Proxies(pctx)
	if err != nil {
		return nil, nil
	}
	for _, p := range proxies {
		if !p.OrphanedAt.IsZero() {
			continue
		}
		active := unitActive(snap, p.Unit)
		bypassed := passthroughAll || p.Passthrough
		onPath := !bypassed && (p.Service != "local" || localEnabled)
		if onPath && !active {
			deadOnPath = append(deadOnPath, p.Service)
		}
		detail := "compressing"
		state := "compressing"
		switch {
		case bypassed:
			detail = "bypassed"
			state = "bypassed"
		case p.Service == "local" && !localEnabled:
			detail = "not in use — local fronting off"
			state = "idle"
		case !active:
			// Same condition as onPath && !active above (deadOnPath) —
			// on-path but the unit isn't up, a real failure rather than an
			// intentional bypass or idle state.
			detail = "down"
			state = "down"
		}
		unit, port := p.Unit, p.Port
		health, rssBytes, restarts := s.compressorHealth(ctx, p.Service)
		rows = append(rows, infraService{
			Name:             "Compressor (" + p.Service + ")",
			Unit:             &unit,
			Port:             &port,
			Active:           active,
			Kind:             "systemd",
			ModeKey:          nil,
			Detail:           ptrString(detail),
			CompressorState:    state,
			CompressorResourceHealth:   health,
			CompressorRSSBytes: rssBytes,
			CompressorRestarts: restarts,
		})
	}
	return rows, deadOnPath
}

// compressorHealth reads a proxy's recent compressor_samples window and
// classifies it via smith.ClassifyCompressorHealth (Sprint 4) — the same
// judgment the compressor_health check applies, so the Dashboard tile and
// smith never disagree about what "healthy" means. Best-effort: a nil
// Compressors dependency or a read error reads as "unknown", never an error
// surfaced to the Dashboard.
func (s *Server) compressorHealth(ctx context.Context, service string) (health string, rssBytes, restarts *int64) {
	if s.deps.Compressors == nil {
		return "", nil, nil
	}
	th := smith.DefaultThresholds()
	if s.deps.Smith != nil {
		th = s.deps.Smith.Thresholds(ctx)
	}
	windowHours := th.CompressorRSSWindowHours
	if windowHours <= 0 {
		windowHours = 6
	}
	since := time.Now().Add(-time.Duration(windowHours * float64(time.Hour)))
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	samples, err := s.deps.Compressors.Range(cctx, service, since)
	if err != nil {
		return "unknown", nil, nil
	}
	result := smith.ClassifyCompressorHealth(samples, th)
	if result.Status == "unknown" {
		return result.Status, nil, nil
	}
	rss := result.WindowEnd.RSSBytes
	nr := int64(result.WindowEnd.NRestarts)
	return result.Status, &rss, &nr
}
