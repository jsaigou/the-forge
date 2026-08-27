import { useEffect, useState } from "react";

// Spotlight — the tracked-rect primitive behind the onboarding tour
// (Sprint 6, replacing the old static-modal tour). A target element's
// getBoundingClientRect() is tracked live (resize/scroll/content-size
// changes) so a body-portaled cutout can follow it without ever cloning
// or re-parenting the real element — the page underneath stays exactly as
// interactive as it was.
//
// Must live in a document.body portal: `.top` is `position: sticky` with a
// `backdrop-filter` (styles/theme.css), which makes it a containing block
// for `position: fixed` descendants, and `.bay`/`.umc-viewport`/`.modal.wide`
// are all `overflow: hidden` — nothing in-tree can host an unclipped
// overlay child. Same reasoning already documented at LoadConfirmModal.tsx
// and DetailModal.tsx.

export type Rect = { x: number; y: number; width: number; height: number };

function measure(el: HTMLElement): Rect {
  const r = el.getBoundingClientRect();
  return { x: r.x, y: r.y, width: r.width, height: r.height };
}

// Tracks `target`'s viewport rect, re-measuring on resize, on scroll
// (capture phase, so it fires for scroll on any ancestor, not just
// window), and on the target's own content-driven size changes via
// ResizeObserver — the same observer mechanism already used by
// UnifiedModelCarousel.tsx for FLIP measurement. Scrolls the target into
// view (centered, so it doesn't land under the sticky header) once, the
// first time it's tracked, following the reduced-motion-aware pattern
// from Settings.tsx's deep-link scroll.
export function useTrackedRect(target: HTMLElement | null): Rect | null {
  const [rect, setRect] = useState<Rect | null>(null);

  useEffect(() => {
    if (!target) {
      setRect(null);
      return;
    }

    const reduce = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    target.scrollIntoView({ behavior: reduce ? "auto" : "smooth", block: "center" });

    let raf = 0;
    function update() {
      setRect(measure(target!));
    }
    update();

    const ro = new ResizeObserver(update);
    ro.observe(target);

    function onReflow() {
      cancelAnimationFrame(raf);
      raf = requestAnimationFrame(update);
    }
    window.addEventListener("resize", onReflow);
    window.addEventListener("scroll", onReflow, true);

    return () => {
      ro.disconnect();
      window.removeEventListener("resize", onReflow);
      window.removeEventListener("scroll", onReflow, true);
      cancelAnimationFrame(raf);
    };
  }, [target]);

  return rect;
}

const CUTOUT_PAD = 8;

// The dimmed page + cutout ring. box-shadow paints the dim OUTSIDE the
// rect (a 9999px spread), so the real element inside shows through with
// zero opacity/filter applied to it — no cloning, no z-index promotion,
// and pointer-events: none keeps the underlying page fully clickable.
export function SpotlightCutout({ rect }: { rect: Rect }) {
  return (
    <div
      className="tour-cutout"
      style={{
        left: rect.x - CUTOUT_PAD,
        top: rect.y - CUTOUT_PAD,
        width: rect.width + CUTOUT_PAD * 2,
        height: rect.height + CUTOUT_PAD * 2,
      }}
    />
  );
}
