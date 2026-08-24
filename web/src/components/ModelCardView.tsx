import { lazy, Suspense, useState, type KeyboardEvent, type MouseEvent } from "react";
import { useCatalogModels, useCatalogOfferings, useConfigCards, useProviders } from "../lib/queries";
import { providerIconSlug } from "../lib/providerPresets";
import type { ModelCard, Status } from "../lib/types";
import { CapabilityBar } from "./CapabilityBar";
import { CopyButton } from "./CopyButton";
import { Icon } from "./Icon";
import { PlayingCard } from "./PlayingCard";

// Mobile F7: detail views are click-reached only — out of the initial bundle.
const DetailModal = lazy(() => import("./detail/DetailModal").then((m) => ({ default: m.DetailModal })));

// Playing-card redesign 2026-08-14: the portrait model card is now a
// playing card (PlayingCard chrome) — logo + badge cluster top-left (≤30%
// of the card width), the name in the frame-interrupting nameplate, the
// company mark as a grayscale watermark bottom-right, and a compact centered
// body (creator/license, description, top capabilities, provider +
// benchmark chips, memory/configs footer). Clicking zooms the card into the
// hero (DetailModal's model kind), passing the card's own rect so the hero
// can FLIP-animate from exactly where the card sat.
//
// The `interactive` prop keeps its pre-redesign contract: the family fan
// renders every card inside a bigger click target, so interactive={false}
// strips this card's own click/keyboard handling AND its CopyButton (the
// only nested <button>) entirely.
export function ModelCardView({
  card,
  status,
  displayCurrency = "USD",
  interactive = true,
}: {
  card: ModelCard;
  status: Status;
  displayCurrency?: string;
  interactive?: boolean;
}) {
  const [showDetail, setShowDetail] = useState(false);
  const [detailRect, setDetailRect] = useState<{ x: number; y: number; width: number; height: number } | null>(null);
  // configCards is already fetched by the Models page's sibling queries
  // elsewhere in the app (Console) at the same "7d" window, so this is a
  // cache hit in practice, not a fresh network round-trip per card.
  const configCards = useConfigCards("7d");
  const modelConfigs = (configCards.data?.cards ?? []).filter((c) => c.model_id === card.id);
  const memReqBytes = card.derived.memory_req_bytes;

  // External provider offerings for this model — linked via the catalog
  // model name (ModelCard.name ↔ CatalogModel.name). All three hooks are
  // cached (SETTINGS_STALE_MS), so per-card calls are cache hits.
  const offerings = useCatalogOfferings();
  const catalogModels = useCatalogModels();
  const providers = useProviders();
  const catalogModel = catalogModels.data?.find(
    (m) => m.name === card.name || m.name === card.id,
  );
  const modelOfferings = (offerings.data ?? []).filter((o) => o.model_id === catalogModel?.id);
  const providerList = providers.data?.providers ?? [];
  const providerByName = (name: string) => providerList.find((p) => p.name === name);
  const providerEnabled = (name: string) => providerByName(name)?.enabled ?? true;

  const providerChips: { provider: string; enabled: boolean }[] = [];
  const seenProviders = new Set<string>();
  for (const o of modelOfferings) {
    if (seenProviders.has(o.provider)) continue;
    seenProviders.add(o.provider);
    providerChips.push({ provider: o.provider, enabled: o.enabled && providerEnabled(o.provider) });
  }

  function openDetail(e?: MouseEvent<HTMLDivElement>) {
    if (!interactive) return;
    // The hero FLIP zoom wants the card's on-screen rect at tap time.
    if (e) {
      const r = e.currentTarget.getBoundingClientRect();
      setDetailRect({ x: r.left, y: r.top, width: r.width, height: r.height });
    } else {
      setDetailRect(null);
    }
    setShowDetail(true);
  }
  function onCardKeyDown(e: KeyboardEvent<HTMLDivElement>) {
    if (e.target !== e.currentTarget) return;
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      openDetail();
    }
  }

  return (
    <PlayingCard
      name={card.name}
      logo={card.logo}
      logoDark={card.logo_dark}
      badges={card.badges}
      watermarkName={card.creator}
      className={`portrait${interactive ? " interactive" : ""}`}
      nameExtra={interactive ? <CopyButton text={card.name} title="Copy model name" /> : undefined}
      rootProps={{
        role: interactive ? "button" : undefined,
        tabIndex: interactive ? 0 : undefined,
        onClick: interactive ? openDetail : undefined,
        onKeyDown: interactive ? onCardKeyDown : undefined,
      }}
    >
      <div className="mmaker">
        {card.creator}{card.license_name ? ` · ${card.license_name}` : ""}
      </div>
      {card.description && <div className="mdesc">{card.description}</div>}
      {card.capabilities.length > 0 && (
        <div className="caps">
          {card.capabilities.slice(0, 3).map((cap) => <CapabilityBar key={cap.id} cap={cap} />)}
        </div>
      )}
      {providerChips.length > 0 && (
        <div className="mproviders">
          {providerChips.map((pc) => (
            <span
              key={pc.provider}
              className={`mprov-chip${pc.enabled ? "" : " disabled"}`}
              title={`${pc.provider}: ${pc.enabled ? "enabled" : "disabled"}`}
            >
              <Icon slug={providerIconSlug(pc.provider)} name={pc.provider} sm />
              <span className="mprov-name">{pc.provider}</span>
              <span className={`mprov-dot ${pc.enabled ? "on" : "off"}`} />
            </span>
          ))}
        </div>
      )}
      {card.capabilities.length > 0 && (
        <div className="mbench">
          {card.capabilities.slice(0, 6).map((cap) => (
            <span
              key={cap.id}
              className="mbench-chip"
              title={`${cap.benchmark}: ${(cap.score * 100).toFixed(1)}% raw`}
            >
              {cap.label} {(cap.score * 100).toFixed(1)}
            </span>
          ))}
        </div>
      )}
      <div className="mfoot" style={{ justifyContent: "center", gap: 10 }}>
        {memReqBytes != null && (
          <span className="chip">{Math.round(memReqBytes / (1024 * 1024 * 1024))} GB</span>
        )}
        {/* Configs don't apply to external/remote-only models — hide the
            chip rather than showing "0 configs" when the model is served
            only via offerings (operator feedback 2026-08-14). */}
        {(modelConfigs.length > 0 || modelOfferings.length === 0) && (
          <span className="chip">{modelConfigs.length} config{modelConfigs.length === 1 ? "" : "s"}</span>
        )}
      </div>
      {showDetail && (
        <Suspense fallback={null}>
          <DetailModal
            initial={{ kind: "model", modelId: card.id, fromRect: detailRect ?? undefined }}
            status={status}
            schedulerStatus={undefined}
            displayCurrency={displayCurrency}
            onClose={() => setShowDetail(false)}
          />
        </Suspense>
      )}
    </PlayingCard>
  );
}
