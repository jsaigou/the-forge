import { useEffect, useLayoutEffect, useRef, useState, type CSSProperties, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { SpotlightCutout, useTrackedRect, type Rect } from "./Spotlight";
import { useOnboarding } from "./useOnboarding";

// Guided first-run tour (Sprint 6, 2026-08-27 — replaces the P4 static
// six-step prose modal). That version never pointed at anything real: no
// DOM targeting, no highlighting, CTAs were plain hash links that dropped
// the operator on a page with no further guidance. This version highlights
// the actual live element for each of the three things worth knowing on
// day one — loading a config, using smith, adding a model — and advances
// only when the operator clicks Next, never by simulating a click itself.
//
// Steps resolve a target via `data-tour-id` (or an existing element id,
// for #model-gallery). Resolution is a short rAF poll, not a single
// synchronous query, because routes are lazy-loaded (App.tsx) and some
// targets depend on react-query data. `targets` is an ordered fallback
// list, not padding: `.load-btn` (data-tour-id="bay-load") only exists in
// the DOM for a bay that is empty, unreserved, and operator-viewed — on a
// box with all four bays loaded it genuinely isn't there, so that step
// falls back to highlighting the bays section as a whole rather than
// showing an empty spotlight.
//
// Escape closes = dismisses (sets the done flag). There is no backdrop click
// to dismiss (the spotlight's dim layer is pointer-events: none by design,
// so the underlying page stays clickable) — Skip (✕) is the explicit escape
// hatch instead.

interface TourStep {
  title: string;
  body: ReactNode;
  hash?: string;
  targets?: string[];
}

const STEPS: TourStep[] = [
  {
    title: "Load bays",
    hash: "console",
    targets: ['[data-tour-id="bays"]'],
    body: (
      <p>
        The Forge loads models onto this box's GPU across four load bays (A1–A4). Request a model through a0
        and the scheduler places it here automatically — you don't normally need to load anything by hand.
      </p>
    ),
  },
  {
    title: "Loading one by hand",
    hash: "console",
    targets: ['[data-tour-id="bay-load"]', '[data-tour-id="bays"]'],
    body: (
      <p>
        A free bay shows a <b>+ Load model</b> button. Clicking it doesn't load anything by itself — it scrolls
        down to the config picker below, where the real Load button lives.
      </p>
    ),
  },
  {
    title: "Pick a config",
    hash: "console",
    targets: ["#model-gallery"],
    body: (
      <p>
        This carousel is the config picker — one card per loadable model configuration. Each card's own{" "}
        <b>Load</b> button opens a confirm dialog and picks (or lets you choose) which bay it lands in.
      </p>
    ),
  },
  {
    title: "Ask smith",
    hash: "console",
    targets: ['[data-tour-id="smith-tray"]'],
    body: (
      <p>
        <b>Smith</b> is the built-in maintenance agent: it runs checks on a schedule, watches for drift and
        failures, and proposes fixes. Click here to expand the chat and ask it anything in plain language.
      </p>
    ),
  },
  {
    title: "Add a model",
    hash: "models",
    targets: ['[data-tour-id="models-add-tab"]'],
    body: (
      <p>
        On the Models page, <b>Add Model</b> searches Hugging Face directly, runs a pre-flight check against
        this hardware, and downloads + auto-registers the result into the catalog.
      </p>
    ),
  },
];

const RESOLVE_TIMEOUT_MS = 2000;
const CARD_MARGIN = 12;
const CARD_WIDTH = 300;

function currentHashTab(): string {
  const raw = location.hash.slice(1);
  const slash = raw.indexOf("/");
  return slash === -1 ? raw : raw.slice(0, slash);
}

function resolveTarget(selectors: string[] | undefined): HTMLElement | null {
  if (!selectors) return null;
  for (const sel of selectors) {
    const el = document.querySelector<HTMLElement>(sel);
    // offsetParent is null for display:none (or unmounted) elements — the
    // one heuristic we need, since none of these targets use fixed
    // positioning themselves.
    if (el && el.offsetParent !== null) return el;
  }
  return null;
}

// Resolves the current step's target, navigating to its hash first if
// needed and polling briefly for the element to appear post-navigation.
//
// Gated on `open`: this must NOT run while the tour is closed. App.tsx's
// own one-time mount effect tags the landing history entry
// (`history.replaceState({app:true,...})`) using a `initial` hash it
// captured at first render, and child effects fire before parent effects
// in the same commit — so an unconditional hash assignment here at mount
// would race that tagging effect and get silently stomped back to
// whatever the original hash was (found live: Replay did nothing because
// this had already "fired" once, uselessly, before the tour ever opened,
// and its dependency never changed again after that). Gating on `open`
// means navigation only ever happens well after that one-time mount
// effect has already run, so there's nothing left to race.
function useStepTarget(step: TourStep, open: boolean): HTMLElement | null {
  const [target, setTarget] = useState<HTMLElement | null>(null);

  useEffect(() => {
    if (!open) {
      setTarget(null);
      return;
    }
    setTarget(null); // clear immediately so a stale cutout never lingers mid-transition

    if (step.hash && currentHashTab() !== step.hash) {
      location.hash = step.hash;
    }

    let cancelled = false;
    let raf = 0;
    const start = performance.now();
    function tick() {
      if (cancelled) return;
      const el = resolveTarget(step.targets);
      if (el) {
        setTarget(el);
        return;
      }
      if (performance.now() - start < RESOLVE_TIMEOUT_MS) {
        raf = requestAnimationFrame(tick);
      }
    }
    tick();

    return () => {
      cancelled = true;
      cancelAnimationFrame(raf);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, open]);

  return target;
}

function cardStyle(rect: Rect | null, cardSize: { width: number; height: number }): CSSProperties {
  if (!rect) {
    return { position: "fixed", top: "50%", left: "50%", transform: "translate(-50%, -50%)", width: CARD_WIDTH };
  }
  const spaceBelow = window.innerHeight - (rect.y + rect.height);
  const placeBelow = spaceBelow >= cardSize.height + CARD_MARGIN || spaceBelow >= rect.y;
  let top = placeBelow ? rect.y + rect.height + CARD_MARGIN : rect.y - cardSize.height - CARD_MARGIN;
  top = Math.max(CARD_MARGIN, Math.min(top, window.innerHeight - cardSize.height - CARD_MARGIN));
  let left = rect.x + rect.width / 2 - cardSize.width / 2;
  left = Math.max(CARD_MARGIN, Math.min(left, window.innerWidth - cardSize.width - CARD_MARGIN));
  return { position: "fixed", top, left, width: CARD_WIDTH };
}

export function OnboardingTour() {
  const { open, dismiss } = useOnboarding();
  const [stepIdx, setStepIdx] = useState(0);
  const cardRef = useRef<HTMLDivElement | null>(null);
  const [cardSize, setCardSize] = useState({ width: CARD_WIDTH, height: 140 });

  // Always start at step 1 on open, including on replay — a partial first
  // run used to leave stepIdx wherever it stopped (a real bug: the tour
  // used to just resume mid-way on replay).
  useEffect(() => {
    if (open) setStepIdx(0);
  }, [open]);

  useEffect(() => {
    if (!open) return;
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") {
        e.preventDefault();
        dismiss();
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, dismiss]);

  useLayoutEffect(() => {
    const el = cardRef.current;
    if (!el) return;
    const ro = new ResizeObserver((entries) => {
      const box = entries[0]?.contentRect;
      if (box) setCardSize({ width: box.width, height: box.height });
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  const clampedIdx = Math.min(stepIdx, STEPS.length - 1);
  const step = STEPS[clampedIdx];
  const isFinal = clampedIdx === STEPS.length - 1;

  const targetEl = useStepTarget(step, open);
  const rect = useTrackedRect(targetEl);

  if (!open) return null;

  return createPortal(
    <>
      {rect && <SpotlightCutout rect={rect} />}
      <div
        ref={cardRef}
        className="tour-card"
        role="dialog"
        aria-modal="true"
        aria-labelledby="onboarding-title"
        data-placement={rect ? "anchored" : "center"}
        style={cardStyle(rect, cardSize)}
      >
        <div style={{ fontSize: 11, color: "var(--text-dim)", marginBottom: 6 }}>
          Getting started · {clampedIdx + 1} of {STEPS.length}
        </div>
        <h3 id="onboarding-title" style={{ marginBottom: 8 }}>{step.title}</h3>
        <div style={{ fontSize: 13, color: "var(--text-dim)", lineHeight: 1.6 }}>{step.body}</div>
        <div className="form-actions" style={{ marginTop: 14 }}>
          {clampedIdx > 0 && (
            <button className="btn" onClick={() => setStepIdx(clampedIdx - 1)}>
              Back
            </button>
          )}
          {!isFinal && (
            <button className="btn primary" onClick={() => setStepIdx(clampedIdx + 1)}>
              Next
            </button>
          )}
          {isFinal && (
            <button className="btn primary" autoFocus onClick={dismiss}>
              Done
            </button>
          )}
          {!isFinal && (
            <button
              className="icon-btn"
              title="Skip tour"
              aria-label="Skip tour"
              style={{ marginLeft: "auto" }}
              onClick={dismiss}
            >
              ✕
            </button>
          )}
        </div>
      </div>
    </>,
    document.body,
  );
}
