import { useEffect, useState } from "react";
import { DashboardCustomPage } from "../components/DashboardCustomPage";
import { ErrorBoundary } from "../components/ErrorBoundary";
import { CompressorSavingsChips } from "../components/CompressionSavingsChips";
import { Icon } from "../components/Icon";
import { RangeToggle } from "../components/RangeToggle";
import { RoutingTree } from "../components/RoutingTree";
import { ServicesBar } from "../components/ServicesBar";
import { ActivityHeatmapWidget } from "../components/widgets/ActivityHeatmapWidget";
import { ElectricityBreakdownWidget } from "../components/widgets/ElectricityBreakdownWidget";
import { MemoryMeterWidget } from "../components/widgets/MemoryMeterWidget";
import { PerModelSpendTableWidget } from "../components/widgets/PerModelSpendTableWidget";
import { ResourceGaugesWidget } from "../components/widgets/ResourceGaugesWidget";
import { ResourceTrendWidget } from "../components/widgets/ResourceTrendWidget";
import { RightNowWidget } from "../components/widgets/RightNowWidget";
import { formatCurrency, formatCurrencyPrecise, formatPct, formatTokens } from "../lib/format";
import { useCostSummary, useDashboardLayout, useCompressorSummary, useInfraServices, useUpdateDashboardLayout, useUsage } from "../lib/queries";
import { useSession } from "../lib/session";
import type { DashboardPage, CompressorSummaryProxy } from "../lib/types";

// Dashboard cost/savings sprint, Phase 5 (joyful-splashing-moonbeam.md):
// three tabs replacing the old single long-scroll page. Notifications are
// surfaced in Diagnostics (#help/smith), not on the Dashboard —
// collector alerts (hang/OOM/crash/GTT/restart) live there alongside smith's
// sweep findings. Plain useState tab state,
// following the CatalogPanel.tsx sub-tab precedent (not the top-level hash
// router, which only handles one level).
//
// Follow-up (operator feedback): a standalone Compressor tab didn't earn its
// keep — folded into Cost as a "Compressor savings" section instead, since
// savings only mean something next to the cost they're saving against.
//
// Pre-release feedback round, Phase 5 (2026-08-12): the Trends tab is gone.
// Its unique content was relocated, not dropped — the multi-series history
// chart + CSV/JSON export and the measured-electricity breakdown moved to
// the new Resources tab (which also gets the memory meter, off Overview,
// plus live RAM/GPU/CPU/temp/storage/network gauges from Phase 4's collector
// fixes); the per-model reliability & spend table moved to Cost, next to
// the rest of the money picture. Most of Dashboard.tsx's own logic moved out
// to components/widgets/ at the same time — this file is now mostly
// composition of those self-contained widgets.
const DASHBOARD_TABS = [
  { key: "overview", label: "Overview" },
  { key: "cost", label: "Cost" },
  { key: "resources", label: "Resources" },
] as const;

// Cost tab's own range toggle (pre-release feedback round, 2026-08-06) —
// long-horizon spend/electricity windows, distinct from Resources' shorter
// diagnostic ranges. "All time" is a large-but-bounded window (not a
// dedicated windowless query mode — parseMetricsWindow/parseUsageWindow
// only ever parse a duration) that in practice covers every real record.
const COST_RANGES = [
  { key: "1mo", label: "1 Month", window: "30d" },
  { key: "6mo", label: "6 Months", window: "180d" },
  { key: "1y", label: "1 Year", window: "365d" },
  { key: "all", label: "All Time", window: "3650d" },
] as const;
type CostRangeKey = (typeof COST_RANGES)[number]["key"];

// Resources tab's short-horizon range — one toggle shared by the trend
// chart and the measured-electricity breakdown below it (Phase 5), same
// "one range governs several sections" shape the old Trends tab used.
const RESOURCE_RANGES = [
  { key: "24h", label: "24h", window: "24h" },
  { key: "72h", label: "72h", window: "72h" },
  { key: "1w", label: "1w", window: "7d" },
  { key: "1m", label: "1m", window: "30d" },
] as const;
type ResourceRangeKey = (typeof RESOURCE_RANGES)[number]["key"];

export function Dashboard() {
  const [tab, setTab] = useState<string>("overview");
  const { canAdmin } = useSession();
  const layout = useDashboardLayout();
  const updateLayout = useUpdateDashboardLayout();

  const customPages = layout.data?.pages ?? [];
  const isDefaultTab = tab === "overview" || tab === "cost" || tab === "resources";
  const activeCustomPage = !isDefaultTab ? customPages.find((p) => p.id === tab) : undefined;

  // Fallback to overview if the selected custom page no longer exists
  // (deleted on another device, or the layout finished loading and the ID
  // isn't there).
  useEffect(() => {
    if (!isDefaultTab && !activeCustomPage && !layout.isLoading) {
      setTab("overview");
    }
  }, [isDefaultTab, activeCustomPage, layout.isLoading]);

  function createPage() {
    const id = crypto.randomUUID();
    const newPage: DashboardPage = { id, name: "New Page", widgets: [] };
    updateLayout.mutate({ pages: [...customPages, newPage] });
    setTab(id);
  }

  function renamePage(id: string, name: string) {
    updateLayout.mutate({ pages: customPages.map((p) => (p.id === id ? { ...p, name } : p)) });
  }

  function deletePage(id: string) {
    updateLayout.mutate({ pages: customPages.filter((p) => p.id !== id) });
    setTab("overview");
  }

  function updatePage(id: string, updated: DashboardPage) {
    updateLayout.mutate({ pages: customPages.map((p) => (p.id === id ? updated : p)) });
  }

  const Active = tab === "overview" ? OverviewTab : tab === "cost" ? CostTab : tab === "resources" ? ResourcesTab : null;

  return (
    <section className="page">
      <div className="eyebrow" style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
        <span>Dashboard</span>
        <div className="tabs" style={{ marginLeft: "auto" }}>
          {DASHBOARD_TABS.map((t) => (
            <button key={t.key} className={`tab ${tab === t.key ? "active" : ""}`} onClick={() => setTab(t.key)}>
              {t.label}
            </button>
          ))}
          {customPages.map((p) => (
            <button key={p.id} className={`tab ${tab === p.id ? "active" : ""}`} onClick={() => setTab(p.id)}>
              {p.name}
            </button>
          ))}
          {canAdmin && (
            <button className="tab" onClick={createPage} title="New custom page" style={{ padding: "3px 8px" }}>+</button>
          )}
        </div>
      </div>
      <ErrorBoundary key={tab} resetKeys={[tab]}>
        {Active ? (
          <Active />
        ) : activeCustomPage ? (
          <>
            {canAdmin && (
              <div className="eyebrow" style={{ display: "flex", gap: 8, alignItems: "center" }}>
                <PageNameInput
                  name={activeCustomPage.name}
                  onRename={(name) => renamePage(activeCustomPage.id, name)}
                />
                <button
                  className="btn"
                  style={{ fontSize: 11 }}
                  onClick={() => {
                    if (window.confirm(`Delete page "${activeCustomPage.name}"?`)) deletePage(activeCustomPage.id);
                  }}
                >
                  Delete Page
                </button>
              </div>
            )}
            <DashboardCustomPage
              page={activeCustomPage}
              canEdit={canAdmin}
              onUpdatePage={(updated) => updatePage(activeCustomPage.id, updated)}
            />
          </>
        ) : (
          <div className="empty-note">Loading…</div>
        )}
      </ErrorBoundary>
    </section>
  );
}

function PageNameInput({ name, onRename }: { name: string; onRename: (name: string) => void }) {
  const [value, setValue] = useState(name);
  useEffect(() => setValue(name), [name]);
  return (
    <input
      value={value}
      onChange={(e) => setValue(e.target.value)}
      onBlur={() => {
        if (value !== name && value.trim()) onRename(value.trim());
      }}
      onKeyDown={(e) => {
        if (e.key === "Enter") (e.target as HTMLInputElement).blur();
      }}
      style={{
        fontSize: 12,
        background: "var(--bg)",
        border: "1px solid var(--border)",
        borderRadius: 4,
        padding: "2px 8px",
        color: "var(--text)",
      }}
    />
  );
}

// ── Overview ──────────────────────────────────────────────────────────────
// "Is it healthy, what is it costing me right now." Token activity heatmap,
// then the Right Now strip (power, electricity, Compressor savings — all on a
// consistent 24h window as of Phase 5). The memory meter lived here before
// Phase 5; it's on the new Resources tab now, alongside the rest of the
// footprint/utilization detail.

function OverviewTab() {
  // Operator feedback 2026-08-14: the infra service chips moved here from
  // the Console's Resources & services section — fleet health at a glance
  // belongs on the landing tab.
  const infraServices = useInfraServices();
  return (
    <>
      <ServicesBar services={infraServices.data?.services} />
      {/* Sprint 10 (docs/v5-headroom-replacement.md): the routing diagram,
          duplicated from Settings → Routing as a read-only copy whose links
          show each route's REAL compressor state × health — the detail the
          aggregated Compressor chip above can't carry. */}
      <div className="eyebrow">Routing</div>
      <div className="card">
        <RoutingTree readOnly />
      </div>
      <ActivityHeatmapWidget />
      <RightNowWidget />
    </>
  );
}

// ── Cost ──────────────────────────────────────────────────────────────────
// Electricity (measured), virtual spend per local model, real API spend per
// provider, Compressor savings, and per-model reliability & spend (relocated
// from the deleted Trends tab, Phase 5). The point-in-time balance/spend
// "Reconciliation" section was removed the same phase — it only ever diffed
// a snapshot, which was the operator's own complaint about it.

function CostTab() {
  const [rangeKey, setRangeKey] = useState<CostRangeKey>("1mo");
  const range = COST_RANGES.find((r) => r.key === rangeKey)!;
  const costSummary = useCostSummary(range.window);
  const usage = useUsage(range.window);
  const compressorSummary = useCompressorSummary(range.window);

  const energy = costSummary.data?.energy;
  const displayCurrency = costSummary.data?.display_currency ?? "USD";
  const fxAsOf = costSummary.data?.fx_as_of;
  const fxStale = costSummary.data?.fx_stale ?? false;

  const compressorProxies = compressorSummary.data?.proxies ?? [];
  const compressorDisplayCurrency = compressorSummary.data?.display_currency ?? "USD";

  // Aggregate usage.external by provider (it's per provider+model) for the
  // per-provider "real API spend" chips in the top stats row — a provider
  // total, not one model row.
  const extByProvider = new Map<string, { cost_native: number; cost_display: number; native_currency: string; requests: number; unmetered: number }>();
  for (const e of usage.data?.external ?? []) {
    const cur = extByProvider.get(e.provider) ?? { cost_native: 0, cost_display: 0, native_currency: e.native_currency, requests: 0, unmetered: 0 };
    cur.cost_native += e.cost_native;
    cur.cost_display += e.cost_display;
    cur.requests += e.requests;
    cur.unmetered += e.requests_unmetered;
    extByProvider.set(e.provider, cur);
  }

  const virtualSpend = usage.data?.totals.local_cost_display;
  const apiSpend = usage.data?.totals.external_cost_display;
  // Electricity (measured) + API spend (real, additive external cost) is a
  // genuine "total cost of operating everything." Virtual spend is NOT
  // included here — it's a separate, curated-throughput *estimate of the
  // same electricity draw* Electricity cost already measures, not a second
  // cost category; summing them would double-count. It stays its own tile,
  // useful for "which model costs the most to run," not for grand-totaling.
  const totalCost = energy != null && apiSpend != null ? energy.cost_display + apiSpend : null;

  return (
    <>
      <div className="eyebrow" style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
        <span>Cost · {range.label}</span>
        <RangeToggle options={COST_RANGES} value={rangeKey} onChange={setRangeKey} />
        {/* Phase 5: dropped the "1:1 (no conversion)" fallback — a chip
            implying FX activity happened when none did was more confusing
            than showing nothing. Only a real conversion gets a chip now. */}
        {fxAsOf != null && (
          <span className="chip" style={{ color: fxStale ? "var(--warn)" : undefined }}>
            FX as of {new Date(fxAsOf * 1000).toLocaleDateString()}
            {fxStale ? " · stale" : ""}
          </span>
        )}
      </div>
      <div className="stats" style={{ gridTemplateColumns: "repeat(auto-fit, minmax(150px, 1fr))" }}>
        <div className="stat">
          <div className="k">Electricity</div>
          <div className="v">{energy ? formatCurrency(energy.cost_display, displayCurrency) : "…"}</div>
          <div className="d">measured — see Resources for the full breakdown</div>
        </div>
        <div className="stat">
          <div className="k">Virtual spend</div>
          <div className="v">{virtualSpend != null ? formatCurrency(virtualSpend, displayCurrency) : "…"}</div>
          <div className="d">est. per local model — not additive with Electricity</div>
        </div>
        <div className="stat">
          <div className="k">API spend</div>
          <div className="v">{apiSpend != null ? formatCurrency(apiSpend, displayCurrency) : "…"}</div>
          <div className="d">real, external providers</div>
        </div>
        {Array.from(extByProvider.entries()).map(([name, e]) => (
          <div className="stat" key={`api-spend:${name}`}>
            <div className="k">{name}</div>
            <div className="v">{formatCurrency(e.cost_display, displayCurrency)}</div>
            <div className="d">{e.requests} req{e.unmetered > 0 ? ` (${e.unmetered} unmetered)` : ""}</div>
          </div>
        ))}
        <div className="stat">
          <div className="k">Total</div>
          <div className="v">{totalCost != null ? formatCurrency(totalCost, displayCurrency) : "…"}</div>
          <div className="d">electricity + API spend</div>
        </div>
        <CompressorSavingsChips window_={range.window} />
      </div>

      <div className="eyebrow">Compressor savings · {range.label}</div>
      <div className="hoom">
        {compressorSummary.isLoading ? (
          <div className="empty-note">Loading Compressor summary…</div>
        ) : compressorProxies.length > 0 ? (
          compressorProxies.map((p) => <ProxyCard key={p.proxy} p={p} displayCurrency={compressorDisplayCurrency} />)
        ) : (
          <div className="empty-note">No Compressor proxies reporting this window.</div>
        )}
      </div>

      <div className="eyebrow" style={{ display: "flex", alignItems: "center", gap: 10 }}>
        <span>Virtual spend · local models · {range.label}</span>
      </div>
      <div className="card">
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 10, lineHeight: 1.5 }}>
          Priced off curated catalog throughput, not measured per-model power — no profiling run has been
          executed against this hardware yet (destructive; needs a coordinated window).
        </div>
        {/* Models with no real spend (0 tokens or a $0 estimate) are dropped
            rather than shown as a meaningless zero row — pre-release
            feedback round, 2026-08-06. */}
        {(() => {
          const nonZeroModels = (usage.data?.models ?? []).filter((m) => m.power_cost_display > 0);
          return (
            <>
              {nonZeroModels.length === 0 && <div className="empty-note">No local usage recorded yet.</div>}
              {nonZeroModels.map((m) => (
                <div className="qrow" key={`local:${m.model}`}>
                  <span className="want">{m.model}</span>
                  <span style={{ fontFamily: "var(--mono)", fontSize: 11, color: "var(--text-mute)" }}>{formatTokens(m.prompt_tokens + m.predicted_tokens)} tok</span>
                  <span className="pos">{formatCurrency(m.power_cost_display, usage.data!.display_currency)}</span>
                </div>
              ))}
            </>
          );
        })()}
      </div>

      <div className="eyebrow">Per-model reliability &amp; spend · {range.label}</div>
      <PerModelSpendTableWidget window_={range.window} />
    </>
  );
}

// ── Compressor savings (a section within the Cost tab, not its own tab — per
// operator feedback, savings only mean something next to the cost they're
// saving against) ────────────────────────────────────────────────────────
// Per-proxy cache-hit rate, requests, mean TTFB/latency/overhead (labelled
// separately from the lifetime since-start gauges), time saved locally
// (TPS-sourced), and honest callouts for what this build genuinely can't
// report (lossless compression, percentiles, remote cache-discount money
// saved, per-interval latency trend — no history endpoint exists yet).

function msFmt(v: number): string {
  return `${v.toFixed(0)} ms`;
}

function ProxyCard({ p, displayCurrency }: { p: CompressorSummaryProxy; displayCurrency: string }) {
  return (
    <div className="prox" style={{ flexDirection: "column", alignItems: "stretch", gap: 10 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
        <Icon slug={p.proxy} name={p.proxy} sm />
        <span className="pn">{p.proxy}</span>
        <span className="chip">{p.kind}</span>
        <span className="pu" style={{ marginLeft: "auto" }}>
          {p.cache_hit_rate_pct != null ? `${formatPct(p.cache_hit_rate_pct)} cache-hit` : "no requests"}
        </span>
      </div>
      <div style={{ display: "flex", gap: 16, flexWrap: "wrap", fontSize: 11.5 }}>
        <span>{p.requests} req · {p.requests_cached} cached · {p.requests_failed} failed · {p.requests_rate_limited} rate-limited</span>
        <span style={{ color: "var(--text-mute)" }}>{formatTokens(p.tokens_in)} in / {formatTokens(p.tokens_out)} out</span>
      </div>
      <div style={{ display: "flex", gap: 16, flexWrap: "wrap", fontSize: 11 }}>
        <span>TTFB mean {p.ttfb_mean_ms != null ? msFmt(p.ttfb_mean_ms) : "—"} <small style={{ color: "var(--text-mute)" }}>
          (lifetime {p.ttfb_min_ms_since_start != null ? msFmt(p.ttfb_min_ms_since_start) : "—"}–{p.ttfb_max_ms_since_start != null ? msFmt(p.ttfb_max_ms_since_start) : "—"})
        </small></span>
        <span>Latency mean {p.latency_mean_ms != null ? msFmt(p.latency_mean_ms) : "—"} <small style={{ color: "var(--text-mute)" }}>
          (lifetime {p.latency_min_ms_since_start != null ? msFmt(p.latency_min_ms_since_start) : "—"}–{p.latency_max_ms_since_start != null ? msFmt(p.latency_max_ms_since_start) : "—"})
        </small></span>
        <span>Overhead mean {p.overhead_mean_ms != null ? msFmt(p.overhead_mean_ms) : "—"}</span>
      </div>
      {p.time_saved_seconds_est != null ? (
        <div className="saved" style={{ marginLeft: 0, textAlign: "left" }}>
          <span className="k">Time saved (est.)</span>
          {Math.round(p.time_saved_seconds_est)}s
          {p.money_saved_display != null && (
            <> · {formatCurrencyPrecise(p.money_saved_display, displayCurrency)} saved</>
          )}
          {/* Per-model accounting (2026-08-06 local-savings prefill sprint):
              this is a SUM over every model that contributed cached requests
              this window, each priced against its OWN real prefill TPS —
              never one blended figure, so the breakdown is the honest
              explanation of the total above, not just extra detail. */}
          {p.prefill_breakdown && p.prefill_breakdown.length > 0 && (
            <div style={{ marginTop: 4, fontSize: 10.5, color: "var(--text-mute)" }}>
              {p.prefill_breakdown.map((m) => (
                <span key={m.mode} style={{ marginRight: 10 }}>
                  {m.mode} {formatPct(m.share * 100)} @ {Math.round(m.tps)} tok/s ({m.source})
                </span>
              ))}
            </div>
          )}
        </div>
      ) : p.kind === "remote" ? (
        <>
          {p.compression_saved_display != null ? (
            <div className="saved" style={{ marginLeft: 0, textAlign: "left" }}>
              <span className="k">Compressor saved (measured)</span>
              {formatCurrencyPrecise(p.compression_saved_display, displayCurrency)} · {formatTokens(p.tokens_saved)} tokens compressed out of the prompt before it reached {p.proxy}
            </div>
          ) : (
            <div style={{ fontSize: 11, color: "var(--text-mute)" }}>
              No tokens compressed out of the prompt this window — usually 0 under --lossless, though not
              always (a real 950-token delta was recorded live 2026-07-31).
            </div>
          )}
          {p.cache_discount_saved_display != null && (
            <div style={{ fontSize: 10.5, color: "var(--text-mute)" }}>
              Provider cache discount (not a Compressor saving — applies with or without Compressor in the path):
              {" "}{formatCurrencyPrecise(p.cache_discount_saved_display, displayCurrency)} · {formatTokens(p.cache_discount_tokens ?? 0)} tokens billed at {p.proxy}'s discounted rate
            </div>
          )}
        </>
      ) : (
        <div style={{ fontSize: 11, color: "var(--text-mute)" }}>No cacheable requests (or no TPS basis) this window — no time-saved estimate.</div>
      )}
    </div>
  );
}

// ── Resources ─────────────────────────────────────────────────────────────
// New tab, Phase 5 (2026-08-12): the memory-usage meter (relocated off
// Overview), live RAM/GPU/CPU/temp/storage/network gauges (Phase 4's
// collector fixes), and — sharing one range toggle — the multi-series trend
// chart + CSV/JSON export and the measured-electricity breakdown, both
// relocated from the deleted Trends tab.

function ResourcesTab() {
  const [rangeKey, setRangeKey] = useState<ResourceRangeKey>("24h");
  const range = RESOURCE_RANGES.find((r) => r.key === rangeKey)!;

  return (
    <>
      <MemoryMeterWidget />
      <ResourceGaugesWidget />
      <div className="eyebrow" style={{ display: "flex", alignItems: "center", gap: 10 }}>
        <span>History range</span>
        <RangeToggle options={RESOURCE_RANGES} value={rangeKey} onChange={setRangeKey} />
      </div>
      <ResourceTrendWidget window_={range.window} />
      <ElectricityBreakdownWidget window_={range.window} />
    </>
  );
}
