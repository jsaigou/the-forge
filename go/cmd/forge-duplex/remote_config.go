package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jsaigou/the-forge/internal/duplex"
	"github.com/jsaigou/the-forge/internal/duplexremote"
)

// Pool-backed engines (ADR-0029). With a daemon locator the engine is not a fixed address: each session names a tenant, the tenant names a pool
// engine, and the Forge daemon loads that engine on demand and leases it for the session.

// parseTenants reads FORGE_DUPLEX_TENANTS: a comma-separated list of "tenant" or "tenant=pool-engine". The tenant is the name a client asks for and
// the name the engine knows itself by; the pool engine is the name registered in Forge's Engines tab (default: the same). Several entries are how
// one service fronts several speech-to-speech models.
func parseTenants(spec string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, engine, _ := strings.Cut(part, "=")
		name, engine = strings.TrimSpace(name), strings.TrimSpace(engine)
		if engine == "" {
			engine = name
		}
		if name == "" {
			return nil, fmt.Errorf("tenant entry %q has no tenant name", part)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("tenant %q is listed twice", name)
		}
		out[name] = engine
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no tenants listed")
	}
	return out, nil
}

// poolDrivers returns the per-session driver constructor for a set of tenants backed by one locator. base carries the settings every tenant shares
// (step timeout, card renderer). One duplexremote.Tenant per tenant is created once and shared by its sessions, as the single-tenant path does.
func poolDrivers(base duplexremote.Config, loc duplexremote.Locator, tenants map[string]string) func(tenant string) (duplex.DuplexDriver, error) {
	byName := map[string]*duplexremote.Tenant{}
	for name, engine := range tenants {
		cfg := base
		cfg.Tenant, cfg.Engine, cfg.Locator = name, engine, loc
		byName[name] = duplexremote.New(cfg)
	}
	names := make([]string, 0, len(tenants))
	for n := range tenants {
		names = append(names, n)
	}
	sort.Strings(names)
	return func(tenant string) (duplex.DuplexDriver, error) {
		t, ok := byName[tenant]
		if !ok {
			return nil, &duplex.Error{Code: duplex.ErrTenantUnavailable, Fatal: true,
				Message: fmt.Sprintf("unknown tenant %q (this service offers: %s)", tenant, strings.Join(names, ", "))}
		}
		return duplex.NewDriver(t, nil), nil
	}
}
