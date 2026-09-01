import { useState } from "react";
import { Bay } from "../components/Bay";
import { Icon } from "../components/Icon";
import { ServiceChip } from "../components/ServicesBar";
import { UnifiedModelCarousel } from "../components/UnifiedModelCarousel";
import { AskSmithButton } from "../components/smith/AskSmithButton";
import { SmithChatTray } from "../components/smith/SmithChatTray";
import { formatCurrency } from "../lib/format";
import { sortConfigCards, type ConfigSortMode } from "../lib/modelSort";
import { providerIconSlug } from "../lib/providerPresets";
import { useConfigCards, useFavorites, useInfraServices, useMetrics, useProviders, useReservations, useSchedulerStatus, useStatus, useUpdateProvider } from "../lib/queries";
import { useSession } from "../lib/session";
import type { Provider, ProviderHealth } from "../lib/types";

// Console usability pass #2, 2026-07-29 (operator feedback: readability +
// things that didn't belong on this page):
// - "Remote offerings" moved to the Models page (RemoteOfferings.tsx) —
//   catalog info, not operational status.
// - External-provider status tiles moved out of the services strip into the
//   Load Bays section, stacked in a fixed column on the right.
// - The resource dials (VRAM / Storage / GPU) left the services grid for
//   their own chip to the right of the services strip, and were reduced to
//   ring + label — the exact "used / total GB" figures were dropped
//   ("Used memory" → "VRAM", "Used storage" → "Storage", GPU's
//   "utilization" sub-line removed).
// - The A0 row's "routing" chip was removed (normal state = noise); only
//   the abnormal passthrough state still shows a chip (ServicesBar.tsx).
//
// Pre-release feedback round, 2026-08-06: Resources and Services merged back
// into one section (the earlier Resources/Services split, and the
// Compressor-savings chips that used to sit in .resources-row, didn't earn
// their keep as a separate section — savings figures live on the Dashboard
// now). Load bays are no longer progressively revealed — showing only
// in-use bays plus the next free one read as an unbalanced, half-empty
// layout; all 4 bays now always render. Same-day follow-up: Services sits
// beside Resources (to its right) in one row, not stacked below it.
//
// Pre-release feedback round, 2026-08-12: the compact smith ActionsTray chip
// dropped from this row — Diagnostics' own PendingActionsCard is the one
// real place to see pending actions; Console keeping a second, redundant
// entry point didn't earn its keep next to the resource/services chips.

// Per-provider status tile (carried over from the deleted StatsStrip's
// provider cells, health helpers included — see that file's history for the
// full lineage: §0.3 credits, BE-3 F4 spend_period, billing_enabled blanking,
// billing_console_url link, BALANCE/SPEND tagging). Usability pass #2
// (2026-07-29): moved from the services strip to a fixed column on the
// right of the Load Bays section.
function healthTitle(h: ProviderHealth): string {
  const parts: string[] = [h.state];
  if (h.source && h.source !== "none") parts.push(`source: ${h.source.replace(/_/g, " ")}`);
  if (h.detail) parts.push(h.detail);
  return parts.join(" · ");
}

function formatCreditsAsOf(epochSec: number): string {
  const d = new Date(epochSec * 1000);
  return d.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false });
}

function ProviderCreditTile({ p }: { p: Provider }) {
  const { canAdmin } = useSession();
  const update = useUpdateProvider();
  const disabled = !p.enabled;
  const supported = p.billing_enabled && p.credits.supported;
  const balance = supported ? p.credits.balance_native : null;
  const spend = supported ? p.credits.spend_period : null;
  const currency = p.credits.currency ?? p.bill_currency;
  const value = balance ?? spend;
  const text = !p.billing_enabled
    ? ""
    : !supported
      ? "no API" // was "no balance API" — clipped in a 1-col-width tile (caught live 2026-07-29)
      : value != null
        ? formatCurrency(value, currency)
        : "—";
  const kindLabel =
    balance != null
      ? `BALANCE${p.credits.as_of != null ? ` · as of ${formatCreditsAsOf(p.credits.as_of)}` : ""}`
      : spend != null
        ? `SPEND · ${p.credits.spend_period_label ?? "period spend"}`
        : null;
  const hasAmount = p.billing_enabled && supported && value != null;
  // 2026-08-14: the light is gone — the chip's border glow carries health
  // (same language as the service chips).
  const glow = disabled ? "off" : ({ reachable: "on", degraded: "warn", down: "crit", unknown: "off" } as const)[p.health.state];
  return (
    <div className={`cchip cchip-provider cchip-${glow}`} title={[disabled ? "disabled" : null, healthTitle(p.health), kindLabel].filter(Boolean).join(" — ")}>
      <Icon slug={providerIconSlug(p.name)} name={p.name} sm />
      <div className="nm">{p.name}</div>
      <span className="pu" style={{ marginLeft: "auto", fontSize: 11, color: hasAmount ? "var(--ok)" : "var(--text-dim)", fontWeight: hasAmount ? 700 : 400 }}>
        {text}
      </span>
      {canAdmin && (
        <button
          className="icon-btn action"
          style={{ width: 20, height: 20, fontSize: 10, flex: "0 0 auto" }}
          disabled={update.isPending}
          title={p.enabled ? "Stop routing this provider (keys and offerings are kept)" : "Resume routing this provider"}
          onClick={(e) => {
            e.stopPropagation();
            update.mutate({ id: p.id, req: { enabled: !p.enabled } });
          }}
        >
          {p.enabled ? "⏸" : "▶"}
        </button>
      )}
      {p.billing_console_url && (
        <a
          href={p.billing_console_url}
          target="_blank"
          rel="noreferrer"
          title={`Open ${p.name}'s billing page`}
          style={{ color: "var(--text-mute)", fontSize: 11, flex: "0 0 auto" }}
          onClick={(e) => e.stopPropagation()}
        >
          ↗
        </a>
      )}
    </div>
  );
}

// Full-width resource bar above smith's suggestions (operator feedback
// 2026-08-15): the Resources dials chip that used to sit beside the services
// strip left with them — two stacked GPU / VRAM bars (label on the left, bar
// filling the rest) give back the at-a-glance utilization. VRAM here is the
// GTT allocation (Strix Halo unified memory — same figure ResourceTrendWidget
// labels "Memory (GTT)"), not system RAM.
function ResourceBar() {
  const metrics = useMetrics();
  const m = metrics.data;

  const vramUsed = m?.gtt_used_bytes ?? null;
  const vramTotal = m?.gtt_total_bytes ?? null;
  const vramPct = vramUsed != null && vramTotal ? (vramUsed / vramTotal) * 100 : null;

  const items = [
    { key: "vram", label: "VRAM", pct: vramPct, fill: "#FF006E" },
  ];

  return (
    <div className="resbar">
      {items.map((it) => (
        <div className="rb-row" key={it.key}>
          <span className="rb-k">{it.label}</span>
          <div className="rb-track">
            <div
              className="rb-fill"
              style={{ width: `${it.pct != null ? Math.max(0, Math.min(100, it.pct)) : 0}%`, background: it.fill }}
            />
          </div>
        </div>
      ))}
    </div>
  );
}

export function Console() {
  const status = useStatus();
  const schedulerStatus = useSchedulerStatus();
  const infraServices = useInfraServices();
  const configCards = useConfigCards("7d");
  const [sortMode, setSortMode] = useState<ConfigSortMode>("alpha");
  const reservations = useReservations();
  const providers = useProviders();
  const favorites = useFavorites();
  const starredIds = new Set(favorites.data?.subject_ids ?? []);

  const providerList = providers.data?.providers ?? [];
  const slotKeys = status.data ? Object.keys(status.data.slot_labels) : [];
  // Operator feedback 2026-08-14: ComfyUI lives in the same column as the
  // providers, not in the services strip.
  const comfyServices = (infraServices.data?.services ?? []).filter((s) => s.name === "ComfyUI");

  return (
    <section className="page">
      <ResourceBar />
      {/* Operator feedback 2026-08-27: replaced the runbook-only suggestions
          tray with a collapsible clone of the "Ask the smith" chat card,
          collapsed by default. */}
      <SmithChatTray />
      {/* Smith deep-link affordance: active collector alerts link to Diagnostics.
          Wave 2 §3 track W2-C — minimal edit, just the deep-link on alert rows.
          Sprint S3-Web adds the one-click "Ask smith" affordance (R5): the
          alert's code/message seed a context card in the smith chat. */}
      {status.data?.alerts && status.data.alerts.length > 0 && (
        <div style={{ marginBottom: 8, display: "flex", flexDirection: "column", gap: 3 }}>
          {status.data.alerts.map((a, i) => (
              <div key={`${a.code}-${i}`} className="cchip" style={{ padding: "5px 10px" }}>
              <span
                className="chip"
                style={{ color: "var(--crit)", borderColor: "color-mix(in srgb, var(--crit) 40%, var(--border))" }}
              >
                {a.code}
              </span>
              <span style={{ fontSize: 11, color: "var(--text-dim)" }}>{a.msg}</span>
              <AskSmithButton
                context={[{ code: a.code, message: a.msg, source: "console", at: Math.floor(Date.now() / 1000), unit: a.unit }]}
                title={`Ask smith about ${a.code}`}
              />
              <a
                href="#help/smith"
                style={{ marginLeft: "auto", fontSize: 10, color: "var(--text-mute)" }}
              >
                investigate →
              </a>
            </div>
          ))}
        </div>
      )}
      {/* Operator feedback 2026-08-14: the Resources section (dials chip)
          left with the services chips — the Dashboard's Resources tab owns
          the utilization picture now. */}
      <div className="bays-row">
        {status.data ? (
          <div className="bays" data-tour-id="bays">
            {slotKeys.map((slotKey) => (
              <Bay
                key={slotKey}
                slotKey={slotKey}
                status={status.data!}
                schedulerStatus={schedulerStatus.data}
                schedulerUpdatedAt={schedulerStatus.dataUpdatedAt}
                configCards={configCards.data?.cards}
                reservations={reservations.data?.reservations}
                onLoadClick={() => document.getElementById("model-gallery")?.scrollIntoView({ behavior: "smooth" })}
              />
            ))}
          </div>
        ) : (
          <div className="empty-note">Loading slot status…</div>
        )}
        {(providerList.length > 0 || comfyServices.length > 0) && (
          <div className="provider-col">
            {comfyServices.map((s) => <ServiceChip key={s.name} s={s} addon />)}
            {comfyServices.length > 0 && providerList.length > 0 && <div className="provider-divider" />}
            {providerList.map((p) => (
              <ProviderCreditTile key={p.name} p={p} />
            ))}
          </div>
        )}
      </div>

      {/* Operator feedback 2026-08-14: the config grid became the same
          playing-card carousel the Models page rides (config variant) —
          flat deck, one slide per config, with the pre-carousel sort
          toggle retained (operator follow-up same day). */}
      <div className="eyebrow" id="model-gallery" style={{ display: "flex", alignItems: "center", gap: 12, flexWrap: "wrap" }}>
        <span>Choose a config</span>
        <div className="sort-toggle" style={{ marginLeft: "auto", display: "flex", gap: 4 }}>
          {(["alpha", "use", "new"] as const).map((m) => (
            <button
              key={m}
              className={`tab ${sortMode === m ? "active" : ""}`}
              style={{ fontSize: 11, padding: "3px 10px" }}
              onClick={() => setSortMode(m)}
            >
              {m === "alpha" ? "A–Z" : m === "use" ? "most used" : "newest"}
            </button>
          ))}
        </div>
      </div>
      {configCards.data && status.data ? (
        <UnifiedModelCarousel
          cards={sortConfigCards(configCards.data.cards, sortMode, starredIds)}
          status={status.data}
          displayCurrency={configCards.data?.display_currency ?? "USD"}
          variant="config"
        />
      ) : (
        <div className="empty-note">Loading config registry…</div>
      )}
    </section>
  );
}
