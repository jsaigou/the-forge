// SPDX-License-Identifier: Apache-2.0

// Package mcp ports forge/mcp_server.py: the HTTP tool-call surface over
// scheduler primitives for fleet-managing agents. Owned by Phase 7 (after
// the scheduler lands).
//
// Frozen external surface (Contract 1 §8): GET /healthz (no auth),
// GET /v1/tools (no auth), POST /v1/tools/<name> (bearer sk-mcp-*, NO
// tailnet bypass — unlike a0). Tools: list_models, ensure_loaded, status,
// can_fit, unload, reservation CRUD. Port 8095 (never 8086 —
// forge-aligner). list_models is a post-freeze addition (audit roadmap
// R2, docs/v5-mcp-audit.md) — read-only, non-mutating, and sourced from
// the same catalog seam a0's /v1/models reads.
//
// Unlike the a0 router, MCP has no tailnet bypass: every POST must carry a
// valid sk-mcp-* key. Per-key identity (the key's name) is what makes
// reservation ownership and queue-position visibility meaningful, so a
// caller is always identified. The auth check is fail-closed — a nil
// Authenticator (skeleton mode) rejects every POST with 401.
//
// Reservation ownership for MCP callers is agent-kind, not "human": the
// key name becomes requested_by/created_by in scheduler calls, and
// ResolveAgentFlags gives agent-created reservations the open (true/true)
// default when the allow_agent_* fields are absent. RBAC stays
// dashboard-only — an MCP Identity carries Role("") which Allows nothing.
package mcp

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// CanFitter is the engine seam MCP needs for the can_fit tool. The
// Scheduler interface deliberately has no CanFit (docs/scheduler.md / the
// Phase 7 handoff), so the engine is injected as a separate dependency.
// engine.Engine satisfies this.
type CanFitter interface {
	CanFit(mode string) (engine.CanFit, error)
}

// ModelLister is the read-only catalog seam MCP needs for list_models:
// exactly the two listings a0's BuildModelsResponse consumes
// (internal/router/catalog.go), so the MCP inventory and GET /v1/models
// cannot drift. store.Catalog satisfies it.
type ModelLister interface {
	ListConfigs(ctx context.Context) ([]store.Config, error)
	ListOfferings(ctx context.Context) ([]store.Offering, error)
}

// Deps are the frozen-interface dependencies (Contract 2). All fields are
// optional (nil → fail-closed/unavailable); Phase 9 swaps real
// implementations in cmd/forge and nowhere else.
type Deps struct {
	// Sched is the on-demand load/status/reservation core, shared with the
	// dashboard and a0. Nil → tool calls that need it return 503
	// "scheduler not wired".
	Sched sched.Scheduler
	// Engine answers the can_fit read-only fit check (CanFit lives on the
	// engine, not the Scheduler). Nil → can_fit returns 503 "engine not
	// wired".
	Engine CanFitter
	// Auth verifies inbound sk-mcp-* bearer tokens. There is NO tailnet
	// bypass — nil Auth means every POST /v1/tools/* is 401 (fail-closed).
	Auth authz.Authenticator
	// Audit records MCP-originated fleet mutations (audit roadmap R3,
	// docs/v5-mcp-audit.md), with the authenticated key name as actor.
	// Nil → mutations proceed unaudited (skeleton mode); audit logging
	// never fails a tool call.
	Audit store.Audit
	// Catalog backs the read-only list_models inventory (visible local
	// Configs + enabled remote Offerings — the same seam a0's
	// BuildModelsResponse reads). Nil → list_models returns 503 "catalog
	// not wired".
	Catalog ModelLister
}

// Server is the MCP server.
type Server struct {
	deps Deps
}

// New returns a skeleton MCP server (healthz + tool discovery only; every
// authenticated POST is 401 fail-closed) — kept zero-arg so cmd/forge's
// `mcp.New().Handler()` call site stays unchanged. Tests and Phase 9
// wiring use NewWithDeps for the full surface.
func New() *Server { return &Server{} }

// NewWithDeps returns an MCP server wired with real dependencies. This is
// the constructor tests and Phase 9 use; New() is the skeleton entry point.
func NewWithDeps(deps Deps) *Server {
	return &Server{deps: deps}
}

// Handler serves the frozen MCP surface (Contract 1 §8):
//
//   - GET  /healthz            — unauthenticated liveness (systemd ExecStartPost)
//   - GET  /v1/tools           — unauthenticated tool discovery listing
//   - POST /v1/tools/{name}    — bearer sk-mcp-* required, NO tailnet bypass
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","service":"mcp"}`))
	})

	mux.HandleFunc("GET /v1/tools", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"tools": tools})
	})

	mux.HandleFunc("POST /v1/tools/{name}", s.callTool)

	return mux
}

// authenticate verifies the sk-mcp-* bearer token. NO tailnet bypass: a
// missing/invalid token, or a nil Authenticator (skeleton mode), always
// fails. Returns the caller's agent identity name on success.
func (s *Server) authenticate(r *http.Request) (agentName string, ok bool) {
	if s.deps.Auth == nil {
		return "", false
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(header, "Bearer ")
	id, err := s.deps.Auth.VerifyBearerFrom(r.Context(), clientIP(r), token, authz.KindMCP)
	if err != nil {
		return "", false
	}
	return id.Name, true
}

// clientIP strips the port from r.RemoteAddr for use as a rate-limit key.
// Falls back to the raw value if it isn't in host:port form.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// callTool authenticates, resolves the tool name, and dispatches. Auth is
// checked first (401 before 404) so an unauthenticated probe never learns
// which tools exist beyond the public /v1/tools listing.
func (s *Server) callTool(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}

	name := r.PathValue("name")
	fn, known := dispatch[name]
	if !known {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error":     "unknown_tool",
			"message":   "Unknown tool: " + name,
			"available": toolNames(),
		})
		return
	}
	fn(s, w, r, agent)
}
