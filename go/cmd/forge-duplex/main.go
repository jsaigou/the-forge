// Command forge-duplex serves the duplex audio WebSocket transport.
//
// Env:
//
//	FORGE_DUPLEX_LISTEN            listen addr (default 127.0.0.1:8086)
//	FORGE_DUPLEX_DRIVER            driver name: "loopback" (default), "mock" or "remote"
//	FORGE_DUPLEX_SAFETY            text safety policy: "" (off, default) or "loop"
//	FORGE_DUPLEX_ENGINE_URL        remote only: engine base URL (default http://127.0.0.1:8090)
//	FORGE_DUPLEX_ENGINE_TENANT     remote only: tenant name on the engine (required unless FORGE_DUPLEX_TENANTS is set)
//	FORGE_DUPLEX_DAEMON_URL        remote, pool-backed: the Forge daemon API (default http://127.0.0.1:5000)
//	FORGE_DUPLEX_DAEMON_KEY        remote, pool-backed: a duplex-scoped Forge key (env file, never argv). Setting it turns the pool on: the engine
//	                               is found, loaded on demand and leased through the daemon (ADR-0029) instead of read from FORGE_DUPLEX_ENGINE_URL
//	FORGE_DUPLEX_TENANTS           pool-backed: "tenant" or "tenant=pool-engine", comma-separated; the tenants clients may ask for
//	FORGE_DUPLEX_ENGINE_TOKEN      remote only: optional engine bearer key (env file, never argv)
//	FORGE_DUPLEX_CARD_RENDER       remote only: "haru" renders the structured card to text (default off)
//	FORGE_DUPLEX_STEP_TIMEOUT_MS   remote only: per-step deadline (default 1000)
//	FORGE_DUPLEX_TOKEN             comma-separated bearer tokens; empty = tailnet/localhost bypass
//	FORGE_DUPLEX_MAX_LANES         concurrent sessions (default 4)
//	FORGE_DUPLEX_MAX_MINUTES       per-session limit (default 30)
//	FORGE_DUPLEX_IDLE_TIMEOUT_S    no-input timeout (default 10)
//	FORGE_DUPLEX_STATS_INTERVAL_S  stats cadence (default 10)
//	FORGE_DUPLEX_DRAIN_TIMEOUT_MS  close/cancel flush window (default 1000)
//	FORGE_DUPLEX_START_TIMEOUT_S   driver.Start budget (default 120)
//	FORGE_DUPLEX_TICKET_TTL_S      short-lived ticket lifetime (default 30)
//	FORGE_DUPLEX_ALLOWED_ORIGINS   extra browser origins, comma-separated
//	FORGE_DUPLEX_ALLOW_ANY_ORIGIN  set "1" to disable the Origin check
//	FORGE_DUPLEX_ALLOWED_NETS      extra CIDRs/IPs beyond loopback + tailnet
//
// The endpoint is GET /v1/audio/duplex (WebSocket), /healthz is liveness and
// POST /v1/audio/duplex/ticket mints a single-use ticket. No long-lived secret
// is accepted in the URL; the browser client (WS3) is not bundled here.
package main

import (
	"context"
	"log"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jsaigou/the-forge/internal/duplex"
	"github.com/jsaigou/the-forge/internal/duplexcard"
	"github.com/jsaigou/the-forge/internal/duplexremote"
	"github.com/jsaigou/the-forge/internal/duplexsafety"
)

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Fatalf("forge-duplex: %s=%q is not a positive whole number", key, v)
	}
	return n
}

func envMillis(key string, def int) time.Duration {
	return time.Duration(envInt(key, def)) * time.Millisecond
}

func envBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes"
}

func splitCSV(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseNets(v string) []netip.Prefix {
	var out []netip.Prefix
	for _, raw := range splitCSV(v) {
		if p, err := netip.ParsePrefix(raw); err == nil {
			out = append(out, p)
			continue
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			log.Fatalf("forge-duplex: FORGE_DUPLEX_ALLOWED_NETS entry %q is not a CIDR or IP", raw)
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out
}

// driverFactory returns the per-session driver constructor. Pool-backed remote mode returns the tenant-aware one (second value) and nil for the first.
func driverFactory(name string) (func() duplex.DuplexDriver, func(string) (duplex.DuplexDriver, error)) {
	if name == "remote" && os.Getenv("FORGE_DUPLEX_DAEMON_KEY") != "" {
		return nil, poolBackedDrivers()
	}
	return plainDriverFactory(name), nil
}

// poolBackedDrivers builds the tenant-aware constructor from the environment (ADR-0029).
func poolBackedDrivers() func(string) (duplex.DuplexDriver, error) {
	spec := os.Getenv("FORGE_DUPLEX_TENANTS")
	if spec == "" {
		spec = os.Getenv("FORGE_DUPLEX_ENGINE_TENANT")
	}
	tenants, err := parseTenants(spec)
	if err != nil {
		log.Fatalf("forge-duplex: pool-backed remote mode needs FORGE_DUPLEX_TENANTS (or FORGE_DUPLEX_ENGINE_TENANT): %v", err)
	}
	base := duplexremote.Config{
		Token:       os.Getenv("FORGE_DUPLEX_ENGINE_TOKEN"),
		StepTimeout: envMillis("FORGE_DUPLEX_STEP_TIMEOUT_MS", 1000),
	}
	switch render := env("FORGE_DUPLEX_CARD_RENDER", ""); render {
	case "":
	case "haru":
		base.CardRenderer = func(card map[string]any) (any, error) {
			text, err := duplexcard.Render(card)
			if err != nil {
				return nil, err
			}
			return text, nil
		}
	default:
		log.Fatalf("forge-duplex: FORGE_DUPLEX_CARD_RENDER=%q is not built in; v1 ships haru", render)
	}
	loc := &duplexremote.DaemonLocator{BaseURL: env("FORGE_DUPLEX_DAEMON_URL", "http://127.0.0.1:5000"), Key: os.Getenv("FORGE_DUPLEX_DAEMON_KEY")}
	return poolDrivers(base, loc, tenants)
}

func plainDriverFactory(name string) func() duplex.DuplexDriver {
	switch name {
	case "", "loopback":
		return func() duplex.DuplexDriver {
			return duplex.NewDriver(duplex.NewLoopbackTenant(), nil)
		}
	case "mock":
		return newMockDriver
	case "remote":
		name := os.Getenv("FORGE_DUPLEX_ENGINE_TENANT")
		if name == "" {
			log.Fatalf("forge-duplex: FORGE_DUPLEX_DRIVER=remote needs FORGE_DUPLEX_ENGINE_TENANT")
		}
		cfg := duplexremote.Config{
			BaseURL:     env("FORGE_DUPLEX_ENGINE_URL", "http://127.0.0.1:8090"),
			Tenant:      name,
			Token:       os.Getenv("FORGE_DUPLEX_ENGINE_TOKEN"),
			StepTimeout: envMillis("FORGE_DUPLEX_STEP_TIMEOUT_MS", 1000),
		}
		switch render := env("FORGE_DUPLEX_CARD_RENDER", ""); render {
		case "":
		case "haru":
			cfg.CardRenderer = func(card map[string]any) (any, error) {
				text, err := duplexcard.Render(card)
				if err != nil {
					return nil, err
				}
				return text, nil
			}
		default:
			log.Fatalf("forge-duplex: FORGE_DUPLEX_CARD_RENDER=%q is not built in; v1 ships haru", render)
		}
		tenant := duplexremote.New(cfg)
		return func() duplex.DuplexDriver { return duplex.NewDriver(tenant, nil) }
	default:
		log.Fatalf("forge-duplex: FORGE_DUPLEX_DRIVER=%q is not built in; v1 ships loopback, mock and remote", name)
		return nil
	}
}

func safetyFactory(name string) func() duplexsafety.Policy {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "off", "none":
		return nil
	case "loop":
		return func() duplexsafety.Policy {
			return duplexsafety.NewLoopDetector(duplexsafety.Default())
		}
	default:
		log.Fatalf("forge-duplex: FORGE_DUPLEX_SAFETY=%q is not built in; v1 ships off and loop", name)
		return nil
	}
}

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.LUTC)

	newDriver, newDriverFor := driverFactory(env("FORGE_DUPLEX_DRIVER", "loopback"))
	srv := New(Options{
		NewDriver:      newDriver,
		NewDriverFor:   newDriverFor,
		NewSafety:      safetyFactory(env("FORGE_DUPLEX_SAFETY", "")),
		MaxLanes:       envInt("FORGE_DUPLEX_MAX_LANES", defaultMaxLanes),
		MaxMinutes:     envInt("FORGE_DUPLEX_MAX_MINUTES", defaultMaxMinutes),
		IdleTimeout:    time.Duration(envInt("FORGE_DUPLEX_IDLE_TIMEOUT_S", int(defaultIdleTimeout/time.Second))) * time.Second,
		StatsInterval:  time.Duration(envInt("FORGE_DUPLEX_STATS_INTERVAL_S", int(defaultStatsInterval/time.Second))) * time.Second,
		DrainTimeout:   envMillis("FORGE_DUPLEX_DRAIN_TIMEOUT_MS", int(defaultDrainTimeout/time.Millisecond)),
		StartTimeout:   time.Duration(envInt("FORGE_DUPLEX_START_TIMEOUT_S", int(defaultStartTimeout/time.Second))) * time.Second,
		TicketTTL:      time.Duration(envInt("FORGE_DUPLEX_TICKET_TTL_S", int(defaultTicketTTL/time.Second))) * time.Second,
		Tokens:         splitCSV(os.Getenv("FORGE_DUPLEX_TOKEN")),
		AllowedOrigins: splitCSV(os.Getenv("FORGE_DUPLEX_ALLOWED_ORIGINS")),
		AllowAnyOrigin: envBool("FORGE_DUPLEX_ALLOW_ANY_ORIGIN"),
		AllowedNets:    parseNets(os.Getenv("FORGE_DUPLEX_ALLOWED_NETS")),
		DefaultTenant:  env("FORGE_DUPLEX_DEFAULT_TENANT", "loopback"),
		Logger:         logger,
	})

	listen := env("FORGE_DUPLEX_LISTEN", "127.0.0.1:8086")
	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		srv.Kill("shutdown")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()

	if len(srv.opts.Tokens) == 0 {
		logger.Printf("forge-duplex: no FORGE_DUPLEX_TOKEN set; relying on the loopback/tailnet gate")
	}
	logger.Printf("forge-duplex: listening on %s (max_lanes=%d max_minutes=%d)", listen, srv.opts.MaxLanes, srv.opts.MaxMinutes)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatalf("forge-duplex: %v", err)
	}
}
