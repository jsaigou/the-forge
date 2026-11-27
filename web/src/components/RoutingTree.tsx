// RoutingTree — provider→model routing dendrogram. Extracted from
// settings/panels/Routing.tsx (Sprint 10, docs/v5-headroom-replacement.md)
// so the Dashboard can render the same diagram read-only, matching the
// TrendChart/ResourceDial extraction precedent from the Dashboard
// cost/savings sprint. Provider nodes (icon + name) sit left, model nodes
// (icon + name) right, each offering a curved bezier link. Line thickness
// encodes priority in five buckets — HIGHER priority value = thicker line
// (the operator's mental model; note this is the reverse of the router's
// "lower value = more preferred" semantics — the diagram answers "how
// strongly prioritized is this link", not "which is served first"; the
// preferred offering is called out in Settings → Routing's editable list).
//
// Link color/state is the provider's health, plus whether the route is on at
// all. Every request goes straight to the provider (the prompt compressor is
// retired, ADR-0017), so there is no compression state to show. Every
// distinction has a legend entry and the unreachable one is also dashed, so
// nothing rides on hue alone.
//
//   reachable     green solid       provider health ok
//   degraded      amber solid       provider reports a partial incident
//   unreachable   red dashed        provider down
//   unknown       neutral solid     no health reading yet
//   disabled      gray, faded       offering or provider disabled
//
// readOnly (the Dashboard copy) suppresses the admin-only long-press
// enable/disable interaction; Settings → Routing keeps it.
//
// Model layout toggle (operator feedback 2026-09-13): providers (left) are
// always alphabetical — that's an invariant, not a mode. Models (right) can
// either stay plain A-Z (default, unchanged from before this toggle existed)
// or be regrouped to cluster near their primary provider's row — "primary"
// mirrors offeringPreference.ts's own rule (lowest priority among enabled
// offerings of enabled providers, same tiebreak), so grouping can never
// disagree with the "preferred" badge shown elsewhere. Grouping is a pure
// display reorder — it doesn't change any link's color/state/priority.
//
// Legend placement: moved off its own full-width row (operator feedback
// 2026-09-13) into the diagram's bottom-left corner, which — since real
// deployments always have far fewer providers than models — sits empty
// below the last provider row. It renders after the node maps (last in DOM)
// so it paints above the SVG's link curves, which do pass through that
// corner; a panel backdrop keeps it legible over them.
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Icon } from "./Icon";
import { creatorIconSlug } from "../lib/creatorIcon";
import { groupOfferingsByModel } from "../lib/offeringPreference";
import { providerIconSlug } from "../lib/providerPresets";
import { RangeToggle } from "./RangeToggle";
import {
  useCatalogModels,
  useCatalogOfferings,
  useProviders,
  useUpdateProvider,
} from "../lib/queries";
import type { CatalogOffering } from "../lib/types";

// Labels come from the "common" translation namespace (routing_tree.layout_*),
// built inside the component where useTranslation is available.
const LAYOUT_MODE_KEYS = ["az", "group"] as const;
type LayoutMode = (typeof LAYOUT_MODE_KEYS)[number];

type LinkVisual = { cls: string; color: string; opacity: number; dash?: string };

// useTextMeasure: width in px of a label in the diagram's own font (canvas measureText, no layout pass). Falls back to a
// per-character estimate where canvas is unavailable.
function useTextMeasure(el: HTMLElement | null): (text: string) => number {
  const ctx = typeof document !== "undefined" ? document.createElement("canvas").getContext("2d") : null;
  if (ctx && el) ctx.font = `12px ${getComputedStyle(el).fontFamily}`;
  return (text: string) => (ctx && el ? ctx.measureText(text).width : text.length * 7);
}

export function RoutingTree({ readOnly = false }: { readOnly?: boolean }) {
  const { t } = useTranslation("common");
  const layoutModeOptions = LAYOUT_MODE_KEYS.map((key) => ({ key, label: t(`routing_tree.layout_${key}`) }));
  const [layoutMode, setLayoutMode] = useState<LayoutMode>("az");
  const offerings = useCatalogOfferings();
  const models = useCatalogModels();
  const providers = useProviders();
  const updateProvider = useUpdateProvider();
  const [box, setBox] = useState<HTMLDivElement | null>(null);
  const [boxWidth, setBoxWidth] = useState(0);
  useLayoutEffect(() => {
    if (!box) return;
    const measure = () => setBoxWidth(Math.round(box.getBoundingClientRect().width));
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(box);
    return () => ro.disconnect();
  }, [box]);

  const offeringList = offerings.data ?? [];
  // Dashboard feedback (2026-09-13): the read-only Dashboard copy hides
  // disabled offerings (and, transitively, any provider/model left with
  // nothing enabled) entirely rather than showing them grayed out — an
  // at-a-glance operations view shouldn't include inert routes. Settings →
  // Routing (readOnly=false) keeps everything visible, since seeing and
  // managing what's disabled is the point of that view.
  const visibleOfferings = readOnly ? offeringList.filter((o) => o.enabled) : offeringList;
  const modelList = models.data ?? [];
  const providerList = providers.data?.providers ?? [];
  const providerByName = new Map(providerList.map((p) => [p.name, p]));
  const modelById = new Map(modelList.map((m) => [m.id, m]));
  // Providers are always alphabetical — an invariant, not part of the
  // layout toggle below (see file header).
  const providerNames = [...new Set(visibleOfferings.map((o) => o.provider))].sort();
  const providerIndex = new Map(providerNames.map((n, i) => [n, i]));
  // "Group" mode's per-model primary provider — same rule as
  // offeringPreference.ts's preferredOfferingIds (lowest priority among
  // enabled offerings of enabled providers, tiebreak provider name then id),
  // so a model's group can never disagree with the "preferred" badge shown
  // elsewhere. Falls back to the group's first entry (any offering) for a
  // model with nothing routable, so it still lands in a deterministic spot
  // instead of being excluded from grouping.
  const providerEnabled = (name: string) => providerByName.get(name)?.enabled ?? true;
  const offeringsByModel = groupOfferingsByModel(visibleOfferings);
  function primaryProvider(modelId: number): string {
    const group = offeringsByModel.get(modelId) ?? [];
    const routable = group.find((o) => o.enabled && providerEnabled(o.provider));
    return (routable ?? group[0])?.provider ?? "";
  }
  const modelIds = [...new Set(visibleOfferings.map((o) => o.model_id))].sort((a, b) => {
    const na = modelById.get(a)?.name ?? `#${a}`;
    const nb = modelById.get(b)?.name ?? `#${b}`;
    if (layoutMode === "group") {
      const ia = providerIndex.get(primaryProvider(a)) ?? Number.MAX_SAFE_INTEGER;
      const ib = providerIndex.get(primaryProvider(b)) ?? Number.MAX_SAFE_INTEGER;
      if (ia !== ib) return ia - ib;
    }
    return na.localeCompare(nb);
  });

  // Layout is computed in REAL pixels from the measured container width (design/QA 2026-11-26). It used to draw the links
  // in a fixed 700-unit SVG viewBox scaled to the container while the icon/label nodes stayed at fixed pixel offsets, so in
  // any container narrower than 700px the line ends drifted off the icons (the SVG also shrank vertically). Now the SVG is
  // 1:1 with the container and node columns shrink with it, so a link always ends exactly at its icon.
  const ROW_H = 56;
  const PAD = 24;
  const NODE_H = 36;
  const NODE_GAP = 12; // between a node column and the link ends
  const MIN_LINK_SPAN = 90; // the curves need at least this much horizontal room
  const W = Math.max(280, Math.min(boxWidth || 600, 900));
  // Each node column is sized to its widest label (icon 26 + gap 6 + text + slack), not a fixed 200px, so the links get all the
  // remaining width instead of being squeezed into the middle between two mostly empty columns. A column is capped so the
  // links keep MIN_LINK_SPAN; past the cap the label ellipsizes.
  const textW = useTextMeasure(box);
  const colCap = Math.max(60, Math.floor((W - MIN_LINK_SPAN) / 2) - NODE_GAP);
  const NODE_EXTRA = 26 + 6 + 6; // icon + gap + slack
  const provLabelW = (n: string) => textW(n) + (providerByName.get(n)?.enabled === false ? 44 : 0);
  const PROV_W = Math.min(colCap, Math.ceil(Math.max(0, ...providerNames.map(provLabelW))) + NODE_EXTRA);
  const MODEL_W = Math.min(colCap, Math.ceil(Math.max(0, ...modelIds.map((id) => textW(modelById.get(id)?.name ?? t("routing_tree.model_fallback_name", { id }))))) + NODE_EXTRA);
  const LEFT_X = PROV_W + NODE_GAP;
  const RIGHT_X = W - MODEL_W - NODE_GAP;
  const rows = Math.max(providerNames.length, modelIds.length);
  // Legend sits below the last provider row (see the legend div's own
  // comment) rather than pinned to the diagram's bottom edge — operator
  // feedback 2026-09-13: anchoring to the bottom made it climb up over the
  // provider column whenever models outnumbered providers by enough that
  // the empty gap below the last provider row was shorter than the legend
  // itself. LEGEND_H is a fixed estimate (5 entries in two columns + a
  // full-width note that wraps to two lines), reserved in H so the legend never overflows past the
  // diagram's own bottom edge either.
  const LEGEND_TOP = PAD + providerNames.length * ROW_H + 8;
  const LEGEND_H = 128;
  // The read-only Dashboard copy now sits in a half-width column, where the overlaid legend would cover the
  // links (design/QA 2026-11-26): there it flows below the diagram instead of overlaying it.
  const legendBelow = readOnly;
  const H = Math.max(rows * ROW_H + PAD * 2, 80, legendBelow ? 0 : LEGEND_TOP + LEGEND_H);
  const midX = (LEFT_X + RIGHT_X) / 2;

  const providerY = new Map(providerNames.map((n, i) => [n, PAD + i * ROW_H + ROW_H / 2]));
  const modelY = new Map(modelIds.map((id, i) => [id, PAD + i * ROW_H + ROW_H / 2]));

  // Long-press enable/disable (Settings copy only): pointerdown starts a
  // 600ms timer + a radial progress ring; holding to completion toggles the
  // provider. A quick tap does nothing (operator asked specifically for a
  // long-touch interaction).
  const [holding, setHolding] = useState<string | null>(null);
  const holdTimer = useRef<number | null>(null);
  function startHold(name: string) {
    if (readOnly) return;
    setHolding(name);
    if (holdTimer.current != null) clearTimeout(holdTimer.current);
    holdTimer.current = window.setTimeout(() => {
      holdTimer.current = null;
      setHolding(null);
      const p = providerByName.get(name);
      if (p) updateProvider.mutate({ id: p.id, req: { enabled: !p.enabled } });
    }, 600);
  }
  function cancelHold() {
    if (holdTimer.current != null) { clearTimeout(holdTimer.current); holdTimer.current = null; }
    setHolding(null);
  }
  useEffect(() => () => { if (holdTimer.current != null) clearTimeout(holdTimer.current); }, []);

  // Five priority buckets — HIGHER priority value = thicker line (operator's
  // mental model; see the file header for the router-semantics caveat).
  function strokeW(priority: number): number {
    if (priority <= 0) return 1.5;
    if (priority <= 25) return 2.5;
    if (priority <= 50) return 3.5;
    if (priority <= 75) return 4.5;
    return 6;
  }

  function linkState(o: CatalogOffering): LinkVisual {
    const prov = providerByName.get(o.provider);
    const provOn = prov?.enabled ?? true;
    if (!provOn || !o.enabled) return { cls: "disabled", color: "var(--text-mute)", opacity: 0.3 };
    switch (prov?.health?.state) {
      case "down":
        return { cls: "unreachable", color: "var(--crit)", opacity: 0.8, dash: "3 4" };
      case "degraded":
        return { cls: "degraded", color: "var(--heat-2)", opacity: 0.9 };
      case "reachable":
        return { cls: "reachable", color: "var(--ok)", opacity: 0.9 };
      default:
        return { cls: "unknown", color: "var(--text-dim)", opacity: 0.8 };
    }
  }

  if (offeringList.length === 0) {
    return <div className="empty-note">{t("routing_tree.no_offerings")}</div>;
  }
  if (visibleOfferings.length === 0) {
    return <div className="empty-note">{t("routing_tree.all_disabled")}</div>;
  }

  const lpressCircumference = 2 * Math.PI * 17;

  const legendEl = (below: boolean) => (
    <div className={below ? "rtree-legend rtree-legend-below" : "rtree-legend"} role="doc-tip" style={below ? undefined : { top: LEGEND_TOP }}>
      <span className="rtree-lg"><i className="rtree-lg-line reachable" /> {t("routing_tree.legend_reachable")}</span>
      <span className="rtree-lg"><i className="rtree-lg-line degraded" /> {t("routing_tree.legend_degraded")}</span>
      <span className="rtree-lg"><i className="rtree-lg-line unreachable" /> {t("routing_tree.legend_unreachable")}</span>
      <span className="rtree-lg"><i className="rtree-lg-line unknown" /> {t("routing_tree.legend_unknown")}</span>
      <span className="rtree-lg"><i className="rtree-lg-line disabled" /> {t("routing_tree.legend_disabled")}</span>
      <span className="rtree-lg-note">
        {t("routing_tree.legend_note")}{readOnly ? "" : t("routing_tree.legend_note_hold_suffix")}
      </span>
    </div>
  );

  return (
    <>
      <div style={{ display: "flex", justifyContent: "flex-end", marginBottom: 8 }}>
        <RangeToggle options={layoutModeOptions} value={layoutMode} onChange={setLayoutMode} />
      </div>
      <div ref={setBox} className={`rtree${readOnly ? " rtree-readonly" : ""}`} style={{ position: "relative", width: "100%", maxWidth: 760, height: H, margin: "0 auto" }}>
        <svg width={W} height={H} viewBox={`0 0 ${W} ${H}`} style={{ position: "absolute", left: 0, top: 0, display: "block", pointerEvents: "none" }}>
          {visibleOfferings.map((o) => {
            const py = providerY.get(o.provider);
            const my = modelY.get(o.model_id);
            if (py === undefined || my === undefined) return null;
            const st = linkState(o);
            const d = `M ${LEFT_X} ${py} C ${midX} ${py}, ${midX} ${my}, ${RIGHT_X} ${my}`;
            return (
              <path
                key={o.id}
                d={d}
                fill="none"
                stroke={st.color}
                strokeWidth={strokeW(o.priority)}
                opacity={st.opacity}
                strokeDasharray={st.dash}
                className={`rtree-link ${st.cls}`}
              />
            );
          })}
        </svg>
        {providerNames.map((name) => {
          const y = providerY.get(name)!;
          const prov = providerByName.get(name);
          const isHolding = holding === name;
          return (
            <div
              key={`p-${name}`}
              className="rtree-node rtree-prov"
              style={{ top: y - NODE_H / 2, left: 0, width: PROV_W, height: NODE_H }}
              onPointerDown={() => startHold(name)}
              onPointerUp={cancelHold}
              onPointerLeave={cancelHold}
              onPointerCancel={cancelHold}
              title={readOnly ? undefined : t("routing_tree.hold_to_toggle_title")}
            >
              {isHolding && (
                <svg className="rtree-lpress" width={40} height={40} viewBox="0 0 40 40" aria-hidden="true">
                  <circle cx={20} cy={20} r={17} fill="none" stroke="var(--heat)" strokeWidth={3} strokeLinecap="round" strokeDasharray={lpressCircumference} />
                </svg>
              )}
              {prov && !prov.enabled && <span className="chip" style={{ color: "var(--warn)" }}>{t("routing_tree.off_chip")}</span>}
              <span className="rtree-label">{name}</span>
              <Icon slug={providerIconSlug(name)} name={name} sm />
            </div>
          );
        })}
        {modelIds.map((id) => {
          const y = modelY.get(id)!;
          const m = modelById.get(id);
          const name = m?.name ?? t("routing_tree.model_fallback_name", { id });
          // Models missing a catalog logo fall back to the creator's brand icon
          // (then the letter badge) instead of rendering nothing.
          const modelSlug = m?.logo || creatorIconSlug(m?.creator);
          return (
            <div key={`m-${id}`} className="rtree-node rtree-model" style={{ top: y - NODE_H / 2, left: RIGHT_X + NODE_GAP, width: MODEL_W, height: NODE_H }}>
              <Icon slug={modelSlug} slugDark={m?.logo_dark} name={name} sm />
              <span className="rtree-label">{name}</span>
            </div>
          );
        })}
        {/* Anchored below the last provider row (LEGEND_TOP, computed above
            H), after the node maps so it paints above the SVG's link curves
            (see file header). */}
        {!legendBelow && legendEl(false)}
      </div>
      {legendBelow && legendEl(true)}
    </>
  );
}
