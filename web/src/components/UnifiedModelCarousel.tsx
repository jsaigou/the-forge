import {
  lazy, Suspense, useEffect, useLayoutEffect, useMemo, useRef, useState,
  type PointerEvent,
} from "react";
import type { ConfigCard, ModelCard, Status } from "../lib/types";
import { ConfigCardView } from "./ConfigCardView";
import { ModelCardView } from "./ModelCardView";
import { PlayingCard } from "./PlayingCard";

// Operator feedback 2026-08-14 (config carousel): the Console's config
// gallery rides this same deck, so the carousel is card-agnostic — models
// and configs share the shape it needs (id/name/family/logo/creator and
// derived.reliability for usage ordering); genealogy is model-only.
type CardUnion = ModelCard | ConfigCard;
const cardGenealogy = (c: CardUnion): string => ("genealogy" in c ? c.genealogy : "");
const byUsage = (a: CardUnion, b: CardUnion) =>
  (b.derived.reliability?.loads_ok ?? 0) - (a.derived.reliability?.loads_ok ?? 0);

// Mobile F7: detail views are click-reached only — out of the initial bundle.
const DetailModal = lazy(() => import("./detail/DetailModal").then((m) => ({ default: m.DetailModal })));

// UnifiedModelCarousel — the model catalog as a deck of playing cards
// (playing-card redesign 2026-08-14):
//
//   - Family cards ride an ARCH path, not a straight line: the track does
//     horizontal placement while every slide carries its own parabola drop,
//     tangent tilt and depth scale as a function of its distance from
//     center. Dragging scrubs the arch continuously; releasing coasts with
//     exponential friction and snaps to the nearest family.
//   - Multi-model families STACK as a corner fan of their real member
//     cards (rotated about the bottom-left corner) — tight off-center,
//     spread when centered, a little wider on hover.
//   - Tapping the centered stack FANS THE MEMBERS OUT from under the
//     family card (the deck stays at the left): swipe to cycle which
//     member is up, tap the up card to zoom it into the hero. Esc, the
//     back button, the deck, or browser Back collapse the fan again.
//   - Momentum physics are unchanged from the 2026-08-14 rebuild: slides
//     follow the pointer 1:1, fling distance = velocity × τ, arrows/dots
//     use a short overshooting snap. Pointer capture is still acquired
//     only after the drag crosses the click threshold (capturing on
//     pointerdown retargets the gesture's click and kills real taps).

const UNGROUPED = "Other";

interface FamilyEntry {
  family: string;
  genealogy: string;
  cards: CardUnion[];
  sorted: CardUnion[];
}

function groupByGenealogyThenFamily(cards: CardUnion[]): FamilyEntry[] {
  const byFamily = new Map<string, CardUnion[]>();
  const genealogyOfFamily = new Map<string, string>();
  for (const card of cards) {
    const family = card.family || UNGROUPED;
    const group = byFamily.get(family);
    if (group) group.push(card);
    else byFamily.set(family, [card]);
    if (!genealogyOfFamily.has(family)) genealogyOfFamily.set(family, cardGenealogy(card));
  }

  const byGenealogy = new Map<string, [string, CardUnion[]][]>();
  for (const [family, familyCards] of byFamily) {
    const genealogy = family === UNGROUPED ? "" : genealogyOfFamily.get(family) ?? "";
    const list = byGenealogy.get(genealogy);
    if (list) list.push([family, familyCards]);
    else byGenealogy.set(genealogy, [[family, familyCards]]);
  }

  const genealogyGroups = [...byGenealogy.entries()].map(([genealogy, families]) => ({
    genealogy,
    families: families.sort(([a], [b]) => {
      if (a === UNGROUPED) return 1;
      if (b === UNGROUPED) return -1;
      return a.localeCompare(b);
    }),
  }));

  genealogyGroups.sort((a, b) => {
    if (a.genealogy === "" && b.genealogy === "") return 0;
    if (a.genealogy === "") return 1;
    if (b.genealogy === "") return -1;
    return a.genealogy.localeCompare(b.genealogy);
  });

  const entries: FamilyEntry[] = [];
  for (const g of genealogyGroups) {
    for (const [family, familyCards] of g.families) {
      entries.push({ family, genealogy: g.genealogy, cards: familyCards, sorted: [...familyCards].sort(byUsage) });
    }
  }
  return entries;
}

// Friction time constant for the fling glide (ms). Fling travel =
// velocity(px/ms) × τ — bigger τ = slipperier. ~400ms gives a noticeable
// skate without the "blew past six cards" blowout.
const FRICTION_TAU = 400;
// Snap transition used by arrow/dot/keyboard moves: short and slightly
// overshooting so discrete moves read as crisp, not gliding.
const SNAP_MS = 180;
const SNAP_EASE = "cubic-bezier(0.34, 1.2, 0.4, 1)";
// Fling glide: long exponential ease-out.
const FLING_EASE = "cubic-bezier(0.2, 0.8, 0.2, 1)";
// Fan-out open/close easing — slight overshoot on open so the deal feels
// physical.
const FAN_OPEN_MS = 360;
const FAN_OPEN_EASE = "cubic-bezier(0.25, 1.05, 0.35, 1)";
const FAN_CLOSE_MS = 300;
const FAN_CLOSE_EASE = "cubic-bezier(0.4, 0.7, 0.3, 1)";
const DRAG_CLICK_THRESHOLD = 6;

// Arch-path geometry (family carousel): drop in px at |rel| = 1 (parabola
// beyond), tilt in deg at |rel| = 1, capped so far slides don't spin.
const ARCH_DROP = 26;
const ARCH_TILT = 7.5;
const ARCH_MAX_TILT = 13;
const ARCH_MAX_REL = 2.6;

// Fan-out geometry: cards overlap like a held hand (gap is a fraction of
// the slide width), on a shallow arch; the up card sits slightly proud.
const FAN_GAP_RATIO = 0.52;
const FAN_ARC = 16;
const FAN_TILT = 5.5;
const FAN_MAX_TILT = 11;
// Corner-fan stacks peek at most this many real member cards.
const PEEK_CAP = 5;

type Rect = { x: number; y: number; width: number; height: number };

export function UnifiedModelCarousel({
  cards,
  status,
  displayCurrency,
  variant = "model",
}: {
  cards: CardUnion[];
  status: Status;
  displayCurrency: string;
  // "config" renders ConfigCardView slides and opens DetailModal's config
  // kind (no FLIP rect — the config hero doesn't take fromRect); "model"
  // is the original Models-page behavior.
  variant?: "model" | "config";
}) {
  const noun = variant === "model" ? "model" : "config";
  // Operator feedback 2026-08-14: the config deck is FLAT — one slide per
  // config, no family grouping/fan (the Console's sort toggle orders it);
  // the model deck keeps genealogy→family grouping.
  const entries = useMemo(
    () =>
      variant === "config"
        ? cards.map((card) => ({ family: card.name, genealogy: "", cards: [card], sorted: [card] }))
        : groupByGenealogyThenFamily(cards),
    [cards, variant],
  );

  const [activeIndex, setActiveIndex] = useState(0);
  const [dragOffset, setDragOffset] = useState(0);
  const [stridePx, setStridePx] = useState(364);
  // Fan-out state: which family is fanned, its open/close phase, and which
  // member card is currently "up".
  const [fanIndex, setFanIndex] = useState<number | null>(null);
  const [fanPhase, setFanPhase] = useState<"enter" | "open" | "exit">("enter");
  const [fanFocus, setFanFocus] = useState(0);
  // Zoomed hero (tapped card) — rendered through DetailModal (model kind
  // with FLIP rect, or config kind, per variant).
  const [zoom, setZoom] = useState<{ card: CardUnion; rect?: Rect } | null>(null);
  // Measured slide/stage metrics (ResizeObserver) — the fan-out overlay
  // positions in px, so it needs real numbers, not CSS vars.
  const [slideW, setSlideW] = useState(350);
  const [stageW, setStageW] = useState(900);

  const umcRef = useRef<HTMLDivElement>(null);
  const viewportRef = useRef<HTMLDivElement>(null);
  const trackRef = useRef<HTMLDivElement>(null);
  const stageRef = useRef<HTMLDivElement>(null);
  const drag = useRef({
    active: false,
    id: -1,
    startX: 0,
    dx: 0,
    stride: 1,
    moved: false,
    mode: "family" as "family" | "fan",
    samples: [] as { x: number; t: number }[],
  });
  const reduceMotion = useRef(typeof window !== "undefined" && window.matchMedia("(prefers-reduced-motion: reduce)").matches);
  const didPushRef = useRef(false);
  const fanIndexRef = useRef<number | null>(null);
  const exitTimer = useRef<number | null>(null);
  const deckRef = useRef<HTMLDivElement>(null);
  // FLIP source rects for the fan-out (operator feedback 2026-08-14: "the
  // animations should treat the cards like actual objects — cards can move,
  // animate or fade away but they shouldn't magically appear in a new
  // location"). Captured from the centered family stack right before the
  // fan mounts (and again before collapse): each fanned member card deals
  // out from the exact pose its stacked twin held on the carousel, and
  // collapses back into it. Members beyond the peek cap deal from the
  // family face, which itself FLIPs into the deck.
  const flipRef = useRef<{ stage: Rect; face: Rect; peeks: Rect[] } | null>(null);

  useEffect(() => {
    if (activeIndex > entries.length - 1) {
      setActiveIndex(Math.max(0, entries.length - 1));
    }
    if (fanIndex !== null && fanIndex >= entries.length) {
      setFanIndex(null);
      setFanPhase("enter");
    }
  }, [entries.length, activeIndex, fanIndex]);

  // Back-navigation: fanning out pushes a phantom history entry so Back
  // collapses to the family carousel before leaving the page.
  useEffect(() => {
    function onPopState() {
      didPushRef.current = false;
      if (fanIndexRef.current !== null) collapseFan();
    }
    window.addEventListener("popstate", onPopState);
    return () => window.removeEventListener("popstate", onPopState);
  }, []);
  fanIndexRef.current = fanIndex;

  // Keep slide/stage metrics fresh — the slide width is a CSS var on .umc
  // (350px desktop, 280px phone), the stage width drives the fan-out
  // gather pose (cards retreat into the deck at the left edge).
  useLayoutEffect(() => {
    const el = umcRef.current;
    if (!el) return;
    const measure = () => {
      const w = parseFloat(getComputedStyle(el).getPropertyValue("--umc-slide-w"));
      if (w > 0) setSlideW(w);
      setStageW(el.clientWidth);
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  // Fan-out open: mount at the FLIP start pose (the stacked poses captured
  // pre-expand — no transition on first paint), then on the next frame set
  // the spring easing and the spread pose so the cards visibly deal out
  // from the stack. The family face FLIPs into the deck the same way.
  useEffect(() => {
    if (fanIndex === null || fanPhase !== "enter") return;
    const deck = deckRef.current;
    const f = flipRef.current;
    if (deck && f && !reduceMotion.current) {
      const rest = deck.getBoundingClientRect();
      if (rest.width > 0) {
        deck.style.transformOrigin = "0 0";
        deck.style.transition = "none";
        deck.style.transform = `translate(${(f.face.x - rest.left).toFixed(1)}px, ${(f.face.y - rest.top).toFixed(1)}px) scale(${(f.face.width / rest.width).toFixed(3)})`;
      }
    }
    let raf2 = 0;
    const raf1 = requestAnimationFrame(() => {
      raf2 = requestAnimationFrame(() => {
        setAnim(reduceMotion.current ? 0 : FAN_OPEN_MS, FAN_OPEN_EASE);
        if (deck && f && !reduceMotion.current) {
          deck.style.transition = `transform ${FAN_OPEN_MS}ms ${FAN_OPEN_EASE}`;
          deck.style.transform = "";
        }
        setFanPhase("open");
      });
    });
    return () => { cancelAnimationFrame(raf1); cancelAnimationFrame(raf2); };
  }, [fanIndex, fanPhase]);

  useEffect(() => () => {
    if (exitTimer.current !== null) window.clearTimeout(exitTimer.current);
  }, []);

  // Operator feedback 2026-08-14: "Left and right keys don't work for
  // navigation" — they only fired while the .umc region held focus. Keys are
  // now global (window-level), with ↓ = drill deeper (fan out / open hero)
  // and ↑ = back a level (collapse the fan). Inputs and open detail modals
  // own the keyboard and are left alone. Lives above the entries.length
  // early return so the hooks run unconditionally.
  const keyHandlerRef = useRef<(e: globalThis.KeyboardEvent) => void>(() => {});
  keyHandlerRef.current = (e) => {
    if (entries.length === 0) return;
    const t = e.target as HTMLElement | null;
    if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT" || t.isContentEditable)) return;
    if (document.querySelector(".modal-backdrop")) return;
    const sa = Math.min(activeIndex, entries.length - 1);
    if (fanIndex !== null) {
      if (e.key === "ArrowLeft") { e.preventDefault(); fanAdvance(-1); }
      else if (e.key === "ArrowRight") { e.preventDefault(); fanAdvance(1); }
      else if (e.key === "Escape" || e.key === "ArrowUp") { e.preventDefault(); userCollapse(); }
      else if (e.key === "ArrowDown") { e.preventDefault(); openFocusedFanCard(); }
      return;
    }
    if (e.key === "ArrowLeft") { e.preventDefault(); advance(-1); }
    else if (e.key === "ArrowRight") { e.preventDefault(); advance(1); }
    else if (e.key === "ArrowDown") {
      e.preventDefault();
      const en = entries[sa];
      if (en.cards.length > 1) expand(sa);
      else openZoom(en.sorted[0]);
    }
  };
  useEffect(() => {
    function onKey(e: globalThis.KeyboardEvent) { keyHandlerRef.current(e); }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  if (entries.length === 0) return null;

  const safeActive = Math.min(activeIndex, entries.length - 1);
  const fanEntry = fanIndex !== null && fanIndex < entries.length ? entries[fanIndex] : null;
  const fanGapPx = Math.max(120, Math.round(slideW * FAN_GAP_RATIO));
  // Fractional positions (drag scrubs these continuously).
  const fNow = safeActive - dragOffset / stridePx;
  const ffNow = fanFocus - dragOffset / fanGapPx;

  // ── animation plumbing ──────────────────────────────────────────────
  // Both the track/slide transforms (family mode) and the fan-card
  // transforms transition through these vars; 0s while dragging, real
  // easing on commit.
  function setAnim(durMs: number, ease: string) {
    const el = umcRef.current;
    if (!el) return;
    el.style.setProperty("--umc-anim-dur", `${durMs}ms`);
    el.style.setProperty("--umc-anim-ease", ease);
  }

  // ── geometry ────────────────────────────────────────────────────────
  const trackTransform = (f: number) =>
    `translateX(calc(50% - var(--umc-slide-w) / 2 - ${f} * (var(--umc-slide-w) + var(--umc-slide-gap))))`;

  function archCss(rel: number): string {
    const r = Math.max(-ARCH_MAX_REL, Math.min(ARCH_MAX_REL, rel));
    const y = ARCH_DROP * r * r;
    const rot = Math.max(-ARCH_MAX_TILT, Math.min(ARCH_MAX_TILT, r * ARCH_TILT));
    const s = 1 - Math.min(0.12, Math.abs(r) * 0.05);
    return `translateY(${y.toFixed(1)}px) rotate(${rot.toFixed(2)}deg) scale(${s.toFixed(3)})`;
  }

  function fanCss(rel: number): { transform: string; zIndex: number } {
    const x = rel * fanGapPx;
    const y = FAN_ARC * rel * rel;
    const rot = Math.max(-FAN_MAX_TILT, Math.min(FAN_MAX_TILT, rel * FAN_TILT));
    const s = Math.abs(rel) < 0.5 ? 1.03 : 1 - Math.min(0.09, Math.abs(rel) * 0.035);
    return {
      transform: `translateX(${x.toFixed(1)}px) translateY(${y.toFixed(1)}px) rotate(${rot.toFixed(2)}deg) scale(${s.toFixed(3)})`,
      zIndex: Math.max(1, 200 - Math.round(Math.abs(rel) * 40)),
    };
  }

  // Where a member card sits while gathered under the deck (fan enter/exit
  // fallback when no FLIP source was captured — e.g. reduced motion).
  function gatherPose(j: number): { transform: string; zIndex: number } {
    const deckLeft = Math.min(36, Math.max(4, stageW * 0.03));
    const dx = deckLeft + (slideW * 0.6) / 2 - stageW / 2;
    return {
      transform: `translateX(${(dx + j * 2).toFixed(1)}px) translateY(8px) rotate(-7deg) scale(0.62)`,
      zIndex: 150 - j,
    };
  }

  // The fanned slide width RIGHT NOW (the CSS var flips 262→350 when .umc
  // gains .fanned, but the slideW state lags a ResizeObserver tick — poses
  // computed during enter/exit must use the live value).
  function liveSlideW(): number {
    const w = parseFloat(getComputedStyle(umcRef.current ?? document.documentElement).getPropertyValue("--umc-slide-w"));
    return w > 0 ? w : slideW;
  }

  // Capture the centered family stack's poses (stage-relative) as the FLIP
  // source for the next fan enter/exit.
  function captureFlipSource() {
    const stage = umcRef.current;
    const slide = trackRef.current?.querySelectorAll<HTMLElement>(".umc-slide")[safeActive];
    if (!stage || !slide || reduceMotion.current) { flipRef.current = null; return; }
    const s = stage.getBoundingClientRect();
    const face = slide.querySelector<HTMLElement>(".umc-stack-face")?.getBoundingClientRect();
    if (!face || face.width === 0) { flipRef.current = null; return; }
    const peeks = [...slide.querySelectorAll<HTMLElement>(".umc-peek")]
      .map((el) => el.getBoundingClientRect())
      .filter((r) => r.width > 0);
    flipRef.current = {
      stage: { x: s.left, y: s.top, width: s.width, height: s.height },
      face: { x: face.left, y: face.top, width: face.width, height: face.height },
      peeks: peeks.map((r) => ({ x: r.left, y: r.top, width: r.width, height: r.height })),
    };
  }

  // Where fan card j begins/ends its deal: the exact stacked pose its twin
  // held on the family carousel (FLIP). The arch wrapper's transform-origin
  // is center, so translate between centers and scale by width ratio.
  // Members beyond the peek cap were hidden behind the face — they deal
  // from the face rect (they emerge from under the family card).
  function flipStartPose(j: number): { transform: string; zIndex: number } | null {
    const f = flipRef.current;
    if (!f) return null;
    const src = j < f.peeks.length ? f.peeks[j] : f.face;
    const w = liveSlideW();
    const restCX = f.stage.width / 2;
    const restCY = 30 + (w * 7) / 10; // 5:7 card
    const dx = src.x + src.width / 2 - restCX;
    const dy = src.y + src.height / 2 - restCY;
    const s = Math.max(0.2, src.width / w);
    return {
      transform: `translateX(${dx.toFixed(1)}px) translateY(${dy.toFixed(1)}px) scale(${s.toFixed(3)})`,
      zIndex: Math.max(1, 200 - j * 40),
    };
  }

  function measureStride(): number {
    const slide = trackRef.current?.querySelector<HTMLElement>(".umc-slide");
    // offsetWidth — transforms (arch scale) lie to getBoundingClientRect.
    const w = slide ? slide.offsetWidth : slideW;
    const gap = parseFloat(getComputedStyle(umcRef.current ?? document.documentElement).getPropertyValue("--umc-slide-gap")) || 14;
    return w + gap;
  }

  // ── commits ─────────────────────────────────────────────────────────
  function landFamily(target: number, kind: "fling" | "snap", flingDist = 0) {
    if (reduceMotion.current) {
      setAnim(0, "linear");
      setDragOffset(0);
      setActiveIndex(target);
      return;
    }
    const dur = kind === "snap" ? SNAP_MS : Math.min(900, 260 + Math.abs(flingDist) * 0.9);
    setAnim(dur, kind === "snap" ? SNAP_EASE : FLING_EASE);
    setDragOffset(0);
    setActiveIndex(target);
  }

  function landFan(target: number, kind: "fling" | "snap", flingDist = 0) {
    if (reduceMotion.current) {
      setAnim(0, "linear");
      setDragOffset(0);
      setFanFocus(target);
      return;
    }
    const dur = kind === "snap" ? SNAP_MS : Math.min(700, 220 + Math.abs(flingDist) * 0.8);
    setAnim(dur, kind === "snap" ? SNAP_EASE : FLING_EASE);
    setDragOffset(0);
    setFanFocus(target);
  }

  // ── shared pointer physics (family arch + fan arch) ─────────────────
  function onPointerDown(e: PointerEvent) {
    if (e.pointerType === "mouse" && e.button !== 0) return;
    const mode = fanIndex === null ? "family" : "fan";
    const strideNow = mode === "family" ? measureStride() : fanGapPx;
    drag.current = {
      active: true, id: e.pointerId, startX: e.clientX, dx: 0,
      stride: strideNow, moved: false, mode,
      samples: [{ x: e.clientX, t: performance.now() }],
    };
    if (mode === "family") setStridePx(strideNow);
    setAnim(0, "linear");
    // NOTE: no setPointerCapture here — capturing on pointerdown retargets
    // the gesture's click to the viewport and kills every card tap (found
    // in the 2026-08-14 visual pass). Capture is acquired in onPointerMove
    // once the drag is real.
  }

  function onPointerMove(e: PointerEvent) {
    const d = drag.current;
    if (!d.active || e.pointerId !== d.id) return;
    const dx = e.clientX - d.startX;
    d.dx = dx;
    if (Math.abs(dx) > DRAG_CLICK_THRESHOLD) {
      d.moved = true;
      try { (e.currentTarget as HTMLElement).setPointerCapture?.(e.pointerId); } catch { /* already captured */ }
    }
    const now = performance.now();
    d.samples.push({ x: e.clientX, t: now });
    if (d.samples.length > 8) d.samples.shift();

    const base = d.mode === "family" ? safeActive : fanFocus;
    const count = d.mode === "family" ? entries.length : (fanEntry?.sorted.length ?? 0);
    // Rubber-band at the ends.
    let off = dx;
    const minOff = -(count - 1 - base) * d.stride;
    const maxOff = base * d.stride;
    if (off < minOff) off = minOff + (off - minOff) * 0.4;
    if (off > maxOff) off = maxOff + (off - maxOff) * 0.4;

    // Direct DOM writes — one per card — keep the arch pinned to the
    // pointer between React renders.
    if (d.mode === "family") {
      const f = base - off / d.stride;
      const track = trackRef.current;
      if (track) {
        track.style.transform = trackTransform(f);
        track.querySelectorAll<HTMLElement>(".umc-slide").forEach((el, i) => {
          // Arch pose goes on the inner wrapper — the slide stays
          // untransformed so its click box hit-tests reliably (WebKit).
          const arch = el.querySelector<HTMLElement>(".umc-arch");
          if (arch) arch.style.transform = archCss(i - f);
          el.style.zIndex = String(Math.max(0, 100 - Math.round(Math.abs(i - f) * 10)));
        });
      }
    } else {
      const ff = base - off / d.stride;
      stageRef.current?.querySelectorAll<HTMLElement>(".umc-fan-card").forEach((el, j) => {
        const pose = fanCss(j - ff);
        const arch = el.querySelector<HTMLElement>(".umc-fan-arch");
        if (arch) arch.style.transform = pose.transform;
        el.style.zIndex = String(pose.zIndex);
      });
    }
    setDragOffset(off); // keep state in sync for transitionless re-render safety
  }

  function onPointerUp(e: PointerEvent) {
    const d = drag.current;
    if (!d.active || e.pointerId !== d.id) return;
    d.active = false;
    try { (e.currentTarget as HTMLElement).releasePointerCapture?.(e.pointerId); } catch { /* not captured */ }
    if (!d.moved) {
      // Plain release with no drag — nothing to animate.
      setDragOffset(0);
      return;
    }
    // Velocity from the trailing ~120ms of samples (px/ms); exponential
    // friction projects the coast distance = v × τ.
    let v = 0;
    const now = performance.now();
    const recent = d.samples.filter((s) => now - s.t <= 120);
    if (recent.length >= 2) {
      const first = recent[0];
      const last = recent[recent.length - 1];
      if (last.t - first.t > 0) v = (last.x - first.x) / (last.t - first.t);
    }
    const projected = reduceMotion.current ? d.dx : d.dx + v * FRICTION_TAU;
    const slidesMoved = Math.round(projected / d.stride);
    if (d.mode === "family") {
      const target = Math.max(0, Math.min(entries.length - 1, safeActive - slidesMoved));
      landFamily(target, "fling", projected);
    } else {
      const n = fanEntry?.sorted.length ?? 1;
      const target = Math.max(0, Math.min(n - 1, fanFocus - slidesMoved));
      landFan(target, "fling", projected);
    }
  }

  // ── navigation ──────────────────────────────────────────────────────
  function advance(dir: number) {
    const next = Math.max(0, Math.min(entries.length - 1, safeActive + dir));
    if (next !== safeActive) landFamily(next, "snap");
  }

  function fanAdvance(dir: number) {
    const n = fanEntry?.sorted.length ?? 1;
    const next = Math.max(0, Math.min(n - 1, Math.round(fanFocus) + dir));
    if (next !== fanFocus) landFan(next, "snap");
  }

  function expand(i: number) {
    didPushRef.current = true;
    try { history.pushState(history.state, ""); } catch { didPushRef.current = false; }
    captureFlipSource();
    setFanFocus(0);
    setDragOffset(0);
    setFanPhase("enter");
    setFanIndex(i);
  }

  // State-only collapse (safe for both user-initiated and popstate paths —
  // the exit timer guard makes double invocation a no-op). The fan cards
  // deal back into the (still-mounted, dimmed) family stack via FLIP.
  function collapseFan() {
    if (exitTimer.current !== null) return;
    captureFlipSource();
    setAnim(reduceMotion.current ? 0 : FAN_CLOSE_MS, FAN_CLOSE_EASE);
    const deck = deckRef.current;
    const f = flipRef.current;
    if (deck && f && !reduceMotion.current) {
      const rest = deck.getBoundingClientRect();
      if (rest.width > 0) {
        deck.style.transformOrigin = "0 0";
        deck.style.transition = `transform ${FAN_CLOSE_MS}ms ${FAN_CLOSE_EASE}`;
        deck.style.transform = `translate(${(f.face.x - rest.left).toFixed(1)}px, ${(f.face.y - rest.top).toFixed(1)}px) scale(${(f.face.width / rest.width).toFixed(3)})`;
      }
    }
    setFanPhase("exit");
    exitTimer.current = window.setTimeout(() => {
      exitTimer.current = null;
      flipRef.current = null;
      setFanIndex(null);
      setFanPhase("enter");
      setDragOffset(0);
    }, reduceMotion.current ? 0 : FAN_CLOSE_MS + 30);
  }

  function userCollapse() {
    collapseFan();
    if (didPushRef.current) {
      didPushRef.current = false;
      try { history.back(); } catch { /* no entry to pop */ }
    }
  }

  function openZoom(card: CardUnion, el?: HTMLElement) {
    let rect: Rect | undefined;
    if (el) {
      const r = el.getBoundingClientRect();
      rect = { x: r.left, y: r.top, width: r.width, height: r.height };
    }
    setZoom({ card, rect });
  }

  function openFocusedFanCard() {
    if (!fanEntry) return;
    const j = Math.max(0, Math.min(fanEntry.sorted.length - 1, Math.round(fanFocus)));
    const card = stageRef.current?.querySelectorAll<HTMLElement>(".umc-fan-card")[j];
    openZoom(fanEntry.sorted[j], card?.querySelector<HTMLElement>(".umc-fan-arch") ?? card);
  }

  // Tap behavior (family mode): a tap on a non-centered slide centers it
  // ("pick it up first"); a tap on the centered stack fans it out.
  function slideClick(i: number) {
    if (drag.current.moved) return; // a drag that released — not a click
    if (i !== safeActive) {
      landFamily(i, "snap");
      return;
    }
    expand(i);
  }

  // One renderer for both card kinds — the deck mechanics are shared, the
  // card face is not (variant prop, operator feedback 2026-08-14).
  function cardView(card: CardUnion, interactive: boolean) {
    return variant === "config" ? (
      <ConfigCardView card={card as ConfigCard} status={status} schedulerStatus={undefined} displayCurrency={displayCurrency} interactive={interactive} />
    ) : (
      <ModelCardView card={card as ModelCard} status={status} displayCurrency={displayCurrency} interactive={interactive} />
    );
  }

  function renderFamilyFace(entry: FamilyEntry, hint: string) {
    const first = entry.sorted[0];
    const redundant = entry.genealogy !== "" && entry.family.toLowerCase() === entry.genealogy.toLowerCase();
    const memberNames = entry.sorted.slice(0, 3).map((m) => m.name).join("  ·  ");
    return (
      <PlayingCard
        name={entry.family}
        logo={first.logo}
        logoDark={first.logo_dark}
        watermarkName={first.creator}
        className="umc-card"
      >
        {entry.genealogy && !redundant && <div className="umc-genealogy">{entry.genealogy}</div>}
        <div className="umc-count">{entry.cards.length} {noun}{entry.cards.length === 1 ? "" : "s"}</div>
        {entry.cards.length > 1 && (
          <div className="umc-members">{memberNames}{entry.cards.length > 3 ? " · …" : ""}</div>
        )}
        {hint && <div className="umc-hint">{hint}</div>}
      </PlayingCard>
    );
  }

  return (
    <div
      ref={umcRef}
      className={`umc${fanIndex !== null ? " fanned" : ""}`}
      tabIndex={0}
      role="region"
      aria-label={variant === "model" ? "Model families" : "Config families"}
    >
      <div
        ref={viewportRef}
        className="umc-viewport"
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerUp}
      >
        <div
          ref={trackRef}
          className="umc-track"
          style={{ transform: trackTransform(fNow) }}
        >
          {entries.map((entry, i) => {
            const firstCard = entry.sorted[0];
            const single = entry.cards.length === 1;
            const rel = i - fNow;
            const active = Math.abs(rel) < 0.5;
            // zIndex only — the arch pose lives on the inner .umc-arch so
            // the slide's own box stays untransformed (reliable clicks).
            const slideStyle = {
              zIndex: Math.max(0, 100 - Math.round(Math.abs(rel) * 10)),
            };
            const slideCls = `umc-slide${active ? " active" : ""}${single ? "" : " stacked"}`;
            const key = `${entry.genealogy || "__standalone__"}-${entry.family}`;
            if (single) {
              return (
                <div key={key} className={slideCls} style={slideStyle}>
                  <div className="umc-arch" style={{ transform: archCss(rel) }}>
                    {/* After a drag, swallow the card's own click (capture beats
                        its bubble handler); the modal portals to <body>. A real
                        tap/click on a non-centered slide centers it first —
                        same contract as the multi-model slides — instead of
                        opening the detail for a card the user was just trying
                        to pick up. Keyboard activation (e.detail 0) still opens
                        directly. */}
                    <div
                      onClickCapture={(e) => {
                        if (drag.current.moved) { e.stopPropagation(); return; }
                        if (i !== safeActive && e.detail > 0) { e.stopPropagation(); landFamily(i, "snap"); }
                      }}
                    >
                      {cardView(firstCard, true)}
                    </div>
                  </div>
                </div>
              );
            }
            return (
              <div
                key={key}
                className={slideCls}
                style={slideStyle}
                onClick={() => slideClick(i)}
                role="button"
                tabIndex={0}
                aria-label={active ? `${entry.family}: ${entry.cards.length} ${noun}s. Tap to fan out.` : `${entry.family}: ${entry.cards.length} ${noun}s. Tap to center.`}
                onKeyDown={(e) => {
                  if (e.key === "Enter" || e.key === " ") {
                    e.preventDefault();
                    if (i !== safeActive) landFamily(i, "snap");
                    else expand(i);
                  }
                }}
              >
                <div className="umc-arch" style={{ transform: archCss(rel) }}>
                  {/* Corner fan of REAL member cards behind the family face —
                      tight off-center, spread at center, wider on hover. */}
                  <div className="umc-stack">
                    {entry.sorted.slice(0, PEEK_CAP).map((m, k) => (
                      <div
                        key={m.id}
                        className="umc-peek"
                        aria-hidden="true"
                        style={{ zIndex: 20 - k, ["--peek-i" as string]: k + 1 }}
                      >
                        {cardView(m, false)}
                      </div>
                    ))}
                    <div className="umc-stack-face">
                      {renderFamilyFace(entry, active ? "Tap to fan out" : "Tap to center")}
                    </div>
                  </div>
                </div>
              </div>
            );
          })}
        </div>
      </div>

      {fanIndex === null && entries.length > 1 && (
        <>
          <button
            type="button"
            className="umc-arrow umc-arrow-left"
            onClick={() => advance(-1)}
            disabled={safeActive === 0}
            aria-label="Previous family"
          >
            ‹
          </button>
          <button
            type="button"
            className="umc-arrow umc-arrow-right"
            onClick={() => advance(1)}
            disabled={safeActive === entries.length - 1}
            aria-label="Next family"
          >
            ›
          </button>
          <div className="umc-dots">
            {entries.map((entry, i) => (
              <button
                key={i}
                type="button"
                className={`umc-dot${i === safeActive ? " active" : ""}`}
                onClick={() => { if (i !== safeActive) landFamily(i, "snap"); }}
                aria-label={`Go to ${entry.family}`}
              />
            ))}
          </div>
        </>
      )}

      {fanEntry && (
        <div
          className={`umc-fanout${fanPhase === "exit" ? " exit" : ""}`}
          onClick={(e) => { if (e.target === e.currentTarget) userCollapse(); }}
        >
          <div
            ref={stageRef}
            className="umc-fan-stage"
            role="region"
            aria-label={`${fanEntry.family} ${noun}s — swipe to browse, tap the front card to zoom`}
            onPointerDown={onPointerDown}
            onPointerMove={onPointerMove}
            onPointerUp={onPointerUp}
            onPointerCancel={onPointerUp}
            onClick={(e) => { if (e.target === e.currentTarget) userCollapse(); }}
          >
            {/* The family card stays as the deck the members fan out from;
                clicking it collapses back to the family carousel. */}
            <div
              ref={deckRef}
              className="umc-fan-deck"
              onClick={userCollapse}
              role="button"
              tabIndex={0}
              aria-label={`Collapse the ${fanEntry.family} fan`}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") { e.preventDefault(); userCollapse(); }
              }}
            >
              {renderFamilyFace(fanEntry, "")}
            </div>
            {fanEntry.sorted.map((m, j) => {
              // enter/exit poses are the FLIP start poses (the stacked twin's
              // exact carousel rect) when captured, else the legacy gather.
              const pose = fanPhase === "open" ? fanCss(j - ffNow) : (flipStartPose(j) ?? gatherPose(j));
              const up = Math.abs(j - ffNow) < 0.5;
              return (
                <div
                  key={m.id}
                  className={`umc-fan-card${up ? " up" : ""}`}
                  style={{ zIndex: pose.zIndex }}
                  role="button"
                  tabIndex={fanPhase === "open" && j === Math.round(fanFocus) ? 0 : -1}
                  aria-label={`${m.name}${up ? " — up card, tap to zoom" : ""}`}
                  onClick={(e) => {
                    if (drag.current.moved) return;
                    if (!up) { landFan(j, "snap"); return; }
                    // Rect for the hero FLIP: measure the transformed arch
                    // wrapper (what the user actually sees and taps).
                    openZoom(m, e.currentTarget.querySelector<HTMLElement>(".umc-fan-arch") ?? e.currentTarget);
                  }}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      openZoom(m, e.currentTarget.querySelector<HTMLElement>(".umc-fan-arch") ?? e.currentTarget);
                    }
                  }}
                >
                  <div className="umc-fan-arch" style={{ transform: pose.transform }}>
                    {cardView(m, false)}
                  </div>
                </div>
              );
            })}
          </div>
          {fanEntry.sorted.length > 1 && (
            <>
              <button
                type="button"
                className="umc-arrow umc-arrow-left"
                onClick={() => fanAdvance(-1)}
                disabled={fanFocus <= 0}
                aria-label={`Previous ${noun}`}
              >
                ‹
              </button>
              <button
                type="button"
                className="umc-arrow umc-arrow-right"
                onClick={() => fanAdvance(1)}
                disabled={fanFocus >= fanEntry.sorted.length - 1}
                aria-label={`Next ${noun}`}
              >
                ›
              </button>
              <div className="umc-dots">
                {fanEntry.sorted.map((m, i) => (
                  <button
                    key={m.id}
                    type="button"
                    className={`umc-dot${i === Math.round(fanFocus) ? " active" : ""}`}
                    onClick={() => { if (i !== fanFocus) landFan(i, "snap"); }}
                    aria-label={`Go to ${m.name}`}
                  />
                ))}
              </div>
            </>
          )}
        </div>
      )}

      {zoom && (
        <Suspense fallback={null}>
          <DetailModal
            initial={
              variant === "config"
                ? { kind: "config", card: zoom.card as ConfigCard }
                : { kind: "model", modelId: zoom.card.id as string, fromRect: zoom.rect }
            }
            status={status}
            schedulerStatus={undefined}
            displayCurrency={displayCurrency}
            onClose={() => setZoom(null)}
          />
        </Suspense>
      )}
    </div>
  );
}
