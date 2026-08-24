// SPDX-License-Identifier: Apache-2.0

// Package store owns all app-mutated state (V5 design decision 1,
// docs/v5-plan.md): one SQLite database, single-writer discipline through
// this API. Owned by track B (Phase 3). Schema v1 is Contract 3
// (migrations/0001_init.sql, annotated in docs/v5-store-schema.md).
//
// The Store interface is Contract 2 — frozen; additive changes go through
// the Phase 9 integration owner.
package store

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by lookups that match no row.
var ErrNotFound = errors.New("store: not found")

// Store groups the per-domain surfaces. Phase 3 implements it on *DB;
// tracks test against fakes of the sub-interfaces they consume.
//
// Sprint 0 §0.3 (BE-3) added the Providers surface (read side of the
// provider_state cache + the extended router_providers columns). Additive
// Contract 2 amendment; existing sub-interfaces are unchanged.
type Store interface {
	Users() Users
	Sessions() Sessions
	Keys() Keys
	Sched() Sched
	Usage() Usage
	Routing() Routing
	Providers() Providers
	Settings() Settings
	Audit() Audit
	Metrics() Metrics
	// Sprint 0-AUTH (§4): identity linking + TOTP secrets + WebAuthn creds
	// + recovery codes (Phase C).
	IdentityLinks() IdentityLinks
	TOTP() TOTP
	WebAuthnCredentials() WebAuthnCredentials
	RecoveryCodes() RecoveryCodes
	// PROFILE track (docs/v5-profiling-benchmarks.md): measured memory +
	// T/s profiles per (mode, n_ctx, backend, parallel).
	ModelProfiles() ModelProfiles
	// PrefillStats (Compressor local-savings prefill sprint, 2026-08-06):
	// durable per-mode real prefill throughput, accumulated passively from
	// ordinary traffic rather than a destructive PROFILE run.
	PrefillStats() PrefillStats
	// MODEL CATALOG track (docs/v5-modes-config-editable.md, ADRs 0001–0005):
	// the store-backed model database replacing models.toml + modes + router
	// remote routes. Phase 1: write (seed) + list (verification). Phase 2/3
	// extend with read-site migration + CRUD.
	Catalog() Catalog
	// Notifications persists collector-detected alerts (product/QA sprint,
	// 2026-07-29) with acknowledge/dismiss, replacing the previously-
	// unconsumed status.alerts feed as the Dashboard's notifications source.
	Notifications() Notifications
	// Favorites persists per-operator starred config cards (product/QA
	// sprint, 2026-07-29 — Console "Choose a config" starring).
	Favorites() Favorites
	// Compressors persists collector-observed process health for Compressor-
	// shaped proxies (Sprint 4, resource bounding + monitoring). Separate
	// from Routing's savings-counter surface — see compressor_samples'
	// doc comment (migration 0057) for why it can't share that table.
	Compressors() Compressors
	// SchedulerJobs persists the cron-style forced-load definitions the
	// daemon's jobs runner fires through sched.EnsureLoaded (P3 track,
	// migration 0066).
	SchedulerJobs() SchedulerJobs
	Close() error
}

// User is a dashboard account (recreated by the first-run wizard — V4 users
// are not migrated).
type User struct {
	ID           int64
	Username     string
	PasswordHash string // Argon2id encoded; never logged
	Role         string // viewer | operator | admin
	CreatedAt    time.Time
	Disabled     bool
}

type Users interface {
	Create(ctx context.Context, u User) (int64, error)
	ByUsername(ctx context.Context, username string) (User, error)
	List(ctx context.Context) ([]User, error)
	Delete(ctx context.Context, id int64) error
}

// Session is a logged-in browser session.
type Session struct {
	ID         string
	UserID     int64
	CSRFToken  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	RemoteAddr string
	UserAgent  string
	// Sprint 0-AUTH (§4): assurance level + elevation timestamp + network
	// principal if bootstrapped from a trusted network identity. Assurance
	// defaults to "password" (the migration column DEFAULT) for sessions
	// created by the existing login flow; network-bootstrapped sessions
	// start at "network".
	Assurance        string
	AssuranceAt      time.Time
	NetworkPrincipal string
}

type Sessions interface {
	Create(ctx context.Context, s Session) error
	Get(ctx context.Context, id string) (Session, error)
	Touch(ctx context.Context, id string, at time.Time) error
	Delete(ctx context.Context, id string) error
	DeleteExpired(ctx context.Context, now time.Time) (int64, error)
	// ElevateSession rotates the session ID, CSRF token, and sets the
	// assurance level + timestamp in one UPDATE (§3.5 step-up). Returns
	// ErrNotFound when oldID does not exist.
	ElevateSession(ctx context.Context, oldID, newID, newCSRF, assurance string, at time.Time) error
}

// APIKey is one bearer key row; the token format is
// sk-<kind>-<keyid>-<secret> (see authz.ParseToken).
type APIKey struct {
	KeyID      string
	Kind       string // forge | router | mcp
	Name       string
	SecretHash string // Argon2id encoded; never logged
	Role       string // forge kind only; "" otherwise
	CreatedAt  time.Time
	LastUsedAt time.Time
	RevokedAt  time.Time // zero = active
}

type Keys interface {
	Create(ctx context.Context, k APIKey) error
	Get(ctx context.Context, keyid string) (APIKey, error)
	List(ctx context.Context, kind string) ([]APIKey, error)
	Revoke(ctx context.Context, keyid string) error
	TouchUsed(ctx context.Context, keyid string, at time.Time) error
}

// Sched persists scheduler state for restart recovery (in-memory state is
// authoritative at runtime) plus reservations. Row shapes mirror
// sched.Ticket / sched.Reservation.
type Sched interface {
	SaveSlot(ctx context.Context, slot, mode string, loadedAt time.Time) error
	Slots(ctx context.Context) (map[string]string, error)
	SaveTicket(ctx context.Context, t QueueRow) error
	DeleteTicket(ctx context.Context, ticketID string) error
	Queue(ctx context.Context) ([]QueueRow, error)
	SaveReservation(ctx context.Context, r ReservationRow) error
	DeleteReservation(ctx context.Context, label string) error
	Reservations(ctx context.Context) ([]ReservationRow, error)
}

type QueueRow struct {
	TicketID    string
	Model       string
	RequestedBy string
	TargetSlot  string
	Status      string
	SmallJob    bool
	Priority    int
	EnqueuedAt  time.Time
	UpdatedAt   time.Time
}

type ReservationRow struct {
	// ID is the surrogate PK (Phase 6 surrogate-key migration, 0042). Label
	// stays the operator-facing handle (reservation routes are still
	// keyed on it); ID exists for internal joins/future FK use only.
	ID                     int64
	Label                  string
	Model                  string
	Start                  time.Time
	End                    time.Time
	Scope                  string
	Bay                    string
	CreatedBy              string
	AllowAgentReschedule   bool
	AllowAgentCancellation bool
	CreatedAt              time.Time
}

// Usage records and aggregates the usage/cost event stream and mode history.
type Usage interface {
	Record(ctx context.Context, e UsageEvent) error
	Events(ctx context.Context, since time.Time, limit int) ([]UsageEvent, error)
	RecordHistory(ctx context.Context, h ModeHistoryEntry) error
	History(ctx context.Context, mode string, limit int) ([]ModeHistoryEntry, error)
	TokenActivity(ctx context.Context, since time.Time) ([]TokenActivityRow, error)
}

// TokenActivityRow is one token-bearing usage_events row (Sprint L activity
// heatmap) — narrower than Events' full UsageEvent, since the handler only
// needs a timestamp to bucket by, a token total to sum, and the kind to
// split local vs. external (request count per bucket/scope is just the row
// count, so it isn't carried per-row).
type TokenActivityRow struct {
	TS     time.Time
	Kind   string // "inference" (local) or "external_request" (remote)
	Tokens int64
}

type UsageEvent struct {
	TS    time.Time
	Kind  string
	Model string
	Slot  string
	// ProviderID is the FK to write for a remote/external event (Phase 6
	// surrogate-key migration, 0042); nil for local events, exactly like
	// the old Provider=="" convention. ProviderName is a read-only
	// join-derived projection populated by Events()/TokenActivity() for
	// display — set it and it is ignored on write.
	ProviderID       *int64
	ProviderName     string
	PromptTokens     int64
	CompletionTokens int64
	CostUSD          float64
	Detail           string

	// CostNative/CostCurrency are the router's own real-time cost figure
	// (cost/savings sprint Phase 4, 2026-07-30), denominated in the
	// provider's own billed currency — never FX-converted at write time, so
	// a later FX correction retroactively fixes displayed history (read
	// paths convert via s.convert). nil/"" when pricing wasn't resolvable
	// for this request (unknown offering) — CostUSD's legacy 0 default must
	// not be read as "free" for such rows.
	CostNative   *float64
	CostCurrency string

	// CachedPromptTokens is the provider-reported cache-hit portion of
	// PromptTokens (e.g. DeepSeek's prompt_cache_hit_tokens / OpenAI's
	// prompt_tokens_details.cached_tokens), nil when the provider's response
	// didn't include a cache breakdown at all.
	CachedPromptTokens *int64

	// Unmetered marks an "external_request" row whose provider response
	// completed but carried no parseable usage object — PromptTokens/
	// CompletionTokens/CostUSD are legitimately 0 here, not real
	// measurements. false (the zero value) for every other kind, so every
	// pre-existing call site (OnTokenSample, load/unload events) is
	// automatically correct without being touched.
	Unmetered bool
}

type ModeHistoryEntry struct {
	Mode          string
	TS            time.Time
	TrainedCtx    int
	ConfiguredCtx int
	ActualCtx     int
	LoadTimeS     float64
	Result        string
}

// Metrics persists the metric_samples time-series (Sprint 0 §0.4, BE-1):
// one row per background-sampler tick, a range read the history endpoint
// downsamples on the way out, and retention pruning. Nullable columns mirror
// collector.Metrics' pointer fields (probe absent that tick maps to nil, not
// zero — a real zero reading and "no data" must stay distinguishable through
// an average).
type Metrics interface {
	RecordSample(ctx context.Context, s MetricSample) error
	// Range returns samples with from <= ts <= to, ordered oldest first.
	Range(ctx context.Context, from, to time.Time) ([]MetricSample, error)
	// Prune deletes samples older than cutoff (ts < cutoff) and reports how
	// many rows were removed.
	Prune(ctx context.Context, cutoff time.Time) (int64, error)
}

// MetricSample is one metric_samples row.
//
// A1 (bytes retrofit, 2026-07-24): every memory field is bytes (was *_mb).
// The DB columns were renamed in place (migration 0009) and existing rows
// converted by ×1024×1024.
type MetricSample struct {
	TS                time.Time
	GTTUsedBytes      *int64
	GTTTotalBytes     *int64
	GPUUsePct         *float64
	MemUsedBytes      *int64
	MemTotalBytes     *int64
	DiskUsedBytes     *int64
	DiskTotalBytes    *int64
	TempCelsius       *float64
	InferenceRSSBytes *int64

	// PackagePowerW, ActiveSlots, EnergyWhTotal added for the cost/savings
	// data layer (2026-07-30). PackagePowerW is the amdgpu PPT rail in
	// watts (nil = sensor absent that tick) — energy is integrated from
	// consecutive samples at read time, never pre-aggregated here, so a
	// later overhead_w/psu_efficiency/rate_per_kwh calibration recomputes
	// every historical figure instead of it being baked into stale rows.
	// ActiveSlots is the count of occupied inference slots this tick (lets
	// the read path separate single-slot-attributable energy from
	// multi-slot energy, which cannot be split per model). EnergyWhTotal is
	// reserved for a future monotonic in-process energy counter (Phase 1b);
	// unpopulated for now.
	PackagePowerW *float64
	ActiveSlots   *int
	EnergyWhTotal *float64

	// CPUPct/NetRxBytesPerSec/NetTxBytesPerSec/GPUJunctionTempC/
	// CPUPackageTempC/NVMeTempC added for Phase 4 collector metrics
	// (2026-08-12) — CPU.Pct is now real jiffy-delta utilization (was
	// load1/nproc*100), network throughput was entirely absent before, and
	// the three temp fields are additional hwmon channels beyond the
	// original GPU-edge TempCelsius above.
	CPUPct           *float64
	NetRxBytesPerSec *float64
	NetTxBytesPerSec *float64
	GPUJunctionTempC *float64
	CPUPackageTempC  *float64
	NVMeTempC        *float64
}

// Compressors persists collector-observed process health for Compressor-
// shaped proxies (Sprint 4). Unlike Routing's RecordSavingsSample/
// SavingsSummary (traffic counters, skipped on scrape failure or an idle
// interval), a Compressors sample is written unconditionally every collector
// tick — up/down, RSS, and systemd's restart counter are exactly the signal
// needed when a proxy is down or leaking, i.e. when the traffic-counter path
// has nothing to report.
type Compressors interface {
	RecordSample(ctx context.Context, s CompressorSampleRow) error
	// Latest returns the most recent sample per proxy, keyed by proxy
	// SERVICE NAME (a read-side join, matching Routing's Savings/
	// SavingsSummary convention) — the Dashboard tile's feed.
	Latest(ctx context.Context) (map[string]CompressorSampleRow, error)
	// Range returns one proxy's samples with ts >= since, oldest first —
	// the smith compressor_health check's feed for slope detection.
	Range(ctx context.Context, service string, since time.Time) ([]CompressorSampleRow, error)
	// Prune deletes samples older than cutoff and reports how many rows
	// were removed.
	Prune(ctx context.Context, cutoff time.Time) (int64, error)
}

// CompressorSampleRow is one compressor_samples row. Service is populated
// only on read (Latest/Range); RecordSample takes ProxyID, matching how
// Routing's RecordSavingsSample resolves service->proxy_id once at the
// call site rather than re-joining per write.
type CompressorSampleRow struct {
	TS        time.Time
	ProxyID   int64
	Service   string
	Up        bool
	MainPID   uint32
	RSSBytes  int64
	NRestarts uint32
}

// Routing persists proxy definitions, provider keys, and savings samples.
type Routing interface {
	SaveProxy(ctx context.Context, p ProxyRow) error
	DeleteProxy(ctx context.Context, service string) error
	Proxies(ctx context.Context) ([]ProxyRow, error)
	// SaveProvider is create-or-update keyed on p.ID: ID == 0 inserts a new
	// provider (name must be unique among non-deleted rows); ID != 0
	// updates that row by id — including a rename, since name is no longer
	// the primary key (Phase 6 surrogate-key migration, 0042).
	SaveProvider(ctx context.Context, p ProviderRow) error
	// DeleteProvider soft-deletes (sets deleted_at) rather than removing
	// the row — forced by usage_events.provider_id becoming a real FK
	// (0042): CASCADE would erase spend history, SET NULL would make it
	// unattributable, RESTRICT would just block the delete. A soft-deleted
	// provider never appears in Providers()/List() and never routes
	// (enabledProviderSet), but its history stays attributable and its
	// name can be reused (partial unique index on name WHERE deleted_at
	// IS NULL).
	DeleteProvider(ctx context.Context, id int64) error
	// Providers returns every NON-deleted provider row, ordered by name.
	Providers(ctx context.Context) ([]ProviderRow, error)
	// ProviderByID / ProviderByName resolve one provider (including
	// soft-deleted rows, so a caller can still e.g. audit-log a deleted
	// provider's name) — used by resolveProviderRef's dual id-or-name
	// dispatch (httpapi) and anywhere else a single lookup beats a full
	// Providers() scan.
	ProviderByID(ctx context.Context, id int64) (ProviderRow, bool, error)
	ProviderByName(ctx context.Context, name string) (ProviderRow, bool, error)
	// LinkProxyToProvider sets (or, with providerID nil, clears) the proxy
	// row's provider_id — the ONE FK that replaced the old bidirectional
	// compressor_proxies.provider <-> router_providers.headroom_proxy string
	// pair (0042). This is now the only way to link/unlink a proxy; there
	// is no more router_providers.headroom_proxy column to write.
	LinkProxyToProvider(ctx context.Context, proxyID int64, providerID *int64) error
	RecordSavings(ctx context.Context, proxyID int64, at time.Time, tokensIn, saved int64) error
	// Savings/SavingsSummary below return maps keyed by proxy SERVICE NAME
	// (not id) — a read-side join, so the many existing callers that index
	// this map by name (Dashboard/Console handlers) need no change.
	Savings(ctx context.Context, since time.Time) (map[string]SavingsTotal, error)

	// RecordSavingsSample appends one interval's per-proxy counter sample
	// (cost/savings sprint, 2026-07-30) plus its labelled breakdowns, in one
	// transaction. See CompressorSavingsSampleRow's doc comment for the delta-vs-
	// lifetime-gauge distinction between fields.
	RecordSavingsSample(ctx context.Context, s CompressorSavingsSampleRow, labels []CompressorLabelSample) error

	// SavingsSummary aggregates every compressor_savings_samples/compressor_label_samples
	// row with ts >= since into one totals-plus-latest-gauges summary per
	// proxy — the read path for the Dashboard's Compression tab.
	SavingsSummary(ctx context.Context, since time.Time) (map[string]CompressorProxySummary, error)
}

// CompressorSavingsSampleRow is one interval's per-proxy Compressor counters, as
// computed by the collector (collector.CompressorSample) from two consecutive
// /metrics scrapes. Counter fields are per-interval DELTAS (so a window's
// total is a plain SUM); the *MinMs/*MaxMs fields are Compressor's own
// lifetime-since-process-start gauges as of this sample, NOT deltas — only
// the most recent one in a window is meaningful, never summed or averaged.
type CompressorSavingsSampleRow struct {
	TS      time.Time
	ProxyID int64

	TokensIn, TokensOut, TokensSaved                              int64
	Requests, RequestsCached, RequestsFailed, RequestsRateLimited int64
	// RequestsTimeout / RequestsCanceled (Sprint 4) are a subset of
	// RequestsFailed's causes, broken out separately: a canceled request
	// (client disconnect) is deliberately NOT counted in RequestsFailed by
	// forge-compress, so these two are additive detail, not double
	// counting.
	RequestsTimeout  int64
	RequestsCanceled int64

	// CacheReadTokens / UncachedTokens are the scalar sums across all
	// providers of compress_cache_read_tokens_total and
	// compress_uncached_input_tokens_total — how many tokens were served
	// from the provider's prompt cache vs. how many were not. Per-provider
	// breakdowns go through CompressorLabelSample.
	CacheReadTokens int64
	UncachedTokens  int64

	// CacheBusts / CacheBustTokensLost are scalar counters from
	// compress_cache_bust_total and compress_cache_bust_tokens_lost_total.
	CacheBusts          int64
	CacheBustTokensLost int64

	TTFBCount            int64
	TTFBSumMs            float64
	TTFBMinMs, TTFBMaxMs *float64

	LatencyCount               int64
	LatencySumMs               float64
	LatencyMinMs, LatencyMaxMs *float64

	OverheadCount                int64
	OverheadSumMs                float64
	OverheadMinMs, OverheadMaxMs *float64
}

// CompressorLabelSample is one interval's per-(proxy,label) delta for a
// labelled Compressor metric. Delta is for int-valued metrics (request
// counts, token counts); DeltaF is for float-valued metrics (timing sums).
// At most one of Delta/DeltaF is non-zero per row.
type CompressorLabelSample struct {
	TS         time.Time
	ProxyID    int64
	LabelKey   string
	LabelValue string
	Metric     string
	Delta      int64
	DeltaF     *float64
}

// CompressorProxySummary is one proxy's aggregated Compressor activity over a
// window: counters summed, min/max gauges taken from the most recent sample
// in the window (never averaged/summed — see CompressorSavingsSampleRow).
type CompressorProxySummary struct {
	Proxy string

	TokensIn, TokensOut, TokensSaved                              int64
	Requests, RequestsCached, RequestsFailed, RequestsRateLimited int64
	RequestsTimeout, RequestsCanceled                             int64

	CacheReadTokens     int64
	UncachedTokens      int64
	CacheBusts          int64
	CacheBustTokensLost int64

	TTFBCount            int64
	TTFBSumMs            float64
	TTFBMinMs, TTFBMaxMs *float64

	LatencyCount               int64
	LatencySumMs               float64
	LatencyMinMs, LatencyMaxMs *float64

	OverheadCount                int64
	OverheadSumMs                float64
	OverheadMinMs, OverheadMaxMs *float64

	RequestsByProvider map[string]int64
	RequestsByModel    map[string]int64

	// Per-provider cache token breakdowns (from compress_cache_read_tokens_total,
	// compress_uncached_input_tokens_total, etc.).
	CacheReadTokensByProvider map[string]int64
	UncachedTokensByProvider  map[string]int64
	ProviderCacheRequests     map[string]int64
	ProviderCacheHitRequests  map[string]int64

	// Per-compressor timing (from compressor_transform_timing_ms_{sum,count}).
	TransformTimingSum   map[string]float64
	TransformTimingCount map[string]int64
}

type ProxyRow struct {
	// ID is the surrogate PK (Phase 6, 0042). Service stays the
	// operator/systemd-facing handle (unit identity is keyed on it, never
	// on a provider name — see docs' "no physical-address pinning" note).
	ID        int64
	Service   string
	Label     string
	Port      int
	TargetURL string
	Unit      string
	// ProviderID is the FK to the linked router_providers row, if any (nil
	// = unlinked). SaveProxy writes exactly what's here — same as every
	// other field — so a caller updating one field (label/port/...) must
	// fetch the existing row first and mutate a copy (the established
	// pattern throughout settings_handlers.go) rather than constructing a
	// bare ProxyRow{Service: x, ...}, or it will silently unlink the proxy.
	// The intended way to change ONLY the link is LinkProxyToProvider.
	ProviderID *int64
	// ProviderName is a read-only join-derived projection (the linked
	// provider's name, "" if unlinked) populated by Proxies(); ignored on
	// write.
	ProviderName string
	Token        string // secret; never logged
	Passthrough  bool
	// OrphanedAt is set (Phase 2, docs/v5-headroom-topology.md) when a proxy
	// is torn down (compressorctl.Provisioner.Teardown stops the unit + removes
	// its env file) but the row is kept for audit/history rather than
	// hard-deleted. Zero = active. router.routing.go's proxy-lookup helpers
	// (compressorServiceForPort, localCompressorBaseURL, remoteCompressorBaseURL)
	// skip any row with a non-zero OrphanedAt, so an orphaned row is never
	// routed to.
	OrphanedAt time.Time
	CreatedAt  time.Time
}

type ProviderRow struct {
	// ID is the surrogate PK (Phase 6, 0042) — the stable handle a rename
	// no longer disturbs. 0 on a struct meant for SaveProvider means
	// "insert new"; nonzero means "update this row" (which may include
	// changing Name — that's how a rename happens now).
	ID        int64
	Name      string
	APIKey    string // secret; never logged
	TargetURL string
	// CompressorProxyName is a read-only join-derived projection: the
	// service name of whichever compressor_proxies row has provider_id =
	// this provider's id, "" if none (0042 dropped the old
	// router_providers.headroom_proxy column — the link now lives solely
	// on the proxy side). Populated by Providers()/List(); ignored on
	// write. To change the link, resolve/create the target proxy and call
	// Routing.LinkProxyToProvider.
	CompressorProxyName string
	// DeletedAt is set by DeleteProvider (soft delete, 0042) — nonzero
	// means this row is gone from every operator-facing list but its
	// history stays attributable. ProviderByID still returns a
	// soft-deleted row (callers that need to distinguish check this
	// field); Providers()/List() never include one.
	DeletedAt time.Time
	Model     string
	Model2    string
	// Sprint 0 §0.2/§0.3 billing + status columns (0002_polish.sql).
	// BillCurrency defaults to "USD" when empty (the column DEFAULT);
	// StatusURL/CreditsURL default to "" (no status page / no balance
	// endpoint configured). Populated by both the read-optimized
	// Providers.List path (BE-3) and the legacy Routing.Providers()
	// SELECT used by provider-key CRUD (BE-5) — both now project all
	// three columns.
	BillCurrency string
	StatusURL    string // status-page JSON endpoint; "" = no status page configured
	CreditsURL   string // balance API endpoint; "" = use per-provider default or unsupported
	// OrgID (0006_provider_org_id.sql, BE-3 F4 fix): X-Org-ID header value
	// some providers require alongside the API key (confirmed for AI&'s
	// Analytics API — docs.aiand.com/analytics/metrics/ "Auth" section).
	// "" = not configured.
	OrgID string
	// BillingEnabled (0016_provider_billing.sql, product/QA sprint):
	// per-provider on/off for the credits/balance API. false ⇒
	// providers.creditsClients.fetch must short-circuit to
	// Credits{Supported:false} without probing, so the Console chip's
	// credits/spend region renders blank rather than stale. Column default
	// is 1 (enabled) — see SaveProvider for the same "explicit value always
	// wins" normalization already used for BillCurrency.
	BillingEnabled bool
	// BillingConsoleURL (0016_provider_billing.sql): the human billing
	// dashboard link (e.g. https://platform.deepseek.com/usage) shown on
	// the provider chip. Distinct from CreditsURL, which is the machine
	// balance-API endpoint. "" = not configured, no auto-discovery (there's
	// nothing to probe for a human-facing page).
	BillingConsoleURL string
	// Enabled (0032_provider_enabled_offering_priority.sql): false disables
	// the provider WITHOUT deleting it — the router skips its offerings in
	// selection, /v1/models listing, and request routing, and the providers
	// service stops refreshing its health/credits cache. Credentials,
	// offerings, and any linked Compressor proxy are preserved so re-enabling
	// restores the provider exactly as it was. Column default 1.
	Enabled bool
	// Country / DataResidencyGroup (0008_model_catalog.sql columns, surfaced
	// here in 0032): where the provider processes data — a Provider-level
	// fact inherited by all its Offerings. Populated from the curated preset
	// at create time (or by hand); "" = unknown. The router never uses these
	// for selection — residency preference is the operator's call, expressed
	// through offering priorities.
	Country            string
	DataResidencyGroup string
	CreatedAt          time.Time
}

type SavingsTotal struct {
	TokensIn int64
	Saved    int64
}

// Settings is the JSON KV surface for app-mutated settings that lived in
// config.toml in V4 (ui.help_button, nfs.shares, scheduler.config,
// router.busy_mode, compressor.passthrough_all). Values are raw JSON.
//
// Sprint 0 §0.12 registers these additional keys (canonical constants in
// internal/httpapi settings_handlers.go): billing.display_currency,
// billing.fx_source_url, billing.fx_refresh_min, metrics.retention_days,
// metrics.sample_interval_s.
//
// Sprint 12 (was H) Phase 1: ui.theme, ui.bookmarks, notes.sections,
// hf.token, and logging.forward_url were removed from this vocabulary — a
// repo-wide grep found zero production readers for any of the five, so they
// existed only as names that looked like real, configurable features.
// Migration 0029 deletes any stored rows. hf.token was a secret; that
// deletion is irreversible (see the migration's own comment).
type Settings interface {
	Get(ctx context.Context, key string) ([]byte, error) // ErrNotFound when unset
	Set(ctx context.Context, key string, value []byte) error
}

// Audit writes the audit log (DB row; an optional JSONL mirror is a Phase 3
// config knob).
type Audit interface {
	Write(ctx context.Context, e AuditEntry) error
	// List returns audit entries most-recent-first, capped at limit
	// (Sprint C — audit_log was write-only before this). actionPrefix
	// filters on a SQL LIKE prefix (e.g. "catalog_config_"); target, when
	// non-empty, must be combined with actionPrefix rather than used alone
	// — target IDs collide across entity types (config 7 and model 7 are
	// both the string "7").
	List(ctx context.Context, actionPrefix, target string, limit int) ([]AuditEntry, error)
}

type AuditEntry struct {
	ID         int64
	TS         time.Time
	Actor      string
	Action     string
	Target     string
	Detail     string // JSON; must never contain secrets
	RemoteAddr string
}

// ── Sprint 0-AUTH (§4): identity linking + TOTP ────────────────────────────

// IdentityLink links a local account to a trusted network principal (e.g. a
// Tailscale login). When the NetworkIdentityProvider resolves a principal
// that has a link, the request bootstraps as that account at L0 (§3.3).
type IdentityLink struct {
	Provider  string // 'tailscale' | 'forward_auth_header'
	Principal string // e.g. 'user@github'
	UserID    int64
	CreatedAt time.Time
}

type IdentityLinks interface {
	Create(ctx context.Context, link IdentityLink) error
	Delete(ctx context.Context, provider, principal string) error
	// ListByUser returns all links for a given user.
	ListByUser(ctx context.Context, userID int64) ([]IdentityLink, error)
	// List returns all links (admin view).
	List(ctx context.Context) ([]IdentityLink, error)
	// Lookup resolves a provider+principal to a user ID. Returns ErrNotFound
	// when no link exists (the caller may then fall back to the anonymous
	// network default role).
	Lookup(ctx context.Context, provider, principal string) (int64, error)
}

// TOTPSecret is one user's TOTP enrollment (confirm-before-activate: the
// secret is created unconfirmed, then confirmed by a valid code on the next
// step). One active row per user (PRIMARY KEY user_id).
type TOTPSecret struct {
	UserID    int64
	Secret    string // base32; §9 Q6: rely on 0600 DB file
	Confirmed bool
	CreatedAt time.Time
}

type TOTP interface {
	// Save creates or replaces a TOTP secret for the user (unconfirmed).
	Save(ctx context.Context, s TOTPSecret) error
	// Get returns the user's TOTP secret row.
	Get(ctx context.Context, userID int64) (TOTPSecret, error)
	// Confirm marks the secret as confirmed (active).
	Confirm(ctx context.Context, userID int64) error
	// Delete removes the user's TOTP secret.
	Delete(ctx context.Context, userID int64) error
}

// WebAuthnCredential is one registered authenticator (one row per credential).
// The public_key is the COSE public key from the WebAuthn library; transports
// is a JSON array of authenticator transport strings. Sign count is updated
// on each assertion to detect cloned authenticators.
type WebAuthnCredential struct {
	ID         string // credential id (base64url)
	UserID     int64
	PublicKey  []byte // COSE public key
	SignCount  uint32
	Transports string // JSON array
	Label      string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

type WebAuthnCredentials interface {
	// Save creates or replaces a WebAuthn credential.
	Save(ctx context.Context, c WebAuthnCredential) error
	// Get returns a credential by its ID.
	Get(ctx context.Context, id string) (WebAuthnCredential, error)
	// ListByUser returns all credentials for a user.
	ListByUser(ctx context.Context, userID int64) ([]WebAuthnCredential, error)
	// Delete removes a credential by ID.
	Delete(ctx context.Context, id string) error
	// UpdateSignCount updates the sign count + last_used_at after a successful assertion.
	UpdateSignCount(ctx context.Context, id string, signCount uint32, at time.Time) error
}

// RecoveryCode is one hashed recovery code for TOTP/passkey lockout (§4/§8).
// Codes are Argon2id-hashed at rest — the plaintext is shown to the user
// exactly once on generation and never stored or logged.
type RecoveryCode struct {
	ID        int64
	UserID    int64
	CodeHash  string // Argon2id encoded
	CreatedAt time.Time
	UsedAt    time.Time // zero = unused
}

type RecoveryCodes interface {
	// Save stores recovery codes for a user (appends; does not replace existing).
	Save(ctx context.Context, codes []RecoveryCode) error
	// ListByUser returns all recovery codes for a user (including used ones).
	ListByUser(ctx context.Context, userID int64) ([]RecoveryCode, error)
	// MarkUsed marks a code as used by its ID.
	MarkUsed(ctx context.Context, id int64, at time.Time) error
	// DeleteAll removes all recovery codes for a user (used when regenerating).
	DeleteAll(ctx context.Context, userID int64) error
}

// ── PROFILE track (docs/v5-profiling-benchmarks.md §4) ───────────────────────

// ModelProfile is one measured memory + T/s profile for a mode at a specific
// config (n_ctx, backend, parallel). The fingerprint identifies the exact
// combination of model file + quant + llama.cpp binary + flags the profile
// was measured against; a changed fingerprint means the profile is stale.
//
// A1 (bytes retrofit, 2026-07-24): SafeMemoryBytes is bytes (was
// SafeMemoryMB). The DB column was renamed in place (migration 0009) and
// existing rows converted by ×1024×1024.
type ModelProfile struct {
	ID int64
	// ConfigID is the real FK (0042) — write this. Mode is a read-only
	// join-derived projection (the config's current name) populated by
	// Get/List for display/logging; ignored on write.
	ConfigID        int64
	Mode            string
	ModelID         string
	NCtx            int // configured target context
	Backend         string
	Parallel        int
	SafeMemoryBytes int64 // peak additive footprint + margin — authoritative needBytes
	PrefillTPS      float64
	DecodeTPS       float64
	ActualNCtx      int // what actually loaded (may be < NCtx)
	Fingerprint     string
	MeasuredAt      time.Time
}

// ModelProfileBenchmark is one depth-sweep measurement (product/QA sprint,
// 2026-07-29 — see docs decision log: measure pp2048+tg128 at empty/25%/
// 50%/~full-context depths, rather than the single, prefix-cache-
// contaminated "prefill" figure the original PROFILE track shipped).
// DepthTokens is how full the KV cache was when this row was measured;
// PP2048TPS is prefill throughput for a FRESH 2048-token prompt appended at
// that depth (never a cache hit — see profile.go's sweep implementation);
// TG128TPS is decode throughput for 128 tokens generated right after.
type ModelProfileBenchmark struct {
	ID          int64
	ProfileID   int64
	DepthTokens int
	PP2048TPS   float64
	TG128TPS    float64
}

type ModelProfiles interface {
	// Save records a profile plus its depth-sweep benchmarks, replacing any
	// existing row for the same (mode, backend, parallel, n_ctx) combo. Old
	// benchmark rows are dropped automatically (ON DELETE CASCADE) when the
	// old profile row is replaced by the new one.
	Save(ctx context.Context, p ModelProfile, benchmarks []ModelProfileBenchmark) error
	// Get returns the latest profile for a config (ErrNotFound when none).
	// Keyed by config_id since 0042 — mode's config.Mode.ConfigID (already
	// populated by the merged-config seam) is the caller's usual source.
	Get(ctx context.Context, configID int64) (ModelProfile, error)
	// List returns all profiles, ordered by config name then measured_at desc.
	List(ctx context.Context) ([]ModelProfile, error)
	// Delete removes a profile by config (cascades to its benchmark rows).
	Delete(ctx context.Context, configID int64) error
	// Benchmarks returns the depth-sweep rows for a profile, ordered by
	// depth_tokens ascending (typical/shallowest first, worst case last).
	Benchmarks(ctx context.Context, profileID int64) ([]ModelProfileBenchmark, error)
}

// PrefillStat is one mode's durable, rolling real-prefill-throughput
// aggregate (Compressor local-savings prefill sprint, 2026-08-06 — see
// migrations/0031_model_prefill_stats.sql). Tokens/Seconds is
// token-weighted by construction (the correct aggregate — a mean of
// per-interval ratios would over-weight tiny, noisy intervals). Zero
// Seconds means no observation has landed yet for this (mode, fingerprint)
// pair.
type PrefillStat struct {
	// ConfigID is the real FK (0042). Mode is a read-only join-derived
	// projection populated by ByMode (which stays keyed by mode name for
	// its map key — see ByMode's doc comment); ignored on write.
	ConfigID      int64
	Mode          string
	Fingerprint   string
	PromptTokens  int64
	PromptSeconds float64
	Samples       int
	FirstSeen     time.Time
	LastSeen      time.Time
}

// TPS is PromptTokens/PromptSeconds, or 0 if nothing observed yet.
func (p PrefillStat) TPS() float64 {
	if p.PromptSeconds <= 0 {
		return 0
	}
	return float64(p.PromptTokens) / p.PromptSeconds
}

// PrefillStats is the passive-observation counterpart to ModelProfiles: a
// durable per-(mode, fingerprint) rolling aggregate of real prefill
// throughput, accumulated from every slot's ordinary /metrics scrapes
// (internal/collector's OnPrefillSample) rather than a destructive,
// operator-triggered profiling run. Keyed by fingerprint so a config change
// (context, backend, extra args, model swap) starts a fresh accumulation
// instead of blending two different performance regimes together.
type PrefillStats interface {
	// AddObservation accumulates one interval's real prefill delta into the
	// running (mode, fingerprint) aggregate (UPSERT — creates the row on
	// first observation). The caller always passes the mode's CURRENT
	// fingerprint (computed fresh at observation time), so a config change
	// naturally starts a new row rather than blending into the old one.
	// tokens/seconds must both be > 0; callers filter idle/unattributable
	// intervals before calling.
	// configID is the caller's already-resolved config identity (0042 —
	// mode name is no longer the join key); callers with only a mode name
	// resolve via store.Catalog().ConfigByName first.
	AddObservation(ctx context.Context, configID int64, fingerprint string, tokens int64, seconds float64) error
	// ByMode returns each mode's CURRENT-regime aggregate, keyed by mode
	// name — the row with the greatest last_seen per mode. Because
	// AddObservation always upserts under the live fingerprint, an old
	// config regime's row stops advancing the moment the config changes, so
	// "greatest last_seen" is equivalent to "matches today's fingerprint"
	// without re-deriving it here (fingerprinting touches config + a GGUF
	// header read; this read path shouldn't have to pay that cost per
	// mode). A window filter is deliberately not offered: typical prefill
	// speed must never expire just because a query window is narrow — see
	// the sprint plan.
	ByMode(ctx context.Context) (map[string]PrefillStat, error)
}

// Notification is one persisted, deduplicated alert (product/QA sprint,
// 2026-07-29). Source codes: INFERENCE_HANG/GTT_HIGH (port-scoped, from
// collector.collectAlerts) and UNIT_OOM/UNIT_CRASH/UNIT_RESTARTED
// (unit-scoped, from collector.unitAlerts — systemd Result/NRestarts, no
// dmesg/kernel access needed).
type Notification struct {
	ID             int64
	Code           string
	Severity       string // info | warn | crit
	Subject        string // unit name or port string; "" if global
	Message        string
	DedupeKey      string
	FirstSeen      time.Time
	LastSeen       time.Time
	Occurrences    int
	AcknowledgedAt *time.Time
	DismissedAt    *time.Time
}

type Notifications interface {
	// Upsert records one observed alert occurrence. If dedupe_key already
	// exists, bumps last_seen/occurrences (and un-dismisses — a recurring
	// alert after dismissal is a new occurrence worth surfacing again)
	// rather than inserting a duplicate row. Returns the row's ID.
	Upsert(ctx context.Context, code, severity, subject, message, dedupeKey string, at time.Time) (int64, error)
	// List returns notifications, newest last_seen first. includeDismissed
	// controls whether dismissed rows are included (the Dashboard panel
	// defaults to excluding them).
	List(ctx context.Context, includeDismissed bool) ([]Notification, error)
	// Acknowledge sets acknowledged_at (idempotent).
	Acknowledge(ctx context.Context, id int64, at time.Time) error
	// Dismiss sets dismissed_at (idempotent).
	Dismiss(ctx context.Context, id int64, at time.Time) error
	// AcknowledgeAll acknowledges every currently-active (non-dismissed,
	// non-acknowledged) notification.
	AcknowledgeAll(ctx context.Context, at time.Time) error
}

// Favorite is one operator's starred subject (product/QA sprint,
// 2026-07-29 — Console config-card starring). SubjectType is "config"
// today; kept as a string (not a hardcoded single-purpose column) so a
// future subject type doesn't need a schema change, matching the existing
// Benchmark/Note subject_type convention.
type Favorite struct {
	ID          int64
	Username    string
	SubjectType string // "config"
	SubjectID   int64
	CreatedAt   time.Time
}

type Favorites interface {
	// Add stars subject for username. Idempotent (UNIQUE constraint) — a
	// double-star is a no-op, not an error.
	Add(ctx context.Context, username, subjectType string, subjectID int64) error
	// Remove un-stars. Idempotent — un-starring something not starred is a
	// no-op, not ErrNotFound (the caller doesn't need to check first).
	Remove(ctx context.Context, username, subjectType string, subjectID int64) error
	// List returns every subject username has starred, of subjectType.
	List(ctx context.Context, username, subjectType string) ([]Favorite, error)
}
