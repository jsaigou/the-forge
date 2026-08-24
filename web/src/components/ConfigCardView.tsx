import { lazy, Suspense, useState, type KeyboardEvent } from "react";
import { apiErrorMessage } from "../lib/api";
import { formatGB, formatCurrency } from "../lib/format";
import { hazardsFor } from "../lib/llamaFlags";
import { useAddFavorite, useFavorites, useProfiles, useRemoveFavorite } from "../lib/queries";
import { useSession } from "../lib/session";
import { useLoadConfig } from "../lib/useLoadConfig";
import type { ConfigCard, SchedulerStatus, Status } from "../lib/types";
import { BadgeColumn } from "./BadgeColumn";
import { CapabilityBar } from "./CapabilityBar";
import { CopyButton } from "./CopyButton";
import { Icon } from "./Icon";
import { InfoTip } from "./InfoTip";
import { LoadConfirmModal } from "./LoadConfirmModal";

// Mobile F7: detail views are click-reached only — out of the initial bundle.
const DetailModal = lazy(() => import("./detail/DetailModal").then((m) => ({ default: m.DetailModal })));

// Config gallery migration (ADR 0006, grilling Q1): strict per-config state.
// A ConfigCard reflects only its own config's slot state — unlike the old
// model-scoped card, it never treats a sibling config of the same model as
// "this card is loaded" (a card for qwen3-coder-1m shows idle even while
// qwen3-coder-256k is loaded elsewhere, and its Load button stays live).
// (Load/evict/409 handling now lives in lib/useLoadConfig.ts — Sprint B
// dedup, previously duplicated verbatim with ModelCardView's ConfigRow.)
//
// Operator feedback 2026-08-14 (config carousel round 2): config cards are
// NOT model cards — they keep their own .mcard visual identity (icon +
// name + star + Load row), not the playing-card chrome. The carousel rides
// them square (1:1, .config-card in theme.css). The `interactive` prop
// mirrors ModelCardView's contract for future non-interactive embeddings:
// strips the card's own click/keyboard handling and its nested buttons.
export function ConfigCardView({
  card,
  status,
  schedulerStatus,
  displayCurrency = "USD",
  interactive = true,
}: {
  card: ConfigCard;
  status: Status;
  schedulerStatus: SchedulerStatus | undefined;
  displayCurrency?: string;
  interactive?: boolean;
}) {
  const { canOperate } = useSession();
  const [showDetail, setShowDetail] = useState(false);
  const loadState = useLoadConfig(card, status);
  const { state, activeSlot, busy, openConfirm, confirming } = loadState;

  // product/QA sprint, 2026-07-29 — starring. favorites.data is undefined
  // while loading; treat as not-starred rather than flashing a false star.
  const favorites = useFavorites();
  const addFavorite = useAddFavorite();
  const removeFavorite = useRemoveFavorite();
  const isStarred = favorites.data?.subject_ids.includes(card.id) ?? false;
  // Surface a failed star/unstar rather than silently doing nothing — this
  // is exactly what the missing-Favorites-dependency 503 (fixed 2026-07-30,
  // see go/cmd/forge/main.go) looked like from here: the click just did
  // nothing and nobody could tell.
  const starError = addFavorite.isError
    ? apiErrorMessage(addFavorite.error)
    : removeFavorite.isError
      ? apiErrorMessage(removeFavorite.error)
      : null;
  function toggleStar() {
    if (isStarred) {
      removeFavorite.mutate({ subjectType: "config", id: card.id });
    } else {
      addFavorite.mutate({ subjectType: "config", id: card.id });
    }
  }

  // FE-2 (docs/v5-review-fixes.md, F6): PROFILE's measured safe_memory_bytes
  // is the honest fit number (weights + KV cache at real max context) — prefer
  // it over the curated, weight-only memory_req_bytes whenever a fresh
  // profile exists for the mode that would actually load. Falls back to the
  // curated estimate (and is flagged "unprofiled" in the UI) otherwise.
  // A1 bytes retrofit: the budget figure is bytes too; no MB↔GB conversions.
  const { data: profilesResp } = useProfiles();
  const profile = profilesResp?.profiles.find((p) => p.mode === card.name);
  const hasFreshProfile = !!profile && !profile.stale;
  const memReqBytes = hasFreshProfile ? profile.safe_memory_bytes : card.derived.memory_req_bytes;
  const fits =
    memReqBytes != null && schedulerStatus
      ? memReqBytes <= schedulerStatus.memory_budget.free_bytes
      : null;

  // §0.2: power_est_per_1m is the canonical cost field; fall back to the
  // deprecated power_cost_per_1k (×1000) if the new field is absent.
  const powerEstPer1m =
    card.performance.power_est_per_1m != null
      ? card.performance.power_est_per_1m
      : card.performance.power_cost_per_1k > 0
        ? card.performance.power_cost_per_1k * 1000
        : null;

  // Sprint B: hazardous flags (e.g. a --parallel that silently splits
  // rather than multiplies context) get a badge on the compact face, not
  // just an explanation buried in the expanded view — an operator scanning
  // the gallery should be able to see it without opening anything.
  const hazards = hazardsFor(card);

  // Usability pass #3 (2026-07-30): the card body opens the expanded detail
  // view — this is the ⓘ button's old destination, promoted to the whole
  // card now that ⓘ is gone. Every interactive child (star, edit,
  // load/evict) must stopPropagation or its click would also open the modal.
  function openDetailFromCard() {
    setShowDetail(true);
  }
  function onCardKeyDown(e: KeyboardEvent<HTMLDivElement>) {
    if (e.target !== e.currentTarget) return; // let inner buttons handle their own keys
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      openDetailFromCard();
    }
  }

  return (
    <div
      className="mcard config-card"
      role={interactive ? "button" : undefined}
      tabIndex={interactive ? 0 : undefined}
      onClick={interactive ? openDetailFromCard : undefined}
      onKeyDown={interactive ? onCardKeyDown : undefined}
    >
      <div className="mtop">
        {/* Sprint K (operator addition): badges move off the icon into
            their own vertical column between it and the name/creator text
            — see BadgeColumn.tsx and theme.css's .bcolumn block for why
            this is a separate component from BadgeCluster rather than a
            layout variant on it. */}
        <div className="bcluster"><Icon slug={card.logo} slugDark={card.logo_dark} name={card.model_name} /></div>
        <BadgeColumn badges={card.badges} />
        <div style={{ flex: "1 1 auto", minWidth: 0 }}>
          <div className="mname">
            {card.name}
            {interactive && <CopyButton text={card.name} title="Copy config name" />}
            {hazards.length > 0 && (
              <span onClick={(e) => e.stopPropagation()}>
                <InfoTip text={hazards.map((h) => h.message).join(" ")} className="hazard-tip">
                  <span className="hazard-badge" aria-hidden="true">⚠</span>
                </InfoTip>
              </span>
            )}
          </div>
          <div className="mmaker">
            {[card.creator, card.license_name, card.family].filter(Boolean).join(" · ")}
          </div>
        </div>
        {/* product/QA sprint, 2026-07-29: the MODEL button is gone (Models
            is reachable via its own tab; this card doesn't need a
            cross-tab shortcut) — replaced with a star toggle. Starred
            configs sort first (see lib/modelSort.ts) and appear in a
            starred list. */}
        {interactive && (
          <button
            className={`icon-btn star-btn ${isStarred ? "starred" : ""}`.trim()}
            style={{ color: starError ? "var(--crit)" : undefined }}
            title={starError ?? (isStarred ? "Unstar" : "Star this config")}
            disabled={addFavorite.isPending || removeFavorite.isPending}
            onClick={(e) => { e.stopPropagation(); toggleStar(); }}
          >
            {isStarred ? "★" : "☆"}
          </button>
        )}
      </div>
      {card.description && <div className="mdesc">{card.description}</div>}
      {card.capabilities.length > 0 && (
        <div className="caps">
          {card.capabilities.slice(0, 3).map((cap) => <CapabilityBar key={cap.id} cap={cap} />)}
        </div>
      )}
      <div className="mfoot">
        <div className="perf" title={hasFreshProfile ? "measured (weights + KV cache at max context)" : "estimated from weight size — run the profiler for a measured value"}>
          <span className="k">memory</span>
          {memReqBytes != null ? `${formatGB(memReqBytes)} GB` : "—"}
        </div>
        {/* FE-2 (F6): decode_tps is PROFILE's measured generation throughput —
            the card previously rendered nothing here even though the design
            called for it. There is no fallback to the curated measured_ts:
            that field is being retired (F7) as untrustworthy, so an
            unprofiled model shows a blank figure instead of a number nobody
            measured (profiling/pricing sprint 2026-08-07: dropped the
            "unprofiled" text entirely — these estimates carry inherent
            uncertainty, so a qualifier adds noise rather than signal). */}
        <div className="perf">
          <span className="k">T/s</span>
          {hasFreshProfile ? profile.decode_tps.toFixed(1) : ""}
        </div>
        {powerEstPer1m != null && powerEstPer1m > 0 && (
          <div className="perf">
            <span className="k">power est /1M</span>
            ~{formatCurrency(powerEstPer1m, displayCurrency)}
          </div>
        )}
        {interactive && (!canOperate ? null : state === "loaded" ? (
          <button className="go loaded" disabled>
            Loaded{activeSlot ? ` · ${status.slot_labels[activeSlot]}` : ""}
          </button>
        ) : state === "loading" ? (
          <button className="go loaded loading-glare" disabled>
            <span
              style={{
                display: "inline-block",
                width: 7,
                height: 7,
                borderRadius: "50%",
                background: "var(--heat)",
                marginRight: 6,
                animation: "pulse 2.4s ease-in-out infinite",
              }}
            />
            Loading…
          </button>
        ) : (
          <button
            className={`go ${state === "evict-needed" && fits === false ? "wontfit" : ""}`}
            disabled={busy}
            onClick={(e) => { e.stopPropagation(); openConfirm(); }}
          >
            {state === "evict-needed" ? "Evict & load" : "Load"}
          </button>
        ))}
      </div>
      {confirming && <LoadConfirmModal card={card} status={status} loadState={loadState} />}
      {showDetail && (
        <Suspense fallback={null}>
          <DetailModal
            initial={{ kind: "config", card }}
            status={status}
            schedulerStatus={schedulerStatus}
            displayCurrency={displayCurrency}
            onClose={() => setShowDetail(false)}
          />
        </Suspense>
      )}
    </div>
  );
}
