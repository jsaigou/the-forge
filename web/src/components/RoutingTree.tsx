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
// Link color/state (Sprint 10): was four states encoding only "does this
// route go via a proxy at all"; now a real state × health matrix driven by
// each proxy's compressor_state/compressor_resource_health from
// GET /api/v1/infra-services (Sprint 3/4) plus the effective-proxy
// resolution the router itself does (dedicated link, else the shared
// "external" instance when compressor.external_enabled is on — the
// external_enabled flag arrived on GET /api/v1/compressor/config in this
// sprint; providers[].compressor_proxy alone only carries dedicated links).
// The encoding grammar: hue = severity (green/teal healthy, amber/orange
// caution, red requests-failing, neutral off-path), dash = the compression
// path itself is broken (traffic flows around it or not at all), glow = a
// compressor is actively in the path. Palette-validated per the dataviz
// guidance; every distinction also has a legend entry, so nothing rides on
// hue alone.
//
//   direct        green solid          no compressor on this route
//   compressing   teal solid + glow    compressing, resource health ok
//   degraded      amber solid + glow   compressing but restart-looping or
//                                      leaking memory — still passing traffic
//   bypassed      blue solid           operator passthrough (global or this
//                                      proxy) — direct by deliberate choice
//                                      (own --route-bypass token, not
//                                      --reserved — see theme.css)
//   autobypass    orange dashed        compressor DOWN; Sprint 8's per-request
//                                      auto-bypass is routing these straight
//                                      to the real upstream, uncompressed
//   failing       red solid            compressor down AND the upstream
//                                      unreachable — requests fail at both
//                                      layers
//   unreachable   red dashed           provider unreachable (pre-existing;
//                                      proxy healthy or absent)
//   disabled      gray, faded          offering or provider disabled
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
import { useEffect, useRef, useState } from "react";
import { Icon } from "./Icon";
import { creatorIconSlug } from "../lib/creatorIcon";
import { groupOfferingsByModel } from "../lib/offeringPreference";
import { providerIconSlug } from "../lib/providerPresets";
import { RangeToggle } from "./RangeToggle";
import {
  useCatalogModels,
  useCatalogOfferings,
  useCompressorConfig,
  useInfraServices,
  useProviders,
  useUpdateProvider,
} from "../lib/queries";
import type { CatalogOffering, CompressorProxy, InfraService } from "../lib/types";

const LAYOUT_MODES = [
  { key: "az", label: "A–Z" },
  { key: "group", label: "Group" },
] as const;
type LayoutMode = (typeof LAYOUT_MODES)[number]["key"];

const COMPRESSOR_ROW_PREFIX = "Compressor (";

type LinkVisual = { cls: string; color: string; opacity: number; dash?: string };

export function RoutingTree({ readOnly = false }: { readOnly?: boolean }) {
  const [layoutMode, setLayoutMode] = useState<LayoutMode>("az");
  const offerings = useCatalogOfferings();
  const models = useCatalogModels();
  const providers = useProviders();
  const updateProvider = useUpdateProvider();
  const compressor = useCompressorConfig();
  const infraServices = useInfraServices();

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
  // Provider→DEDICATED proxy comes from the Compressor config (the Provider
  // GET shape doesn't surface compressor_proxy; the compressor config's
  // providers list is the join of router_providers to their linked
  // compressor_proxies row).
  const linkedProxyByProviderName = new Map(
    (compressor.data?.providers ?? []).map((p) => [p.name, p.compressor_proxy]),
  );
  const proxyByService = new Map<string, CompressorProxy>(
    (compressor.data?.proxies ?? []).map((p) => [p.service, p]),
  );
  // Per-proxy live state/health, keyed by service name — the rows are
  // server-computed in services_handlers.go's compressorServiceRows (the
  // same branch logic behind the Console chip), so the diagram never
  // re-derives posture from prose.
  const proxyInfoByService = new Map<string, InfraService>();
  for (const s of infraServices.data?.services ?? []) {
    if (s.name.startsWith(COMPRESSOR_ROW_PREFIX)) {
      proxyInfoByService.set(s.name.slice(COMPRESSOR_ROW_PREFIX.length, -1), s);
    }
  }

  // Effective proxy for a provider, mirroring the router's
  // remoteCompressorBaseURL: a dedicated (non-orphaned) link wins; otherwise
  // the shared "external" instance when external fronting is enabled; else
  // "" (direct). A dedicated link pointing at a missing/orphaned row falls
  // through exactly like the router's OrphanedAt check does.
  function effectiveProxy(providerName: string): string {
    const dedicated = linkedProxyByProviderName.get(providerName) ?? "";
    const row = dedicated ? proxyByService.get(dedicated) : undefined;
    if (dedicated && row && !row.orphaned) return dedicated;
    if (compressor.data?.external_enabled) {
      const ext = proxyByService.get("external");
      if (ext && !ext.orphaned) return "external";
    }
    return "";
  }

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

  // Double spacing (ROW_H 28→56) so provider/model icons + names breathe.
  const ROW_H = 56;
  const PAD = 24;
  const LEFT_X = 168;
  const RIGHT_X = 532;
  const W = 700;
  const rows = Math.max(providerNames.length, modelIds.length);
  // Legend sits below the last provider row (see the legend div's own
  // comment) rather than pinned to the diagram's bottom edge — operator
  // feedback 2026-09-13: anchoring to the bottom made it climb up over the
  // provider column whenever models outnumbered providers by enough that
  // the empty gap below the last provider row was shorter than the legend
  // itself. LEGEND_H is a fixed estimate (7 two-column entries + 1
  // full-width note), reserved in H so the legend never overflows past the
  // diagram's own bottom edge either.
  const LEGEND_TOP = PAD + providerNames.length * ROW_H + 8;
  const LEGEND_H = 118;
  const H = Math.max(rows * ROW_H + PAD * 2, 80, LEGEND_TOP + LEGEND_H);
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

    const service = effectiveProxy(o.provider);
    const info = service ? proxyInfoByService.get(service) : undefined;
    const state = info?.compressor_state ?? "";
    const health = info?.compressor_resource_health ?? "";
    const provDown = prov?.health?.state === "down";

    if (service && state === "down") {
      // The unit on this route is down. Sprint 8 retries such requests
      // straight against the real upstream, so traffic still flows — unless
      // the upstream itself is unreachable, in which case both layers are
      // broken and the request fails.
      if (provDown) return { cls: "failing", color: "var(--crit)", opacity: 0.9 };
      return { cls: "autobypass", color: "var(--heat)", opacity: 0.9, dash: "3 4" };
    }
    if (provDown) return { cls: "unreachable", color: "var(--crit)", opacity: 0.8, dash: "3 4" };
    if (!service) return { cls: "direct", color: "var(--ok)", opacity: 0.9 };
    // Operator passthrough (global or per-proxy) — direct by deliberate
    // choice, distinct from Sprint 8's failure-driven auto-bypass above.
    if (state === "bypassed") return { cls: "bypassed", color: "var(--route-bypass)", opacity: 0.8 };
    // Still compressing, but the process is restart-looping or leaking
    // (Sprint 4's resource health) — traffic passes, health is not ok.
    if (health === "restarting" || health === "memory_growth") {
      return { cls: "degraded", color: "var(--heat-2)", opacity: 0.9 };
    }
    // "compressing" (and defensively "idle"/unknown — no live remote route
    // can produce them): a compressor is in the path and healthy.
    return { cls: "compressing", color: "var(--cool)", opacity: 0.9 };
  }

  if (offeringList.length === 0) {
    return <div className="empty-note">No offerings configured — nothing to map yet.</div>;
  }
  if (visibleOfferings.length === 0) {
    return <div className="empty-note">All offerings are currently disabled.</div>;
  }

  const lpressCircumference = 2 * Math.PI * 17;

  return (
    <>
      <div style={{ display: "flex", justifyContent: "flex-end", marginBottom: 8 }}>
        <RangeToggle options={LAYOUT_MODES} value={layoutMode} onChange={setLayoutMode} />
      </div>
      <div className={`rtree${readOnly ? " rtree-readonly" : ""}`} style={{ position: "relative", width: "100%", maxWidth: 720, height: H, margin: "0 auto" }}>
        <svg width={W} height={H} viewBox={`0 0 ${W} ${H}`} style={{ position: "absolute", inset: 0, width: "100%", height: "100%", display: "block", pointerEvents: "none" }}>
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
              style={{ top: y - 18, left: 0, width: LEFT_X - 14 }}
              onPointerDown={() => startHold(name)}
              onPointerUp={cancelHold}
              onPointerLeave={cancelHold}
              onPointerCancel={cancelHold}
              title={readOnly ? undefined : "Hold to enable/disable this provider"}
            >
              {isHolding && (
                <svg className="rtree-lpress" width={40} height={40} viewBox="0 0 40 40" aria-hidden="true">
                  <circle cx={20} cy={20} r={17} fill="none" stroke="var(--heat)" strokeWidth={3} strokeLinecap="round" strokeDasharray={lpressCircumference} />
                </svg>
              )}
              <Icon slug={providerIconSlug(name)} name={name} sm />
              <span className="rtree-label">{name}</span>
              {prov && !prov.enabled && <span className="chip" style={{ color: "var(--warn)" }}>off</span>}
            </div>
          );
        })}
        {modelIds.map((id) => {
          const y = modelY.get(id)!;
          const m = modelById.get(id);
          const name = m?.name ?? `Model #${id}`;
          // Models missing a catalog logo fall back to the creator's brand icon
          // (then the letter badge) instead of rendering nothing.
          const modelSlug = m?.logo || creatorIconSlug(m?.creator);
          return (
            <div key={`m-${id}`} className="rtree-node rtree-model" style={{ top: y - 18, right: 0, width: W - RIGHT_X - 14 }}>
              <Icon slug={modelSlug} slugDark={m?.logo_dark} name={name} sm />
              <span className="rtree-label">{name}</span>
            </div>
          );
        })}
        {/* Anchored below the last provider row (LEGEND_TOP, computed above
            H), after the node maps so it paints above the SVG's link curves
            (see file header). "direct" (no compressor anywhere in the path)
            is dropped from this legend: with the shared "external" proxy
            covering every provider that has no dedicated one, every
            currently-enabled route always has SOME compressor in its path
            on this deployment, so that line/color never actually appears —
            it'd be pure noise. linkState's `direct` case itself stays (a
            defensive fallback if that ever stops being true, e.g. the
            operator disables the shared external proxy), it's just no
            longer documented in the key since it can't happen today. */}
        <div className="rtree-legend" role="doc-tip" style={{ top: LEGEND_TOP }}>
          <span className="rtree-lg"><i className="rtree-lg-line compressing" /> compressing</span>
          <span className="rtree-lg"><i className="rtree-lg-line degraded" /> compressing · degraded</span>
          <span className="rtree-lg"><i className="rtree-lg-line bypassed" /> operator bypass</span>
          <span className="rtree-lg"><i className="rtree-lg-line autobypass" /> auto-bypassed (compressor down)</span>
          <span className="rtree-lg"><i className="rtree-lg-line failing" /> failing (compressor + upstream down)</span>
          <span className="rtree-lg"><i className="rtree-lg-line unreachable" /> provider unreachable</span>
          <span className="rtree-lg"><i className="rtree-lg-line disabled" /> disabled</span>
          <span className="rtree-lg-note">
            thicker = higher priority{readOnly ? "" : " · hold a provider node to enable/disable"}
          </span>
        </div>
      </div>
    </>
  );
}
