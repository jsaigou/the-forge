import type { CSSProperties } from "react";

// SquareSnake — Sprint K's slot active-LLM indicator (amicro.vercel.app,
// Geometric Shapes → Loaders: a 3×3 grid of small rounded squares where
// illumination travels through the cells in a snake path). A badge-sized
// loader icon, not a whole-card animated outline — see
// docs/v5-prerelease-readiness.md's ground-truth correction for why that
// distinction mattered for scope.
//
// Rendered next to Bay.tsx's status dot when a loaded slot's
// status.slot_activity[slot] is true; a static dot otherwise. The snake
// order is boustrophedon (left→right, then right→left, then left→right) —
// a real snake path through the grid, not a plain row-major sweep.
const SNAKE_ORDER = [0, 1, 2, 5, 4, 3, 6, 7, 8];

export function SquareSnake({ title }: { title?: string }) {
  return (
    <span className="square-snake" role="status" aria-label={title ?? "actively generating"} title={title}>
      {SNAKE_ORDER.map((cellIndex, step) => (
        <span key={cellIndex} className="ss-cell" style={{ "--ss-step": step } as CSSProperties} />
      ))}
    </span>
  );
}
