import { useLayoutEffect, useRef, useState } from "react";
import { formatCurrency, formatGB } from "../../lib/format";
import { useCatalogModels, useCatalogOfferings, useConfigCards, useModelCards, useProviders } from "../../lib/queries";
import { providerIconSlug } from "../../lib/providerPresets";
import { useSession } from "../../lib/session";
import type { Status } from "../../lib/types";
import { CapabilityBar } from "../CapabilityBar";
import { ChangeHistory } from "../ChangeHistory";
import { ConfigRow } from "../ConfigRow";
import { CopyButton } from "../CopyButton";
import { Icon } from "../Icon";
import { PlayingCard } from "../PlayingCard";
import { NotesInline } from "./NotesInline";

// ModelHeroView — the zoomed playing card (playing-card redesign
// 2026-08-14). Replaces the old ModelDetailView hero: tapping a model card
// zooms it into a LARGE card that keeps the 5:7 ratio, with a compact
// detail UI on the front (identity, spec table, capabilities, providers,
// the config list with Load buttons) and a real card FLIP (rotateY,
// preserve-3d) to the back — key features, notes, load history, and the
// change history. Edit/Benchmarks still push their panel views onto
// DetailModal's stack via the controls under the card.
//
// Mount animation is FLIP: when opened from a tap, `fromRect` carries the
// tapped card's screen rect and the scene animates from that exact spot to
// its centered resting place; without a rect (keyboard open, "View model →"
// from a config detail) the mhero-in keyframes give a plain pop-in.
export function ModelHeroView({
  modelId,
  fromRect,
  status,
  displayCurrency = "USD",
  canGoBack,
  onBack,
  onClose,
  onEdit,
  onEditBenchmarks,
}: {
  modelId: string;
  fromRect?: { x: number; y: number; width: number; height: number };
  status: Status;
  displayCurrency?: string;
  canGoBack: boolean;
  onBack: () => void;
  onClose: () => void;
  onEdit: () => void;
  onEditBenchmarks: () => void;
}) {
  const { canAdmin } = useSession();
  const modelCards = useModelCards("7d");
  const configCards = useConfigCards("7d");
  const [flipped, setFlipped] = useState(false);
  const sceneRef = useRef<HTMLDivElement>(null);

  // FLIP from the tapped card's rect (mount-only).
  useLayoutEffect(() => {
    const el = sceneRef.current;
    if (!el || !fromRect) return;
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;
    const r = el.getBoundingClientRect();
    if (r.width === 0 || r.height === 0) return;
    const s = Math.max(0.05, fromRect.width / r.width);
    const dx = fromRect.x + fromRect.width / 2 - (r.left + r.width / 2);
    const dy = fromRect.y + fromRect.height / 2 - (r.top + r.height / 2);
    el.style.animation = "none"; // suppress the pop-in keyframes for the FLIP
    el.style.transform = `translate(${dx}px, ${dy}px) scale(${s})`;
    el.style.opacity = "0.5";
    void el.offsetWidth; // flush the start pose before enabling the transition
    el.style.transition = "transform .34s cubic-bezier(.25, .9, .3, 1), opacity .28s ease";
    el.style.transform = "";
    el.style.opacity = "";
    el.addEventListener("transitionend", () => { el.style.transition = ""; }, { once: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps -- mount-only by design
  }, []);

  const card = modelCards.data?.cards.find((c) => c.id === modelId);
  const modelConfigs = (configCards.data?.cards ?? []).filter((c) => c.model_id === modelId);

  // External provider offerings (same cached-hook chain as ModelCardView).
  const offerings = useCatalogOfferings();
  const catalogModels = useCatalogModels();
  const providers = useProviders();
  const catalogModel = catalogModels.data?.find((m) => m.name === card?.name || m.name === card?.id);
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

  if (!card) {
    return (
      <div className="mhero">
        <div className="mhero-scene" ref={sceneRef}>
          <div className="empty-note">Loading model…</div>
        </div>
      </div>
    );
  }

  const powerEstPer1m =
    card.performance.power_est_per_1m != null
      ? card.performance.power_est_per_1m
      : card.performance.power_cost_per_1k > 0
        ? card.performance.power_cost_per_1k * 1000
        : null;
  // Drop consecutive repeats (creator === genealogy === family is common —
  // "DeepSeek · DeepSeek · DeepSeek V4" read as a stutter).
  const makerParts = [card.creator, card.license_name, card.genealogy, card.family];
  const makerLine = makerParts.filter((s, i) => s && s !== makerParts[i - 1]).join(" · ");
  const hist = card.derived.history;

  return (
    <div className="mhero">
      {canGoBack && (
        <button className="icon-btn mhero-backbtn" title="Back" aria-label="Back" onClick={onBack}>←</button>
      )}
      <button className="icon-btn mhero-close" title="Close" aria-label="Close" onClick={onClose}>✕</button>

      <div className="mhero-scene" ref={sceneRef} onClick={(e) => e.stopPropagation()}>
        {/* Operator feedback 2026-08-14: tapping the card flips it. Clicks on
            real controls inside the card (links, Load buttons, copy/edit)
            are left alone. */}
        <div
          className={`mhero-card${flipped ? " flipped" : ""}`}
          onClick={(e) => {
            const t = e.target as HTMLElement;
            if (t.closest("a,button,input,select,textarea,[role='button']")) return;
            setFlipped((f) => !f);
          }}
        >
          {/* ── front: identity + specs + configs ── */}
          <div className="mhero-face front">
            <PlayingCard
              name={card.name}
              logo={card.logo}
              logoDark={card.logo_dark}
              badges={card.badges}
              watermarkName={card.creator}
              className="hero"
              nameExtra={
                <CopyButton text={card.name} title="Copy model name" />
              }
            >
              {canAdmin && (
                <div className="mhero-toolbar">
                  <button className="btn primary sm" onClick={onEdit}>Edit model</button>
                  <button className="btn sm" onClick={onEditBenchmarks}>Benchmarks</button>
                </div>
              )}
              <div className="mhero-maker">{makerLine}</div>
              {card.description && <div className="mhero-desc">{card.description}</div>}
              <div className="spec-table">
                {card.derived.arch && <div className="spec-row"><span className="k">Architecture</span><span className="v">{card.derived.arch}</span></div>}
                {card.modalities.length > 0 && (
                  <div className="spec-row">
                    <span className="k">Modalities</span>
                    <span className="v">{card.modalities.map((m) => m[0].toUpperCase() + m.slice(1)).join(", ")}</span>
                  </div>
                )}
                {card.derived.trained_ctx != null && (
                  <div className="spec-row"><span className="k">Trained context</span><span className="v">{card.derived.trained_ctx.toLocaleString()}</span></div>
                )}
                {card.derived.file_size_bytes != null && (
                  <div className="spec-row"><span className="k">File size</span><span className="v">{formatGB(card.derived.file_size_bytes)} GB</span></div>
                )}
                {card.derived.memory_req_bytes != null && (
                  <div className="spec-row"><span className="k">Memory</span><span className="v">{formatGB(card.derived.memory_req_bytes)} GB</span></div>
                )}
                {powerEstPer1m != null && powerEstPer1m > 0 && (
                  <div className="spec-row"><span className="k">Power est /1M</span><span className="v">~{formatCurrency(powerEstPer1m, displayCurrency)}</span></div>
                )}
                {card.derived.reliability && (
                  <div className="spec-row">
                    <span className="k">Reliability</span>
                    <span className="v">
                      {card.derived.reliability.loads_ok} ok · {card.derived.reliability.load_failures} failed
                      {card.derived.reliability.inference_hangs > 0 && ` · ${card.derived.reliability.inference_hangs} hangs`}
                    </span>
                  </div>
                )}
                {card.license_name && (
                  <div className="spec-row">
                    <span className="k">License</span>
                    <span className="v">
                      {card.license_url ? <a href={card.license_url} target="_blank" rel="noreferrer">{card.license_name}</a> : card.license_name}
                    </span>
                  </div>
                )}
                {card.hf_repo && (
                  <div className="spec-row"><span className="k">HF repo</span><span className="v"><a href={`https://huggingface.co/${card.hf_repo}`} target="_blank" rel="noreferrer">{card.hf_repo}</a></span></div>
                )}
              </div>
              {card.capabilities.length > 0 && (
                <div className="caps">
                  {card.capabilities.slice(0, 4).map((cap) => <CapabilityBar key={cap.id} cap={cap} />)}
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
              <div className="mhero-eyebrow-row">
                <div className="eyebrow" style={{ fontSize: 11 }}>
                  Configs {modelConfigs.length > 0 ? `(${modelConfigs.length})` : ""}
                </div>
              </div>
              <div className="mhero-configs">
                {!configCards.data ? (
                  <div className="empty-note">Loading configs…</div>
                ) : modelConfigs.length === 0 ? (
                  <div className="empty-note">{modelOfferings.length > 0 ? "Remote-only model — served via offerings." : "No configs for this model yet."}</div>
                ) : (
                  modelConfigs.map((c) => <ConfigRow key={c.id} card={c} status={status} />)
                )}
              </div>
            </PlayingCard>
          </div>

          {/* ── back: the story — features, notes, change history ── */}
          <div className="mhero-face back">
            <PlayingCard
              name={card.name}
              logo={card.logo}
              logoDark={card.logo_dark}
              watermarkName={card.creator}
              className="hero"
            >
              {card.key_features.length > 0 && (
                <>
                  <div className="eyebrow" style={{ fontSize: 11 }}>Key features</div>
                  <ul className="mhero-features">
                    {card.key_features.map((f) => <li key={f}>{f}</li>)}
                  </ul>
                </>
              )}
              {hist && (
                <div className="mproviders">
                  {hist.last_result && (
                    <span className="chip" title="Last load result">{hist.last_result}</span>
                  )}
                  {hist.avg_load_time_s != null && (
                    <span className="chip" title="Average load time">{Math.round(hist.avg_load_time_s)}s avg load</span>
                  )}
                  {hist.ctx_reduction_rate > 0 && (
                    <span className="chip" title="Context reduction rate">ctx −{(hist.ctx_reduction_rate * 100).toFixed(0)}%</span>
                  )}
                </div>
              )}
              <NotesInline subjectType="model" subjectId={Number(modelId)} />
              {canAdmin && (
                <>
                  <div className="eyebrow" style={{ fontSize: 11 }}>Change history</div>
                  <ChangeHistory actionPrefix="catalog_model_" target={card.id} />
                </>
              )}
            </PlayingCard>
          </div>
        </div>
      </div>

      <div className="mhero-controls" onClick={(e) => e.stopPropagation()}>
        <button
          type="button"
          className="mhero-flipbtn"
          onClick={() => setFlipped((f) => !f)}
          aria-pressed={flipped}
        >
          {flipped ? "↩ Flip back to the front" : "↻ Flip for history & notes"}
        </button>
      </div>
    </div>
  );
}
